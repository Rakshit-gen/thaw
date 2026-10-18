//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Rakshit-gen/thaw/host"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/restore"
	"github.com/Rakshit-gen/thaw/tasks"
	"golang.org/x/sys/unix"
)

var vmCommands = []command{
	{"base", "boot an environment, warm it and snapshot it", cmdBase},
	{"run", "restore a snapshot, run a task, print timings", cmdRun},
	{"bench", "restore every strategy on every environment and record it", cmdBench},
}

type hostFlags struct {
	common
	fc, kernel, rootfs, agent, work string
}

func (h *hostFlags) register(fs *flag.FlagSet) {
	h.common.register(fs)
	exe, _ := os.Executable()
	fs.StringVar(&h.fc, "firecracker", envOr("THAW_FIRECRACKER", "/usr/local/bin/firecracker"), "firecracker binary")
	fs.StringVar(&h.kernel, "kernel", envOr("THAW_KERNEL", "/opt/thaw/vmlinux"), "guest kernel")
	fs.StringVar(&h.rootfs, "rootfs", envOr("THAW_ROOTFS", "/opt/thaw/rootfs.ext4"), "guest root filesystem")
	fs.StringVar(&h.agent, "agent", envOr("THAW_AGENT", filepath.Join(filepath.Dir(exe), "thaw-guest")), "thaw-guest binary")
	fs.StringVar(&h.work, "work", envOr("THAW_WORK", "/var/lib/thaw/work"), "scratch directory")
}

func (h *hostFlags) host(r *repo.Repo) (*host.Host, error) {
	return host.New(host.Config{Firecracker: h.fc, Kernel: h.kernel, Rootfs: h.rootfs, Agent: h.agent, Work: h.work, Repo: r})
}

func cmdBase(ctx context.Context, args []string) error {
	var hf hostFlags
	fs := flag.NewFlagSet("base", flag.ExitOnError)
	hf.register(fs)
	env := fs.String("env", "py-data", "environment: "+envNames())
	fs.Parse(args)
	hf.profile = "none" // uploads go at local speed; shaping is for restores
	r, _, err := hf.open()
	if err != nil {
		return err
	}
	h, err := hf.host(r)
	if err != nil {
		return err
	}
	s, st, err := h.Base(ctx, *env)
	if err != nil {
		return err
	}
	fmt.Printf("%s  %s  boot %v, setup %v, snapshot %v, upload %v\n", s.ID, s.Env,
		st.Boot.Round(time.Millisecond), st.Setup.Round(time.Millisecond), st.Snapshot.Round(time.Millisecond), st.Write.Total.Round(time.Millisecond))
	fmt.Printf("  %d blocks, %d zero, %d stored (%s raw, %s after compression)\n",
		st.Write.Blocks, st.Write.Zero, st.Write.New, mib(st.Write.NewBytes), mib(st.Write.Stored))
	fmt.Printf("  setup said: %s\n", strings.TrimSpace(st.SetupOut))
	return nil
}

func envNames() string {
	var n []string
	for _, e := range tasks.Envs {
		n = append(n, e.Name)
	}
	return strings.Join(n, ", ")
}

func cmdRun(ctx context.Context, args []string) error {
	var hf hostFlags
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	hf.register(fs)
	id := fs.String("snap", "", "snapshot ID")
	taskName := fs.String("task", "", "task to run; the environment's first if empty")
	strategy := fs.String("strategy", "thaw", "full, lazy, hydrate or thaw")
	mode := fs.String("mode", "stage", "stage or install")
	save := fs.Bool("save", true, "store the trace so later plans learn from it")
	fork := fs.Bool("fork", false, "fork the VM after the task and restore the fork")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Parse(args)

	r, counted, err := hf.open()
	if err != nil {
		return err
	}
	h, err := hf.host(r)
	if err != nil {
		return err
	}
	snap, err := r.Snapshot(ctx, *id)
	if err != nil {
		return err
	}
	t, err := pickTask(snap.Env, *taskName)
	if err != nil {
		return err
	}
	opt := host.RestoreOptions{Strategy: restore.Strategy(*strategy)}
	if *mode == "install" {
		opt.Mode = restore.Install
	}
	res, err := runOnce(ctx, h, counted, snap.ID, t, opt, *save, *fork)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(res)
	}
	printRun(res)
	if res.Fork != nil {
		fmt.Printf("fork %s: paused %v, %d dirty mem blocks, %d dirty disk blocks, uploaded %s in %v\n",
			res.Fork.Snapshot, ms(res.Fork.PauseMs), res.Fork.DirtyMem, res.Fork.DirtyDisk, mib(res.Fork.Uploaded), ms(res.Fork.TotalMs))
		printRun(*res.Fork.Restore)
	}
	return nil
}

