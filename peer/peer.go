// Package peer shares block caches between hosts.
//
// When twenty forks of one snapshot start on five hosts, each host would
// otherwise fetch the same working set from the object store. Instead
// every block has an owner host, picked by rendezvous hashing on the 4 MiB
// region of the pack it lives in. Owning whole regions rather than single
// blocks keeps neighbouring blocks on one owner, so the owner can still
// read them from the store with one ranged get.
// A host that misses its own cache asks the owner; the owner serves it
// from its cache or reads it through from the store once and keeps it, so
// the fleet fetches each block from the store about once.
//
// Rendezvous hashing (highest random weight) means adding or removing a
// host only moves the blocks that host owned, with no ring to rebalance.
package peer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/fetch"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
)

// RegionBits sets the size of the unit hosts own: 4 MiB of a pack.
const RegionBits = 22

// Region names the part of a pack a block lives in.
func Region(l repo.Loc) string { return fmt.Sprintf("%s/%d", l.Pack, l.Off>>RegionBits) }

// Owner returns which of hosts owns key.
func Owner(hosts []string, key string) string {
	var best string
	var bestW uint64
	for _, h := range hosts {
		f := fnv.New64a()
		f.Write([]byte(h))
		f.Write([]byte{0})
		f.Write([]byte(key))
		w := mix(f.Sum64())
		if best == "" || w > bestW {
			best, bestW = h, w
		}
	}
	return best
}

// mix is the splitmix64 finalizer; FNV alone spreads short keys poorly.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

// Wire format. A request is a list of wants; a response is the blocks the
// server could get, each prefixed with its ID and length.
//
//	want: id[16] packLen[1] pack[packLen] off[4] len[4] flags[1] raw[4]
//	got:  id[16] n[4] data[n]

func encodeWants(ws []fetch.Want) []byte {
	var b bytes.Buffer
	var u [4]byte
	for _, w := range ws {
		b.Write(w.ID[:])
		b.WriteByte(byte(len(w.Loc.Pack)))
		b.WriteString(w.Loc.Pack)
		binary.LittleEndian.PutUint32(u[:], w.Loc.Off)
		b.Write(u[:])
		binary.LittleEndian.PutUint32(u[:], w.Loc.Len)
		b.Write(u[:])
		b.WriteByte(w.Loc.Flags)
		binary.LittleEndian.PutUint32(u[:], uint32(w.Len))
		b.Write(u[:])
	}
	return b.Bytes()
}

func decodeWants(b []byte) ([]fetch.Want, error) {
	var out []fetch.Want
	for len(b) > 0 {
		if len(b) < 17 {
			return nil, errors.New("peer: short want")
		}
		var w fetch.Want
		copy(w.ID[:], b)
		pl := int(b[16])
		b = b[17:]
		if len(b) < pl+13 {
			return nil, errors.New("peer: short want")
		}
		w.Loc.Pack = string(b[:pl])
		b = b[pl:]
		w.Loc.Off = binary.LittleEndian.Uint32(b)
		w.Loc.Len = binary.LittleEndian.Uint32(b[4:])
		w.Loc.Flags = b[8]
		w.Len = int(binary.LittleEndian.Uint32(b[9:]))
		b = b[13:]
		if w.Len <= 0 || w.Len > 16<<20 {
			return nil, fmt.Errorf("peer: block length %d", w.Len)
		}
		out = append(out, w)
	}
	return out, nil
}

// Server serves this host's cache to its peers.
type Server struct {
	Cache *cache.Cache
	// Repo, if set, lets the server read blocks it doesn't have through
	// from the store and keep them. Without it the server only serves hits.
	Repo *repo.Repo

	mu       sync.Mutex
	inflight map[image.ID]*flight

	stats struct {
		sync.Mutex
		Requests, Hits, ReadThrough, Joined, Misses int64
	}
}

type flight struct {
	done chan struct{}
	data []byte
}

// ServerStats count what a server did.
type ServerStats struct {
	Requests, Hits, ReadThrough, Joined, Misses int64
}

// Stats returns the counters.
func (s *Server) Stats() ServerStats {
	s.stats.Lock()
	defer s.stats.Unlock()
	return ServerStats{s.stats.Requests, s.stats.Hits, s.stats.ReadThrough, s.stats.Joined, s.stats.Misses}
}

func (s *Server) count(f func()) {
	s.stats.Lock()
	f()
	s.stats.Unlock()
}

