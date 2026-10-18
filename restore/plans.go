package restore

import (
	"context"

	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/trace"
)

// PlanFor builds a prefetch plan for a snapshot from its own traces. A new
// fork has none, so it borrows its ancestors': a fork's memory and disk
// line up block for block with its parent's, and what the parent touched
// on resume is a good first guess for the child. Ancestors' traces go
// first so the snapshot's own count as newest.
//
// It walks up until it has at least min traces or runs out of ancestors.
// It returns the plan and which snapshots the traces came from.
func PlanFor(ctx context.Context, r *repo.Repo, snap *repo.Snapshot, min int, opt plan.Options) (*plan.Plan, []string, error) {
	if min <= 0 {
		min = 3
	}
	var chain [][]*trace.Trace
	var from []string
	have := 0
	for s, depth := snap, 0; s != nil && have < min && depth < 16; depth++ {
		raw, err := r.Traces(ctx, s.ID)
		if err != nil {
			return nil, nil, err
		}
		var ts []*trace.Trace
		for _, b := range raw {
			t, err := trace.Decode(b)
			if err != nil {
				continue // a damaged trace costs a little accuracy, not the restore
			}
			ts = append(ts, t)
		}
		if len(ts) > 0 {
			chain = append(chain, ts)
			from = append(from, s.ID)
			have += len(ts)
		}
		if s.Parent == "" {
			break
		}
		p, err := r.Snapshot(ctx, s.Parent)
		if err != nil {
			break // parent deleted; use what we have
		}
		s = p
	}
	var all []*trace.Trace
	for i := len(chain) - 1; i >= 0; i-- {
		all = append(all, chain[i]...)
	}
	return plan.Build(all, opt), from, nil
}

// SaveTrace stores a finished restore's trace with its snapshot, unless it
// came from an install-mode restore.
func SaveTrace(ctx context.Context, r *repo.Repo, t *trace.Trace) error {
	if t.Mode == "install" {
		return nil
	}
	return r.PutTrace(ctx, t.Snapshot, t.Name(), t.Encode())
}
