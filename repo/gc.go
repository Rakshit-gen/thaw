package repo

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Rakshit-gen/thaw/objstore"
)

// GC deletes packs no committed snapshot uses, in two phases so it can run
// while snapshots are being written.
//
// The danger is a writer that decides to reuse a block in pack P (because
// P's index says the block is there), then commits a manifest pointing at P
// after GC has already decided P is garbage and deleted it. Every fork
// reuses its parent's blocks, so this race is the normal case, not a corner.
//
// The protocol:
//
//   - Phase 1. GC writes a tombstone tomb/P for every pack no committed
//     manifest references. It deletes nothing.
//   - Phase 2, a later run. For each tombstone at least Grace old (by the
//     store's clock), GC lists the committed manifests again. If P is
//     referenced now it was rescued: drop the tombstone. If not, delete
//     index/P, then packs/P, then the tombstone.
//   - Writers list tombstones before loading the index and never reuse a
//     tombstoned pack. A writer commits within Grace of that lookup or gives
//     up (ErrTooSlow).
//
// Why that is enough: say a writer looked up at time L and reused P.
// If P was condemned before L, the writer saw the tombstone and didn't reuse
// it. If P was condemned at T >= L, GC can only delete it after T+Grace >=
// L+Grace, and by then the writer has committed (it commits before L+Grace)
// so phase 2's fresh listing sees the reference and rescues P. Both sides
// measure Grace as a duration on one clock each (the writer's monotonic
// clock, the store's clock for tombstone age), so clock offsets between
// hosts don't matter; only clock rate does, which is why the writer stops at
// 90% of Grace.
//
// Deleting the index before the pack matters too: a crash between the two
// then leaves a pack with no index, which nobody can reuse, instead of an
// index pointing at a pack that is gone.

// GCStats says what one GC run did.
type GCStats struct {
	Snapshots       int   // committed snapshots
	Packs           int   // packs in the store
	LivePacks       int   // packs some snapshot uses
	Condemned       int   // tombstones written this run
	Rescued         int   // tombstones dropped because a snapshot uses the pack again
	Deleted         int   // packs deleted
	DeletedBytes    int64 // bytes in deleted packs
	LiveBytes       int64 // bytes in live packs
	DeadBytes       int64 // bytes in live packs no snapshot uses (compaction would free them)
	OrphanManifests int   // manifests no committed record names
	OrphanTraces    int
}

// gcMode exists so tests can run the unsafe single-phase GC and check that
// the concurrency test catches it.
type gcMode int

const (
	gcTwoPhase gcMode = iota
	gcUnsafeImmediate
)

// GC runs one pass of the collector.
func (r *Repo) GC(ctx context.Context) (GCStats, error) { return r.gc(ctx, gcTwoPhase) }