func ms(v float64) time.Duration {
	return time.Duration(v * float64(time.Millisecond)).Round(time.Millisecond)
}

func printRun(r RunResult) {
	status := "ok"
	if !r.OK {
		status = "WRONG: " + r.Err
	}
	fmt.Printf("%s %s %s/%s: answered in %v (resumed %v, connected %v), %s\n", r.Snapshot, r.Env, r.Strategy, r.Task,
		ms(r.AnsweredMs), ms(r.ResumedMs), ms(r.ConnectedMs), status)
	fmt.Printf("  touched %d blocks: %d staged, %d waited on, %d zero; staged %d unused; %s fetched in %d requests\n",
		r.Touched, r.FromStage, r.Demand, r.Zero, r.Unused, mib(r.Bytes), r.Requests)
}

func pickTask(env, name string) (tasks.Task, error) {
	if name == "" {
		ts := tasks.ForEnv(env)
		if len(ts) == 0 {
			return tasks.Task{}, fmt.Errorf("no tasks for environment %q", env)
		}
		return ts[0], nil
	}
	t, ok := tasks.ByName(name)
	if !ok {
		return t, fmt.Errorf("unknown task %q", name)
	}
	if t.Env != env {
		return t, fmt.Errorf("task %s is for %s, the snapshot is %s", t.Name, t.Env, env)
	}
	return t, nil
}

