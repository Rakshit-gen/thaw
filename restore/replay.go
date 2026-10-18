package restore

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/trace"
)

// Replay plays a recorded restore back against a session, standing in for
// the guest: it touches the same blocks in the same order, and between
// touches it spends the same time computing as the recorded guest did.
// What changes is how long each touch waits, which is what a strategy
// decides.
//
// A guest has several vCPUs and the disk has its own queue, so touches
// overlap. Replay splits the trace into lanes: a touch that began while an
// earlier one was still waiting goes on another lane, and lanes run
// concurrently. Ordering between lanes is lost, which is the main way a
// replay differs from the real thing.
func Replay(ctx context.Context, s *Session, t *trace.Trace) (ReplayResult, error) {
	answered, ok := t.Mark("answered")
	if !ok && len(t.Events) > 0 {
		last := t.Events[len(t.Events)-1]
		answered = last.At + last.Wait
	}
	fileIdx := map[string]int{}
	for i, n := range s.Names {
		fileIdx[n] = i
	}

	type lane struct {
		end    time.Duration // when its last recorded touch finished waiting
		events []trace.Event
	}
	var lanes []*lane
	var recordedEnd time.Duration
	for _, ev := range t.Events {
		var best *lane
		for _, l := range lanes {
			if l.end <= ev.At && (best == nil || l.end > best.end) {
				best = l
			}
		}
		if best == nil {
			best = &lane{}
			lanes = append(lanes, best)
		}
		best.events = append(best.events, ev)
		best.end = ev.At + ev.Wait
		if best.end > recordedEnd {
			recordedEnd = best.end
		}
	}
	tail := answered - recordedEnd
	if tail < 0 {
		tail = 0
	}

	start := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	var stalled time.Duration
	var stalls []time.Duration
	ends := make([]time.Duration, len(lanes))
	for li, l := range lanes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var recPrev, playPrev time.Duration
			for _, ev := range l.events {
				fi, ok := fileIdx[t.Files[ev.File]]
				if !ok {
					continue
				}
				target := playPrev + (ev.At - recPrev)
				if d := target - time.Since(start); d > 0 {
					select {
					case <-time.After(d):
					case <-ctx.Done():
						return
					}
				}
				t0 := time.Since(start)
				if _, err := s.Get(ctx, fi, int(ev.Block)); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
				playPrev = time.Since(start)
				recPrev = ev.At + ev.Wait
				w := playPrev - t0
				mu.Lock()
				stalled += w
				if w > 500*time.Microsecond {
					stalls = append(stalls, w)
				}
				mu.Unlock()
			}
			ends[li] = playPrev
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return ReplayResult{}, firstErr
	}
	var end time.Duration
	for _, e := range ends {
		end = max(end, e)
	}
	// The recorded guest kept computing after its last touch; so does this
	// one, without sleeping through it.
	sort.Slice(stalls, func(i, j int) bool { return stalls[i] > stalls[j] })
	return ReplayResult{
		Answered: end + tail,
		Stalled:  stalled,
		Stalls:   len(stalls),
		Lanes:    len(lanes),
		Touches:  len(t.Events),
		Baseline: answered,
	}, nil
}

// ReplayResult is how a replayed restore went.
type ReplayResult struct {
	Answered time.Duration // from session start to the guest's answer
	Stalled  time.Duration // summed over lanes, time spent waiting on blocks
	Stalls   int           // touches that waited over half a millisecond
	Lanes    int
	Touches  int
	Baseline time.Duration // the recorded time to answer
}