func (r *Repo) gc(ctx context.Context, mode gcMode) (GCStats, error) {
	var st GCStats
	now, err := r.storeNow(ctx)
	if err != nil {
		return st, err
	}
	// Listing happens after now, so anything committed before now is seen.
	tombObjs, err := r.st.List(ctx, "tomb/")
	if err != nil {
		return st, err
	}
	live, liveBytes, snaps, err := r.liveSet(ctx)
	if err != nil {
		return st, err
	}
	st.Snapshots = len(snaps)
	packs, err := r.st.List(ctx, "packs/")
	if err != nil {
		return st, err
	}
	tombed := map[string]time.Time{}
	for _, o := range tombObjs {
		tombed[strings.TrimPrefix(o.Key, "tomb/")] = o.Modified
	}

	for _, o := range packs {
		p := strings.TrimPrefix(o.Key, "packs/")
		st.Packs++
		if live[p] {
			st.LivePacks++
			st.LiveBytes += o.Size
			st.DeadBytes += o.Size - liveBytes[p]
			continue
		}
		if mode == gcUnsafeImmediate {
			if err := r.deletePack(ctx, p); err != nil {
				return st, err
			}
			st.Deleted++
			st.DeletedBytes += o.Size
			continue
		}
		if _, ok := tombed[p]; !ok {
			if err := r.st.Put(ctx, "tomb/"+p, nil); err != nil {
				return st, err
			}
			st.Condemned++
		}
	}

	for p, t := range tombed {
		if live[p] {
			if err := r.st.Delete(ctx, "tomb/"+p); err != nil {
				return st, err
			}
			st.Rescued++
			continue
		}
		if now.Sub(t) < r.Grace {
			continue
		}
		size := int64(0)
		for _, o := range packs {
			if o.Key == "packs/"+p {
				size = o.Size
			}
		}
		if err := r.deletePack(ctx, p); err != nil {
			return st, err
		}
		if err := r.st.Delete(ctx, "tomb/"+p); err != nil {
			return st, err
		}
		st.Deleted++
		st.DeletedBytes += size
	}

	// Manifests and traces of snapshots that don't exist, once they're old
	// enough that no writer can still be about to commit them.
	// A manifest is in use only if a record names it: repack leaves the
	// manifest it replaced behind under a live snapshot.
	committed := map[string]bool{}
	inUse := map[string]bool{}
	for _, s := range snaps {
		committed[s.ID] = true
		for _, f := range s.Files {
			inUse[f.Manifest] = true
		}
	}
	for _, prefix := range []string{"manifests/", "traces/"} {
		objs, err := r.st.List(ctx, prefix)
		if err != nil {
			return st, err
		}
		for _, o := range objs {
			keep := inUse[o.Key]
			if prefix == "traces/" {
				keep = committed[snapshotOfKey(o.Key, prefix)]
			}
			if keep || now.Sub(o.Modified) < r.Grace {
				continue
			}
			if err := r.st.Delete(ctx, o.Key); err != nil {
				return st, err
			}
			if prefix == "manifests/" {
				st.OrphanManifests++
			} else {
				st.OrphanTraces++
			}
		}
	}
	return st, nil
}

func (r *Repo) deletePack(ctx context.Context, p string) error {
	if err := r.st.Delete(ctx, "index/"+p); err != nil {
		return err
	}
	r.forgetPack(p)
	return r.st.Delete(ctx, "packs/"+p)
}

// liveSet lists committed snapshots and the packs their manifests use, with
// the stored bytes each pack contributes.
func (r *Repo) liveSet(ctx context.Context) (map[string]bool, map[string]int64, []*Snapshot, error) {
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	live := map[string]bool{}
	bytes := map[string]int64{}
	type span struct {
		pack string
		off  uint32
	}
	counted := map[span]bool{}
	for _, s := range snaps {
		for name := range s.Files {
			m, err := r.Manifest(ctx, s, name)
			if err != nil {
				if errors.Is(err, objstore.ErrNotFound) {
					continue // the snapshot was deleted while we read it
				}
				return nil, nil, nil, fmt.Errorf("gc: %w", err)
			}
			for _, ref := range m.Refs {
				if ref.Zero() {
					continue
				}
				p := m.Packs[ref.Pack]
				live[p] = true
				if k := (span{p, ref.Off}); !counted[k] {
					counted[k] = true
					bytes[p] += int64(ref.Len)
				}
			}
		}
	}
	return live, bytes, snaps, nil
}

func (r *Repo) tombstones(ctx context.Context) (map[string]bool, error) {
	objs, err := r.st.List(ctx, "tomb/")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, o := range objs {
		out[strings.TrimPrefix(o.Key, "tomb/")] = true
	}
	return out, nil
}

// storeNow reads the store's clock by writing an object and reading back
// its modification time, so every GC run measures tombstone age on the same
// clock that stamped the tombstones.
func (r *Repo) storeNow(ctx context.Context) (time.Time, error) {
	if err := r.st.Put(ctx, "gc/clock", nil); err != nil {
		return time.Time{}, err
	}
	objs, err := r.st.List(ctx, "gc/clock")
	if err != nil {
		return time.Time{}, err
	}
	for _, o := range objs {
		if o.Key == "gc/clock" {
			return o.Modified, nil
		}
	}
	return time.Time{}, errors.New("gc: clock object vanished")
}

func snapshotOfKey(key, prefix string) string {
	rest := strings.TrimPrefix(key, prefix)
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i]
	}
	return rest
}