// ServeHTTP answers POST /blocks.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/blocks" {
		http.Error(w, "POST /blocks", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wants, err := decodeWants(body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.count(func() { s.stats.Requests++ })
	got := s.get(r.Context(), wants)
	w.Header().Set("Content-Type", "application/octet-stream")
	bw := bufio.NewWriterSize(w, 256<<10)
	var u [4]byte
	for _, want := range wants {
		d, ok := got[want.ID]
		if !ok {
			continue
		}
		bw.Write(want.ID[:])
		binary.LittleEndian.PutUint32(u[:], uint32(len(d)))
		bw.Write(u[:])
		bw.Write(d)
	}
	bw.Flush()
}

// get serves wants from the cache, then reads the rest through, one
// request per run of neighbouring blocks, joining reads already under way.
func (s *Server) get(ctx context.Context, wants []fetch.Want) map[image.ID][]byte {
	out := map[image.ID][]byte{}
	var todo []fetch.Want
	var waits []*flight
	var waitIDs []image.ID
	for _, w := range wants {
		if _, dup := out[w.ID]; dup {
			continue
		}
		if d, ok := s.Cache.Get(w.ID); ok {
			out[w.ID] = d
			s.count(func() { s.stats.Hits++ })
			continue
		}
		if s.Repo == nil {
			s.count(func() { s.stats.Misses++ })
			continue
		}
		s.mu.Lock()
		if s.inflight == nil {
			s.inflight = map[image.ID]*flight{}
		}
		if f := s.inflight[w.ID]; f != nil {
			s.mu.Unlock()
			waits = append(waits, f)
			waitIDs = append(waitIDs, w.ID)
			s.count(func() { s.stats.Joined++ })
			continue
		}
		s.inflight[w.ID] = &flight{done: make(chan struct{})}
		s.mu.Unlock()
		todo = append(todo, w)
	}

	sort.Slice(todo, func(i, j int) bool {
		if todo[i].Loc.Pack != todo[j].Loc.Pack {
			return todo[i].Loc.Pack < todo[j].Loc.Pack
		}
		return todo[i].Loc.Off < todo[j].Loc.Off
	})
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < len(todo); {
		j := i + 1
		for j < len(todo) && todo[j].Loc.Pack == todo[i].Loc.Pack &&
			todo[j].Loc.Off == todo[j-1].Loc.Off+todo[j-1].Loc.Len {
			j++
		}
		run := todo[i:j]
		i = j
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := s.readThrough(ctx, run)
			mu.Lock()
			for id, d := range got {
				out[id] = d
			}
			mu.Unlock()
			for _, w := range run {
				s.mu.Lock()
				f := s.inflight[w.ID]
				delete(s.inflight, w.ID)
				s.mu.Unlock()
				f.data = got[w.ID]
				close(f.done)
			}
		}()
	}
	wg.Wait()
	for k, f := range waits {
		select {
		case <-f.done:
			if f.data != nil {
				out[waitIDs[k]] = f.data
			}
		case <-ctx.Done():
		}
	}
	return out
}

func (s *Server) readThrough(ctx context.Context, run []fetch.Want) map[image.ID][]byte {
	out := map[image.ID][]byte{}
	first, last := run[0].Loc, run[len(run)-1].Loc
	raw, err := s.Repo.Store().GetRange(ctx, "packs/"+first.Pack, int64(first.Off), int64(last.Off+last.Len-first.Off))
	for _, w := range run {
		var d []byte
		if err == nil {
			at := w.Loc.Off - first.Off
			d, err = image.Expand(raw[at:at+w.Loc.Len], w.Loc.Flags, w.Len, w.ID)
		}
		if err != nil {
			// Pack gone or damaged: try the block on its own, which looks
			// it up again by ID.
			var e2 error
			if d, e2 = s.Repo.ReadLoc(ctx, w.Loc, w.ID, w.Len); e2 != nil {
				s.count(func() { s.stats.Misses++ })
				continue
			}
		}
		s.Cache.Put(w.ID, d)
		out[w.ID] = d
		s.count(func() { s.stats.ReadThrough++ })
	}
	return out
}

// Ring is the client side: a fetch.Peer that sends each block to its owner.
type Ring struct {
	Self  string   // this host's address
	Hosts []string // every host, including Self, as base URLs
	// Local is this host's own server. Blocks this host owns go through it
	// in-process, so a local restore and a peer asking for the same region
	// share one read from the store. Without it they are left to the caller.
	Local *Server
	// Link, if set, delays each peer request as if it crossed that link.
	// Benchmarks run every host on one machine and need it.
	Link   *objstore.Link
	Client *http.Client
}

// GetMany asks each owner for its blocks, in parallel.
func (r *Ring) GetMany(ctx context.Context, want []fetch.Want) (map[image.ID][]byte, error) {
	by := map[string][]fetch.Want{}
	for _, w := range want {
		o := Owner(r.Hosts, Region(w.Loc))
		if o == r.Self && r.Local == nil {
			continue
		}
		by[o] = append(by[o], w)
	}
	out := map[image.ID][]byte{}
	if len(by) == 0 {
		return out, nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for host, ws := range by {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var got map[image.ID][]byte
			var err error
			if host == r.Self {
				got = r.Local.get(ctx, ws)
			} else {
				got, err = r.ask(ctx, host, ws)
			}
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for id, d := range got {
				out[id] = d
			}
		}()
	}
	wg.Wait()
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func (r *Ring) ask(ctx context.Context, host string, ws []fetch.Want) (map[image.ID][]byte, error) {
	start := time.Now()
	c := r.Client
	if c == nil {
		c = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, host+"/blocks", bytes.NewReader(encodeWants(ws)))
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s: %s", host, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if r.Link != nil {
		if err := r.Link.Wait(ctx, start, int64(len(body))); err != nil {
			return nil, err
		}
	}
	lens := map[image.ID]int{}
	for _, w := range ws {
		lens[w.ID] = w.Len
	}
	out := map[image.ID][]byte{}
	for len(body) > 0 {
		if len(body) < 20 {
			return nil, errors.New("peer: short response")
		}
		var id image.ID
		copy(id[:], body)
		n := int(binary.LittleEndian.Uint32(body[16:]))
		body = body[20:]
		if n > len(body) || n != lens[id] {
			return nil, errors.New("peer: bad block length")
		}
		d := body[:n:n]
		body = body[n:]
		// A peer is another host, not a trusted one: check what it sent.
		if image.Sum(d) != id {
			return nil, fmt.Errorf("peer %s: block %s fails its checksum", host, id)
		}
		out[id] = d
	}
	return out, nil
}
