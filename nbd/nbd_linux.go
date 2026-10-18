// Package nbd serves a snapshot's disk to the kernel as a block device, so
// Firecracker can use it as a drive while its blocks are still in the
// object store.
//
// It uses the kernel NBD client's ioctl interface over a socketpair, with
// no network and no handshake: thaw configures the device itself and then
// speaks the transmission protocol on its end of the pair. Reads come from
// the snapshot through the restore session. Writes go to a private
// copy-on-write overlay, a sparse file next to the VM, so forks of one
// snapshot never see each other's writes. The overlay's dirty blocks are
// exactly what a snapshot of this VM has to upload for its disk.
package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

const (
	ioSetSock       = 0xab00
	ioSetBlksize    = 0xab01
	ioDoIt          = 0xab03
	ioClearSock     = 0xab04
	ioClearQue      = 0xab05
	ioSetSizeBlocks = 0xab07
	ioDisconnect    = 0xab08
	ioSetTimeout    = 0xab09
	ioSetFlags      = 0xab0a

	flagHasFlags  = 1 << 0
	flagSendFlush = 1 << 2
	flagSendTrim  = 1 << 5

	reqMagic   = 0x25609513
	replyMagic = 0x67446698

	cmdRead  = 0
	cmdWrite = 1
	cmdDisc  = 2
	cmdFlush = 3
	cmdTrim  = 4
)

// Source returns block i of the base disk image, or nil if it is all zeros.
type Source func(ctx context.Context, block int) ([]byte, error)

// Stats count what a device did.
type Stats struct {
	Reads, Writes, Flushes, Trims int64
	ReadBytes, WriteBytes         int64
	Dirty                         int // blocks copied into the overlay
}

// Device is one attached /dev/nbdN.
type Device struct {
	Path  string
	size  int64
	block int
	src   Source

	dev     *os.File
	sock    *os.File // our end
	overlay *os.File

	locks [64]sync.Mutex // striped by block
	mu    sync.Mutex
	dirty []bool

	wmu  sync.Mutex // serialises replies
	done chan error
	wg   sync.WaitGroup

	reads, writes, flushes, trims, rbytes, wbytes atomic.Int64
}

// Free returns the first /dev/nbdN no one is using.
func Free() (string, error) {
	devs, _ := filepath.Glob("/sys/block/nbd*")
	for _, d := range devs {
		if _, err := os.Stat(filepath.Join(d, "pid")); errors.Is(err, os.ErrNotExist) {
			if b, _ := os.ReadFile(filepath.Join(d, "size")); strings.TrimSpace(string(b)) == "0" {
				return "/dev/" + filepath.Base(d), nil
			}
		}
	}
	return "", errors.New("nbd: no free device (modprobe nbd nbds_max=64?)")
}

// Attach serves a disk of size bytes, made of blocks of the given size, on
// dev. Writes go to a sparse overlay file at overlay.
func Attach(dev string, size int64, block int, src Source, overlay string) (*Device, error) {
	d := &Device{Path: dev, size: size, block: block, src: src, done: make(chan error, 1)}
	d.dirty = make([]bool, (size+int64(block)-1)/int64(block))
	var err error
	if d.overlay, err = os.OpenFile(overlay, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600); err != nil {
		return nil, err
	}
	if err := d.overlay.Truncate(size); err != nil {
		d.overlay.Close()
		return nil, err
	}
	if d.dev, err = os.OpenFile(dev, os.O_RDWR, 0); err != nil {
		d.overlay.Close()
		return nil, err
	}
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		d.close()
		return nil, err
	}
	d.sock = os.NewFile(uintptr(fds[1]), "nbd-server")
	kfd := fds[0]
	fd := int(d.dev.Fd())
	steps := []struct {
		req uint
		arg int
	}{
		{ioClearSock, 0},
		{ioSetBlksize, 4096},
		{ioSetSizeBlocks, int(size / 4096)},
		{ioSetTimeout, 120},
		{ioSetFlags, flagHasFlags | flagSendFlush | flagSendTrim},
		{ioSetSock, kfd},
	}
	for _, s := range steps {
		if err := unix.IoctlSetInt(fd, s.req, s.arg); err != nil {
			unix.Close(kfd)
			d.close()
			return nil, fmt.Errorf("nbd: ioctl %#x on %s: %w", s.req, dev, err)
		}
	}
	go func() {
		// NBD_DO_IT runs the kernel side until the device is disconnected.
		runtime.LockOSThread()
		err := unix.IoctlSetInt(fd, ioDoIt, 0)
		unix.Close(kfd)
		d.done <- err
	}()
	go d.serve()
	return d, nil
}

func (d *Device) close() {
	if d.sock != nil {
		d.sock.Close()
	}
	if d.dev != nil {
		d.dev.Close()
	}
	if d.overlay != nil {
		d.overlay.Close()
	}
}

// Size is the disk size in bytes.
func (d *Device) Size() int64 { return d.size }

