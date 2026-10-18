// Package vm drives Firecracker: start the process, configure and boot a
// VM or load a snapshot through the API socket, pause, snapshot, and talk
// to the guest agent over vsock.
//
// Every VM runs in its own directory and the snapshot refers to its files
// by relative names (rootfs, v.sock), so a snapshot taken in one directory
// loads in another. That is how forks of one snapshot get their own disk
// and vsock socket without patching device state.
package vm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AgentPort is the vsock port thaw-guest listens on.
const AgentPort = 52

// Machine is one Firecracker process.
type Machine struct {
	Dir  string
	cmd  *exec.Cmd
	api  *http.Client
	done chan error
}

// Start runs firecracker in dir and waits for its API socket.
func Start(ctx context.Context, bin, dir string) (*Machine, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "api.sock")
	os.Remove(sock)
	console, err := os.Create(filepath.Join(dir, "console.log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin, "--api-sock", "api.sock", "--level", "Warning")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = console, console
	if err := cmd.Start(); err != nil {
		console.Close()
		return nil, err
	}
	m := &Machine{Dir: dir, cmd: cmd, done: make(chan error, 1)}
	go func() { m.done <- cmd.Wait(); console.Close() }()
	m.api = &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
	}}
	// The socket file appears at bind, a moment before firecracker listens,
	// so wait for a connection to go through rather than for the file.
	for {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return m, nil
		}
		select {
		case err := <-m.done:
			return nil, fmt.Errorf("vm: firecracker exited: %v: %s", err, m.Console())
		case <-ctx.Done():
			m.Kill()
			return nil, ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// Console returns the tail of the guest's serial console.
func (m *Machine) Console() string {
	b, _ := os.ReadFile(filepath.Join(m.Dir, "console.log"))
	if len(b) > 4096 {
		b = b[len(b)-4096:]
	}
	return string(b)
}

// PID is the Firecracker process ID.
func (m *Machine) PID() int { return m.cmd.Process.Pid }

func (m *Machine) call(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://fc"+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := m.api.Do(req)
	if err != nil {
		return fmt.Errorf("vm: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("vm: %s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// BootConfig describes a fresh VM.
type BootConfig struct {
	Kernel   string // path to the uncompressed kernel
	Args     string
	Rootfs   string // relative to the VM directory
	VCPUs    int
	MemMiB   int
	GuestCID uint32
}

// DefaultArgs boots straight into thaw-guest as init.
const DefaultArgs = "console=ttyS0 reboot=k panic=1 pci=off rw init=/sbin/thaw-guest quiet"

// Boot configures and starts a fresh VM.
func (m *Machine) Boot(ctx context.Context, c BootConfig) error {
	if c.Args == "" {
		c.Args = DefaultArgs
	}
	if c.GuestCID == 0 {
		c.GuestCID = 3
	}
	steps := []struct {
		path string
		body any
	}{
		{"/boot-source", map[string]any{"kernel_image_path": c.Kernel, "boot_args": c.Args}},
		{"/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": c.Rootfs, "is_root_device": true, "is_read_only": false}},
		{"/machine-config", map[string]any{"vcpu_count": c.VCPUs, "mem_size_mib": c.MemMiB, "track_dirty_pages": true}},
		{"/vsock", map[string]any{"guest_cid": c.GuestCID, "uds_path": "v.sock"}},
		{"/actions", map[string]any{"action_type": "InstanceStart"}},
	}
	for _, s := range steps {
		if err := m.call(ctx, http.MethodPut, s.path, s.body); err != nil {
			return err
		}
	}
	return nil
}

// Pause stops the vCPUs.
func (m *Machine) Pause(ctx context.Context) error {
	return m.call(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"})
}

// Resume starts them again.
func (m *Machine) Resume(ctx context.Context) error {
	return m.call(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"})
}

// Snapshot writes a snapshot of a paused VM. kind is "Full" or "Diff"; a
// diff holds only pages written since the VM was booted or loaded, with
// holes everywhere else. Paths are relative to the VM directory.
func (m *Machine) Snapshot(ctx context.Context, kind, state, mem string) error {
	return m.call(ctx, http.MethodPut, "/snapshot/create", map[string]any{
		"snapshot_type": kind, "snapshot_path": state, "mem_file_path": mem,
	})
}

// LoadConfig says how to load a snapshot. Exactly one of MemFile and
// UffdSocket is set.
type LoadConfig struct {
	State      string
	MemFile    string // load memory from a local file
	UffdSocket string // or have a userfaultfd handler serve it
	Resume     bool
}

// Load loads a snapshot into a freshly started Firecracker. Dirty page
// tracking is always on, so the VM can be snapshotted again as a diff.
func (m *Machine) Load(ctx context.Context, c LoadConfig) error {
	body := map[string]any{"snapshot_path": c.State, "resume_vm": c.Resume, "track_dirty_pages": true}
	switch {
	case c.UffdSocket != "":
		body["mem_backend"] = map[string]string{"backend_type": "Uffd", "backend_path": c.UffdSocket}
	case c.MemFile != "":
		body["mem_backend"] = map[string]string{"backend_type": "File", "backend_path": c.MemFile}
	default:
		return errors.New("vm: load needs a memory file or a uffd socket")
	}
	return m.call(ctx, http.MethodPut, "/snapshot/load", body)
}

// Kill stops Firecracker.
func (m *Machine) Kill() {
	if m.cmd.Process != nil {
		m.cmd.Process.Kill()
	}
	select {
	case <-m.done:
	case <-time.After(5 * time.Second):
	}
}

// Exited reports whether Firecracker has exited.
func (m *Machine) Exited() bool {
	select {
	case err := <-m.done:
		m.done <- err
		return true
	default:
		return false
	}
}

// Agent is a connection to thaw-guest.
type Agent struct {
	c net.Conn
	r *bufio.Reader
}

// Request and Response mirror thaw-guest's wire format.
type Request struct {
	Op      string          `json:"op"`
	Env     string          `json:"env,omitempty"`
	Cmd     []string        `json:"cmd,omitempty"`
	Cwd     string          `json:"cwd,omitempty"`
	Code    string          `json:"code,omitempty"`
	Query   json.RawMessage `json:"query,omitempty"`
	Timeout int             `json:"timeout_ms,omitempty"`
}

type Response struct {
	OK  bool    `json:"ok"`
	Out string  `json:"out"`
	Err string  `json:"err"`
	Ms  float64 `json:"ms"`
}

// Dial connects to the agent through Firecracker's vsock socket, retrying
// until ctx ends: right after boot the agent may not be listening yet.
func (m *Machine) Dial(ctx context.Context) (*Agent, error) {
	sock := filepath.Join(m.Dir, "v.sock")
	for {
		a, err := dialOnce(ctx, sock)
		if err == nil {
			return a, nil
		}
		if m.Exited() {
			return nil, fmt.Errorf("vm: firecracker exited: %s", m.Console())
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("vm: agent: %w (last error %v)", ctx.Err(), err)
		case <-time.After(time.Millisecond):
		}
	}
}

func dialOnce(ctx context.Context, sock string) (*Agent, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(c, "CONNECT %d\n", AgentPort)
	r := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, err := r.ReadString('\n')
	c.SetReadDeadline(time.Time{})
	if err != nil || !strings.HasPrefix(line, "OK ") {
		c.Close()
		return nil, fmt.Errorf("vsock connect: %q %v", line, err)
	}
	return &Agent{c: c, r: r}, nil
}

// Do sends one request and waits for the answer.
func (a *Agent) Do(ctx context.Context, req Request) (Response, error) {
	if dl, ok := ctx.Deadline(); ok {
		a.c.SetDeadline(dl)
		defer a.c.SetDeadline(time.Time{})
	}
	b, _ := json.Marshal(req)
	if _, err := a.c.Write(append(b, '\n')); err != nil {
		return Response{}, err
	}
	line, err := a.r.ReadBytes('\n')
	if err != nil {
		return Response{}, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, err
	}
	if !resp.OK {
		return resp, fmt.Errorf("guest: %s", strings.TrimSpace(resp.Err))
	}
	return resp, nil
}

// Close closes the connection.
func (a *Agent) Close() error { return a.c.Close() }
