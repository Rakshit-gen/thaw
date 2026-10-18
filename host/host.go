//go:build linux

// Package host runs agent VMs on one Linux machine: it boots environments
// and snapshots them, restores snapshots with any strategy, runs tasks, and
// forks running VMs into new snapshots.
//
// A lazy restore wires three things together. Firecracker loads the VM
// state with its memory backed by a userfaultfd that thaw serves; the root
// disk is an NBD device that thaw serves; and both pull blocks through one
// restore session, which knows what to prefetch and records what the guest
// touched.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/fetch"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/nbd"
	"github.com/Rakshit-gen/thaw/plan"
	"github.com/Rakshit-gen/thaw/repo"
	"github.com/Rakshit-gen/thaw/restore"
	"github.com/Rakshit-gen/thaw/tasks"
	"github.com/Rakshit-gen/thaw/trace"
	"github.com/Rakshit-gen/thaw/uffd"
	"github.com/Rakshit-gen/thaw/vm"
)

// Block is the block size for memory and disk.
const Block = 64 << 10

// Config is where things are on this host.
type Config struct {
	Firecracker string // firecracker binary
	Kernel      string // guest kernel (vmlinux)
	Rootfs      string // guest root filesystem with the environments' packages
	Agent       string // thaw-guest binary, copied into the rootfs as /sbin/thaw-guest
	Work        string // scratch space for VM directories and images

	Repo  *repo.Repo
	Cache *cache.Cache // may be nil
	Peer  fetch.Peer   // may be nil
}

// Host runs VMs.
type Host struct {
	cfg Config
	cid atomic.Uint32

	imgMu sync.Mutex
	image string
}

// New returns a host.
func New(cfg Config) (*Host, error) {
	for _, p := range []string{cfg.Firecracker, cfg.Kernel, cfg.Rootfs, cfg.Agent} {
		if _, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("host: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(cfg.Work, "vms"), 0o755); err != nil {
		return nil, err
	}
	h := &Host{cfg: cfg}
	h.cid.Store(2)
	return h, nil
}

// Repo is the host's snapshot repository.
func (h *Host) Repo() *repo.Repo { return h.cfg.Repo }

// guestImage is the rootfs with the current agent installed, built once.
func (h *Host) guestImage() (string, error) {
	h.imgMu.Lock()
	defer h.imgMu.Unlock()
	if h.image != "" {
		return h.image, nil
	}
	out := filepath.Join(h.cfg.Work, "guest.ext4")
	if err := copySparse(h.cfg.Rootfs, out); err != nil {
		return "", err
	}
	mnt, err := os.MkdirTemp("", "thaw-mnt")
	if err != nil {
		return "", err
	}
	defer os.Remove(mnt)
	if out, err := exec.Command("mount", "-o", "loop", out, mnt).CombinedOutput(); err != nil {
		return "", fmt.Errorf("host: mount guest image: %v: %s", err, out)
	}
	err = copyFile(h.cfg.Agent, filepath.Join(mnt, "sbin", "thaw-guest"), 0o755)
	if uerr := exec.Command("umount", mnt).Run(); err == nil {
		err = uerr
	}
	if err != nil {
		return "", err
	}
	h.image = out
	return out, nil
}

func copySparse(src, dst string) error {
	if out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("host: copy %s: %v: %s", src, err, out)
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, mode)
}

func (h *Host) vmDir(kind string) (string, error) {
	return os.MkdirTemp(filepath.Join(h.cfg.Work, "vms"), kind+"-")
}

// BaseStats describe building a base snapshot.
type BaseStats struct {
	Boot, Setup, Snapshot time.Duration
	Write                 repo.WriteStats
	SetupOut              string
}