func (d *Device) serve() {
	hdr := make([]byte, 28)
	for {
		if _, err := io.ReadFull(d.sock, hdr); err != nil {
			return
		}
		if binary.BigEndian.Uint32(hdr) != reqMagic {
			return
		}
		typ := binary.BigEndian.Uint16(hdr[6:]) // low bits are the command, high bits flags
		handle := binary.BigEndian.Uint64(hdr[8:])
		off := int64(binary.BigEndian.Uint64(hdr[16:]))
		n := int(binary.BigEndian.Uint32(hdr[24:]))
		switch typ & 0xff {
		case cmdRead:
			d.reads.Add(1)
			d.rbytes.Add(int64(n))
			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				buf := make([]byte, n)
				err := d.readAt(buf, off)
				d.reply(handle, err, buf)
			}()
		case cmdWrite:
			buf := make([]byte, n)
			if _, err := io.ReadFull(d.sock, buf); err != nil {
				return
			}
			d.writes.Add(1)
			d.wbytes.Add(int64(n))
			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				d.reply(handle, d.writeAt(buf, off), nil)
			}()
		case cmdFlush:
			d.flushes.Add(1)
			d.wg.Wait() // writes before the flush are done
			d.reply(handle, nil, nil)
		case cmdTrim:
			d.trims.Add(1)
			d.reply(handle, nil, nil)
		case cmdDisc:
			return
		default:
			d.reply(handle, unix.EINVAL, nil)
		}
	}
}

func (d *Device) reply(handle uint64, err error, data []byte) {
	var hdr [16]byte
	binary.BigEndian.PutUint32(hdr[0:], replyMagic)
	if err != nil {
		binary.BigEndian.PutUint32(hdr[4:], uint32(unix.EIO))
		data = nil
	}
	binary.BigEndian.PutUint64(hdr[8:], handle)
	d.wmu.Lock()
	d.sock.Write(hdr[:])
	if data != nil {
		d.sock.Write(data)
	}
	d.wmu.Unlock()
}

func (d *Device) isDirty(b int) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dirty[b]
}

// readAt reads from the overlay for dirty blocks and the base otherwise.
func (d *Device) readAt(p []byte, off int64) error {
	ctx := context.Background()
	bs := int64(d.block)
	for len(p) > 0 {
		b := int(off / bs)
		in := off - int64(b)*bs
		n := min(int64(len(p)), bs-in)
		if b >= len(d.dirty) {
			return io.ErrUnexpectedEOF
		}
		l := &d.locks[b%len(d.locks)]
		l.Lock()
		if d.isDirty(b) {
			_, err := d.overlay.ReadAt(p[:n], off)
			l.Unlock()
			if err != nil {
				return err
			}
		} else {
			l.Unlock()
			data, err := d.src(ctx, b)
			if err != nil {
				return err
			}
			if data == nil {
				clear(p[:n])
			} else {
				avail := max(0, min(n, int64(len(data))-in))
				copy(p[:avail], data[in:in+avail])
				clear(p[avail:n])
			}
		}
		p = p[n:]
		off += n
	}
	return nil
}

// writeAt copies each touched block into the overlay on its first write,
// then writes there.
func (d *Device) writeAt(p []byte, off int64) error {
	ctx := context.Background()
	bs := int64(d.block)
	for len(p) > 0 {
		b := int(off / bs)
		in := off - int64(b)*bs
		n := min(int64(len(p)), bs-in)
		if b >= len(d.dirty) {
			return io.ErrUnexpectedEOF
		}
		l := &d.locks[b%len(d.locks)]
		l.Lock()
		if !d.isDirty(b) && (in != 0 || n != bs) {
			// A partial write needs the rest of the block from the base.
			base, err := d.src(ctx, b)
			if err != nil {
				l.Unlock()
				return err
			}
			if base != nil {
				if _, err := d.overlay.WriteAt(base, int64(b)*bs); err != nil {
					l.Unlock()
					return err
				}
			}
		}
		_, err := d.overlay.WriteAt(p[:n], off)
		if err == nil {
			d.mu.Lock()
			d.dirty[b] = true
			d.mu.Unlock()
		}
		l.Unlock()
		if err != nil {
			return err
		}
		p = p[n:]
		off += n
	}
	return nil
}

// Dirty lists the blocks written since attach.
func (d *Device) Dirty() []int {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []int
	for i, x := range d.dirty {
		if x {
			out = append(out, i)
		}
	}
	return out
}

// ReadAt reads the disk as the guest sees it now. With the VM paused it is
// a consistent image for a snapshot.
func (d *Device) ReadAt(p []byte, off int64) (int, error) {
	if off >= d.size {
		return 0, io.EOF
	}
	n := len(p)
	if off+int64(n) > d.size {
		n = int(d.size - off)
	}
	if err := d.readAt(p[:n], off); err != nil {
		return 0, err
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Stats returns the counters.
func (d *Device) Stats() Stats {
	return Stats{
		Reads: d.reads.Load(), Writes: d.writes.Load(), Flushes: d.flushes.Load(), Trims: d.trims.Load(),
		ReadBytes: d.rbytes.Load(), WriteBytes: d.wbytes.Load(), Dirty: len(d.Dirty()),
	}
}

// Detach disconnects the device and removes the overlay.
func (d *Device) Detach() error {
	fd := int(d.dev.Fd())
	unix.IoctlSetInt(fd, ioDisconnect, 0)
	d.sock.Close()
	<-d.done // NBD_DO_IT returns an error even on a clean disconnect
	unix.IoctlSetInt(fd, ioClearQue, 0)
	unix.IoctlSetInt(fd, ioClearSock, 0)
	d.wg.Wait()
	name := d.overlay.Name()
	d.close()
	return os.Remove(name)
}