func runOnce(ctx context.Context, h *host.Host, counted *objstore.Counting, id string, t tasks.Task, opt host.RestoreOptions, save, fork bool) (RunResult, error) {
	before := counted.Counts()
	v, err := h.Restore(ctx, id, opt)
	if err != nil {
		return RunResult{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	res := v.Run(tctx, t)
	cancel()
	marks := v.Marks()
	st := v.Session.Stats()
	out := RunResult{
		Snapshot: id, Env: v.Snap.Env, Task: t.Name, Strategy: string(st.Strategy), Mode: "stage",
		OK: res.OK, Err: res.Err, GuestMs: res.GuestMs,
		StateMs: msOf(marks["state"]), DownloadMs: msOf(marks["downloaded"]),
		ResumedMs: msOf(marks["resumed"]), ConnectedMs: msOf(marks["connected"]), AnsweredMs: msOf(marks["answered"]),
		Touched: st.Touched, FromStage: st.FromStage, Demand: st.Demand, Zero: st.Zero,
		Staged: st.Staged, Unused: st.Unused, Planned: st.Planned, DemandWait: msOf(st.Fetch.DemandWait),
	}
	if opt.Mode == restore.Install {
		out.Mode = "install"
	}
	if v.Mem != nil {
		out.MemFaults = v.Mem.Stats().Faults
	}
	if v.Disk != nil {
		out.DiskReads = v.Disk.Stats().Reads
	}
	var forkRes *ForkResult
	if fork && res.OK {
		child, fst, err := v.Fork(ctx)
		if err != nil {
			v.Close(ctx, false)
			return out, fmt.Errorf("fork: %w", err)
		}
		forkRes = &ForkResult{Snapshot: child.ID, PauseMs: msOf(fst.Pause), TotalMs: msOf(fst.Total),
			DirtyMem: fst.DirtyMem, DirtyDisk: fst.DirtyDisk, Uploaded: fst.Write.Stored, Reused: fst.Write.Reused, New: fst.Write.New}
	}
	if _, err := v.Close(ctx, save && res.OK); err != nil {
		return out, err
	}
	d := counted.Counts().Sub(before)
	out.Bytes, out.Requests = d.BytesOut, d.Gets
	if forkRes != nil {
		cr, err := runOnce(ctx, h, counted, forkRes.Snapshot, t, opt, false, false)
		if err != nil {
			return out, fmt.Errorf("restore fork: %w", err)
		}
		forkRes.Restore = &cr
		out.Fork = forkRes
	}
	return out, nil
}

// BenchResult is everything thaw bench measured.
type BenchResult struct {
	When     time.Time      `json:"when"`
	Machine  string         `json:"machine"`
	Note     string         `json:"note"`
	Profiles []string       `json:"profiles"`
	Bases    []BaseResult   `json:"bases"`
	Runs     []RunResult    `json:"runs"`
	Forks    []RunResult    `json:"forks"`
	Repacks  []RepackResult `json:"repacks,omitempty"`
	Training int            `json:"training_traces"`
	Took     time.Duration  `json:"took_ns"`
}

// BaseResult is building one environment's base snapshot, which is also the
// cold start an agent pays without snapshots.
type BaseResult struct {
	Env        string  `json:"env"`
	Snapshot   string  `json:"snapshot"`
	BootMs     float64 `json:"boot_ms"`
	SetupMs    float64 `json:"setup_ms"`
	SnapshotMs float64 `json:"snapshot_ms"`
	UploadMs   float64 `json:"upload_ms"`
	Blocks     int     `json:"blocks"`
	ZeroBlocks int     `json:"zero_blocks"`
	Stored     int64   `json:"stored"`
	MemMiB     int     `json:"mem_mib"`
}

func cmdBench(ctx context.Context, args []string) error {
	var hf hostFlags
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	hf.register(fs)
	out := fs.String("out", "results/vm.json", "where to write results")
	envs := fs.String("envs", "py-data,repo,node-ts", "environments")
	profiles := fs.String("profiles", "s3-express,s3-standard", "store profiles to restore under")
	strategies := fs.String("strategies", "full,lazy,hydrate,thaw", "strategies")
	reps := fs.Int("reps", 2, "measured restores per strategy, task and profile")
	train := fs.Int("train", 2, "training restores per task before measuring")
	forks := fs.Bool("forks", true, "also fork after each thaw run and restore the fork")
	repack := fs.Bool("repack", true, "afterwards, repack each base and run thaw again")
	reuse := fs.String("bases", "", "comma-separated env=snapshot pairs to reuse instead of building")
	resume := fs.Bool("resume", false, "keep the runs already in -out and do only the missing ones")
	fs.Parse(args)
	start := time.Now()

	result := BenchResult{When: start.UTC(), Machine: machine(), Profiles: strings.Split(*profiles, ","),
		Note: "Firecracker inside a Lima VM (nested virtualization on Apple silicon). Store is a local directory shaped to each profile's latency and bandwidth."}
	var took time.Duration // time spent by the runs being resumed
	have := map[string]int{}
	if *resume {
		if err := readJSON(*out, &result); err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		took = result.Took
		for _, r := range result.Runs {
			have[runKey(r.Profile, r.Env, r.Task, r.Strategy)]++
		}
		logf("resuming with %d runs already recorded", len(result.Runs))
	}
	save := func() error { result.Took = took + time.Since(start); return writeJSON(*out, result) }

	plain := hf
	plain.profile = "none"
	baseRepo, _, err := plain.open()
	if err != nil {
		return err
	}
	baseHost, err := plain.host(baseRepo)
	if err != nil {
		return err
	}
	bases := map[string]string{}
	for _, kv := range strings.Split(*reuse, ",") {
		if k, v, ok := strings.Cut(kv, "="); ok {
			bases[k] = v
		}
	}
	for _, env := range strings.Split(*envs, ",") {
		if i := slices.IndexFunc(result.Bases, func(b BaseResult) bool { return b.Env == env }); *resume && i >= 0 {
			bases[env] = result.Bases[i].Snapshot
			continue
		}
		if id := bases[env]; id != "" {
			s, err := baseRepo.Snapshot(ctx, id)
			if err != nil {
				return err
			}
			e, _ := tasks.EnvByName(env)
			num := func(k string) float64 { var v float64; fmt.Sscan(s.Meta[k], &v); return v }
			result.Bases = append(result.Bases, BaseResult{Env: env, Snapshot: id, BootMs: num("boot_ms"), SetupMs: num("setup_ms"),
				SnapshotMs: num("snapshot_ms"), Stored: s.Files["mem"].Stored + s.Files["disk"].Stored, MemMiB: e.MemMiB})
			continue
		}
		logf("building base snapshot for %s", env)
		s, st, err := baseHost.Base(ctx, env)
		if err != nil {
			return fmt.Errorf("base %s: %w", env, err)
		}
		e, _ := tasks.EnvByName(env)
		bases[env] = s.ID
		result.Bases = append(result.Bases, BaseResult{Env: env, Snapshot: s.ID, BootMs: msOf(st.Boot), SetupMs: msOf(st.Setup),
			SnapshotMs: msOf(st.Snapshot), UploadMs: msOf(st.Write.Total), Blocks: st.Write.Blocks, ZeroBlocks: st.Write.Zero,
			Stored: st.Write.Stored, MemMiB: e.MemMiB})
		logf("  %s: boot %v, setup %v", s.ID, st.Boot.Round(time.Millisecond), st.Setup.Round(time.Millisecond))
		if err := save(); err != nil {
			return err
		}
	}

	for _, prof := range result.Profiles {
		pf := hf
		pf.profile = prof
		r, counted, err := pf.open()
		if err != nil {
			return err
		}
		h, err := pf.host(r)
		if err != nil {
			return err
		}
		for _, env := range strings.Split(*envs, ",") {
			id := bases[env]
			ts := tasks.ForEnv(env)
			// Training: lazy restores whose traces the plan learns from.
			// Only under the first profile; the plan is about what the
			// guest touches, which doesn't depend on the store.
			trained := 0
			if *resume {
				tr, err := baseRepo.Traces(ctx, id)
				if err != nil {
					return err
				}
				trained = len(tr) / len(ts)
			}
			if prof == result.Profiles[0] {
				for i := trained; i < *train; i++ {
					for _, t := range ts {
						res, err := runOnce(ctx, h, counted, id, t, host.RestoreOptions{Strategy: restore.Lazy}, true, false)
						if err != nil {
							return fmt.Errorf("training %s: %w", t.Name, err)
						}
						if !res.OK {
							return fmt.Errorf("training %s: wrong answer: %s", t.Name, res.Err)
						}
						result.Training++
						logf("train %s %s: %v", env, t.Name, ms(res.AnsweredMs))
					}
				}
			}
			for rep := 0; rep < *reps; rep++ {
				for _, t := range ts {
					for _, s := range strings.Split(*strategies, ",") {
						if k := runKey(prof, env, t.Name, s); have[k] > 0 {
							have[k]--
							continue
						}
						opt := host.RestoreOptions{Strategy: restore.Strategy(s)}
						fork := *forks && s == "thaw" && rep == 0 && prof == result.Profiles[0]
						res, err := runOnce(ctx, h, counted, id, t, opt, false, fork)
						if err != nil {
							return errors.Join(fmt.Errorf("%s %s %s: %w", env, t.Name, s, err), save())
						}
						res.Profile = prof
						if res.Fork != nil {
							res.Fork.Restore.Profile = prof
							result.Forks = append(result.Forks, res)
						}
						result.Runs = append(result.Runs, res)
						logf("%s %s %-7s %-18s answered %8v  waited on %4d blocks  %s", prof, env, s, t.Name, ms(res.AnsweredMs), res.Demand, okStr(res))
						if ctx.Err() != nil {
							return errors.Join(save(), ctx.Err())
						}
					}
				}
				if err := save(); err != nil {
					return err
				}
			}
		}
	}

	if *resume {
		// Training traces from the interrupted run were stored but not
		// counted; every trace in the store is a training trace, since
		// measured runs don't save theirs.
		result.Training = 0
		for _, env := range strings.Split(*envs, ",") {
			tr, err := baseRepo.Traces(ctx, bases[env])
			if err != nil {
				return err
			}
			result.Training += len(tr)
		}
	}
	if !*repack {
		return save()
	}
	// Repack rewrites each base's hot blocks in plan order, then thaw runs
	// again on the new layout. It goes last because it changes the snapshot
	// every strategy reads.
	for _, env := range strings.Split(*envs, ",") {
		s, err := baseRepo.Snapshot(ctx, bases[env])
		if err != nil {
			return err
		}
		p, _, err := restore.PlanFor(ctx, baseRepo, s, 3, defaultPlan)
		if err != nil {
			return err
		}
		order := map[string][]int{}
		for _, f := range p.Files {
			order[f] = p.Order(f)
		}
		_, st, err := baseRepo.Repack(ctx, s, order, 0)
		if err != nil {
			return fmt.Errorf("repack %s: %w", env, err)
		}
		result.Repacks = append(result.Repacks, RepackResult{Env: env, Blocks: st.Blocks, Bytes: st.Bytes, Packs: st.Packs, TookMs: msOf(st.Took)})
		logf("repacked %s: %d blocks into %d packs", env, st.Blocks, st.Packs)
	}
	for _, prof := range result.Profiles {
		pf := hf
		pf.profile = prof
		r, counted, err := pf.open()
		if err != nil {
			return err
		}
		h, err := pf.host(r)
		if err != nil {
			return err
		}
		for _, env := range strings.Split(*envs, ",") {
			for rep := 0; rep < *reps; rep++ {
				for _, t := range tasks.ForEnv(env) {
					res, err := runOnce(ctx, h, counted, bases[env], t, host.RestoreOptions{Strategy: restore.Thaw}, false, false)
					if err != nil {
						return fmt.Errorf("%s %s repacked: %w", env, t.Name, err)
					}
					res.Profile, res.Strategy = prof, "thaw+repack"
					result.Runs = append(result.Runs, res)
					logf("%s %s %-11s %-18s answered %8v  waited on %4d blocks  %s", prof, env, res.Strategy, t.Name, ms(res.AnsweredMs), res.Demand, okStr(res))
				}
			}
			if err := save(); err != nil {
				return err
			}
		}
	}
	return save()
}

func runKey(profile, env, task, strategy string) string {
	return profile + "/" + env + "/" + task + "/" + strategy
}

// RepackResult is one base snapshot's repack.
type RepackResult struct {
	Env    string  `json:"env"`
	Blocks int     `json:"blocks"`
	Bytes  int64   `json:"bytes"`
	Packs  int     `json:"packs"`
	TookMs float64 `json:"took_ms"`
}

func okStr(r RunResult) string {
	if r.OK {
		return "ok"
	}
	return "WRONG " + r.Err
}

func machine() string {
	b, _ := os.ReadFile("/proc/cpuinfo")
	cpus := strings.Count(string(b), "processor\t:")
	var mem string
	if m, err := os.ReadFile("/proc/meminfo"); err == nil {
		mem = strings.Fields(strings.SplitN(string(m), "\n", 2)[0])[1] + " kB"
	}
	var uts string
	if v, err := os.ReadFile("/proc/version"); err == nil {
		f := strings.Fields(string(v))
		if len(f) > 2 {
			uts = f[2]
		}
	}
	return fmt.Sprintf("linux %s, %d CPUs, %s RAM", uts, cpus, mem)
}

func init() { liveRun = liveRestore }

// liveRestore restores a real VM for the console and streams its block
// events the same way a replay does.
func liveRestore(ctx context.Context, c common, req RunRequest, emit func(*Run, [][4]float64)) (*Run, error) {
	hf := hostFlags{common: c}
	fs := flag.NewFlagSet("live", flag.ContinueOnError)
	hf.register(fs) // defaults for the host paths
	hf.common = c
	hf.profile = req.Profile
	r, counted, err := hf.open()
	if err != nil {
		return nil, err
	}
	h, err := hf.host(r)
	if err != nil {
		return nil, err
	}
	snap, err := r.Snapshot(ctx, req.Snapshot)
	if err != nil {
		return nil, err
	}
	t, err := pickTask(snap.Env, req.Task)
	if err != nil {
		return nil, err
	}
	e := &emitter{run: Run{Snapshot: snap.ID, Env: snap.Env, Task: t.Name, Strategy: req.Strategy, Profile: req.Profile, Source: "vm", Marks: map[string]float64{}}}
	done := make(chan struct{})
	defer func() {
		select {
		case <-done:
		default:
			close(done)
		}
	}()
	before := counted.Counts()
	v, err := h.Restore(ctx, snap.ID, host.RestoreOptions{Strategy: restore.Strategy(req.Strategy), OnEvent: e.event})
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	e.run.Files = runFiles(v.Session)
	e.mu.Unlock()
	go e.pump(done, emit)
	tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	res := v.Run(tctx, t)
	cancel()
	st := v.Session.Stats()
	for k, d := range v.Marks() {
		e.run.Marks[k] = msOf(d)
	}
	v.Close(ctx, false)
	close(done)
	if !res.OK {
		return nil, fmt.Errorf("wrong answer: %s", res.Err)
	}
	cnt := counted.Counts().Sub(before)
	e.run.Stats = map[string]float64{"touched": float64(st.Touched), "demand": float64(st.Demand), "staged": float64(st.Staged),
		"unused": float64(st.Unused), "planned": float64(st.Planned), "gets": float64(cnt.Gets), "bytes": float64(cnt.BytesOut), "guest_ms": res.GuestMs}
	return e.finish(emit), nil
}

func init() {
	vmCommands = append(vmCommands, command{"doctor", "check that this host can run VMs", cmdDoctor})
}

// cmdDoctor checks everything a restore needs and says how to fix what is
// missing.
func cmdDoctor(ctx context.Context, args []string) error {
	var hf hostFlags
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	hf.register(fs)
	fs.Parse(args)
	bad := 0
	check := func(name string, err error, fix string) {
		if err == nil {
			fmt.Printf("  ok    %s\n", name)
			return
		}
		bad++
		fmt.Printf("  FAIL  %s: %v\n        %s\n", name, err, fix)
	}
	openRW := func(p string) error {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err == nil {
			f.Close()
		}
		return err
	}
	exists := func(p string) error { _, err := os.Stat(p); return err }

	fmt.Println("host")
	check("running as root", func() error {
		if os.Geteuid() != 0 {
			return errors.New("not root")
		}
		return nil
	}(), "userfaultfd and NBD need root; run with sudo -E")
	check("/dev/kvm", openRW("/dev/kvm"), "enable virtualization, or nested virtualization in the VM you are in")
	check("userfaultfd", uffdAvailable(), "needs a kernel with CONFIG_USERFAULTFD")
	check("nbd module", exists("/sys/block/nbd0"), "sudo modprobe nbd")
	fmt.Println("files")
	check("firecracker "+hf.fc, exists(hf.fc), "install Firecracker or pass -firecracker")
	check("kernel "+hf.kernel, exists(hf.kernel), "copy an uncompressed guest kernel there or pass -kernel")
	check("rootfs "+hf.rootfs, exists(hf.rootfs), "sudo scripts/build-rootfs.sh, or pass -rootfs")
	check("agent "+hf.agent, exists(hf.agent), "thaw-guest ships next to thaw; or pass -agent")
	fmt.Println("store")
	r, _, err := hf.open()
	if err == nil {
		_, err = r.Snapshots(ctx)
	}
	check("store "+hf.store, err, "pass -store a writable directory or s3:// URL")
	if bad > 0 {
		return fmt.Errorf("%d checks failed", bad)
	}
	fmt.Println("this host can run thaw VMs")
	return nil
}

// uffdAvailable checks that this kernel can create a userfaultfd, which
// Firecracker does for every lazy restore.
func uffdAvailable() error {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		return errno
	}
	unix.Close(int(fd))
	return nil
}
