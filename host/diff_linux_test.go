package host

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A diff snapshot is sparse: written pages hold data, the rest are holes.
// diffMem must report exactly the blocks with data as dirty and lay the
// written pages over the base.
func TestDiffMemOverlaysWrittenPages(t *testing.T) {
	const blk = 64 << 10
	path := filepath.Join(t.TempDir(), "mem.diff")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(8 * blk)
	page := bytes.Repeat([]byte{0xAB}, 4096)
	f.WriteAt(page, 2*blk+8192) // one page inside block 2
	f.WriteAt(page, 5*blk)      // first page of block 5
	f.Close()

	base := func(b int) ([]byte, error) { return bytes.Repeat([]byte{byte(b)}, blk), nil }
	d, err := openDiff(path, blk, base)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if len(d.dirty) != 2 || !d.dirty[2] || !d.dirty[5] {
		t.Fatalf("dirty = %v, want blocks 2 and 5", d.dirty)
	}
	got, err := d.block(2)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 2 || got[8192] != 0xAB || got[8192+4095] != 0xAB || got[8192+4096] != 2 {
		t.Fatal("block 2 is not the base with the written page laid over it")
	}
	buf := make([]byte, 3*blk)
	if _, err := d.ReadAt(buf, 4*blk); err != nil {
		t.Fatal(err)
	}
	if buf[0] != 4 || buf[blk] != 0xAB || buf[blk+4096] != 5 || buf[2*blk] != 6 {
		t.Fatal("ReadAt across blocks 4 to 6 is wrong")
	}
}
