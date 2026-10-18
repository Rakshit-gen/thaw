// Command thaw-guest is the agent inside each VM. It runs as PID 1, sets up
// the bare minimum of a Linux userland, and answers requests from the host
// on vsock port 52.
//
// Requests are JSON lines:
//
//	{"op":"ping"}
//	{"op":"setup","env":"py-data"}            start and warm an environment's workers
//	{"op":"exec","cmd":["git","status"],"cwd":"/work/django"}
//	{"op":"py","code":"print(df.shape)"}      run code in the warm Python kernel
//	{"op":"ts","query":{...}}                 ask the warm TypeScript language service
//	{"op":"sync"}                             flush the page cache to disk before a snapshot
//
// Each gets one JSON line back: {"ok":true,"out":"...","ms":12.5}.

//go:build linux

package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

//go:embed pykernel.py
var pykernel []byte

//go:embed tsworker.js
var tsworker []byte

const port = 52

type request struct {
	Op      string          `json:"op"`
	Env     string          `json:"env,omitempty"`
	Cmd     []string        `json:"cmd,omitempty"`
	Cwd     string          `json:"cwd,omitempty"`
	Code    string          `json:"code,omitempty"`
	Query   json.RawMessage `json:"query,omitempty"`
	Timeout int             `json:"timeout_ms,omitempty"`
}

type response struct {
	OK  bool    `json:"ok"`
	Out string  `json:"out,omitempty"`
	Err string  `json:"err,omitempty"`
	Ms  float64 `json:"ms"`
}

func main() {
	if os.Getpid() == 1 {
		initSystem()
		go reap()
	}
	ln, err := listenVsock(port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "thaw-guest:", err)
		if os.Getpid() == 1 {
			select {} // PID 1 must not exit; the kernel would panic
		}
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "thaw-guest: listening on vsock port", port)
	for {
		nfd, _, err := unix.Accept(ln)
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go serve(os.NewFile(uintptr(nfd), "vsock"))
	}
}