// Base boots an environment from scratch, warms it, snapshots it and
// uploads the snapshot.
func (h *Host) Base(ctx context.Context, envName string) (*repo.Snapshot, BaseStats, error) {
	var st BaseStats
	env, ok := tasks.EnvByName(envName)
	if !ok {
		return nil, st, fmt.Errorf("host: unknown environment %q", envName)
	}
	img, err := h.guestImage()
	if err != nil {
		return nil, st, err
	}
	dir, err := h.vmDir("base-" + envName)
	if err != nil {
		return nil, st, err
	}
	defer os.RemoveAll(dir)
	if err := copySparse(img, filepath.Join(dir, "rootfs")); err != nil {
		return nil, st, err
	}

	t0 := time.Now()
	m, err := vm.Start(ctx, h.cfg.Firecracker, dir)
	if err != nil {
		return nil, st, err
	}
	defer m.Kill()
	if err := m.Boot(ctx, vm.BootConfig{Kernel: h.cfg.Kernel, Rootfs: "rootfs", VCPUs: env.VCPUs, MemMiB: env.MemMiB, GuestCID: 3}); err != nil {
		return nil, st, err
	}
	dctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	a, err := m.Dial(dctx)
	cancel()
	if err != nil {
		return nil, st, fmt.Errorf("%w\nconsole:\n%s", err, m.Console())
	}
	defer a.Close()
	if _, err := a.Do(ctx, vm.Request{Op: "ping"}); err != nil {
		return nil, st, err
	}
	st.Boot = time.Since(t0)

	t1 := time.Now()
	sctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	resp, err := a.Do(sctx, vm.Request{Op: "setup", Env: envName})
	cancel()
	if err != nil {
		return nil, st, fmt.Errorf("host: setup %s: %w: %s", envName, err, resp.Out)
	}
	st.SetupOut = resp.Out
	// One run of each task, so the snapshot holds a VM that has done the
	// kind of work it will be asked to do, the way an agent's VM would.
	for _, t := range tasks.ForEnv(envName) {
		if r, err := a.Do(ctx, t.Req); err != nil {
			return nil, st, fmt.Errorf("host: warm-up %s: %w", t.Name, err)
		} else if err := t.Check(r.Out); err != nil {
			return nil, st, err
		}
	}
	if _, err := a.Do(ctx, vm.Request{Op: "sync"}); err != nil {
		return nil, st, err
	}
	st.Setup = time.Since(t1)

	t2 := time.Now()
	if err := m.Pause(ctx); err != nil {
		return nil, st, err
	}
	if err := m.Snapshot(ctx, "Full", "state", "mem"); err != nil {
		return nil, st, err
	}
	st.Snapshot = time.Since(t2)
	m.Kill()

	snap, ws, err := h.cfg.Repo.Write(ctx, repo.Snapshot{Env: envName, Meta: map[string]string{
		"vcpus": fmt.Sprint(env.VCPUs), "mem_mib": fmt.Sprint(env.MemMiB), "kind": "base",
		"boot_ms": fmt.Sprint(st.Boot.Milliseconds()), "setup_ms": fmt.Sprint(st.Setup.Milliseconds()),
		"snapshot_ms": fmt.Sprint(st.Snapshot.Milliseconds()),
	}}, []repo.FileSpec{
		{Name: "mem", Path: filepath.Join(dir, "mem"), Block: Block},
		{Name: "disk", Path: filepath.Join(dir, "rootfs"), Block: Block},
		{Name: "state", Path: filepath.Join(dir, "state"), Block: Block},
	}, repo.WriteOptions{Compress: true})
	st.Write = ws
	return snap, st, err
}

// RestoreOptions pick how to restore.
type RestoreOptions struct {
	Strategy restore.Strategy
	Mode     restore.Mode
	Plan     *plan.Plan // for thaw; built from stored traces if nil
	Fetch    fetch.Options
	OnEvent  func(restore.Event)
}

// VM is a restored, running VM.
type VM struct {
	Snap    *repo.Snapshot
	Session *restore.Session
	Machine *vm.Machine
	Agent   *vm.Agent
	Mem     *uffd.Handler
	Disk    *nbd.Device

	host     *Host
	dir      string
	strategy restore.Strategy
	start    time.Time
	marks    map[string]time.Duration
	serveErr chan error
	cancel   context.CancelFunc

	memOverride map[int][]byte // blocks as of the last fork, for forks of forks
	closed      bool
}

