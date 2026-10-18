package repo

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/Rakshit-gen/thaw/objstore"
)

func TestRepackPutsHotBlocksTogether(t *testing.T) {
	ctx := context.Background()
	dir, _ := objstore.NewDir(t.TempDir())
	r := Open(dir, "")
	data := fakeImage(300, 4096, 1, 7)
	copy(data[3*4096:], bytes.Repeat([]byte("compressible "), 300)) // one deflated hot block
	snap, _, err := r.Write(ctx, Snapshot{Env: "t"}, []FileSpec{{Name: "mem", Block: 4096, Reader: bytes.NewReader(data), Size: int64(len(data))}}, WriteOptions{PackSize: 64 << 10, Compress: true})
	if err != nil {
		t.Fatal(err)
	}
	hot := []int{250, 3, 177, 90, 12, 299, 41}
	out, st, err := r.Repack(ctx, snap, map[string][]int{"mem": hot}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if st.Blocks != len(hot) || st.Packs != 1 {
		t.Fatalf("%+v", st)
	}
	// A fresh reader sees the new manifest, the hot blocks back to back in
	// plan order, and every block still reads back right.
	r2 := Open(dir, "")
	got, err := r2.Snapshot(ctx, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	m, err := r2.Manifest(ctx, got, "mem")
	if err != nil {
		t.Fatal(err)
	}
	first := m.Refs[hot[0]]
	end := first.Off
	for _, i := range hot {
		ref := m.Refs[i]
		if ref.Pack != first.Pack || ref.Off != end {
			t.Fatalf("block %d at pack %d off %d, want pack %d off %d", i, ref.Pack, ref.Off, first.Pack, end)
		}
		end += ref.Len
	}
	for i := range m.Refs {
		want := data[i*4096 : (i+1)*4096]
		if m.Refs[i].Zero() {
			continue
		}
		b, err := r2.ReadLoc(ctx, LocOf(m, i), m.Refs[i].ID, 4096)
		if err != nil || !bytes.Equal(b, want) {
			t.Fatalf("block %d: %v", i, err)
		}
	}

	// GC: the old manifest goes once it's older than Grace, and so do packs
	// that only it used. Old packs with cold blocks stay.
	r.Grace = 0
	time.Sleep(10 * time.Millisecond)
	if _, err := r.GC(ctx); err != nil {
		t.Fatal(err)
	}
	gs, err := r.GC(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if objs, _ := dir.List(ctx, "manifests/"+snap.ID+"/"); len(objs) != 1 {
		t.Fatalf("%d manifests left, want 1", len(objs))
	}
	m2, _ := r.Manifest(ctx, got, "mem")
	for i := range m2.Refs {
		if m2.Refs[i].Zero() {
			continue
		}
		if _, err := r.ReadLoc(ctx, LocOf(m2, i), m2.Refs[i].ID, 4096); err != nil {
			t.Fatalf("after gc %+v: block %d: %v", gs, i, err)
		}
	}
}
