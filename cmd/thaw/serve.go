package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/restore"
	"github.com/Rakshit-gen/thaw/trace"
)

// A Run is a restore as the console draws it: the shape of each file, then
// every block event in order. thaw serve streams one; thaw export writes
// them to files so the site can play them back without a server.
type Run struct {
	Snapshot string    `json:"snapshot"`
	Env      string    `json:"env"`
	Task     string    `json:"task"`
	Strategy string    `json:"strategy"`
	Profile  string    `json:"profile"`
	Source   string    `json:"source"` // "replay" or "vm"
	Files    []RunFile `json:"files"`
	// Events are [ms since start, file, block, kind]; kinds are below.
	Events [][4]float64       `json:"events"`
	Marks  map[string]float64 `json:"marks"`
	Stats  map[string]float64 `json:"stats"`
}

// Event kinds in a Run.
const (
	evDemand  = 0 // the guest waited for it
	evStaged  = 1 // the guest touched it and it was already here
	evCached  = 2 // the guest touched it and the host cache had it
	evZero    = 3 // the guest touched an all-zero block
	evArrived = 4 // it arrived from the store, wanted or not
)

var kindCode = map[string]float64{"demand": evDemand, "staged": evStaged, "cached": evCached, "zero": evZero, "fetched": evArrived, "prefetched": evArrived}

// RunFile is one file of the snapshot. Zero lists alternating runs of
// non-zero and zero blocks, starting with non-zero.
type RunFile struct {
	Name   string `json:"name"`
	Blocks int    `json:"blocks"`
	Block  int    `json:"block"`
	Zero   []int  `json:"zero_runs"`
}

func runFiles(s *restore.Session) []RunFile {
	var out []RunFile
	for i, m := range s.Files {
		f := RunFile{Name: s.Names[i], Blocks: m.Blocks(), Block: m.Block}
		cur, n := false, 0
		for _, ref := range m.Refs {
			if ref.Zero() != cur {
				f.Zero = append(f.Zero, n)
				cur, n = !cur, 0
			}
			n++
		}
		f.Zero = append(f.Zero, n)
		out = append(out, f)
	}
	return out
}

// RunRequest names a restore to show.
type RunRequest struct {
	Snapshot string `json:"snapshot"`
	Task     string `json:"task"`
	Strategy string `json:"strategy"`
	Profile  string `json:"profile"`
	Live     bool   `json:"live"` // restore a real VM instead of replaying
}

// emitter collects a run's events and hands them out in batches.
type emitter struct {
	mu     sync.Mutex
	run    Run
	start  time.Time
	notify func(head *Run, events [][4]float64)
}

func (e *emitter) event(ev restore.Event) {
	k, ok := kindCode[ev.What]
	if !ok {
		return
	}
	e.mu.Lock()
	e.run.Events = append(e.run.Events, [4]float64{msOf(ev.At), float64(ev.File), float64(ev.Block), k})
	e.mu.Unlock()
}