// listenVsock listens on a vsock port for any CID. The net package doesn't
// know AF_VSOCK, so this stays at the syscall level.
func listenVsock(port uint32) (int, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return -1, err
	}
	if err := unix.Listen(fd, 64); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func initSystem() {
	mounts := []struct{ src, dst, typ, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", ""},
		{"devpts", "/dev/pts", "devpts", ""},
		{"tmpfs", "/tmp", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=755"},
	}
	for _, m := range mounts {
		os.MkdirAll(m.dst, 0o755)
		unix.Mount(m.src, m.dst, m.typ, 0, m.data)
	}
	unix.Mount("", "/", "", unix.MS_REMOUNT, "")
	unix.Sethostname([]byte("thaw"))
	os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	os.Setenv("HOME", "/root")
	os.Setenv("LANG", "C.UTF-8")
	os.Setenv("PYTHONDONTWRITEBYTECODE", "1")
	// The loopback interface, for anything that insists on 127.0.0.1.
	exec.Command("ip", "link", "set", "lo", "up").Run()
}

// reap collects orphaned children, which is PID 1's job.
func reap() {
	for {
		var ws unix.WaitStatus
		pid, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func serve(c io.ReadWriteCloser) {
	defer c.Close()
	r := bufio.NewReaderSize(c, 1<<20)
	enc := json.NewEncoder(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req request
		var resp response
		start := time.Now()
		if err := json.Unmarshal(line, &req); err != nil {
			resp.Err = err.Error()
		} else {
			out, err := handle(req)
			resp.Out = out
			if err != nil {
				resp.Err = err.Error()
			} else {
				resp.OK = true
			}
		}
		resp.Ms = float64(time.Since(start).Microseconds()) / 1000
		if enc.Encode(resp) != nil {
			return
		}
	}
}

func handle(req request) (string, error) {
	switch req.Op {
	case "ping":
		return "pong", nil
	case "sync":
		unix.Sync()
		return "", nil
	case "exec":
		return run(req)
	case "py":
		return workers.ask("py", map[string]string{"code": req.Code})
	case "ts":
		return workers.ask("ts", req.Query)
	case "setup":
		return setup(req.Env)
	}
	return "", fmt.Errorf("unknown op %q", req.Op)
}

func run(req request) (string, error) {
	if len(req.Cmd) == 0 {
		return "", errors.New("exec: no command")
	}
	timeout := 120 * time.Second
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, req.Cmd[0], req.Cmd[1:]...)
	cmd.Dir = req.Cwd
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	s := out.String()
	if len(s) > 64<<10 {
		s = s[:64<<10] + "\n[truncated]"
	}
	return s, err
}

// worker is a long-lived interpreter that holds an environment's warm state
// and answers one JSON line per request.
type worker struct {
	mu  sync.Mutex
	in  io.WriteCloser
	out *bufio.Reader
}

type pool struct {
	mu sync.Mutex
	m  map[string]*worker
}

var workers = &pool{m: map[string]*worker{}}

func (p *pool) start(name string, cmd *exec.Cmd) (*worker, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	w := &worker{in: in, out: bufio.NewReaderSize(out, 1<<20)}
	p.mu.Lock()
	p.m[name] = w
	p.mu.Unlock()
	return w, nil
}

func (p *pool) ask(name string, msg any) (string, error) {
	p.mu.Lock()
	w := p.m[name]
	p.mu.Unlock()
	if w == nil {
		return "", fmt.Errorf("no %s worker; run setup first", name)
	}
	b, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.in.Write(append(b, '\n')); err != nil {
		return "", err
	}
	line, err := w.out.ReadBytes('\n')
	if err != nil {
		return "", err
	}
	var r struct {
		OK  bool   `json:"ok"`
		Out string `json:"out"`
		Err string `json:"err"`
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return "", fmt.Errorf("%s worker: %w", name, err)
	}
	if !r.OK {
		return r.Out, errors.New(r.Err)
	}
	return r.Out, nil
}

// setup starts an environment's workers and brings them to the state the
// snapshot should capture.
func setup(env string) (string, error) {
	os.MkdirAll("/run/thaw", 0o755)
	switch env {
	case "py-data":
		os.WriteFile("/run/thaw/pykernel.py", pykernel, 0o644)
		if _, err := workers.start("py", exec.Command("python3", "-u", "/run/thaw/pykernel.py")); err != nil {
			return "", err
		}
		return workers.ask("py", map[string]string{"code": pySetup})
	case "repo":
		// Warm git's view of the work tree and Python's of Django, the way
		// an agent that had been working in the repo would leave them.
		var out strings.Builder
		for _, c := range [][]string{
			{"git", "status", "--short"},
			{"git", "log", "--oneline", "-1"},
			{"python3", "-c", "import django; print(django.get_version())"},
		} {
			o, err := run(request{Cmd: c, Cwd: "/work/django"})
			if err != nil {
				return o, err
			}
			out.WriteString(o)
		}
		return out.String(), nil
	case "node-ts":
		os.WriteFile("/run/thaw/tsworker.js", tsworker, 0o644)
		cmd := exec.Command("node", "--max-old-space-size=900", "/run/thaw/tsworker.js")
		cmd.Env = append(os.Environ(), "NODE_PATH=/usr/local/lib/node_modules")
		if _, err := workers.start("ts", cmd); err != nil {
			return "", err
		}
		return workers.ask("ts", map[string]string{"op": "warm"})
	}
	return "", fmt.Errorf("unknown env %q", env)
}

// pySetup builds the dataframe the py-data tasks query: three million
// orders, about 200 MB in memory. The seed is fixed so every boot builds
// the same frame.
const pySetup = `
import numpy as np, pandas as pd
rng = np.random.default_rng(42)
n = 3_000_000
df = pd.DataFrame({
    "customer": rng.integers(0, 200_000, n),
    "region": pd.Categorical(rng.choice(["apac", "emea", "latam", "na"], n, p=[.3, .25, .1, .35])),
    "product": rng.integers(0, 5_000, n),
    "price": rng.gamma(2.0, 30.0, n).round(2),
    "qty": rng.integers(1, 6, n),
    "ts": pd.Timestamp("2025-01-01") + pd.to_timedelta(rng.integers(0, 365 * 86400, n), unit="s"),
})
df["revenue"] = df.price * df.qty
print(df.shape, round(df.memory_usage(deep=True).sum() / 2**20), "MiB")
`
