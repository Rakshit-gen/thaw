package cache

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Rakshit-gen/thaw/image"
)

func block(i int) ([]byte, image.ID) {
	b := bytes.Repeat([]byte(fmt.Sprintf("block %06d ", i)), 400)[:4096]
	return b, image.Sum(b)
}

func TestPutGetReopen(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(dir, 1<<30, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		b, id := block(i)
		if err := c.Put(id, b); err != nil {
			t.Fatal(err)
		}
	}
	_, missing := block(1000)
	if _, ok := c.Get(missing); ok {
		t.Fatal("got a block that was never put")
	}
	c.Close()
	c, err = Open(dir, 1<<30, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		b, id := block(i)
		got, ok := c.Get(id)
		if !ok || !bytes.Equal(got, b) {
			t.Fatalf("block %d after reopen: ok=%v", i, ok)
		}
	}
	if st := c.Stats(); st.Blocks != 100 || st.Segments < 6 {
		t.Fatalf("stats %+v", st)
	}
}

func TestTornTailIsCutOff(t *testing.T) {
	dir := t.TempDir()
	c, _ := Open(dir, 1<<30, 1<<30)
	for i := range 10 {
		b, id := block(i)
		c.Put(id, b)
	}
	c.Close()
	// Chop the last record in half, as a crash mid-write would.
	p := filepath.Join(dir, "seg-00000000.log")
	info, _ := os.Stat(p)
	os.Truncate(p, info.Size()-2000)
	c, err := Open(dir, 1<<30, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	if st := c.Stats(); st.Blocks != 9 {
		t.Fatalf("%d blocks survive, want 9", st.Blocks)
	}
	b, id := block(9)
	c.Put(id, b) // writes after the cut, not after the garbage
	c.Close()
	c, _ = Open(dir, 1<<30, 1<<30)
	if got, ok := c.Get(id); !ok || !bytes.Equal(got, b) {
		t.Fatal("block written after recovery is lost")
	}
}

func TestEvictionGivesUsedBlocksASecondPass(t *testing.T) {
	// Room for about 4 segments of 4 blocks each.
	rec := int64(headerLen + 4096)
	c, _ := Open(t.TempDir(), 16*rec, 4*rec)
	for i := range 4 {
		b, id := block(i)
		c.Put(id, b)
	}
	_, hot := block(1)
	c.Get(hot) // used: should survive the first eviction of segment 0
	for i := 4; i < 20; i++ {
		b, id := block(i)
		c.Put(id, b)
	}
	if !c.Has(hot) {
		t.Fatal("a block read since it was written was evicted on the first pass")
	}
	_, cold := block(0)
	if c.Has(cold) {
		t.Fatal("a block nobody read should have gone")
	}
	if st := c.Stats(); st.Bytes > 16*rec+4*rec || st.Moved == 0 || st.Evicted == 0 {
		t.Fatalf("stats %+v", st)
	}
}

func TestConcurrentUse(t *testing.T) {
	rec := int64(headerLen + 4096)
	c, _ := Open(t.TempDir(), 64*rec, 8*rec)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				b, id := block((g*7 + i) % 200)
				c.Put(id, b)
				if got, ok := c.Get(id); ok && !bytes.Equal(got, b) {
					t.Error("wrong bytes")
					return
				}
			}
		}()
	}
	wg.Wait()
}