// replayRun replays the newest recorded trace of a task against the store,
// calling emit every 50 ms with the new events.
func replayRun(ctx context.Context, c common, req RunRequest, emit func(*Run, [][4]float64)) (*Run, error) {
	c.profile = req.Profile
	r, counted, err := c.open()
	if err != nil {
		return nil, err
	}
	snap, err := r.Snapshot(ctx, req.Snapshot)
	if err != nil {
		return nil, err
	}
	traces, err := decodedTraces(ctx, r, snap)
	if err != nil {
		return nil, err
	}
	var held *trace.Trace
	var learn []*trace.Trace
	for i := len(traces) - 1; i >= 0; i-- {
		if traces[i].Task == req.Task && held == nil {
			held = traces[i]
			continue
		}
		learn = append([]*trace.Trace{traces[i]}, learn...)
	}
	if held == nil {
		return nil, fmt.Errorf("no recorded trace of %q on %s", req.Task, snap.ID)
	}
	e := &emitter{run: Run{Snapshot: snap.ID, Env: snap.Env, Task: req.Task, Strategy: req.Strategy, Profile: req.Profile, Source: "replay", Marks: map[string]float64{}}}
	opt := restore.Options{Strategy: restore.Strategy(req.Strategy), OnEvent: e.event}
	if opt.Strategy == restore.Thaw {
		opt.Plan = plan.Build(learn, defaultPlan)
	}
	s, err := restore.New(ctx, r, snap, opt)
	if err != nil {
		return nil, err
	}
	e.run.Files = runFiles(s)
	done := make(chan struct{})
	go e.pump(done, emit)
	t0 := time.Now()
	err = s.Start(ctx)
	var rr restore.ReplayResult
	if err == nil {
		e.run.Marks["resumed"] = msOf(time.Since(t0))
		rr, err = restore.Replay(ctx, s, held)
	}
	st := s.Stats()
	s.Close()
	close(done)
	if err != nil {
		return nil, err
	}
	e.run.Marks["answered"] = msOf(time.Since(t0))
	e.run.Marks["recorded"] = msOf(rr.Baseline)
	cnt := counted.Counts()
	e.run.Stats = map[string]float64{"touched": float64(st.Touched), "demand": float64(st.Demand), "staged": float64(st.Staged),
		"unused": float64(st.Unused), "planned": float64(st.Planned), "gets": float64(cnt.Gets), "bytes": float64(cnt.BytesOut), "stalled_ms": msOf(rr.Stalled)}
	return e.finish(emit), nil
}

// pump sends new events every 50 ms until done.
func (e *emitter) pump(done chan struct{}, emit func(*Run, [][4]float64)) {
	sent := 0
	head := false
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		e.mu.Lock()
		evs := e.run.Events[sent:]
		sent = len(e.run.Events)
		var h *Run
		if !head && len(e.run.Files) > 0 {
			hr := e.run
			hr.Events = nil
			h, head = &hr, true
		}
		e.mu.Unlock()
		if emit != nil && (h != nil || len(evs) > 0) {
			emit(h, evs)
		}
	}
}

func (e *emitter) finish(emit func(*Run, [][4]float64)) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	sort.SliceStable(e.run.Events, func(i, j int) bool { return e.run.Events[i][0] < e.run.Events[j][0] })
	r := e.run
	return &r
}