// Restore starts a VM from a snapshot.
func (h *Host) Restore(ctx context.Context, snapID string, opt RestoreOptions) (*VM, error) {
	start := time.Now()
	r := h.cfg.Repo
	snap, err := r.Snapshot(ctx, snapID)
	if err != nil {
		return nil, err
	}
	if opt.Strategy == "" {
		opt.Strategy = restore.Thaw
	}
	if opt.Strategy == restore.Thaw && opt.Plan == nil {
		if opt.Plan, _, err = restore.PlanFor(ctx, r, snap, 3, plan.Options{}); err != nil {
			return nil, err
		}
	}
	dir, err := h.vmDir("vm")
	if err != nil {
		return nil, err
	}
	v := &VM{Snap: snap, host: h, dir: dir, strategy: opt.Strategy, start: start, marks: map[string]time.Duration{}, memOverride: map[int][]byte{}}
	ok := false
	defer func() {
		if !ok {
			v.teardown()
		}
	}()
	vctx, cancel := context.WithCancel(context.Background())
	v.cancel = cancel

	var mem atomic.Pointer[uffd.Handler] // set once Firecracker connects
	sopt := restore.Options{
		Strategy: opt.Strategy, Mode: opt.Mode, Plan: opt.Plan, Fetch: opt.Fetch,
		Cache: h.cfg.Cache, Peer: h.cfg.Peer, OnEvent: opt.OnEvent,
		Install: func(file, block int, data []byte) {
			if hd := mem.Load(); hd != nil && file == 0 {
				hd.Install(block, data)
			}
		},
	}
	sess, err := restore.New(ctx, r, snap, sopt)
	if err != nil {
		return nil, err
	}
	v.Session = sess
	sess.Recorder().Set("restore_strategy", string(opt.Strategy))

	// Device state is small; fetch it whole.
	if err := h.fetchFile(ctx, snap, "state", filepath.Join(dir, "state")); err != nil {
		return nil, err
	}
	v.mark("state")
	if err := sess.Start(vctx); err != nil {
		return nil, err
	}

	if opt.Strategy == restore.Full {
		// The baseline: everything local first, then Firecracker's own file
		// backend and a plain disk image.
		if err := sess.WriteFile(0, filepath.Join(dir, "mem")); err != nil {
			return nil, err
		}
		if err := sess.WriteFile(1, filepath.Join(dir, "rootfs")); err != nil {
			return nil, err
		}
		v.mark("downloaded")
		m, err := vm.Start(ctx, h.cfg.Firecracker, dir)
		if err != nil {
			return nil, err
		}
		v.Machine = m
		if err := m.Load(ctx, vm.LoadConfig{State: "state", MemFile: "mem", Resume: true}); err != nil {
			return nil, fmt.Errorf("%w: %s", err, m.Console())
		}
	} else {
		dev, err := nbd.Free()
		if err != nil {
			return nil, err
		}
		disk := sess.Files[1]
		d, err := nbd.Attach(dev, disk.Size, disk.Block, func(ctx context.Context, b int) ([]byte, error) {
			return sess.Get(ctx, 1, b)
		}, filepath.Join(dir, "disk.overlay"))
		if err != nil {
			return nil, err
		}
		v.Disk = d
		if err := os.Symlink(dev, filepath.Join(dir, "rootfs")); err != nil {
			return nil, err
		}
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "mem.sock"), Net: "unix"})
		if err != nil {
			return nil, err
		}
		defer ln.Close()
		m, err := vm.Start(ctx, h.cfg.Firecracker, dir)
		if err != nil {
			return nil, err
		}
		v.Machine = m
		accepted := make(chan error, 1)
		go func() {
			hd, err := uffd.Accept(ln, Block, func(ctx context.Context, b int) ([]byte, error) {
				return sess.Get(ctx, 0, b)
			})
			if err == nil {
				v.Mem = hd
				mem.Store(hd)
				v.serveErr = make(chan error, 1)
				go func() { v.serveErr <- hd.Serve(vctx) }()
			}
			accepted <- err
		}()
		if err := m.Load(ctx, vm.LoadConfig{State: "state", UffdSocket: "mem.sock", Resume: true}); err != nil {
			return nil, fmt.Errorf("%w: %s", err, m.Console())
		}
		if err := <-accepted; err != nil {
			return nil, err
		}
	}
	v.mark("resumed")
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	a, err := v.Machine.Dial(dctx)
	dcancel()
	if err != nil {
		return nil, err
	}
	v.Agent = a
	v.mark("connected")
	ok = true
	return v, nil
}

func (h *Host) fetchFile(ctx context.Context, snap *repo.Snapshot, name, path string) error {
	m, err := h.cfg.Repo.Manifest(ctx, snap, name)
	if err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(m.Size); err != nil {
		return err
	}
	for i, ref := range m.Refs {
		if ref.Zero() {
			continue
		}
		b, err := h.cfg.Repo.ReadLoc(ctx, repo.LocOf(m, i), ref.ID, m.BlockLen(i))
		if err != nil {
			return err
		}
		if _, err := f.WriteAt(b, int64(i)*int64(m.Block)); err != nil {
			return err
		}
	}
	return nil
}

func (v *VM) mark(name string) {
	v.marks[name] = time.Since(v.start)
	v.Session.Recorder().Mark(name)
}

