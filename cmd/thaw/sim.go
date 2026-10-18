package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/fetch"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/peer"
	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/restore"
	"github.com/Rakshit-gen/thaw/trace"
)

var defaultPlan = plan.Options{}

// The sweeps below replay traces recorded on real VMs. A replay stands in
// for the guest: it touches the same blocks with the same compute gaps in
// between, and only the waiting changes. That lets one recorded workload be
// run against stores, settings and fleet sizes the test machine can't
// reproduce. Every row in sim.json says it is a replay.

// SimRow is one replayed restore, or one aggregate of several.
type SimRow struct {
	Sweep    string  `json:"sweep"`
	Env      string  `json:"env"`
	Task     string  `json:"task,omitempty"`
	Variant  string  `json:"variant"`
	Profile  string  `json:"profile"`
	Answered float64 `json:"answered_ms"`
	Baseline float64 `json:"recorded_ms"` // what the real VM took when the trace was recorded
	Stalled  float64 `json:"stalled_ms"`
	Stalls   int     `json:"stalls"`
	Gets     int64   `json:"gets"`
	Bytes    int64   `json:"bytes"`
	Touched  int     `json:"touched"`
	// Plan quality against the replayed trace, for thaw rows.
	Planned   int     `json:"planned,omitempty"`
	Precision float64 `json:"precision,omitempty"`
	Recall    float64 `json:"recall,omitempty"`
	// Fan-out rows.
	N           int     `json:"n,omitempty"`
	P50         float64 `json:"p50_ms,omitempty"`
	P95         float64 `json:"p95_ms,omitempty"`
	CostPer1000 float64 `json:"cost_per_1000_usd,omitempty"` // store requests only
}

// SimResult is everything thaw sim measured.
type SimResult struct {
	When  time.Time     `json:"when"`
	Note  string        `json:"note"`
	Rows  []SimRow      `json:"rows"`
	Took  time.Duration `json:"took_ns"`
	Store string        `json:"store"`
}

type simEnv struct {
	snap   *repo.Snapshot
	traces []*trace.Trace // stage-mode traces with an answer, oldest first
	tasks  []string
}

// test returns the newest trace of task, and the traces a plan for it may
// learn from: every other trace, or with others only those of other tasks.
func (e *simEnv) test(task string, others bool) (*trace.Trace, []*trace.Trace) {
	var held *trace.Trace
	for i := len(e.traces) - 1; i >= 0; i-- {
		if e.traces[i].Task == task {
			held = e.traces[i]
			break
		}
	}
	var learn []*trace.Trace
	for _, t := range e.traces {
		if t == held || (others && t.Task == task) {
			continue
		}
		learn = append(learn, t)
	}
	return held, learn
}

type simulator struct {
	c        common
	parallel int
	mu       sync.Mutex
	res      SimResult
}

// repoFor opens a fresh view of the store shaped by profile, with its own
// link and request counter, as one host would have.
func (sm *simulator) repoFor(profile string, seed uint64) (*repo.Repo, *objstore.Counting, objstore.Store, error) {
	st, err := objstore.Open(sm.c.store)
	if err != nil {
		return nil, nil, nil, err
	}
	counted := objstore.NewCounting(st)
	var s objstore.Store = counted
	if profile != "none" {
		p, ok := objstore.ProfileByName(profile)
		if !ok {
			return nil, nil, nil, fmt.Errorf("unknown profile %q", profile)
		}
		s = objstore.NewShaped(counted, p, seed)
	}
	return repo.Open(s, ""), counted, s, nil
}

type replayCase struct {
	sweep, variant, profile string
	env                     *simEnv
	trace                   *trace.Trace
	opt                     restore.Options
	seed                    uint64
}

func (sm *simulator) replay(ctx context.Context, rc replayCase) (SimRow, error) {
	r, counted, _, err := sm.repoFor(rc.profile, rc.seed)
	if err != nil {
		return SimRow{}, err
	}
	t0 := time.Now()
	s, err := restore.New(ctx, r, rc.env.snap, rc.opt)
	if err != nil {
		return SimRow{}, err
	}
	if err := s.Start(ctx); err != nil {
		return SimRow{}, err
	}
	setup := time.Since(t0) // the whole download, for full
	rr, err := restore.Replay(ctx, s, rc.trace)
	s.Close()
	if err != nil {
		return SimRow{}, err
	}
	c := counted.Counts()
	row := SimRow{Sweep: rc.sweep, Env: rc.env.snap.Env, Task: rc.trace.Task, Variant: rc.variant, Profile: rc.profile,
		Answered: msOf(setup + rr.Answered), Baseline: msOf(rr.Baseline), Stalled: msOf(rr.Stalled), Stalls: rr.Stalls,
		Gets: c.Gets, Bytes: c.BytesOut, Touched: rr.Touches}
	if rc.opt.Plan != nil {
		sc := rc.opt.Plan.Evaluate(rc.trace)
		row.Planned, row.Precision, row.Recall = sc.Planned, sc.Precision, sc.Recall
	}
	return row, nil
}

