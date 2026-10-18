package repo

import (
	"bytes"
	"context"
	"math/rand/v2"
	"testing"

	"github.com/Rakshit-gen/thaw/objstore"
)

// fakeImage is a file of n blocks where some are zero and the rest hold
// distinct bytes derived from seed.
func fakeImage(n, block int, seed uint64, zeroEvery int) []byte {
	b := make([]byte, n*block)
	r := rand.New(rand.NewPCG(seed, 7))
	for i := range n {
		if zeroEvery > 0 && i%zeroEvery == 0 {
			continue
		}
		blk := b[i*block : (i+1)*block]
		for j := 0; j < block; j += 8 {
			v := r.Uint64()
			for k := range 8 {
				blk[j+k] = byte(v >> (8 * k))
			}
		}
	}
	return b
}

func newRepo(t testing.TB) (*Repo, *objstore.Counting) {
	d, err := objstore.NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := objstore.NewCounting(d)
	return Open(c, t.TempDir()), c
}

func readAll(t testing.TB, r *Repo, s *Snapshot, file string) []byte {
	ctx := context.Background()
	m, err := r.Manifest(ctx, s, file)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 0, m.Size)
	for i, ref := range m.Refs {
		n := m.BlockLen(i)
		if ref.Zero() {
			out = append(out, make([]byte, n)...)
			continue
		}
		b, err := r.ReadLoc(ctx, LocOf(m, i), ref.ID, n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

func TestWriteReadBack(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	mem := fakeImage(40, 65536, 1, 3)
	mem = append(mem, []byte("a short tail")...) // last block is short
	s, st, err := r.Write(ctx, Snapshot{Env: "test"}, []FileSpec{
		{Name: "mem", Reader: bytes.NewReader(mem), Size: int64(len(mem))},
	}, WriteOptions{PackSize: 256 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if st.Zero != 14 || st.New != 27 || st.Packs < 6 {
		t.Fatalf("stats %+v", st)
	}
	if got := readAll(t, r, s, "mem"); !bytes.Equal(got, mem) {
		t.Fatal("read back differs")
	}
	got, err := r.Snapshot(ctx, s.ID)
	if err != nil || got.Files["mem"].Size != int64(len(mem)) {
		t.Fatalf("record: %+v %v", got, err)
	}
}

func TestForkUploadsOnlyWhatChanged(t *testing.T) {
	r, c := newRepo(t)
	ctx := context.Background()
	base := fakeImage(64, 65536, 2, 0)
	parent, _, err := r.Write(ctx, Snapshot{Env: "test"}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(base), Size: int64(len(base))}}, WriteOptions{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	child := append([]byte(nil), base...)
	for _, i := range []int{3, 17, 40} {
		child[i*65536+100] ^= 0xff
	}
	before := c.Counts()
	// A second repo client, as if on another host: it has to learn what
	// exists from the store's index objects.
	r2 := Open(r.Store(), t.TempDir())
	s, st, err := r2.Write(ctx, Snapshot{Env: "test", Parent: parent.ID}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(child), Size: int64(len(child))}}, WriteOptions{Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	if st.New != 3 || st.Reused != 61 {
		t.Fatalf("stats %+v", st)
	}
	if up := c.Counts().Sub(before).BytesIn; up > 3*65536+64<<10 {
		t.Fatalf("uploaded %d bytes for three changed blocks", up)
	}
	if !bytes.Equal(readAll(t, r2, s, "mem"), child) || !bytes.Equal(readAll(t, r, parent, "mem"), base) {
		t.Fatal("read back differs")
	}
	if s.Files["mem"].New != 3*65536 {
		t.Fatalf("New = %d", s.Files["mem"].New)
	}
}

func TestDuplicateBlocksInOneWrite(t *testing.T) {
	r, _ := newRepo(t)
	one := fakeImage(1, 65536, 3, 0)
	img := bytes.Repeat(one, 5)
	s, st, err := r.Write(context.Background(), Snapshot{}, []FileSpec{{Name: "disk", Reader: bytes.NewReader(img), Size: int64(len(img))}}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.New != 1 || st.Reused != 4 {
		t.Fatalf("stats %+v", st)
	}
	if !bytes.Equal(readAll(t, r, s, "disk"), img) {
		t.Fatal("read back differs")
	}
}

func TestOrderDecidesPackLayout(t *testing.T) {
	r, _ := newRepo(t)
	img := fakeImage(16, 65536, 4, 0)
	order := []int{9, 2, 14}
	s, _, err := r.Write(context.Background(), Snapshot{}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(img), Size: int64(len(img)), Order: order}}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := r.Manifest(context.Background(), s, "mem")
	for k, i := range order {
		if m.Refs[i].Off != uint32(k*65536) {
			t.Fatalf("block %d at %d, want %d", i, m.Refs[i].Off, k*65536)
		}
	}
	if m.Refs[0].Off != 3*65536 {
		t.Fatalf("first unlisted block at %d", m.Refs[0].Off)
	}
}

func TestGCKeepsLiveAndCollectsDead(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := fakeImage(20, 65536, 5, 0)
	b := fakeImage(20, 65536, 6, 0)
	sa, _, _ := r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(a), Size: int64(len(a))}}, WriteOptions{PackSize: 256 << 10})
	sb, _, _ := r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(b), Size: int64(len(b))}}, WriteOptions{PackSize: 256 << 10})
	r.Grace = 0 // tombstones are old enough at once; fine with no writers
	st, err := r.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Condemned != 0 || st.Deleted != 0 {
		t.Fatalf("nothing is dead yet: %+v", st)
	}
	r.Delete(ctx, sa.ID)
	st, _ = r.GC(ctx) // phase 1
	if st.Condemned != 5 || st.Deleted != 0 || st.OrphanManifests != 1 {
		t.Fatalf("phase 1: %+v", st)
	}
	st, _ = r.GC(ctx) // phase 2
	if st.Deleted != 5 {
		t.Fatalf("phase 2: %+v", st)
	}
	if !bytes.Equal(readAll(t, r, sb, "mem"), b) {
		t.Fatal("live snapshot damaged")
	}
	if left, _ := r.Store().List(ctx, "packs/"); len(left) != 5 {
		t.Fatalf("%d packs left", len(left))
	}
}

