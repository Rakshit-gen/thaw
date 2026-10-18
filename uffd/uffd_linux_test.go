package uffd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	testBlock = 64 << 10
	testSize  = 64 * testBlock
)

func content(b int) []byte {
	if b%5 == 0 {
		return nil // zero block
	}
	return bytes.Repeat([]byte(fmt.Sprintf("%07d|", b)), testBlock/8)
}

// TestMain lets the test binary play Firecracker in a child process. The
// faulting side has to be another process: a Go thread stuck in a page
// fault can't stop for the garbage collector, and if the handler is in the
// same process the collector waits for a fault only the handler can serve.
func TestMain(m *testing.M) {
	if sock := os.Getenv("THAW_FAKE_FIRECRACKER"); sock != "" {
		if err := fakeFirecracker(sock); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeFirecracker does what Firecracker does on a Uffd snapshot load: map
// anonymous memory, register it with a fresh userfaultfd, send the fd and
// mapping to the handler's socket. Then it plays the guest, touching every
// page from several threads, and checks what it got.
func fakeFirecracker(sock string) error {
	fd, _, errno := unix.Syscall(unix.SYS_USERFAULTFD, unix.O_CLOEXEC|unix.O_NONBLOCK, 0, 0)
	if errno != 0 {
		return fmt.Errorf("userfaultfd: %v", errno)
	}
	api := struct{ api, features, ioctls uint64 }{api: 0xaa, features: 1 << 3}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, 0xc018aa3f, uintptr(unsafe.Pointer(&api))); e != 0 {
		return fmt.Errorf("UFFDIO_API: %v", e)
	}
	mem, err := unix.Mmap(-1, 0, testSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return err
	}
	reg := struct{ start, len, mode, ioctls uint64 }{start: uint64(uintptr(unsafe.Pointer(&mem[0]))), len: testSize, mode: 1}
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, fd, 0xc020aa00, uintptr(unsafe.Pointer(&reg))); e != 0 {
		return fmt.Errorf("UFFDIO_REGISTER: %v", e)
	}
	// Two regions, like a guest with a hole in its physical address map.
	half := uint64(testSize / 2)
	body, _ := json.Marshal([]Region{
		{Base: reg.start, Size: half, Offset: 0, PageSize: 4096},
		{Base: reg.start + half, Size: half, Offset: half, PageSize: 4096},
	})
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		return err
	}
	defer c.Close()
	if _, _, err := c.WriteMsgUnix(body, unix.UnixRights(int(fd)), nil); err != nil {
		return err
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := g; p < testSize/4096; p += 4 {
				_ = mem[p*4096+100]
			}
		}()
	}
	wg.Wait()
	for b := range testSize / testBlock {
		want := content(b)
		if want == nil {
			want = make([]byte, testBlock)
		}
		if !bytes.Equal(mem[b*testBlock:(b+1)*testBlock], want) {
			return fmt.Errorf("block %d has the wrong bytes", b)
		}
	}
	return nil
}

func TestServesFaultsFromSource(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("userfaultfd needs root here")
	}
	sock := filepath.Join(t.TempDir(), "uffd.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: sock, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "THAW_FAKE_FIRECRACKER="+sock)
	var out bytes.Buffer
	child.Stdout, child.Stderr = &out, &out
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	asked := map[int]int{}
	h, err := Accept(ln, testBlock, func(_ context.Context, b int) ([]byte, error) {
		mu.Lock()
		asked[b]++
		mu.Unlock()
		return content(b), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if h.PID != int32(child.Process.Pid) {
		t.Fatalf("peer pid %d, want %d", h.PID, child.Process.Pid)
	}
	// Install one block ahead of time, as install mode does.
	if err := h.Install(7, content(7)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Serve(ctx) }()
	if err := child.Wait(); err != nil {
		t.Fatalf("guest side: %v: %s", err, out.String())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for b, n := range asked {
		if n > 1 {
			t.Fatalf("block %d fetched %d times", b, n)
		}
	}
	if asked[7] != 0 {
		t.Fatal("installed block was fetched on fault")
	}
	st := h.Stats()
	t.Logf("%+v", st)
	if st.Blocks != 63 || st.Installed != 1 || st.Zero != 13 {
		t.Fatalf("%+v", st)
	}
	h.Close()
}