// runAll replays cases a few at a time and records their rows.
func (sm *simulator) runAll(ctx context.Context, cases []replayCase) error {
	sem := make(chan struct{}, sm.parallel)
	var wg sync.WaitGroup
	var firstErr error
	for _, rc := range cases {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			row, err := sm.replay(ctx, rc)
			sm.mu.Lock()
			defer sm.mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("%s %s %s: %w", rc.sweep, rc.variant, rc.trace.Task, err)
				}
				return
			}
			sm.res.Rows = append(sm.res.Rows, row)
			logf("%-9s %-8s %-18s %-22s %-11s %8.0f ms (recorded %6.0f)", row.Sweep, row.Env, row.Task, row.Variant, row.Profile, row.Answered, row.Baseline)
		}()
	}
	wg.Wait()
	return firstErr
}

func cmdSim(ctx context.Context, args []string) error {
	sm := &simulator{}
	fs := flag.NewFlagSet("sim", flag.ExitOnError)
	sm.c.register(fs)
	out := fs.String("out", "results/sim.json", "where to write results")
	sweeps := fs.String("sweeps", "strategies,fetch,training,minfreq,fanout,blocksize", "which sweeps to run")
	fs.IntVar(&sm.parallel, "parallel", 4, "replays at once")
	fs.Parse(args)
	start := time.Now()
	sm.res = SimResult{When: start.UTC(), Store: sm.c.store,
		Note: "Replays of traces recorded on real Firecracker VMs, against a local store shaped to each profile. Compute gaps come from the recording; only waiting on blocks is simulated."}
	save := func() error {
		sm.mu.Lock()
		defer sm.mu.Unlock()
		sm.res.Took = time.Since(start)
		sort.SliceStable(sm.res.Rows, func(i, j int) bool {
			a, b := sm.res.Rows[i], sm.res.Rows[j]
			if a.Sweep != b.Sweep {
				return a.Sweep < b.Sweep
			}
			if a.Env != b.Env {
				return a.Env < b.Env
			}
			return a.Task < b.Task
		})
		return writeJSON(*out, sm.res)
	}

	envs, err := sm.load(ctx)
	if err != nil {
		return err
	}
	for _, sw := range strings.Split(*sweeps, ",") {
		var err error
		switch sw {
		case "strategies":
			err = sm.strategies(ctx, envs)
		case "fetch":
			err = sm.fetchSweep(ctx, envs)
		case "training":
			err = sm.training(ctx, envs)
		case "minfreq":
			err = sm.minFreq(ctx, envs)
		case "fanout":
			err = sm.fanout(ctx, envs)
		case "blocksize":
			err = sm.blocksize(ctx, envs)
		default:
			err = fmt.Errorf("unknown sweep %q", sw)
		}
		if err != nil {
			return errors2(save(), err)
		}
		if err := save(); err != nil {
			return err
		}
	}
	return save()
}

func errors2(a, b error) error {
	if a != nil {
		return fmt.Errorf("%w (and saving failed: %v)", b, a)
	}
	return b
}

