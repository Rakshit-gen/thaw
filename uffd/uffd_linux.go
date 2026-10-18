// Package uffd serves a Firecracker VM's memory from userspace with
// userfaultfd.
//
// When Firecracker loads a snapshot with the Uffd memory backend it maps
// fresh anonymous memory for the guest, registers it with a userfaultfd,
// and hands that fd plus a description of the mapping to whoever listens
// on the backend socket. From then on the first touch of any guest page
// stops the vCPU and queues a page-fault message; nothing runs until the
// handler copies the page in.
//
// The handler here installs a whole snapshot block (64 KiB by default) per
// fault, not one page: the guest that touches one page of a block usually
// touches its neighbours, and one fetch per block is what the store and
// the planner work in. Faults on different blocks are served concurrently,
// so one vCPU waiting on the network doesn't hold up the other.
package uffd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl numbers and message layout from linux/userfaultfd.h.
const (
	uffdioWake       = 0x8010aa02
	uffdioCopy       = 0xc028aa03
	uffdioZeropage   = 0xc020aa04
	uffdioUnregister = 0x8010aa01

	eventPagefault = 0x12
	eventRemove    = 0x15
	eventUnmap     = 0x16

	msgSize = 32
)

type uffdioRange struct{ start, len uint64 }

type uffdioCopyArg struct {
	dst, src, len, mode uint64
	copied              int64
}

type uffdioZeroArg struct {
	rng      uffdioRange
	mode     uint64
	zeropage int64
}

// Region is one guest memory mapping as Firecracker describes it.
type Region struct {
	Base     uint64 `json:"base_host_virt_addr"`
	Size     uint64 `json:"size"`
	Offset   uint64 `json:"offset"` // where the region starts in the memory file
	PageSize uint64 `json:"page_size"`
}

// Source returns block i of the memory file, or nil if it is all zeros.
type Source func(ctx context.Context, block int) ([]byte, error)

// Stats count what a handler did.
type Stats struct {
	Faults    int64 // page-fault messages read
	Blocks    int64 // blocks installed because of a fault
	Installed int64 // blocks installed ahead of any fault (install mode)
	Zero      int64 // blocks installed as zero pages
	Woken     int64 // faults on blocks already in place
	Retries   int64 // EAGAIN retries
	Removed   int64 // ranges the guest gave back (balloon)
}

// Handler serves page faults for one VM.
type Handler struct {
	fd      int
	Regions []Region
	PID     int32 // Firecracker's PID, from the socket's peer credentials
	block   uint64
	src     Source
	conn    *net.UnixConn

	mu    sync.Mutex
	state map[int]uint8 // 1 installing, 2 installed

	failed atomic.Pointer[error]
	wg     sync.WaitGroup

	faults, blocks, installed, zero, woken, retries, removed atomic.Int64
}

// Accept waits for Firecracker to connect to ln and send the mappings and
// the userfaultfd.
func Accept(ln *net.UnixListener, block int, src Source) (*Handler, error) {
	conn, err := ln.AcceptUnix()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 64<<10)
	oob := make([]byte, unix.CmsgSpace(4))
	n, oobn, _, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("uffd: reading handshake: %w", err)
	}
	cmsgs, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(cmsgs) == 0 {
		conn.Close()
		return nil, errors.New("uffd: handshake carried no fd")
	}
	fds, err := unix.ParseUnixRights(&cmsgs[0])
	if err != nil || len(fds) == 0 {
		conn.Close()
		return nil, errors.New("uffd: handshake carried no fd")
	}
	h := &Handler{fd: fds[0], block: uint64(block), src: src, conn: conn, state: map[int]uint8{}}
	if err := json.Unmarshal(buf[:n], &h.Regions); err != nil {
		h.Close()
		return nil, fmt.Errorf("uffd: mappings: %w", err)
	}
	if len(h.Regions) == 0 {
		h.Close()
		return nil, errors.New("uffd: no memory regions")
	}
	raw, err := conn.SyscallConn()
	if err == nil {
		raw.Control(func(fd uintptr) {
			if cred, err := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED); err == nil {
				h.PID = cred.Pid
			}
		})
	}
	return h, nil
}

