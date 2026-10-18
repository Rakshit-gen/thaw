package restore

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/trace"
)

const blk = 64 << 10

func memImage(blocks int, seed uint64) []byte {
	b := make([]byte, blocks*blk)
	rng := rand.New(rand.NewPCG(seed, 1))
	for i := range blocks {
		if rng.IntN(4) == 0 {
			continue // a quarter of guest memory is never written
		}
		copy(b[i*blk:], bytes.Repeat([]byte(fmt.Sprintf("page %d.%d ", seed, i)), blk/10))
	}
	return b
}

// workload is a made-up restore: up to 150 touches spread over the image
// with a millisecond of compute between them.
func workload(snap string, blocks int, seed uint64) *trace.Trace {
	rng := rand.New(rand.NewPCG(seed, 2))
	t := &trace.Trace{Snapshot: snap, Files: []string{"mem"}, Blocks: []int{blk}, Marks: map[string]int64{}}
	seen := map[uint32]bool{}
	at := time.Duration(0)
	for len(t.Events) < min(150, blocks/2) {
		b := uint32(rng.IntN(blocks))
		if seen[b] {
			continue
		}
		seen[b] = true
		t.Events = append(t.Events, trace.Event{At: at, Block: b})
		at += time.Millisecond
	}
	t.Marks["answered"] = int64(at + 20*time.Millisecond)
	return t
}

func setup(t *testing.T, data []byte, p objstore.Profile) (*repo.Repo, *repo.Snapshot) {
	t.Helper()
	dir, _ := objstore.NewDir(t.TempDir())
	ctx := context.Background()
	w := repo.Open(dir, "")
	s, _, err := w.Write(ctx, repo.Snapshot{Env: "test"}, []repo.FileSpec{{Name: "mem", Block: blk, Reader: bytes.NewReader(data), Size: int64(len(data))}}, repo.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return repo.Open(objstore.NewShaped(dir, p, 7), ""), s
}

func TestStrategiesOnAReplayedWorkload(t *testing.T) {
	const blocks = 1024 // 64 MiB
	data := memImage(blocks, 1)
	r, snap := setup(t, data, objstore.S3Express)
	w := workload(snap.ID, blocks, 1)
	pl := plan.Build([]*trace.Trace{w}, plan.Options{})
	ctx := context.Background()

	got := map[Strategy]time.Duration{}
	for _, strat := range Strategies {
		s, err := New(ctx, r, snap, Options{Strategy: strat, Plan: pl})
		if err != nil {
			t.Fatal(err)
		}
		begin := time.Now()
		if err := s.Start(ctx); err != nil {
			t.Fatal(err)
		}
		before := time.Since(begin) // the full strategy downloads here
		res, err := Replay(ctx, s, w)
		if err != nil {
			t.Fatal(err)
		}
		got[strat] = before + res.Answered
		st := s.Stats()
		tr := s.Close()
		t.Logf("%-8s answered %7v  stalled %7v  demand %3d staged-hit %3d  requests %d+%d",
			strat, got[strat].Round(time.Millisecond), res.Stalled.Round(time.Millisecond), st.Demand, st.FromStage,
			st.Fetch.DemandRequests, st.Fetch.PrefetchRequests)
		if len(tr.Events) != len(w.Events) {
			t.Fatalf("%s: trace has %d touches, guest made %d", strat, len(tr.Events), len(w.Events))
		}
		// The trace must show every block the guest touched, prefetched or
		// not, or the next plan would learn from its own guesses.
		if p := pl.Evaluate(tr); p.Recall < 0.99 {
			t.Fatalf("%s: trace recall against the plan %.2f", strat, p.Recall)
		}
	}
	if got[Thaw] >= got[Lazy] || got[Thaw] >= got[Full] {
		t.Fatalf("thaw %v should beat lazy %v and full %v", got[Thaw], got[Lazy], got[Full])
	}
}

func TestGetReturnsTheSnapshotBytes(t *testing.T) {
	data := memImage(200, 3)
	r, snap := setup(t, data, objstore.Peer)
	ctx := context.Background()
	s, err := New(ctx, r, snap, Options{Strategy: Hydrate})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	for i := 199; i >= 0; i-- {
		b, err := s.Get(ctx, 0, i)
		if err != nil {
			t.Fatal(err)
		}
		want := data[i*blk : (i+1)*blk]
		if b == nil {
			if !bytes.Equal(want, make([]byte, blk)) {
				t.Fatalf("block %d came back as zero", i)
			}
			continue
		}
		if !bytes.Equal(b, want) {
			t.Fatalf("block %d wrong", i)
		}
	}
	s.Wait()
	if st := s.Stats(); st.Unused != 0 || st.Touched != 200 {
		t.Fatalf("%+v", st)
	}
}

func TestForkBorrowsParentTraces(t *testing.T) {
	data := memImage(100, 4)
	dir, _ := objstore.NewDir(t.TempDir())
	ctx := context.Background()
	r := repo.Open(dir, "")
	spec := []repo.FileSpec{{Name: "mem", Block: blk, Reader: bytes.NewReader(data), Size: int64(len(data))}}
	parent, _, err := r.Write(ctx, repo.Snapshot{Env: "test"}, spec, repo.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	child, _, err := r.Write(ctx, repo.Snapshot{Env: "test", Parent: parent.ID}, spec, repo.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		w := workload(parent.ID, 100, uint64(10+i))
		w.Started = time.Now().Add(time.Duration(i) * time.Second)
		if err := SaveTrace(ctx, r, w); err != nil {
			t.Fatal(err)
		}
	}
	p, from, err := PlanFor(ctx, r, child, 3, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(from) != 1 || from[0] != parent.ID || p.Traces != 3 || len(p.Items) == 0 {
		t.Fatalf("plan from %v with %d traces and %d items", from, p.Traces, len(p.Items))
	}
}

func TestInstallModeInstallsPrefetchedBlocks(t *testing.T) {
	data := memImage(64, 5)
	r, snap := setup(t, data, objstore.Peer)
	ctx := context.Background()
	var installed atomic.Int32
	s, err := New(ctx, r, snap, Options{Strategy: Hydrate, Mode: Install, Install: func(file, block int, d []byte) {
		if bytes.Equal(d, data[block*blk:(block+1)*blk]) {
			installed.Add(1)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	s.Wait()
	nonzero := 0
	for _, ref := range s.Files[0].Refs {
		if !ref.Zero() {
			nonzero++
		}
	}
	if int(installed.Load()) != nonzero {
		t.Fatalf("installed %d of %d", installed.Load(), nonzero)
	}
	if tr := s.Close(); tr.Mode != "install" {
		t.Fatal("install-mode trace not marked")
	}
}
