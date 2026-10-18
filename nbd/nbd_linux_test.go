package nbd

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const testBlock = 64 << 10

func base(b int) []byte {
	if b%4 == 3 {
		return nil
	}
	return bytes.Repeat([]byte(fmt.Sprintf("disk%04d", b)), testBlock/8)
}

func TestReadWriteThroughKernel(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	exec.Command("modprobe", "nbd").Run()
	dev, err := Free()
	if err != nil {
		t.Skip(err)
	}
	const size = 64 * testBlock
	d, err := Attach(dev, size, testBlock, func(_ context.Context, b int) ([]byte, error) { return base(b), nil }, filepath.Join(t.TempDir(), "overlay"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Detach()

	f, err := os.OpenFile(dev, os.O_RDWR|os.O_SYNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := make([]byte, size)
	if _, err := f.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	for b := range 64 {
		want := base(b)
		if want == nil {
			want = make([]byte, testBlock)
		}
		if !bytes.Equal(got[b*testBlock:(b+1)*testBlock], want) {
			t.Fatalf("block %d reads wrong", b)
		}
	}
	// A partial write into block 5 keeps the rest of the block.
	if _, err := f.WriteAt([]byte("hello from the guest"), 5*testBlock+4096); err != nil {
		t.Fatal(err)
	}
	f.Sync()
	if dirty := d.Dirty(); len(dirty) != 1 || dirty[0] != 5 {
		t.Fatalf("dirty %v", dirty)
	}
	blk := make([]byte, testBlock)
	if _, err := d.ReadAt(blk, 5*testBlock); err != nil {
		t.Fatal(err)
	}
	want := base(5)
	copy(want[4096:], "hello from the guest")
	if !bytes.Equal(blk, want) {
		t.Fatal("overlay lost the rest of the block")
	}
	t.Logf("%+v", d.Stats())
}