// Size is the guest memory size.
func (h *Handler) Size() uint64 {
	var n uint64
	for _, r := range h.Regions {
		n += r.Size
	}
	return n
}

// Stats returns the counters.
func (h *Handler) Stats() Stats {
	return Stats{h.faults.Load(), h.blocks.Load(), h.installed.Load(), h.zero.Load(), h.woken.Load(), h.retries.Load(), h.removed.Load()}
}

// Serve handles faults until ctx ends or the VM goes away. If serving a
// fault fails the guest can never continue, so Serve kills Firecracker
// rather than leave it hung, and returns the error.
func (h *Handler) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer h.wg.Wait()
	buf := make([]byte, msgSize*64)
	fds := []unix.PollFd{{Fd: int32(h.fd), Events: unix.POLLIN}}
	for {
		if p := h.failed.Load(); p != nil {
			h.kill()
			return *p
		}
		if ctx.Err() != nil {
			return nil
		}
		fds[0].Revents = 0
		n, err := unix.Poll(fds, 50)
		if err != nil {
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			return err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLHUP|unix.POLLERR) != 0 {
			return nil // Firecracker exited
		}
		m, err := unix.Read(h.fd, buf)
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EINTR) {
				continue
			}
			return fmt.Errorf("uffd: read: %w", err)
		}
		for off := 0; off+msgSize <= m; off += msgSize {
			msg := buf[off : off+msgSize]
			switch msg[0] {
			case eventPagefault:
				addr := *(*uint64)(unsafe.Pointer(&msg[16]))
				h.faults.Add(1)
				h.fault(ctx, addr)
			case eventRemove, eventUnmap:
				start := *(*uint64)(unsafe.Pointer(&msg[8]))
				end := *(*uint64)(unsafe.Pointer(&msg[16]))
				h.remove(start, end)
			}
		}
	}
}

func (h *Handler) fail(err error) {
	h.failed.CompareAndSwap(nil, &err)
}

func (h *Handler) kill() {
	if h.PID > 0 {
		unix.Kill(int(h.PID), unix.SIGKILL)
	}
}

// fileOffset maps a guest address to its offset in the memory file.
func (h *Handler) fileOffset(addr uint64) (uint64, bool) {
	for _, r := range h.Regions {
		if addr >= r.Base && addr < r.Base+r.Size {
			return r.Offset + (addr - r.Base), true
		}
	}
	return 0, false
}

func (h *Handler) fault(ctx context.Context, addr uint64) {
	off, ok := h.fileOffset(addr)
	if !ok {
		h.fail(fmt.Errorf("uffd: fault at %#x outside guest memory", addr))
		return
	}
	b := int(off / h.block)
	h.mu.Lock()
	st := h.state[b]
	if st == 0 {
		h.state[b] = 1
	}
	h.mu.Unlock()
	switch st {
	case 1:
		return // being installed; the copy wakes this fault too
	case 2:
		// Installed after this fault was queued. Wake the page in case the
		// copy's wake came first.
		page := addr &^ 4095
		r := uffdioRange{page, 4096}
		unix.Syscall(unix.SYS_IOCTL, uintptr(h.fd), uffdioWake, uintptr(unsafe.Pointer(&r)))
		h.woken.Add(1)
		return
	}
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		data, err := h.src(ctx, b)
		if err != nil {
			if ctx.Err() == nil {
				h.fail(fmt.Errorf("uffd: block %d: %w", b, err))
			}
			return
		}
		if err := h.place(b, data); err != nil {
			h.fail(err)
			return
		}
		h.blocks.Add(1)
	}()
}

// Install puts a block in guest memory before the guest asks for it. It
// does nothing if the block is already there or on its way.
func (h *Handler) Install(b int, data []byte) error {
	h.mu.Lock()
	if h.state[b] != 0 {
		h.mu.Unlock()
		return nil
	}
	h.state[b] = 1
	h.mu.Unlock()
	if err := h.place(b, data); err != nil {
		return err
	}
	h.installed.Add(1)
	return nil
}

