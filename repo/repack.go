package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/image"
)

// RepackStats says what a repack did.
type RepackStats struct {
	Blocks   int // hot blocks copied
	Bytes    int64
	Packs    int
	Requests int // ranged gets made to read them
	Took     time.Duration
}

// Repack copies the hot blocks of a snapshot into new packs, in the order a
// restore will want them, and points the snapshot's manifests at the
// copies. order names the hot blocks of each file in plan order.
//
// A snapshot is written in file order, so the blocks a resume touches are
// scattered across every pack and a prefetcher needs one request per block.
// After a repack they sit end to end and a few long ranged gets bring in
// the whole working set.
//
// Restores already running keep reading the old manifest. Once GC deletes
// the old packs their reads fail over to the copies (see ReadLoc). The old
// packs stay as long as another snapshot still uses them.
func (r *Repo) Repack(ctx context.Context, s *Snapshot, order map[string][]int, packSize int) (*Snapshot, RepackStats, error) {
	start := time.Now()
	var st RepackStats
	if packSize == 0 {
		packSize = 8 << 20
	}
	lookup := time.Now()

	out := *s
	out.Files = map[string]FileInfo{}
	for k, v := range s.Files {
		out.Files[k] = v
	}
	for name, hot := range order {
		fi, ok := s.Files[name]
		if !ok {
			continue
		}
		m, err := r.Manifest(ctx, s, name)
		if err != nil {
			return nil, st, err
		}
		var blocks []int
		seen := map[int]bool{}
		for _, i := range hot {
			if i >= 0 && i < len(m.Refs) && !m.Refs[i].Zero() && !seen[i] {
				seen[i] = true
				blocks = append(blocks, i)
			}
		}
		if len(blocks) == 0 {
			continue
		}

		// Read the stored bytes, coalescing neighbours in the old packs.
		stored := make([][]byte, len(blocks))
		flags := make([]uint8, len(blocks))
		type run struct{ first, last int } // indexes into blocks, same pack, contiguous
		var runs []run
		for k := 0; k < len(blocks); k++ {
			if len(runs) > 0 {
				lr := &runs[len(runs)-1]
				prev, cur := m.Refs[blocks[lr.last]], m.Refs[blocks[k]]
				if prev.Pack == cur.Pack && prev.Off+prev.Len == cur.Off && k == lr.last+1 {
					lr.last = k
					continue
				}
			}
			runs = append(runs, run{k, k})
		}
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, 16)
		var wg sync.WaitGroup
		for _, rn := range runs {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				a, z := m.Refs[blocks[rn.first]], m.Refs[blocks[rn.last]]
				b, err := r.st.GetRange(ctx, "packs/"+m.Packs[a.Pack], int64(a.Off), int64(z.Off+z.Len-a.Off))
				if err != nil {
					// Fall back to one block at a time, which follows a
					// block to its new home if its pack was repacked away.
					b = nil
				}
				for k := rn.first; k <= rn.last; k++ {
					ref := m.Refs[blocks[k]]
					if b != nil {
						stored[k] = b[ref.Off-a.Off : ref.Off-a.Off+ref.Len]
						flags[k] = ref.Flags
						continue
					}
					raw, err := r.ReadLoc(ctx, LocOf(m, blocks[k]), ref.ID, m.BlockLen(blocks[k]))
					if err != nil {
						mu.Lock()
						firstErr = err
						mu.Unlock()
						return
					}
					stored[k], flags[k] = raw, 0 // ReadLoc hands back expanded bytes

				}
			}()
			st.Requests++
		}
		wg.Wait()
		if firstErr != nil {
			return nil, st, firstErr
		}

		// Write them out in plan order.
		nm := &image.Manifest{Size: m.Size, Block: m.Block, Packs: append([]string(nil), m.Packs...), Refs: append([]image.Ref(nil), m.Refs...)}
		packIdx := map[string]uint32{}
		for i, p := range nm.Packs {
			packIdx[p] = uint32(i)
		}
		var buf []byte
		var ents []indexEntry
		var slots []int
		flush := func() error {
			if len(buf) == 0 {
				return nil
			}
			var raw [16]byte
			rand.Read(raw[:])
			id := hex.EncodeToString(raw[:])
			if err := r.st.Put(ctx, "packs/"+id, buf); err != nil {
				return err
			}
			if err := r.st.Put(ctx, "index/"+id, encodeIndex(ents)); err != nil {
				return err
			}
			pi := uint32(len(nm.Packs))
			nm.Packs = append(nm.Packs, id)
			packIdx[id] = pi
			for k, i := range slots {
				nm.Refs[i].Pack, nm.Refs[i].Off = pi, ents[k].off
			}
			r.mu.Lock()
			r.seen[id] = true
			r.mu.Unlock()
			st.Packs++
			buf, ents, slots = nil, nil, nil
			return nil
		}
		for k, i := range blocks {
			ref := m.Refs[i]
			ents = append(ents, indexEntry{id: ref.ID, off: uint32(len(buf)), len: uint32(len(stored[k])), flags: flags[k]})
			nm.Refs[i].Len, nm.Refs[i].Flags = uint32(len(stored[k])), flags[k]
			buf = append(buf, stored[k]...)
			slots = append(slots, i)
			st.Blocks++
			st.Bytes += int64(len(stored[k]))
			if len(buf) >= packSize {
				if err := flush(); err != nil {
					return nil, st, err
				}
			}
		}
		if err := flush(); err != nil {
			return nil, st, err
		}
		// Drop packs no ref uses any more.
		used := map[uint32]bool{}
		for _, ref := range nm.Refs {
			if !ref.Zero() {
				used[ref.Pack] = true
			}
		}
		var packs []string
		remap := map[uint32]uint32{}
		for i, p := range nm.Packs {
			if used[uint32(i)] {
				remap[uint32(i)] = uint32(len(packs))
				packs = append(packs, p)
			}
		}
		for i := range nm.Refs {
			if !nm.Refs[i].Zero() {
				nm.Refs[i].Pack = remap[nm.Refs[i].Pack]
			}
		}
		nm.Packs = packs

		key := fmt.Sprintf("manifests/%s/%s.%s", s.ID, name, NewID())
		if err := r.st.Put(ctx, key, nm.Encode()); err != nil {
			return nil, st, err
		}
		fi.Manifest = key
		out.Files[name] = fi
	}
	// The new packs are referenced by nothing until the record is updated,
	// so the same Grace rule as Write applies.
	if time.Since(lookup) > r.Grace-r.Grace/10 {
		return nil, st, ErrTooSlow
	}
	if err := r.updateRecord(ctx, &out); err != nil {
		return nil, st, err
	}
	st.Took = time.Since(start)
	return &out, st, nil
}
