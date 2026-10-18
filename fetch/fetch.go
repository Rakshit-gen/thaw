// Package fetch gets snapshot blocks from wherever they are cheapest: the
// host cache, a peer host, or the object store.
//
// Two kinds of work share the network. Demand fetches are blocks the guest
// is stalled on right now. Prefetches are blocks the plan says it will want
// soon. A prefetch that holds the only free connection while the guest
// waits is the classic way a prefetcher makes things slower, so a few
// connections are reserved for demand and prefetch never takes them.
//
// Requests are ranged GETs on packs. Blocks that sit next to each other in
// a pack are fetched with one request, up to MaxSpan bytes, reading through
// holes up to MaxGap. A demand fetch can also bring along the planned
// blocks that follow it in the pack (fault-around): on S3 a request costs
// tens of milliseconds before the first byte, and the next few hundred
// kilobytes after it cost almost nothing.
package fetch

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
)

// Block names one block of one file of a snapshot.
type Block struct {
	File  int
	Index int
}

// Origin says where a block came from.
type Origin uint8

const (
	FromStore Origin = iota
	FromCache
	FromPeer
)

func (o Origin) String() string {
	return [...]string{"store", "cache", "peer"}[o]
}

// Want describes a block for a peer: what it is and where the store has it,
// so a peer that misses can read it through.
type Want struct {
	ID  image.ID
	Loc repo.Loc
	Len int // raw length
}

// Peer is another host's block cache.
type Peer interface {
	// GetMany returns the blocks it could get. Missing ones are absent from
	// the map; an error means the peer could not be asked at all.
	GetMany(ctx context.Context, want []Want) (map[image.ID][]byte, error)
}

// Options tune a Fetcher.
type Options struct {
	Streams  int   // requests in flight at once; 16 if zero
	Reserved int   // of those, kept for demand; 4 if zero, -1 for none
	MaxSpan  int64 // longest coalesced request; 4 MiB if zero
	MaxGap   int64 // biggest hole a request may read through; 256 KiB if zero
	// FaultAround is how many wanted blocks after a demand block, in the
	// same pack, ride along on its request. Zero turns it off.
	FaultAround int
}

func (o *Options) defaults() {
	if o.Streams == 0 {
		o.Streams = 16
	}
	switch {
	case o.Reserved == 0:
		o.Reserved = 4
	case o.Reserved < 0:
		o.Reserved = 0
	}
	if o.Reserved >= o.Streams {
		o.Reserved = o.Streams - 1
	}
	if o.MaxSpan == 0 {
		o.MaxSpan = 4 << 20
	}
	if o.MaxGap == 0 {
		o.MaxGap = 256 << 10
	}
}

// Stats count what a Fetcher did. Bytes are what crossed the network,
// including holes read through.
type Stats struct {
	DemandRequests, PrefetchRequests int64
	DemandBlocks, PrefetchBlocks     int64 // fetched from the store
	AroundBlocks                     int64 // fetched by fault-around
	CacheHits, PeerHits              int64
	Bytes, HoleBytes                 int64
	DemandWait                       time.Duration // total time demand callers waited
	Joined                           int64         // demands that joined a fetch already in flight
}

// Deliver receives every block the fetcher gets, from any source, once.
type Deliver func(b Block, data []byte, from Origin)

// Fetcher fetches the blocks of one snapshot. Safe for concurrent use.
type Fetcher struct {
	repo    *repo.Repo
	files   []*image.Manifest
	cache   *cache.Cache
	peer    Peer
	opt     Options
	deliver Deliver

	demandSlots chan struct{}
	sharedSlots chan struct{}

	// packs lists every stored block of each pack in offset order, for
	// coalescing and fault-around.
	packs map[string][]Block
	pos   map[Block]int // index of a block in its pack's list

	mu         sync.Mutex
	inflight   map[Block]*call
	done       map[Block]bool
	delivering map[Block]bool
	want       func(Block) bool

	st struct {
		demandReq, prefetchReq, demandBlocks, prefetchBlocks, around atomic.Int64
		cacheHits, peerHits, bytes, holes, wait, joined              atomic.Int64
	}
}

type call struct {
	done chan struct{}
	data []byte
	from Origin
	err  error
}

