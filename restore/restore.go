// Package restore serves a snapshot to a resuming VM block by block.
//
// The VM side (a userfaultfd handler for memory, an NBD server for disk)
// calls Session.Get when the guest touches a block for the first time. Get
// returns at once if the block is staged in host memory and otherwise
// fetches it while the guest waits. Meanwhile a prefetcher stages the
// blocks a strategy asks for:
//
//	full      everything, before the guest runs (the baseline most systems use)
//	lazy      nothing; every block is fetched on first touch
//	hydrate   everything, in file order, while the guest runs
//	thaw      the plan learned from earlier restores, in predicted order
//
// Prefetched blocks are staged rather than installed in guest memory, so
// the guest still asks for each one and the trace of this restore is
// exact. Install mode copies them straight into guest memory instead,
// which saves a fault per block but leaves a trace that only shows the
// misses, so install-mode traces are never used for planning.
package restore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/fetch"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/trace"
)

// Strategy picks what to prefetch.
type Strategy string

const (
	Full    Strategy = "full"
	Lazy    Strategy = "lazy"
	Hydrate Strategy = "hydrate"
	Thaw    Strategy = "thaw"
)

// Strategies lists them in the order benchmarks report them.
var Strategies = []Strategy{Full, Lazy, Hydrate, Thaw}

// Mode is how prefetched blocks reach the guest.
type Mode int

const (
	Stage   Mode = iota // held in host memory until the guest faults on them
	Install             // copied into guest memory as they arrive
)

// Event is something that happened to a block, for live views.
type Event struct {
	At    time.Duration
	File  int
	Block int
	What  string // "demand", "staged", "cached", "zero" for touches; "fetched" and "prefetched" for arrivals
}

// Options configure a Session.
type Options struct {
	Strategy Strategy
	Mode     Mode
	Files    []string // snapshot files to serve; mem and disk if empty
	Plan     *plan.Plan
	// HydrateAfter fetches the rest of the snapshot once the plan is done,
	// so the VM no longer depends on the store. Off by default: most agent
	// VMs live for seconds and never need the rest.
	HydrateAfter bool
	Fetch        fetch.Options
	Cache        *cache.Cache
	Peer         fetch.Peer
	// Install is called with each prefetched block in Install mode.
	Install func(file, block int, data []byte)
	// OnEvent, if set, is called for every touch and arrival.
	OnEvent func(Event)
}

// Session is one restore of one snapshot.
type Session struct {
	Snap  *repo.Snapshot
	Names []string
	Files []*image.Manifest

	opt Options
	f   *fetch.Fetcher
	rec *trace.Recorder

	mu       sync.Mutex
	staged   [][][]byte
	origin   [][]fetch.Origin
	prefetch []fetch.Block // what the strategy asked for

	cancel   context.CancelFunc
	prefDone chan struct{}
	prefErr  error

	touched, fromStage, fromCache, demand, zero atomic.Int64
	planned                                     atomic.Int64
}

// New opens a session. It reads the manifests but fetches no blocks.
func New(ctx context.Context, r *repo.Repo, snap *repo.Snapshot, opt Options) (*Session, error) {
	if opt.Strategy == "" {
		opt.Strategy = Thaw
	}
	names := opt.Files
	if len(names) == 0 {
		for _, n := range []string{"mem", "disk"} {
			if _, ok := snap.Files[n]; ok {
				names = append(names, n)
			}
		}
	}
	s := &Session{Snap: snap, Names: names, opt: opt}
	blocks := make([]int, len(names))
	for i, n := range names {
		m, err := r.Manifest(ctx, snap, n)
		if err != nil {
			return nil, err
		}
		s.Files = append(s.Files, m)
		s.staged = append(s.staged, make([][]byte, m.Blocks()))
		s.origin = append(s.origin, make([]fetch.Origin, m.Blocks()))
		blocks[i] = m.Block
	}
	s.rec = trace.NewRecorder(snap.ID, names, blocks)
	s.rec.Set("strategy", string(opt.Strategy))
	if opt.Mode == Install {
		s.rec.Set("mode", "install")
	}
	s.f = fetch.New(r, s.Files, opt.Cache, opt.Peer, opt.Fetch, s.deliver)
	s.prefetch = s.list()
	if opt.Strategy == Thaw && opt.Plan != nil {
		want := make([]map[int]bool, len(names))
		for i := range want {
			want[i] = map[int]bool{}
		}
		for _, b := range s.prefetch {
			want[b.File][b.Index] = true
		}
		s.f.SetWant(func(b fetch.Block) bool { return want[b.File][b.Index] })
	}
	return s, nil
}

