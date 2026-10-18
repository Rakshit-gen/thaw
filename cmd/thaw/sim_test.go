package main

import (
	"testing"

	"github.com/Rakshit-gen/thaw/trace"
)

func TestRemapKeepsFirstTouchPerBigBlock(t *testing.T) {
	tr := &trace.Trace{Files: []string{"mem"}, Blocks: []int{64 << 10}, Events: []trace.Event{
		{At: 1, Block: 0, Kind: trace.Zero},
		{At: 2, Block: 3, Kind: trace.Demand},
		{At: 3, Block: 4, Kind: trace.Demand},
		{At: 4, Block: 1, Kind: trace.Staged},
	}}
	got := remap(tr, 256<<10)
	if len(got.Events) != 2 || got.Events[0].Block != 0 || got.Events[1].Block != 1 || got.Events[1].At != 3 {
		t.Fatalf("events = %+v", got.Events)
	}
	if got.Events[0].Kind != trace.Demand || got.Blocks[0] != 256<<10 {
		t.Fatal("remapped events should be re-judged at the new size")
	}
	if len(tr.Events) != 4 || tr.Blocks[0] != 64<<10 {
		t.Fatal("remap changed its input")
	}
}
