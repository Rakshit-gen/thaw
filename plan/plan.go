// Package plan turns past restores of a snapshot into a prefetch plan: the
// blocks the next restore will probably need, in the order it will probably
// need them.
//
// Each trace says which blocks one restore touched and when. A block goes
// into the plan if a large enough share of recent traces touched it, and
// its place in the plan is the weighted median of when they touched it.
// Newer traces count for more, so a plan follows a workload that drifts.
package plan

import (
	"sort"
	"time"

	"github.com/Rakshit-gen/thaw/trace"
)

// Item is one block to prefetch.
type Item struct {
	File  uint8
	Block uint32
	At    time.Duration // predicted first touch, since the restore began
	Freq  float64       // weighted share of traces that touched it
}

// Plan is an ordered list of blocks to fetch ahead of the guest.
type Plan struct {
	Files  []string
	Items  []Item
	Traces int
}

// Options tune Build.
type Options struct {
	// MinFreq is the weighted share of traces that must have touched a
	// block for it to be planned. Zero means 0.3.
	MinFreq float64
	// Decay is the weight of each older trace relative to the next newer
	// one. Zero means 0.8.
	Decay float64
	// MaxTraces caps how many of the newest traces are used. Zero means 20.
	MaxTraces int
}

// Build makes a plan from traces, oldest first. Blocks a trace saw as
// all-zero are left out: they cost nothing to serve. Traces from
// install-mode restores are ignored.
func Build(traces []*trace.Trace, opt Options) *Plan {
	if opt.MinFreq == 0 {
		opt.MinFreq = 0.3
	}
	if opt.Decay == 0 {
		opt.Decay = 0.8
	}
	if opt.MaxTraces == 0 {
		opt.MaxTraces = 20
	}
	// Install-mode traces only show the blocks prefetch missed.
	kept := traces[:0:0]
	for _, t := range traces {
		if t.Mode != "install" {
			kept = append(kept, t)
		}
	}
	traces = kept
	if len(traces) > opt.MaxTraces {
		traces = traces[len(traces)-opt.MaxTraces:]
	}
	p := &Plan{Traces: len(traces)}
	if len(traces) == 0 {
		return p
	}
	fileIdx := map[string]uint8{}
	for _, t := range traces {
		for _, f := range t.Files {
			if _, ok := fileIdx[f]; !ok {
				fileIdx[f] = uint8(len(p.Files))
				p.Files = append(p.Files, f)
			}
		}
	}

	type key struct {
		file  uint8
		block uint32
	}
	type obs struct {
		at time.Duration
		w  float64
	}
	seen := map[key][]obs{}
	var total float64
	w := 1.0
	for i := len(traces) - 1; i >= 0; i-- { // newest first
		t := traces[i]
		total += w
		for _, ev := range t.Events {
			if ev.Kind == trace.Zero {
				continue
			}
			k := key{fileIdx[t.Files[ev.File]], ev.Block}
			seen[k] = append(seen[k], obs{ev.At, w})
		}
		w *= opt.Decay
	}
	for k, os := range seen {
		var sum float64
		for _, o := range os {
			sum += o.w
		}
		freq := sum / total
		if freq+1e-9 < opt.MinFreq {
			continue
		}
		sort.Slice(os, func(i, j int) bool { return os[i].at < os[j].at })
		// Weighted median of first-touch times.
		var acc float64
		at := os[len(os)-1].at
		for _, o := range os {
			acc += o.w
			if acc >= sum/2 {
				at = o.at
				break
			}
		}
		p.Items = append(p.Items, Item{File: k.file, Block: k.block, At: at, Freq: freq})
	}
	sort.Slice(p.Items, func(i, j int) bool {
		a, b := p.Items[i], p.Items[j]
		if a.At != b.At {
			return a.At < b.At
		}
		if a.File != b.File {
			return a.File < b.File
		}
		return a.Block < b.Block
	})
	return p
}

// Order returns the planned blocks of one file, in plan order. Snapshot
// writes use it to pack hot blocks in the order they'll be read.
func (p *Plan) Order(file string) []int {
	var out []int
	for _, it := range p.Items {
		if p.Files[it.File] == file {
			out = append(out, int(it.Block))
		}
	}
	return out
}

// Score compares a plan with what a restore actually touched. Precision is
// the share of planned blocks the restore used; recall is the share of
// blocks the restore needed (and had to fetch) that the plan had.
type Score struct {
	Planned, Needed, Hit int
	Precision, Recall    float64
}

// Evaluate scores p against a trace of a restore.
func (p *Plan) Evaluate(t *trace.Trace) Score {
	type key struct {
		file  string
		block uint32
	}
	planned := map[key]bool{}
	for _, it := range p.Items {
		planned[key{p.Files[it.File], it.Block}] = true
	}
	var s Score
	s.Planned = len(planned)
	for _, ev := range t.Events {
		if ev.Kind == trace.Zero {
			continue
		}
		s.Needed++
		if planned[key{t.Files[ev.File], ev.Block}] {
			s.Hit++
		}
	}
	if s.Planned > 0 {
		s.Precision = float64(s.Hit) / float64(s.Planned)
	}
	if s.Needed > 0 {
		s.Recall = float64(s.Hit) / float64(s.Needed)
	}
	return s
}