// list is what the strategy prefetches, in order.
func (s *Session) list() []fetch.Block {
	var out []fetch.Block
	all := func() {
		for fi, m := range s.Files {
			for i, ref := range m.Refs {
				if !ref.Zero() {
					out = append(out, fetch.Block{File: fi, Index: i})
				}
			}
		}
	}
	switch s.opt.Strategy {
	case Full, Hydrate:
		all()
	case Thaw:
		if s.opt.Plan == nil {
			break
		}
		idx := map[string]int{}
		for i, n := range s.Names {
			idx[n] = i
		}
		for _, it := range s.opt.Plan.Items {
			fi, ok := idx[s.opt.Plan.Files[it.File]]
			if !ok || int(it.Block) >= len(s.Files[fi].Refs) || s.Files[fi].Refs[it.Block].Zero() {
				continue
			}
			out = append(out, fetch.Block{File: fi, Index: int(it.Block)})
		}
		s.planned.Store(int64(len(out)))
		if s.opt.HydrateAfter {
			all()
		}
	}
	return out
}

// Start begins prefetching. For the full strategy it returns only once
// every block is staged, since that strategy doesn't let the guest run
// before then.
func (s *Session) Start(ctx context.Context) error {
	ctx, s.cancel = context.WithCancel(ctx)
	s.prefDone = make(chan struct{})
	if s.opt.Strategy == Full {
		err := s.f.Prefetch(ctx, s.prefetch)
		s.rec.Mark("fetched")
		close(s.prefDone)
		return err
	}
	go func() {
		defer close(s.prefDone)
		if err := s.f.Prefetch(ctx, s.prefetch); err != nil && !errors.Is(err, context.Canceled) {
			s.prefErr = err
		}
		s.rec.Mark("prefetched")
	}()
	return nil
}

// Planned is how many blocks the plan asked for.
func (s *Session) Planned() int { return int(s.planned.Load()) }

func (s *Session) deliver(b fetch.Block, data []byte, from fetch.Origin) {
	s.mu.Lock()
	if s.staged[b.File][b.Index] == nil {
		s.staged[b.File][b.Index] = data
		s.origin[b.File][b.Index] = from
	}
	s.mu.Unlock()
	if s.opt.Install != nil && s.opt.Mode == Install {
		s.opt.Install(b.File, b.Index, data)
	}
	if s.opt.OnEvent != nil {
		s.opt.OnEvent(Event{At: s.rec.Elapsed(), File: b.File, Block: b.Index, What: "fetched"})
	}
}

// Recorder is the session's trace recorder, for marks like "answered".
func (s *Session) Recorder() *trace.Recorder { return s.rec }

// IsZero reports whether a block is all zeros.
func (s *Session) IsZero(file, block int) bool { return s.Files[file].Refs[block].Zero() }