// New makes a fetcher for blocks of files (indexed as Block.File). cache
// and peer may be nil. deliver is called for each block fetched.
func New(r *repo.Repo, files []*image.Manifest, c *cache.Cache, peer Peer, opt Options, deliver Deliver) *Fetcher {
	opt.defaults()
	f := &Fetcher{
		repo: r, files: files, cache: c, peer: peer, opt: opt, deliver: deliver,
		demandSlots: make(chan struct{}, opt.Reserved),
		sharedSlots: make(chan struct{}, opt.Streams-opt.Reserved),
		packs:       map[string][]Block{},
		pos:         map[Block]int{},
		inflight:    map[Block]*call{},
		done:        map[Block]bool{},
		delivering:  map[Block]bool{},
	}
	for fi, m := range files {
		for i, ref := range m.Refs {
			if ref.Zero() {
				continue
			}
			p := m.Packs[ref.Pack]
			f.packs[p] = append(f.packs[p], Block{fi, i})
		}
	}
	for p, bs := range f.packs {
		sort.Slice(bs, func(i, j int) bool { return f.loc(bs[i]).Off < f.loc(bs[j]).Off })
		for i, b := range bs {
			f.pos[b] = i
		}
		f.packs[p] = bs
	}
	return f
}

// SetWant tells fault-around which blocks are worth bringing along: the
// planned ones not yet present.
func (f *Fetcher) SetWant(want func(Block) bool) {
	f.mu.Lock()
	f.want = want
	f.mu.Unlock()
}

func (f *Fetcher) loc(b Block) repo.Loc { return repo.LocOf(f.files[b.File], b.Index) }

func (f *Fetcher) id(b Block) image.ID { return f.files[b.File].Refs[b.Index].ID }

func (f *Fetcher) rawLen(b Block) int { return f.files[b.File].BlockLen(b.Index) }

// Stats returns the counters so far.
func (f *Fetcher) Stats() Stats {
	return Stats{
		DemandRequests: f.st.demandReq.Load(), PrefetchRequests: f.st.prefetchReq.Load(),
		DemandBlocks: f.st.demandBlocks.Load(), PrefetchBlocks: f.st.prefetchBlocks.Load(),
		AroundBlocks: f.st.around.Load(),
		CacheHits:    f.st.cacheHits.Load(), PeerHits: f.st.peerHits.Load(),
		Bytes: f.st.bytes.Load(), HoleBytes: f.st.holes.Load(),
		DemandWait: time.Duration(f.st.wait.Load()), Joined: f.st.joined.Load(),
	}
}

// ErrDelivered means the block arrived (and was delivered) while the caller
// was asking for it. The caller should read it from wherever Deliver put it.
var ErrDelivered = errors.New("fetch: block already delivered")

// Demand gets one block the guest is waiting for. Zero blocks must not be
// asked for. It returns ErrDelivered if the block was delivered by some
// other fetch before this one could start.
func (f *Fetcher) Demand(ctx context.Context, b Block) ([]byte, Origin, error) {
	start := time.Now()
	defer func() { f.st.wait.Add(int64(time.Since(start))) }()

	if data, from, err, ok := f.join(ctx, b); ok {
		return data, from, err
	}
	if f.cache != nil {
		if data, ok := f.cache.Get(f.id(b)); ok {
			f.st.cacheHits.Add(1)
			f.finish(b, data, FromCache)
			return data, FromCache, nil
		}
	}

	// A reserved slot if one is free, else whichever frees up first.
	var slot chan struct{}
	select {
	case f.demandSlots <- struct{}{}:
		slot = f.demandSlots
	default:
		select {
		case f.demandSlots <- struct{}{}:
			slot = f.demandSlots
		case f.sharedSlots <- struct{}{}:
			slot = f.sharedSlots
		case <-ctx.Done():
			return nil, FromStore, ctx.Err()
		}
	}
	defer func() { <-slot }()

	f.mu.Lock()
	if f.done[b] || f.inflight[b] != nil {
		f.mu.Unlock()
		data, from, err, _ := f.join(ctx, b)
		return data, from, err
	}
	group := f.aroundLocked(b)
	calls := f.claimLocked(group)
	f.mu.Unlock()

	f.st.demandReq.Add(1)
	f.st.demandBlocks.Add(1)
	f.st.around.Add(int64(len(group) - 1))
	f.fetchGroup(ctx, group)
	c := calls[0]
	<-c.done
	return c.data, c.from, c.err
}