// Marks are named moments since the restore began.
func (v *VM) Marks() map[string]time.Duration { return v.marks }

// Dir is the VM's directory.
func (v *VM) Dir() string { return v.dir }

// Result is one task run.
type Result struct {
	Task     string
	OK       bool
	Out      string
	Err      string
	GuestMs  float64       // time the guest spent on it
	Took     time.Duration // as the host saw it
	SinceRun time.Duration // from the start of the restore to the answer
}

// Run runs a task and checks the answer. The first answer is marked
// "answered" in the trace: that moment is what thaw optimises.
func (v *VM) Run(ctx context.Context, t tasks.Task) Result {
	t0 := time.Now()
	resp, err := v.Agent.Do(ctx, t.Req)
	res := Result{Task: t.Name, Out: resp.Out, GuestMs: resp.Ms, Took: time.Since(t0)}
	if err == nil {
		err = t.Check(resp.Out)
	}
	if err != nil {
		res.Err = err.Error()
	} else {
		res.OK = true
	}
	if _, done := v.marks["answered"]; !done {
		v.Session.Recorder().Set("task", t.Name)
		v.mark("answered")
	}
	res.SinceRun = time.Since(v.start)
	return res
}

// Close stops the VM and returns its trace. If save is set and the trace
// is fit to learn from, it is stored with the snapshot.
func (v *VM) Close(ctx context.Context, save bool) (*trace.Trace, error) {
	if v.closed {
		return nil, errors.New("host: vm closed twice")
	}
	tr := v.Session.Close()
	var err error
	if save && v.strategy != restore.Full {
		err = restore.SaveTrace(ctx, v.host.cfg.Repo, tr)
	}
	v.teardown()
	return tr, err
}

func (v *VM) teardown() {
	v.closed = true
	if v.Agent != nil {
		v.Agent.Close()
	}
	if v.Machine != nil {
		v.Machine.Kill()
	}
	if v.cancel != nil {
		v.cancel()
	}
	if v.serveErr != nil {
		<-v.serveErr
	}
	if v.Mem != nil {
		v.Mem.Close()
	}
	if v.Disk != nil {
		v.Disk.Detach()
	}
	if v.Session != nil {
		v.Session.Close()
	}
	os.RemoveAll(v.dir)
}

// ForkStats describe a fork.
type ForkStats struct {
	Pause     time.Duration // VM paused and diff snapshot written
	DirtyMem  int           // memory blocks the VM wrote since its restore
	DirtyDisk int
	Write     repo.WriteStats
	Total     time.Duration
}

// Fork snapshots a running VM as a child of the snapshot it came from and
// lets it run on. Only what the VM changed is read and uploaded.
func (v *VM) Fork(ctx context.Context) (*repo.Snapshot, ForkStats, error) {
	var st ForkStats
	t0 := time.Now()
	if v.strategy == restore.Full {
		return nil, st, errors.New("host: forking needs a lazy restore")
	}
	if _, err := v.Agent.Do(ctx, vm.Request{Op: "sync"}); err != nil {
		return nil, st, err
	}
	if err := v.Machine.Pause(ctx); err != nil {
		return nil, st, err
	}
	statePath, memPath := "fork.state", "fork.mem"
	err := v.Machine.Snapshot(ctx, "Diff", statePath, memPath)
	st.Pause = time.Since(t0)
	if err != nil {
		v.Machine.Resume(ctx)
		return nil, st, err
	}
	defer os.Remove(filepath.Join(v.dir, statePath))
	defer os.Remove(filepath.Join(v.dir, memPath))

	mem, err := openDiff(filepath.Join(v.dir, memPath), Block, func(b int) ([]byte, error) {
		if d, ok := v.memOverride[b]; ok {
			return d, nil
		}
		return v.Session.Get(ctx, 0, b)
	})
	if err != nil {
		v.Machine.Resume(ctx)
		return nil, st, err
	}
	defer mem.Close()
	// The VM can run again: the diff file and the disk overlay hold what
	// the child needs, and nothing below touches guest memory. Writes the
	// guest makes to its disk from here on would leak into the child, so
	// the disk is read first.
	diskDirty := map[int]bool{}
	for _, b := range v.Disk.Dirty() {
		diskDirty[b] = true
	}
	diskCopy, err := snapshotDisk(v.Disk, diskDirty, filepath.Join(v.dir, "fork.disk"))
	if err != nil {
		v.Machine.Resume(ctx)
		return nil, st, err
	}
	defer diskCopy.Close()
	if err := v.Machine.Resume(ctx); err != nil {
		return nil, st, err
	}
	st.DirtyMem, st.DirtyDisk = len(mem.dirty), len(diskDirty)

	parent := v.Snap
	child, ws, err := v.host.cfg.Repo.Write(ctx, repo.Snapshot{Env: parent.Env, Parent: parent.ID, Meta: map[string]string{
		"vcpus": parent.Meta["vcpus"], "mem_mib": parent.Meta["mem_mib"], "kind": "fork",
	}}, []repo.FileSpec{
		{Name: "mem", Block: Block, Reader: mem, Size: v.Session.Files[0].Size, Base: v.Session.Files[0], Dirty: func(b int) bool { return mem.dirty[b] }},
		{Name: "disk", Block: Block, Reader: diskCopy, Size: v.Disk.Size(), Base: v.Session.Files[1], Dirty: func(b int) bool { return diskDirty[b] }},
		{Name: "state", Path: filepath.Join(v.dir, statePath), Block: Block},
	}, repo.WriteOptions{Compress: true})
	st.Write = ws
	st.Total = time.Since(t0)
	if err != nil {
		return nil, st, err
	}
	// A later fork of this VM builds on this one: remember the memory as
	// the child has it.
	for b := range mem.dirty {
		if d, err := mem.block(b); err == nil {
			v.memOverride[b] = d
		}
	}
	return child, st, nil
}