func TestGCRescuesAPackReusedAfterCondemning(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	a := fakeImage(8, 65536, 7, 0)
	sa, _, _ := r.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(a), Size: int64(len(a))}}, WriteOptions{})
	r.Grace = 0
	r.Delete(ctx, sa.ID)
	r.GC(ctx) // condemns a's pack
	// A new snapshot with the same content: the writer must not reuse the
	// condemned pack, so it uploads a fresh copy.
	r2 := Open(r.Store(), "")
	sb, st, err := r2.Write(ctx, Snapshot{}, []FileSpec{{Name: "mem", Reader: bytes.NewReader(a), Size: int64(len(a))}}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if st.Reused != 0 {
		t.Fatalf("reused blocks from a condemned pack: %+v", st)
	}
	r.GC(ctx)
	if !bytes.Equal(readAll(t, r2, sb, "mem"), a) {
		t.Fatal("snapshot damaged")
	}
}

func TestForkWithBaseReadsOnlyDirtyBlocks(t *testing.T) {
	r, _ := newRepo(t)
	ctx := context.Background()
	const block = 4096
	data := fakeImage(200, block, 9, 6)
	parent, _, err := r.Write(ctx, Snapshot{Env: "t"}, []FileSpec{{Name: "mem", Block: block, Reader: bytes.NewReader(data), Size: int64(len(data))}}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pm, _ := r.Manifest(ctx, parent, "mem")
	child := append([]byte(nil), data...)
	copy(child[17*block:], bytes.Repeat([]byte{0xee}, block))
	reads := &countingReader{r: bytes.NewReader(child)}
	s, st, err := r.Write(ctx, Snapshot{Env: "t", Parent: parent.ID}, []FileSpec{{
		Name: "mem", Block: block, Reader: reads, Size: int64(len(child)),
		Base: pm, Dirty: func(i int) bool { return i == 17 },
	}}, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reads.n != 2 || st.New != 1 { // hash pass and pack pass of block 17
		t.Fatalf("read %d blocks, %d new", reads.n, st.New)
	}
	if got := readAll(t, r, s, "mem"); !bytes.Equal(got, child) {
		t.Fatal("fork reads back wrong")
	}
}

type countingReader struct {
	r *bytes.Reader
	n int
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.n++
	return c.r.ReadAt(p, off)
}
