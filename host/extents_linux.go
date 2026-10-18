//go:build linux

package host

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// dataExtents lists the parts of a sparse file that hold data. A
// Firecracker diff snapshot writes only dirty pages and leaves holes
// elsewhere, so its data extents are exactly the dirty memory.
func dataExtents(f *os.File) ([][2]int64, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd())
	var out [][2]int64
	for off := int64(0); off < info.Size(); {
		data, err := unix.Seek(fd, off, unix.SEEK_DATA)
		if errors.Is(err, unix.ENXIO) {
			break // no data past off
		}
		if err != nil {
			return nil, err
		}
		hole, err := unix.Seek(fd, data, unix.SEEK_HOLE)
		if err != nil {
			return nil, err
		}
		out = append(out, [2]int64{data, hole})
		off = hole
	}
	return out, nil
}