// join waits for a fetch of b already under way. ok is false if there is
// none and b has not been delivered.
func (f *Fetcher) join(ctx context.Context, b Block) (data []byte, from Origin, err error, ok bool) {
	f.mu.Lock()
	if f.done[b] {
		f.mu.Unlock()
		return nil, FromStore, ErrDelivered, true
	}
	c := f.inflight[b]
	f.mu.Unlock()
	if c == nil {
		return nil, FromStore, nil, false
	}
	f.st.joined.Add(1)
	select {
	case <-c.done:
		if c.err != nil {
			return nil, c.from, c.err, true
		}
		return c.data, c.from, nil, true
	case <-ctx.Done():
		return nil, FromStore, ctx.Err(), true
	}
}

// aroundLocked is b plus the wanted blocks after it in its pack that can
// ride on the same request.
func (f *Fetcher) aroundLocked(b Block) []Block {
	group := []Block{b}
	if f.opt.FaultAround == 0 || f.want == nil {
		return group
	}
	l := f.loc(b)
	list := f.packs[l.Pack]
	start, end := int64(l.Off), int64(l.Off)+int64(l.Len)
	for i := f.pos[b] + 1; i < len(list) && len(group) <= f.opt.FaultAround; i++ {
		n := list[i]
		nl := f.loc(n)
		if int64(nl.Off)-end > f.opt.MaxGap || int64(nl.Off)+int64(nl.Len)-start > f.opt.MaxSpan {
			break
		}
		if f.done[n] || f.inflight[n] != nil || !f.want(n) {
			break
		}
		if f.cache != nil && f.cache.Has(f.id(n)) {
			break
		}
		group = append(group, n)
		end = int64(nl.Off) + int64(nl.Len)
	}
	return group
}

func (f *Fetcher) claimLocked(group []Block) []*call {
	calls := make([]*call, len(group))
	for i, b := range group {
		c := &call{done: make(chan struct{})}
		f.inflight[b] = c
		calls[i] = c
	}
	return calls
}

// finish records that b arrived, delivers it once and wakes its waiters.
// b only counts as done once Deliver has returned, so a caller told
// ErrDelivered can rely on finding the block where Deliver put it.
func (f *Fetcher) finish(b Block, data []byte, from Origin) {
	f.mu.Lock()
	first := !f.done[b] && !f.delivering[b]
	if first {
		f.delivering[b] = true
	}
	f.mu.Unlock()
	if first && f.deliver != nil {
		f.deliver(b, data, from)
	}
	f.mu.Lock()
	if first {
		delete(f.delivering, b)
	}
	f.done[b] = true
	c := f.inflight[b]
	delete(f.inflight, b)
	f.mu.Unlock()
	if c != nil {
		c.data, c.from = data, from
		close(c.done)
	}
}

// fail wakes b's waiters with an error. b can be fetched again.
func (f *Fetcher) fail(b Block, err error) {
	f.mu.Lock()
	c := f.inflight[b]
	delete(f.inflight, b)
	f.mu.Unlock()
	if c != nil {
		c.err = err
		close(c.done)
	}
}

// fetchGroup gets blocks of one pack, in offset order, that the caller has
// claimed. Every block ends up finished or failed.
func (f *Fetcher) fetchGroup(ctx context.Context, group []Block) {
	if f.peer != nil {
		want := make([]Want, len(group))
		for i, b := range group {
			want[i] = Want{ID: f.id(b), Loc: f.loc(b), Len: f.rawLen(b)}
		}
		got, err := f.peer.GetMany(ctx, want)
		if err == nil && len(got) > 0 {
			rest := group[:0:0]
			for _, b := range group {
				if data, ok := got[f.id(b)]; ok {
					f.st.peerHits.Add(1)
					if f.cache != nil {
						f.cache.Put(f.id(b), data)
					}
					f.finish(b, data, FromPeer)
				} else {
					rest = append(rest, b)
				}
			}
			group = rest
		}
		// What the peers didn't have may now have holes in it; don't read
		// through blocks that already arrived.
		for len(group) > 0 {
			n := 1
			end := int64(f.loc(group[0]).Off) + int64(f.loc(group[0]).Len)
			for n < len(group) {
				l := f.loc(group[n])
				if int64(l.Off)-end > f.opt.MaxGap {
					break
				}
				end = int64(l.Off) + int64(l.Len)
				n++
			}
			f.fetchRange(ctx, group[:n])
			group = group[n:]
		}
		return
	}
	f.fetchRange(ctx, group)
}