// load finds base snapshots with recorded traces, newest per environment.
func (sm *simulator) load(ctx context.Context) ([]*simEnv, error) {
	r, _, _, err := sm.repoFor("none", 0)
	if err != nil {
		return nil, err
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Created.After(snaps[j].Created) })
	seen := map[string]bool{}
	var out []*simEnv
	for _, s := range snaps {
		if s.Meta["kind"] != "base" || seen[s.Env] {
			continue
		}
		raw, err := r.Traces(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		e := &simEnv{snap: s}
		have := map[string]bool{}
		for _, b := range raw {
			t, err := trace.Decode(b)
			if err != nil || t.Mode == "install" {
				continue
			}
			if _, ok := t.Mark("answered"); !ok || t.Task == "" {
				continue
			}
			e.traces = append(e.traces, t)
			if !have[t.Task] {
				have[t.Task] = true
				e.tasks = append(e.tasks, t.Task)
			}
		}
		if len(e.traces) < 2 {
			continue
		}
		sort.Slice(e.traces, func(i, j int) bool { return e.traces[i].Started.Before(e.traces[j].Started) })
		sort.Strings(e.tasks)
		seen[s.Env] = true
		out = append(out, e)
		logf("%s: %s with %d traces over %d tasks", s.Env, s.ID, len(e.traces), len(e.tasks))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no base snapshots with traces in %s; run thaw bench first", sm.c.store)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].snap.Env < out[j].snap.Env })
	return out, nil
}

// strategies: every strategy under every store profile, plus thaw with a
// plan that has never seen the task it is restoring for.
func (sm *simulator) strategies(ctx context.Context, envs []*simEnv) error {
	var cases []replayCase
	for _, e := range envs {
		for _, task := range e.tasks {
			held, learn := e.test(task, false)
			_, others := e.test(task, true)
			for _, prof := range []string{"s3-standard", "s3-express", "peer"} {
				for _, v := range []struct {
					name string
					opt  restore.Options
				}{
					{"full", restore.Options{Strategy: restore.Full}},
					{"lazy", restore.Options{Strategy: restore.Lazy}},
					{"hydrate", restore.Options{Strategy: restore.Hydrate}},
					{"thaw", restore.Options{Strategy: restore.Thaw, Plan: plan.Build(learn, defaultPlan)}},
					{"thaw, unseen task", restore.Options{Strategy: restore.Thaw, Plan: plan.Build(others, defaultPlan)}},
				} {
					cases = append(cases, replayCase{sweep: "strategy", variant: v.name, profile: prof, env: e, trace: held, opt: v.opt, seed: 7})
				}
			}
		}
	}
	return sm.runAll(ctx, cases)
}

// fetchSweep varies the fetcher's knobs under thaw and lazy.
func (sm *simulator) fetchSweep(ctx context.Context, envs []*simEnv) error {
	var cases []replayCase
	for _, e := range envs {
		for _, task := range e.tasks {
			held, learn := e.test(task, false)
			p := plan.Build(learn, defaultPlan)
			add := func(name string, s restore.Strategy, o fetch.Options) {
				opt := restore.Options{Strategy: s, Fetch: o}
				if s == restore.Thaw {
					opt.Plan = p
				}
				cases = append(cases, replayCase{sweep: "fetch", variant: name, profile: "s3-express", env: e, trace: held, opt: opt, seed: 11})
			}
			for _, n := range []int{4, 8, 16, 32} {
				add(fmt.Sprintf("thaw, %d streams", n), restore.Thaw, fetch.Options{Streams: n, Reserved: max(1, n/4)})
			}
			add("thaw, shared streams", restore.Thaw, fetch.Options{Reserved: -1})
			add("thaw, no coalescing", restore.Thaw, fetch.Options{MaxSpan: 64 << 10, MaxGap: 1})
			add("thaw, fault-around 8", restore.Thaw, fetch.Options{FaultAround: 8})
			add("lazy", restore.Lazy, fetch.Options{})
			add("lazy, fault-around 8", restore.Lazy, fetch.Options{FaultAround: 8})
		}
	}
	return sm.runAll(ctx, cases)
}

// training: how many traces a plan needs.
func (sm *simulator) training(ctx context.Context, envs []*simEnv) error {
	var cases []replayCase
	for _, e := range envs {
		for _, task := range e.tasks {
			held, learn := e.test(task, false)
			for _, k := range []int{0, 1, 2, 3, 5, 9} {
				if k > len(learn) {
					continue
				}
				p := plan.Build(learn[len(learn)-k:], defaultPlan)
				cases = append(cases, replayCase{sweep: "training", variant: fmt.Sprintf("%d traces", k), profile: "s3-express", env: e, trace: held,
					opt: restore.Options{Strategy: restore.Thaw, Plan: p}, seed: 13})
			}
		}
	}
	return sm.runAll(ctx, cases)
}

// minFreq: how often a block must show up in the traces to be planned.
// Traces pool every task of a snapshot, so a block only one task touches
// is in about 1/tasks of them. A low threshold keeps those blocks for that
// task and makes every other task fetch them for nothing.
func (sm *simulator) minFreq(ctx context.Context, envs []*simEnv) error {
	var cases []replayCase
	for _, e := range envs {
		for _, task := range e.tasks {
			held, learn := e.test(task, false)
			for _, f := range []float64{0.1, 0.2, 0.3, 0.5, 0.8} {
				p := plan.Build(learn, plan.Options{MinFreq: f})
				cases = append(cases, replayCase{sweep: "minfreq", variant: fmt.Sprintf("min share %.1f", f), profile: "s3-express", env: e, trace: held,
					opt: restore.Options{Strategy: restore.Thaw, Plan: p}, seed: 19})
			}
		}
	}
	return sm.runAll(ctx, cases)
}

// fanout: n agents restore the same snapshot at once. Without help each
// one reads the hot set from the store; a shared host cache or a peer
// ring reads it once.
func (sm *simulator) fanout(ctx context.Context, envs []*simEnv) error {
	for _, e := range envs {
		task := e.tasks[0]
		held, learn := e.test(task, false)
		p := plan.Build(learn, defaultPlan)
		for _, n := range []int{1, 4, 16} {
			for _, setup := range []string{"independent", "one host cache", "peer ring of 4"} {
				row, err := sm.fanoutOnce(ctx, e, held, p, n, setup)
				if err != nil {
					return fmt.Errorf("fanout %s n=%d: %w", setup, n, err)
				}
				sm.mu.Lock()
				sm.res.Rows = append(sm.res.Rows, row)
				sm.mu.Unlock()
				logf("fanout   %-8s n=%-3d %-16s p50 %6.0f ms  p95 %6.0f ms  %5d gets  $%.4f per 1000", e.snap.Env, n, setup, row.P50, row.P95, row.Gets, row.CostPer1000)
			}
		}
	}
	return nil
}

func (sm *simulator) fanoutOnce(ctx context.Context, e *simEnv, held *trace.Trace, p *plan.Plan, n int, setup string) (SimRow, error) {
	const hosts = 4
	type hostView struct {
		r       *repo.Repo
		counted *objstore.Counting
		cache   *cache.Cache
		peer    fetch.Peer
	}
	var views []*hostView
	var dirs []string
	defer func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}()
	newCache := func() (*cache.Cache, error) {
		d, err := os.MkdirTemp("", "thaw-sim-cache")
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, d)
		return cache.Open(d, 4<<30, 0)
	}
	switch setup {
	case "independent":
		for i := 0; i < n; i++ {
			r, c, _, err := sm.repoFor("s3-express", uint64(100+i))
			if err != nil {
				return SimRow{}, err
			}
			views = append(views, &hostView{r: r, counted: c})
		}
	case "one host cache":
		r, c, _, err := sm.repoFor("s3-express", 100)
		if err != nil {
			return SimRow{}, err
		}
		ch, err := newCache()
		if err != nil {
			return SimRow{}, err
		}
		views = append(views, &hostView{r: r, counted: c, cache: ch})
	case "peer ring of 4":
		var urls []string
		var servers []*peer.Server
		for i := 0; i < hosts; i++ {
			r, c, _, err := sm.repoFor("s3-express", uint64(100+i))
			if err != nil {
				return SimRow{}, err
			}
			ch, err := newCache()
			if err != nil {
				return SimRow{}, err
			}
			srv := &peer.Server{Cache: ch, Repo: r}
			hs := httptest.NewServer(srv)
			defer hs.Close()
			urls = append(urls, hs.URL)
			servers = append(servers, srv)
			views = append(views, &hostView{r: r, counted: c, cache: ch})
		}
		for i, v := range views {
			v.peer = &peer.Ring{Self: urls[i], Hosts: urls, Local: servers[i], Link: objstore.NewLink(objstore.Peer, uint64(200+i))}
		}
	}

	answers := make([]float64, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		v := views[i%len(views)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			t0 := time.Now()
			s, err := restore.New(ctx, v.r, e.snap, restore.Options{Strategy: restore.Thaw, Plan: p, Cache: v.cache, Peer: v.peer})
			if err != nil {
				errs <- err
				return
			}
			s.Start(ctx)
			_, err = restore.Replay(ctx, s, held)
			s.Close()
			if err != nil {
				errs <- err
				return
			}
			answers[i] = msOf(time.Since(t0))
		}()
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return SimRow{}, err
	}
	var total objstore.Counts
	for _, v := range views {
		c := v.counted.Counts()
		total.Gets += c.Gets
		total.BytesOut += c.BytesOut
	}
	sort.Float64s(answers)
	return SimRow{Sweep: "fanout", Env: e.snap.Env, Task: held.Task, Variant: setup, Profile: "s3-express", N: n,
		P50: answers[n/2], P95: answers[min(n-1, n*95/100)], Answered: answers[n/2], Baseline: recorded(held),
		Gets: total.Gets, Bytes: total.BytesOut, CostPer1000: total.RequestCost(objstore.S3StandardPrice) / float64(n) * 1000}, nil
}