// diffMem reads guest memory as a diff snapshot left it: pages the guest
// wrote come from the sparse diff file, the rest from the base.
type diffMem struct {
	f     *os.File
	bs    int
	base  func(b int) ([]byte, error)
	ext   [][2]int64 // data extents of the diff file
	dirty map[int]bool
}

func openDiff(path string, block int, base func(int) ([]byte, error)) (*diffMem, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d := &diffMem{f: f, bs: block, base: base, dirty: map[int]bool{}}
	ext, err := dataExtents(f)
	if err != nil {
		f.Close()
		return nil, err
	}
	d.ext = ext
	for _, e := range ext {
		for b := e[0] / int64(block); b*int64(block) < e[1]; b++ {
			d.dirty[int(b)] = true
		}
	}
	return d, nil
}

func (d *diffMem) Close() error { return d.f.Close() }

// block builds block b: the base, with the diff's pages laid over it.
func (d *diffMem) block(b int) ([]byte, error) {
	out := make([]byte, d.bs)
	base, err := d.base(b)
	if err != nil {
		return nil, err
	}
	copy(out, base)
	lo, hi := int64(b)*int64(d.bs), int64(b+1)*int64(d.bs)
	for _, e := range d.ext {
		s, t := max(lo, e[0]), min(hi, e[1])
		if s >= t {
			continue
		}
		if _, err := d.f.ReadAt(out[s-lo:t-lo], s); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
	}
	return out, nil
}

func (d *diffMem) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		b := int((off + int64(n)) / int64(d.bs))
		in := int((off + int64(n)) % int64(d.bs))
		blk, err := d.block(b)
		if err != nil {
			return n, err
		}
		n += copy(p[n:], blk[in:])
	}
	return n, nil
}

// snapshotDisk copies the disk's dirty blocks to a file, so the child's
// disk is fixed at the moment of the fork while the VM runs on.
func snapshotDisk(dev *nbd.Device, dirty map[int]bool, path string) (*frozenDisk, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	os.Remove(path) // the open file is enough
	buf := make([]byte, Block)
	for b := range dirty {
		if _, err := dev.ReadAt(buf, int64(b)*Block); err != nil && !errors.Is(err, io.EOF) {
			f.Close()
			return nil, err
		}
		if _, err := f.WriteAt(buf, int64(b)*Block); err != nil {
			f.Close()
			return nil, err
		}
	}
	return &frozenDisk{f: f, dirty: dirty, dev: dev}, nil
}

type frozenDisk struct {
	f     *os.File
	dirty map[int]bool
	dev   *nbd.Device
}

func (d *frozenDisk) ReadAt(p []byte, off int64) (int, error) {
	if d.dirty[int(off/Block)] {
		return d.f.ReadAt(p, off)
	}
	return d.dev.ReadAt(p, off)
}

func (d *frozenDisk) Close() error { return d.f.Close() }

// ManifestOf is a convenience for callers that want a snapshot's file.
func (h *Host) ManifestOf(ctx context.Context, s *repo.Snapshot, file string) (*image.Manifest, error) {
	return h.cfg.Repo.Manifest(ctx, s, file)
}
