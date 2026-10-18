package fetch

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
)

const blk = 64 << 10

// slow adds a fixed delay to every ranged get, so slot tests don't depend
// on random latency.
type slow struct {
	objstore.Store
	d time.Duration
}

func (s slow) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	time.Sleep(s.d)
	return s.Store.GetRange(ctx, key, off, n)
}

func content(blocks int) []byte {
	b := make([]byte, blocks*blk)
	for i := range blocks {
		if i%10 == 9 {
			continue // leave some zero blocks
		}
		copy(b[i*blk:], bytes.Repeat([]byte(fmt.Sprintf("block %05d|", i)), blk/12))
	}
	return b
}

type fixture struct {
	r     *repo.Repo
	count *objstore.Counting
	m     *image.Manifest
	data  []byte
	base  objstore.Counts // requests made by setup itself
}

func (fx *fixture) gets() int64 { return fx.count.Counts().Gets - fx.base.Gets }

func setup(t *testing.T, blocks int, delay time.Duration) *fixture {
	t.Helper()
	dir, err := objstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := content(blocks)
	w := repo.Open(dir, "")
	ctx := context.Background()
	s, _, err := w.Write(ctx, repo.Snapshot{Env: "test"}, []repo.FileSpec{{Name: "mem", Block: blk, Reader: bytes.NewReader(data), Size: int64(len(data))}}, repo.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	count := objstore.NewCounting(slow{dir, delay})
	r := repo.Open(count, "")
	m, err := r.Manifest(ctx, s, "mem")
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{r: r, count: count, m: m, data: data, base: count.Counts()}
}

func (fx *fixture) want(i int) []byte { return fx.data[i*blk : (i+1)*blk] }

type sink struct {
	mu   sync.Mutex
	got  map[Block][]byte
	from map[Block]Origin
	dups int
}

func newSink() *sink { return &sink{got: map[Block][]byte{}, from: map[Block]Origin{}} }

func (s *sink) deliver(b Block, data []byte, from Origin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.got[b]; ok {
		s.dups++
	}
	s.got[b] = data
	s.from[b] = from
}

func TestDemandAndPrefetchDeliverRightBytesOnce(t *testing.T) {
	fx := setup(t, 200, 0)
	s := newSink()
	f := New(fx.r, []*image.Manifest{fx.m}, nil, nil, Options{}, s.deliver)
	ctx := context.Background()
	data, _, err := f.Demand(ctx, Block{0, 5})
	if err != nil || !bytes.Equal(data, fx.want(5)) {
		t.Fatalf("demand: %v", err)
	}
	var all []Block
	for i := range 200 {
		all = append(all, Block{0, i})
	}
	if err := f.Prefetch(ctx, all); err != nil {
		t.Fatal(err)
	}
	if s.dups != 0 {
		t.Fatalf("%d blocks delivered twice", s.dups)
	}
	if len(s.got) != 180 {
		t.Fatalf("%d blocks delivered, want 180 (20 are zero)", len(s.got))
	}
	for b, d := range s.got {
		if !bytes.Equal(d, fx.want(b.Index)) {
			t.Fatalf("block %v has wrong bytes", b)
		}
	}
	if _, _, err := f.Demand(ctx, Block{0, 7}); err != ErrDelivered {
		t.Fatalf("demand after delivery: %v", err)
	}
	// 179 blocks of 64 KiB in at most 4 MiB requests, broken where zero
	// blocks leave holes: holes are under MaxGap, so they are read through.
	st := f.Stats()
	// The one hole is block 5, which demand fetched before prefetch began.
	if st.PrefetchRequests > 4 || st.HoleBytes > blk {
		t.Fatalf("prefetch used %d requests, %d hole bytes: %+v", st.PrefetchRequests, st.HoleBytes, st)
	}
}

func TestConcurrentDemandsShareOneRequest(t *testing.T) {
	fx := setup(t, 20, 30*time.Millisecond)
	f := New(fx.r, []*image.Manifest{fx.m}, nil, nil, Options{}, newSink().deliver)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, _, err := f.Demand(context.Background(), Block{0, 3})
			if err == ErrDelivered {
				return
			}
			if err != nil || !bytes.Equal(d, fx.want(3)) {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 {
		t.Fatal("a waiter got the wrong bytes")
	}
	if g := fx.gets(); g != 1 {
		t.Fatalf("%d gets for one block", g)
	}
}

func TestReservedSlotsKeepDemandFast(t *testing.T) {
	const delay = 25 * time.Millisecond
	run := func(reserved int) time.Duration {
		fx := setup(t, 120, delay)
		// One block per request so prefetch keeps every slot busy.
		f := New(fx.r, []*image.Manifest{fx.m}, nil, nil, Options{Streams: 4, Reserved: reserved, MaxSpan: blk}, newSink().deliver)
		var all []Block
		for i := range 100 {
			all = append(all, Block{0, i})
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go f.Prefetch(ctx, all)
		// Ask halfway through a round of prefetch requests.
		time.Sleep(3*delay + delay/2)
		start := time.Now()
		if _, _, err := f.Demand(ctx, Block{0, 110}); err != nil {
			t.Fatal(err)
		}
		return time.Since(start)
	}
	withReserve, shared := run(1), run(-1)
	if withReserve > delay*3/2 {
		t.Fatalf("demand took %v with a reserved slot, want about %v", withReserve, delay)
	}
	if shared-withReserve < delay/4 {
		t.Fatalf("demand took %v sharing slots, %v with a reserve; expected it to queue", shared, withReserve)
	}
	t.Logf("demand latency: reserved %v, shared %v", withReserve, shared)
}

func TestFaultAroundBringsPlannedNeighbors(t *testing.T) {
	fx := setup(t, 40, 0)
	s := newSink()
	f := New(fx.r, []*image.Manifest{fx.m}, nil, nil, Options{FaultAround: 6}, s.deliver)
	f.SetWant(func(b Block) bool { return b.Index < 20 })
	if _, _, err := f.Demand(context.Background(), Block{0, 2}); err != nil {
		t.Fatal(err)
	}
	// Blocks 3..8 ride along (block 9 is zero and isn't in the pack).
	if len(s.got) != 7 || fx.gets() != 1 {
		t.Fatalf("delivered %d with %d gets", len(s.got), fx.gets())
	}
	for i := 2; i <= 8; i++ {
		if !bytes.Equal(s.got[Block{0, i}], fx.want(i)) {
			t.Fatalf("block %d", i)
		}
	}
	if _, _, err := f.Demand(context.Background(), Block{0, 16}); err != nil {
		t.Fatal(err)
	}
	if !f.Delivered(Block{0, 18}) || f.Delivered(Block{0, 20}) {
		t.Fatal("fault-around went past what was wanted")
	}
}

func TestCacheServesSecondRestore(t *testing.T) {
	fx := setup(t, 60, 0)
	c, err := cache.Open(t.TempDir(), 1<<30, 0)
	if err != nil {
		t.Fatal(err)
	}
	var all []Block
	for i := range 60 {
		all = append(all, Block{0, i})
	}
	f := New(fx.r, []*image.Manifest{fx.m}, c, nil, Options{}, newSink().deliver)
	f.Prefetch(context.Background(), all)
	before := fx.count.Counts().Gets

	s := newSink()
	f2 := New(fx.r, []*image.Manifest{fx.m}, c, nil, Options{}, s.deliver)
	f2.Prefetch(context.Background(), all[:30])
	d, from, err := f2.Demand(context.Background(), Block{0, 40})
	if err != nil || from != FromCache || !bytes.Equal(d, fx.want(40)) {
		t.Fatalf("from %v err %v", from, err)
	}
	if fx.count.Counts().Gets != before {
		t.Fatal("second restore went to the store")
	}
	if st := f2.Stats(); st.CacheHits != 28 {
		t.Fatalf("cache hits %d, want 28", st.CacheHits)
	}
}

type fakePeer struct {
	has   map[image.ID][]byte
	calls atomic.Int32
}

func (p *fakePeer) GetMany(_ context.Context, want []Want) (map[image.ID][]byte, error) {
	p.calls.Add(1)
	out := map[image.ID][]byte{}
	for _, w := range want {
		if d, ok := p.has[w.ID]; ok {
			out[w.ID] = d
		}
	}
	return out, nil
}

func TestPeerFirstThenStore(t *testing.T) {
	fx := setup(t, 30, 0)
	p := &fakePeer{has: map[image.ID][]byte{}}
	for i := 0; i < 10; i++ {
		p.has[fx.m.Refs[i].ID] = fx.want(i)
	}
	s := newSink()
	f := New(fx.r, []*image.Manifest{fx.m}, nil, p, Options{}, s.deliver)
	var all []Block
	for i := range 30 {
		all = append(all, Block{0, i})
	}
	f.Prefetch(context.Background(), all)
	st := f.Stats()
	if st.PeerHits != 9 || len(s.got) != 27 {
		t.Fatalf("peer hits %d, delivered %d", st.PeerHits, len(s.got))
	}
	for b, d := range s.got {
		if !bytes.Equal(d, fx.want(b.Index)) {
			t.Fatalf("block %v", b)
		}
		if (b.Index < 10) != (s.from[b] == FromPeer) {
			t.Fatalf("block %v came from %v", b, s.from[b])
		}
	}
}