// fetchRange reads blocks of one pack, in offset order, with one ranged get.
func (f *Fetcher) fetchRange(ctx context.Context, group []Block) {

	first, last := f.loc(group[0]), f.loc(group[len(group)-1])
	off := int64(first.Off)
	n := int64(last.Off) + int64(last.Len) - off
	raw, err := f.repo.Store().GetRange(ctx, "packs/"+first.Pack, off, n)
	if errors.Is(err, objstore.ErrNotFound) {
		// Repacked away since the manifest was read. ReadLoc looks each
		// block up again by ID.
		for _, b := range group {
			data, err := f.repo.ReadLoc(ctx, f.loc(b), f.id(b), f.rawLen(b))
			if err != nil {
				f.fail(b, err)
				continue
			}
			f.st.bytes.Add(int64(len(data)))
			f.store(b, data)
		}
		return
	}
	if err != nil {
		for _, b := range group {
			f.fail(b, err)
		}
		return
	}
	f.st.bytes.Add(n)
	hole := n
	for _, b := range group {
		l := f.loc(b)
		hole -= int64(l.Len)
		at := int64(l.Off) - off
		data, err := image.Expand(raw[at:at+int64(l.Len)], l.Flags, f.rawLen(b), f.id(b))
		if err != nil {
			f.fail(b, err)
			continue
		}
		f.store(b, data)
	}
	f.st.holes.Add(hole)
}

func (f *Fetcher) store(b Block, data []byte) {
	if f.cache != nil {
		f.cache.Put(f.id(b), data)
	}
	f.finish(b, data, FromStore)
}

// Prefetch fetches blocks in the order given, skipping any already
// delivered or in flight, until all are done or ctx ends. It only uses
// shared slots, so demand fetches always have the reserved ones.
func (f *Fetcher) Prefetch(ctx context.Context, blocks []Block) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	i := 0
	for i < len(blocks) {
		b := blocks[i]
		f.mu.Lock()
		skip := f.done[b] || f.inflight[b] != nil
		f.mu.Unlock()
		if skip || f.files[b.File].Refs[b.Index].Zero() {
			i++
			continue
		}
		if f.cache != nil {
			if data, ok := f.cache.Get(f.id(b)); ok {
				f.st.cacheHits.Add(1)
				f.finish(b, data, FromCache)
				i++
				continue
			}
		}
		select {
		case f.sharedSlots <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
		f.mu.Lock()
		var group []Block
		group, i = f.runLocked(blocks, i)
		f.claimLocked(group)
		f.mu.Unlock()
		if len(group) == 0 {
			<-f.sharedSlots
			continue
		}
		f.st.prefetchReq.Add(1)
		f.st.prefetchBlocks.Add(int64(len(group)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-f.sharedSlots }()
			f.fetchGroup(ctx, group)
		}()
	}
	return nil
}

// runLocked takes the longest run of blocks[i:] that one request can
// fetch: same pack, offsets moving forward within MaxGap, at most MaxSpan
// in all. It returns the run and where the next one starts.
func (f *Fetcher) runLocked(blocks []Block, i int) ([]Block, int) {
	for i < len(blocks) && (f.done[blocks[i]] || f.inflight[blocks[i]] != nil) {
		i++
	}
	if i == len(blocks) {
		return nil, i
	}
	b := blocks[i]
	l := f.loc(b)
	group := []Block{b}
	start, end := int64(l.Off), int64(l.Off)+int64(l.Len)
	i++
	for ; i < len(blocks); i++ {
		n := blocks[i]
		ref := f.files[n.File].Refs[n.Index]
		if ref.Zero() {
			continue
		}
		if f.done[n] || f.inflight[n] != nil {
			continue
		}
		nl := f.loc(n)
		if nl.Pack != l.Pack || int64(nl.Off) < end || int64(nl.Off)-end > f.opt.MaxGap ||
			int64(nl.Off)+int64(nl.Len)-start > f.opt.MaxSpan {
			break
		}
		if f.cache != nil && f.cache.Has(f.id(n)) {
			break
		}
		group = append(group, n)
		end = int64(nl.Off) + int64(nl.Len)
	}
	return group, i
}

// Delivered reports whether b has been delivered.
func (f *Fetcher) Delivered(b Block) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.done[b]
}

// String is for logs.
func (b Block) String() string { return fmt.Sprintf("%d:%d", b.File, b.Index) }