func decodedTraces(ctx context.Context, r *repo.Repo, s *repo.Snapshot) ([]*trace.Trace, error) {
	raw, err := r.Traces(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	var out []*trace.Trace
	for _, b := range raw {
		t, err := trace.Decode(b)
		if err == nil && t.Mode != "install" && t.Task != "" {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

// liveRun restores a real VM; it is set on Linux.
var liveRun func(ctx context.Context, c common, req RunRequest, emit func(*Run, [][4]float64)) (*Run, error)

func cmdServe(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	c.register(fs)
	addr := fs.String("addr", "127.0.0.1:7070", "listen address")
	results := fs.String("results", "results", "directory with vm.json and sim.json")
	web := fs.String("web", "", "directory of the built site to serve at /")
	fs.Parse(args)

	var busy sync.Mutex // one VM restore at a time; replays are cheap
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/snapshots", func(w http.ResponseWriter, r *http.Request) {
		rp, _, err := c.open()
		if err != nil {
			httpErr(w, err)
			return
		}
		snaps, err := rp.Snapshots(r.Context())
		if err != nil {
			httpErr(w, err)
			return
		}
		type row struct {
			*repo.Snapshot
			Tasks []string `json:"tasks"`
		}
		var out []row
		for _, s := range snaps {
			ts, _ := decodedTraces(r.Context(), rp, s)
			seen := map[string]bool{}
			var names []string
			for _, t := range ts {
				if !seen[t.Task] {
					seen[t.Task] = true
					names = append(names, t.Task)
				}
			}
			sort.Strings(names)
			out = append(out, row{s, names})
		}
		writeJSONTo(w, out)
	})
	mux.HandleFunc("GET /api/results/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if name != "vm" && name != "sim" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(*results, name+".json"))
	})
	mux.HandleFunc("GET /api/run", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		req := RunRequest{Snapshot: q.Get("snapshot"), Task: q.Get("task"), Strategy: q.Get("strategy"), Profile: q.Get("profile"), Live: q.Get("live") == "1"}
		if req.Strategy == "" {
			req.Strategy = "thaw"
		}
		if req.Profile == "" {
			req.Profile = "s3-express"
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			httpErr(w, errors.New("streaming unsupported"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		var mu sync.Mutex
		send := func(kind string, v any) {
			b, _ := json.Marshal(v)
			mu.Lock()
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
			fl.Flush()
			mu.Unlock()
		}
		emit := func(head *Run, evs [][4]float64) {
			if head != nil {
				send("head", head)
			}
			if len(evs) > 0 {
				send("events", evs)
			}
		}
		run := replayRun
		if req.Live {
			if liveRun == nil {
				send("error", map[string]string{"error": "live restores need Linux with KVM; this server can only replay"})
				return
			}
			if !busy.TryLock() {
				send("error", map[string]string{"error": "a VM restore is already running"})
				return
			}
			defer busy.Unlock()
			run = liveRun
		}
		res, err := run(r.Context(), c, req, emit)
		if err != nil {
			send("error", map[string]string{"error": err.Error()})
			return
		}
		res.Events = nil
		send("done", res)
	})
	if *web != "" {
		mux.Handle("/", http.FileServer(http.Dir(*web)))
	}
	srv := &http.Server{Addr: *addr, Handler: cors(mux)}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	logf("thaw serve on http://%s (store %s, live restores %v)", *addr, c.store, liveRun != nil)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// cors lets the site's dev server on another port talk to a local thaw.
func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); strings.HasPrefix(o, "http://localhost:") || strings.HasPrefix(o, "http://127.0.0.1:") {
			w.Header().Set("Access-Control-Allow-Origin", o)
		}
		h.ServeHTTP(w, r)
	})
}

func httpErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

func writeJSONTo(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// cmdExport writes replayed runs for the site to play back.
func cmdExport(ctx context.Context, args []string) error {
	var c common
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	c.register(fs)
	out := fs.String("out", "web/public/runs", "directory to write runs into")
	strategies := fs.String("strategies", "full,lazy,hydrate,thaw", "strategies")
	profile := fs.String("profile-run", "s3-express", "profile the runs replay under")
	fs.Parse(args)
	c.profile = "none"
	r, _, err := c.open()
	if err != nil {
		return err
	}
	snaps, err := r.Snapshots(ctx)
	if err != nil {
		return err
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].Created.After(snaps[j].Created) })
	if err := os.MkdirAll(*out, 0o755); err != nil {
		return err
	}
	type entry struct {
		File     string  `json:"file"`
		Env      string  `json:"env"`
		Task     string  `json:"task"`
		Strategy string  `json:"strategy"`
		Answered float64 `json:"answered_ms"`
	}
	var index []entry
	seen := map[string]bool{}
	for _, s := range snaps {
		if s.Meta["kind"] != "base" || seen[s.Env] {
			continue
		}
		ts, err := decodedTraces(ctx, r, s)
		if err != nil || len(ts) < 2 {
			continue
		}
		seen[s.Env] = true
		task := ts[len(ts)-1].Task
		for _, st := range strings.Split(*strategies, ",") {
			run, err := replayRun(ctx, c, RunRequest{Snapshot: s.ID, Task: task, Strategy: st, Profile: *profile}, nil)
			if err != nil {
				return fmt.Errorf("%s %s: %w", s.Env, st, err)
			}
			name := fmt.Sprintf("%s-%s.json", s.Env, st)
			if err := writeJSON(filepath.Join(*out, name), run); err != nil {
				return err
			}
			index = append(index, entry{name, s.Env, task, st, run.Marks["answered"]})
			logf("exported %s: %d events, answered %.0f ms", name, len(run.Events), run.Marks["answered"])
		}
	}
	return writeJSON(filepath.Join(*out, "index.json"), index)
}