// Get returns a block the guest touched. A nil slice with no error means
// the block is all zeros.
func (s *Session) Get(ctx context.Context, file, block int) ([]byte, error) {
	at := s.rec.Elapsed()
	if block < 0 || block >= len(s.staged[file]) {
		return nil, fmt.Errorf("restore: block %d of %s out of range", block, s.Names[file])
	}
	if s.IsZero(file, block) {
		s.touch(file, block, trace.Zero, at, 0)
		return nil, nil
	}
	s.mu.Lock()
	data := s.staged[file][block]
	s.mu.Unlock()
	if data != nil {
		s.touch(file, block, trace.Staged, at, 0)
		return data, nil
	}
	data, from, err := s.f.Demand(ctx, fetch.Block{File: file, Index: block})
	if errors.Is(err, fetch.ErrDelivered) {
		s.mu.Lock()
		data = s.staged[file][block]
		s.mu.Unlock()
		err = nil
		from = fetch.FromStore
	}
	if err != nil {
		return nil, err
	}
	kind := trace.Demand
	if from == fetch.FromCache {
		kind = trace.Cached
	}
	s.touch(file, block, kind, at, s.rec.Elapsed()-at)
	return data, nil
}

func (s *Session) touch(file, block int, k trace.Kind, at, wait time.Duration) {
	if !s.rec.Touch(file, uint32(block), k, at, wait) {
		return
	}
	s.touched.Add(1)
	switch k {
	case trace.Staged:
		s.fromStage.Add(1)
	case trace.Cached:
		s.fromCache.Add(1)
	case trace.Demand:
		s.demand.Add(1)
	case trace.Zero:
		s.zero.Add(1)
	}
	if s.opt.OnEvent != nil {
		s.opt.OnEvent(Event{At: at, File: file, Block: block, What: k.String()})
	}
}

// Stats summarise a session.
type Stats struct {
	Strategy  Strategy
	Touched   int64 // distinct blocks the guest touched
	FromStage int64 // touched blocks that were already staged
	FromCache int64 // touched blocks served from the host cache on demand
	Demand    int64 // touched blocks the guest waited on the network for
	Zero      int64
	Staged    int64 // blocks staged in all
	Unused    int64 // staged blocks the guest never touched
	Planned   int
	Fetch     fetch.Stats
}

// Stats returns what the session has done so far.
func (s *Session) Stats() Stats {
	st := Stats{
		Strategy: s.opt.Strategy, Touched: s.touched.Load(), FromStage: s.fromStage.Load(),
		FromCache: s.fromCache.Load(), Demand: s.demand.Load(), Zero: s.zero.Load(),
		Planned: s.Planned(), Fetch: s.f.Stats(),
	}
	s.mu.Lock()
	for _, f := range s.staged {
		for _, b := range f {
			if b != nil {
				st.Staged++
			}
		}
	}
	s.mu.Unlock()
	st.Unused = st.Staged - (st.FromStage + st.FromCache + st.Demand)
	if st.Unused < 0 {
		st.Unused = 0
	}
	return st
}

// Wait blocks until prefetching has finished.
func (s *Session) Wait() error {
	<-s.prefDone
	return s.prefErr
}

// Close stops prefetching and returns the trace.
func (s *Session) Close() *trace.Trace {
	if s.cancel != nil {
		s.cancel()
		<-s.prefDone
	}
	t := s.rec.Trace()
	t.Mode = "stage"
	if s.opt.Mode == Install {
		t.Mode = "install"
	}
	t.Task = t.Meta["task"]
	return &t
}

// WriteFile writes file as staged so far to path, with holes for zero and
// missing blocks. The full strategy uses it to hand Firecracker plain local
// files once everything has arrived.
func (s *Session) WriteFile(file int, path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	m := s.Files[file]
	if err := f.Truncate(m.Size); err != nil {
		f.Close()
		return err
	}
	s.mu.Lock()
	blocks := s.staged[file]
	s.mu.Unlock()
	for i, b := range blocks {
		if b == nil {
			if !s.IsZero(file, i) {
				f.Close()
				return fmt.Errorf("restore: block %d of %s was never fetched", i, s.Names[file])
			}
			continue
		}
		if _, err := f.WriteAt(b, int64(i)*int64(m.Block)); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}
