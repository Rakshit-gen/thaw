package repo

import (
	"bytes"
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rakshit-gen/thaw/objstore"
)

var errCrashed = errors.New("client crashed")

// chaosStore delays every request a little and, once armed, fails every
// request after a set number, as if the process using it died mid-write.
type chaosStore struct {
	objstore.Store
	rng      *rand.Rand
	mu       sync.Mutex
	opsLeft  int // -1: never crash
	maxDelay time.Duration
}

func (c *chaosStore) step() error {
	c.mu.Lock()
	d := time.Duration(c.rng.Int64N(int64(c.maxDelay)))
	crashed := c.opsLeft == 0
	if c.opsLeft > 0 {
		c.opsLeft--
	}
	c.mu.Unlock()
	time.Sleep(d)
	if crashed {
		return errCrashed
	}
	return nil
}

func (c *chaosStore) Put(ctx context.Context, k string, b []byte) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.Store.Put(ctx, k, b)
}
func (c *chaosStore) Get(ctx context.Context, k string) ([]byte, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.Store.Get(ctx, k)
}
func (c *chaosStore) GetRange(ctx context.Context, k string, off, n int64) ([]byte, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.Store.GetRange(ctx, k, off, n)
}
func (c *chaosStore) List(ctx context.Context, p string) ([]objstore.Object, error) {
	if err := c.step(); err != nil {
		return nil, err
	}
	return c.Store.List(ctx, p)
}
func (c *chaosStore) Delete(ctx context.Context, k string) error {
	if err := c.step(); err != nil {
		return err
	}
	return c.Store.Delete(ctx, k)
}

type chaosResult struct {
	commits, tooSlow, crashes, deletes, gcRuns, packsDeleted int
	damaged                                                  []string
}

// runChaos has writers fork snapshots from each other while a deleter
// removes snapshots and a collector runs GC, with random delays and writers
// that crash mid-write. Afterwards every snapshot that committed and wasn't
// deleted must read back byte for byte.
func runChaos(t *testing.T, mode gcMode, dur time.Duration, seed uint64) chaosResult {
	t.Helper()
	dir, err := objstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const grace = 150 * time.Millisecond
	const blocks, block = 24, 4096

	var (
		mu       sync.Mutex
		contents = map[string][]byte{} // committed snapshot -> its bytes
		res      chaosResult
	)
	ctx := context.Background()
	base := fakeImage(blocks, block, seed, 5)
	{
		r := Open(dir, "")
		s, _, err := r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Block: block, Reader: bytes.NewReader(base), Size: int64(len(base))}}, WriteOptions{PackSize: 4 * block})
		if err != nil {
			t.Fatal(err)
		}
		contents[s.ID] = base
	}

	stop := time.Now().Add(dur)
	var wg sync.WaitGroup
	var nextSeed atomic.Uint64
	nextSeed.Store(seed * 1000)

	pick := func(rng *rand.Rand) (string, []byte) {
		mu.Lock()
		defer mu.Unlock()
		ids := make([]string, 0, len(contents))
		for id := range contents {
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			return "", nil
		}
		id := ids[rng.IntN(len(ids))]
		return id, contents[id]
	}

	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(seed, uint64(w)))
			// One long-lived client per writer, like a host daemon, so a
			// stale in-memory index is part of what's tested.
			cs := &chaosStore{Store: dir, rng: rand.New(rand.NewPCG(seed, uint64(w)+100)), opsLeft: -1, maxDelay: 4 * time.Millisecond}
			r := Open(cs, "")
			r.Grace = grace
			for time.Now().Before(stop) {
				parent, pc := pick(rng)
				if pc == nil {
					pc = base
				}
				child := append([]byte(nil), pc...)
				for range 1 + rng.IntN(3) {
					i := rng.IntN(blocks)
					child[i*block+rng.IntN(block)] ^= byte(1 + rng.IntN(255))
				}
				crash := rng.IntN(5) == 0
				cs.mu.Lock()
				if crash {
					cs.opsLeft = rng.IntN(12)
				} else {
					cs.opsLeft = -1
				}
				cs.mu.Unlock()
				s, _, err := r.Write(ctx, Snapshot{Parent: parent}, []FileSpec{{Name: "mem", Block: block, Reader: bytes.NewReader(child), Size: int64(len(child))}}, WriteOptions{PackSize: 4 * block, Uploads: 2})
				mu.Lock()
				switch {
				case err == nil:
					contents[s.ID] = child
					res.commits++
				case errors.Is(err, ErrTooSlow):
					res.tooSlow++
				case errors.Is(err, errCrashed):
					res.crashes++
					// A crashed process comes back with nothing in memory.
					r = Open(cs, "")
					r.Grace = grace
				default:
					mu.Unlock()
					t.Errorf("write: %v", err)
					return
				}
				mu.Unlock()
			}
		}()
	}

	wg.Add(1)
	go func() { // deleter
		defer wg.Done()
		rng := rand.New(rand.NewPCG(seed, 50))
		r := Open(dir, "")
		for time.Now().Before(stop) {
			time.Sleep(time.Duration(5+rng.IntN(20)) * time.Millisecond)
			mu.Lock()
			if len(contents) > 3 {
				for id := range contents {
					if rng.IntN(2) == 0 {
						delete(contents, id)
						mu.Unlock()
						r.Delete(ctx, id)
						mu.Lock()
						res.deletes++
						break
					}
				}
			}
			mu.Unlock()
		}
	}()

	wg.Add(1)
	go func() { // collector
		defer wg.Done()
		cs := &chaosStore{Store: dir, rng: rand.New(rand.NewPCG(seed, 60)), opsLeft: -1, maxDelay: 2 * time.Millisecond}
		r := Open(cs, "")
		r.Grace = grace
		for time.Now().Before(stop) {
			st, err := r.gc(ctx, mode)
			if err != nil {
				t.Errorf("gc: %v", err)
				return
			}
			mu.Lock()
			res.gcRuns++
			res.packsDeleted += st.Deleted
			mu.Unlock()
		}
	}()
	wg.Wait()

	r := Open(dir, "")
	for id, want := range contents {
		s, err := r.Snapshot(ctx, id)
		if err != nil {
			res.damaged = append(res.damaged, id+": "+err.Error())
			continue
		}
		m, err := r.Manifest(ctx, s, "mem")
		if err != nil {
			res.damaged = append(res.damaged, id+": "+err.Error())
			continue
		}
		var got []byte
		for i, ref := range m.Refs {
			if ref.Zero() {
				got = append(got, make([]byte, m.BlockLen(i))...)
				continue
			}
			b, err := r.ReadLoc(ctx, LocOf(m, i), ref.ID, m.BlockLen(i))
			if err != nil {
				res.damaged = append(res.damaged, id+": "+err.Error())
				break
			}
			got = append(got, b...)
		}
		if len(got) == len(want) && !bytes.Equal(got, want) {
			res.damaged = append(res.damaged, id+": wrong bytes")
		}
	}
	return res
}

func TestChaosTwoPhaseGCLosesNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for seed := uint64(1); seed <= 3; seed++ {
		res := runChaos(t, gcTwoPhase, 2*time.Second, seed)
		t.Logf("seed %d: %d commits, %d too slow, %d crashed, %d snapshots deleted, %d GC runs deleted %d packs", seed, res.commits, res.tooSlow, res.crashes, res.deletes, res.gcRuns, res.packsDeleted)
		if res.commits < 20 || res.gcRuns < 5 || res.packsDeleted < 20 {
			t.Fatalf("not enough happened to mean anything: %+v", res)
		}
		if len(res.damaged) > 0 {
			t.Fatalf("seed %d: %d snapshots damaged, first: %s", seed, len(res.damaged), res.damaged[0])
		}
	}
}

// The single-phase collector deletes unreferenced packs at once. It must
// lose data under the same load, or the test above proves nothing.
func TestChaosCatchesUnsafeGC(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	for seed := uint64(1); seed <= 5; seed++ {
		res := runChaos(t, gcUnsafeImmediate, 2*time.Second, seed)
		if len(res.damaged) > 0 {
			t.Logf("seed %d: unsafe GC damaged %d of the surviving snapshots, e.g. %s", seed, len(res.damaged), res.damaged[0])
			return
		}
	}
	t.Fatal("the unsafe collector never damaged a snapshot; the chaos test is too weak")
}

// After writers stop, two GC runs past the grace period leave only packs
// that live snapshots use: crashed writes don't leak.
func TestGCLeavesNoLeaksAfterCrashes(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	dir, _ := objstore.NewDir(t.TempDir())
	ctx := context.Background()
	img := fakeImage(16, 4096, 9, 0)
	for i := range 6 {
		cs := &chaosStore{Store: dir, rng: rand.New(rand.NewPCG(9, uint64(i))), opsLeft: 3 + i, maxDelay: time.Millisecond}
		r := Open(cs, "")
		mod := append([]byte(nil), img...)
		mod[i*4096] ^= 1
		r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Block: 4096, Reader: bytes.NewReader(mod), Size: int64(len(mod))}}, WriteOptions{PackSize: 2 * 4096, Uploads: 1})
	}
	r := Open(dir, "")
	keep, _, err := r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Block: 4096, Reader: bytes.NewReader(img), Size: int64(len(img))}}, WriteOptions{PackSize: 2 * 4096})
	if err != nil {
		t.Fatal(err)
	}
	r.Grace = 0
	r.GC(ctx)
	st, _ := r.GC(ctx)
	if st.Packs-st.Deleted != st.LivePacks || st.LivePacks == 0 {
		t.Fatalf("leak: %+v", st)
	}
	if !bytes.Equal(readAll(t, r, keep, "mem"), img) {
		t.Fatal("live snapshot damaged")
	}
}

func TestSlowWriterGivesUp(t *testing.T) {
	dir, _ := objstore.NewDir(t.TempDir())
	cs := &chaosStore{Store: dir, rng: rand.New(rand.NewPCG(1, 1)), opsLeft: -1, maxDelay: 20 * time.Millisecond}
	r := Open(cs, "")
	r.Grace = 30 * time.Millisecond
	img := fakeImage(32, 4096, 3, 0)
	_, _, err := r.Write(context.Background(), Snapshot{}, []FileSpec{{Name: "mem", Block: 4096, Reader: bytes.NewReader(img), Size: int64(len(img))}}, WriteOptions{PackSize: 4096, Uploads: 1})
	if !errors.Is(err, ErrTooSlow) {
		t.Fatalf("got %v, want ErrTooSlow", err)
	}
	if snaps, _ := Open(dir, "").Snapshots(context.Background()); len(snaps) != 0 {
		t.Fatal("a write that gave up must not commit")
	}
}