func recorded(t *trace.Trace) float64 {
	d, _ := t.Mark("answered")
	return msOf(d)
}

// blocksize rewrites a base snapshot at bigger blocks and replays the same
// traces mapped onto them. Bigger blocks mean fewer requests but more bytes
// the guest never wanted. Smaller than the recorded 64 KiB can't be
// replayed: the traces don't say which part of a block was touched.
func (sm *simulator) blocksize(ctx context.Context, envs []*simEnv) error {
	r, _, _, err := sm.repoFor("none", 0)
	if err != nil {
		return err
	}
	for _, e := range envs {
		for _, bs := range []int{64 << 10, 256 << 10, 1 << 20} {
			snap := e.snap
			if bs != 64<<10 {
				snap, err = rewrite(ctx, r, e.snap, bs)
				if err != nil {
					return fmt.Errorf("rewrite %s at %d: %w", e.snap.Env, bs, err)
				}
			}
			ce := &simEnv{snap: snap, tasks: e.tasks}
			for _, t := range e.traces {
				ce.traces = append(ce.traces, remap(t, bs))
			}
			var cases []replayCase
			for _, task := range ce.tasks {
				held, learn := ce.test(task, false)
				name := fmt.Sprintf("%d KiB", bs>>10)
				cases = append(cases,
					replayCase{sweep: "blocksize", variant: "thaw, " + name, profile: "s3-express", env: ce, trace: held, opt: restore.Options{Strategy: restore.Thaw, Plan: plan.Build(learn, defaultPlan)}, seed: 17},
					replayCase{sweep: "blocksize", variant: "lazy, " + name, profile: "s3-express", env: ce, trace: held, opt: restore.Options{Strategy: restore.Lazy}, seed: 17})
			}
			err := sm.runAll(ctx, cases)
			if snap != e.snap {
				r.Delete(ctx, snap.ID)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// rewrite copies a snapshot's mem and disk at another block size.
func rewrite(ctx context.Context, r *repo.Repo, s *repo.Snapshot, block int) (*repo.Snapshot, error) {
	sess, err := restore.New(ctx, r, s, restore.Options{Strategy: restore.Lazy})
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	var specs []repo.FileSpec
	for i, m := range sess.Files {
		specs = append(specs, repo.FileSpec{Name: sess.Names[i], Block: block, Size: m.Size, Reader: &sessionReader{ctx: ctx, s: sess, file: i}})
	}
	meta := map[string]string{"kind": "rewrite", "of": s.ID}
	out, _, err := r.Write(ctx, repo.Snapshot{Env: s.Env, Meta: meta}, specs, repo.WriteOptions{Compress: true})
	return out, err
}

type sessionReader struct {
	ctx  context.Context
	s    *restore.Session
	file int
}

func (sr *sessionReader) ReadAt(p []byte, off int64) (int, error) {
	m := sr.s.Files[sr.file]
	n := 0
	for n < len(p) && off+int64(n) < m.Size {
		pos := off + int64(n)
		b := int(pos / int64(m.Block))
		in := int(pos % int64(m.Block))
		data, err := sr.s.Get(sr.ctx, sr.file, b)
		if err != nil {
			return n, err
		}
		want := min(len(p)-n, m.BlockLen(b)-in)
		if data == nil {
			clear(p[n : n+want])
		} else {
			copy(p[n:n+want], data[in:])
		}
		n += want
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// remap moves a trace onto bigger blocks, keeping each block's first touch.
func remap(t *trace.Trace, block int) *trace.Trace {
	c := *t
	c.Blocks = make([]int, len(t.Blocks))
	c.Events = nil
	seen := map[[2]uint32]bool{}
	for i := range c.Blocks {
		c.Blocks[i] = block
	}
	for _, ev := range t.Events {
		ratio := uint32(block / t.Blocks[ev.File])
		ev.Block /= ratio
		k := [2]uint32{uint32(ev.File), ev.Block}
		if seen[k] {
			continue
		}
		seen[k] = true
		// Whether a block is all zeros depends on its size; let the
		// session decide again rather than trust the 64 KiB answer.
		ev.Kind = trace.Demand
		c.Events = append(c.Events, ev)
	}
	return &c
}