// place copies block b into every region it overlaps and marks it
// installed.
func (h *Handler) place(b int, data []byte) error {
	start := uint64(b) * h.block
	end := start + h.block
	if data != nil && len(data)%4096 != 0 {
		// The last block of a file can be short; pad it to whole pages.
		data = append(data, make([]byte, 4096-len(data)%4096)...)
	}
	if data != nil && uint64(len(data)) < h.block {
		end = start + uint64(len(data))
	}
	if data != nil && uintptr(unsafe.Pointer(&data[0]))%4096 != 0 {
		// UFFDIO_COPY wants a page-aligned source.
		aligned := alignedBuf(len(data))
		copy(aligned, data)
		data = aligned
	}
	for _, r := range h.Regions {
		lo, hi := max(start, r.Offset), min(end, r.Offset+r.Size)
		if lo >= hi {
			continue
		}
		dst := r.Base + (lo - r.Offset)
		var err error
		if data == nil {
			err = h.zeroRange(dst, hi-lo)
		} else {
			err = h.copyRange(dst, data[lo-start:hi-start])
		}
		if err != nil {
			return err
		}
	}
	if data == nil {
		h.zero.Add(1)
	}
	h.mu.Lock()
	h.state[b] = 2
	h.mu.Unlock()
	return nil
}

func alignedBuf(n int) []byte {
	b := make([]byte, n+4096)
	off := int(uintptr(unsafe.Pointer(&b[0])) % 4096)
	if off != 0 {
		off = 4096 - off
	}
	return b[off : off+n]
}

// copyRange copies src to dst. Pages already present (EEXIST) are skipped
// by falling back to one page at a time. EAGAIN means the mapping is
// changing under us; it is retried.
func (h *Handler) copyRange(dst uint64, src []byte) error {
	for len(src) > 0 {
		arg := uffdioCopyArg{dst: dst, src: uint64(uintptr(unsafe.Pointer(&src[0]))), len: uint64(len(src))}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.fd), uffdioCopy, uintptr(unsafe.Pointer(&arg)))
		runtime.KeepAlive(src) // arg.src holds its address as a plain integer
		if errno == 0 {
			return nil
		}
		done := uint64(0)
		if arg.copied > 0 {
			done = uint64(arg.copied)
		}
		switch errno {
		case unix.EEXIST:
			// Some page in the range is there already. Skip the pages
			// copied so far plus the one that exists.
			done += 4096
		case unix.EAGAIN:
			h.retries.Add(1)
			time.Sleep(50 * time.Microsecond)
		case unix.ENOENT, unix.ESRCH:
			return nil // the VM is gone
		default:
			return fmt.Errorf("uffd: copy %#x+%d: %w", dst, len(src), errno)
		}
		if done > uint64(len(src)) {
			done = uint64(len(src))
		}
		dst += done
		src = src[done:]
	}
	return nil
}

func (h *Handler) zeroRange(dst, n uint64) error {
	for n > 0 {
		arg := uffdioZeroArg{rng: uffdioRange{dst, n}}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(h.fd), uffdioZeropage, uintptr(unsafe.Pointer(&arg)))
		if errno == 0 {
			return nil
		}
		done := uint64(0)
		if arg.zeropage > 0 {
			done = uint64(arg.zeropage)
		}
		switch errno {
		case unix.EEXIST:
			done += 4096
		case unix.EAGAIN:
			h.retries.Add(1)
			time.Sleep(50 * time.Microsecond)
		case unix.ENOENT, unix.ESRCH:
			return nil
		default:
			return fmt.Errorf("uffd: zeropage %#x+%d: %w", dst, n, errno)
		}
		done = min(done, n)
		dst += done
		n -= done
	}
	return nil
}

// remove handles memory the guest gave back through the balloon: from now
// on the kernel should hand out zero pages there, so stop tracking it.
func (h *Handler) remove(start, end uint64) {
	h.removed.Add(1)
	r := uffdioRange{start, end - start}
	unix.Syscall(unix.SYS_IOCTL, uintptr(h.fd), uffdioUnregister, uintptr(unsafe.Pointer(&r)))
}

// Close releases the fd and the socket.
func (h *Handler) Close() error {
	h.wg.Wait()
	unix.Close(h.fd)
	return h.conn.Close()
}
