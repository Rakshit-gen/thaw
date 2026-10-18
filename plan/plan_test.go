package plan

import (
	"testing"
	"time"

	"github.com/Rakshit-gen/thaw/trace"
)

func tr(events ...trace.Event) *trace.Trace {
	return &trace.Trace{Files: []string{"mem", "disk"}, Blocks: []int{65536, 65536}, Events: events}
}

func ev(ms int, file uint8, block uint32) trace.Event {
	return trace.Event{At: time.Duration(ms) * time.Millisecond, File: file, Block: block}
}

func TestBuildOrdersByMedianTime(t *testing.T) {
	p := Build([]*trace.Trace{
		tr(ev(1, 0, 10), ev(5, 0, 20), ev(9, 1, 3)),
		tr(ev(2, 0, 10), ev(3, 1, 3), ev(8, 0, 20)),
		tr(ev(1, 0, 10), ev(4, 1, 3), ev(6, 0, 20), ev(7, 0, 99)),
	}, Options{MinFreq: 0.5, Decay: 1})
	if len(p.Items) != 3 {
		t.Fatalf("got %d items, want 3 (block 99 is in one trace of three): %+v", len(p.Items), p.Items)
	}
	want := []uint32{10, 3, 20}
	for i, it := range p.Items {
		if it.Block != want[i] {
			t.Fatalf("order %v", p.Items)
		}
	}
	if p.Items[1].At != 4*time.Millisecond || p.Files[p.Items[1].File] != "disk" {
		t.Fatalf("disk block 3: %+v", p.Items[1])
	}
	if o := p.Order("mem"); len(o) != 2 || o[0] != 10 || o[1] != 20 {
		t.Fatalf("mem order %v", o)
	}
}

func TestNewerTracesCountMore(t *testing.T) {
	old := tr(ev(1, 0, 1))
	recent := tr(ev(1, 0, 2))
	// Five old traces touch block 1, the two newest touch block 2. With a
	// strong decay the plan follows the new workload.
	ts := []*trace.Trace{old, old, old, old, old, recent, recent}
	p := Build(ts, Options{MinFreq: 0.5, Decay: 0.5})
	if len(p.Items) != 1 || p.Items[0].Block != 2 {
		t.Fatalf("plan %+v", p.Items)
	}
	p = Build(ts, Options{MinFreq: 0.5, Decay: 1})
	if len(p.Items) != 1 || p.Items[0].Block != 1 {
		t.Fatalf("without decay %+v", p.Items)
	}
}

func TestZeroBlocksAreNotPlanned(t *testing.T) {
	z := trace.Event{At: time.Millisecond, Block: 5, Kind: trace.Zero}
	p := Build([]*trace.Trace{tr(z, ev(2, 0, 6))}, Options{})
	if len(p.Items) != 1 || p.Items[0].Block != 6 {
		t.Fatalf("plan %+v", p.Items)
	}
}

func TestEvaluate(t *testing.T) {
	p := Build([]*trace.Trace{tr(ev(1, 0, 1), ev(2, 0, 2), ev(3, 0, 3), ev(4, 0, 4))}, Options{})
	s := p.Evaluate(tr(ev(1, 0, 1), ev(2, 0, 2), ev(3, 0, 9)))
	if s.Planned != 4 || s.Needed != 3 || s.Hit != 2 || s.Precision != 0.5 {
		t.Fatalf("%+v", s)
	}
}

func TestTraceRoundTrip(t *testing.T) {
	r := trace.NewRecorder("snap", []string{"mem", "disk"}, []int{65536, 65536})
	r.Touch(0, 7, trace.Demand, r.Elapsed(), 3*time.Millisecond)
	if r.Touch(0, 7, trace.Staged, r.Elapsed(), 0) {
		t.Fatal("second touch should not count")
	}
	r.Touch(1, 2, trace.Staged, r.Elapsed(), 0)
	r.Mark("answered")
	r.Set("task", "x")
	tt := r.Trace()
	got, err := trace.Decode(tt.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 2 || got.Events[0].Block != 7 || got.Events[0].Wait != 3*time.Millisecond || got.Events[1].Kind != trace.Staged {
		t.Fatalf("%+v", got.Events)
	}
	if _, ok := got.Mark("answered"); !ok || got.Meta["task"] != "x" || got.Snapshot != "snap" {
		t.Fatalf("%+v", got)
	}
}
