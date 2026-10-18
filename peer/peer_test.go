package peer

import (
	"bytes"
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Rakshit-gen/thaw/cache"
	"github.com/Rakshit-gen/thaw/fetch"
	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/objstore"
	"github.com/Rakshit-gen/thaw/repo"
)

func TestOwnerIsStableAndSpread(t *testing.T) {
	hosts := []string{"a", "b", "c", "d"}
	count := map[string]int{}
	moved := 0
	for i := range 4000 {
		id := fmt.Sprintf("pack%d/%d", i/7, i%7)
		o := Owner(hosts, id)
		count[o]++
		// Removing host d only moves the blocks d owned.
		if o2 := Owner(hosts[:3], id); o2 != o {
			if o != "d" {
				t.Fatalf("block owned by %s moved to %s", o, o2)
			}
			moved++
		}
	}
	for h, n := range count {
		if n < 800 || n > 1200 {
			t.Fatalf("host %s owns %d of 4000", h, n)
		}
	}
	if moved != count["d"] {
		t.Fatal("moved blocks don't match d's share")
	}
}

func TestFleetFetchesEachBlockOnce(t *testing.T) {
	ctx := context.Background()
	dir, _ := objstore.NewDir(t.TempDir())
	const blk = 64 << 10
	data := make([]byte, 120*blk)
	for i := range 120 {
		copy(data[i*blk:], bytes.Repeat([]byte(fmt.Sprintf("b%04d", i)), blk/5))
	}
	snap, _, err := repo.Open(dir, "").Write(ctx, repo.Snapshot{Env: "t"}, []repo.FileSpec{{Name: "mem", Block: blk, Reader: bytes.NewReader(data), Size: int64(len(data))}}, repo.WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	counting := objstore.NewCounting(dir)

	type host struct {
		repo  *repo.Repo
		cache *cache.Cache
		srv   *httptest.Server
		local *Server
	}
	var hosts []*host
	var urls []string
	for range 3 {
		h := &host{repo: repo.Open(counting, "")}
		h.cache, _ = cache.Open(t.TempDir(), 1<<30, 0)
		h.local = &Server{Cache: h.cache, Repo: h.repo}
		h.srv = httptest.NewServer(h.local)
		defer h.srv.Close()
		hosts = append(hosts, h)
		urls = append(urls, h.srv.URL)
	}
	before := counting.Counts()
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := h.repo.Manifest(ctx, snap, "mem")
			if err != nil {
				t.Error(err)
				return
			}
			ring := &Ring{Self: urls[i], Hosts: urls, Local: h.local}
			var got atomic.Int32
			f := fetch.New(h.repo, []*image.Manifest{m}, h.cache, ring, fetch.Options{}, func(b fetch.Block, d []byte, _ fetch.Origin) {
				if !bytes.Equal(d, data[b.Index*blk:(b.Index+1)*blk]) {
					t.Errorf("host %d block %d wrong", i, b.Index)
				}
				got.Add(1)
			})
			var all []fetch.Block
			for j := range 120 {
				all = append(all, fetch.Block{Index: j})
			}
			f.Prefetch(ctx, all)
			if got.Load() != 120 {
				t.Errorf("host %d got %d blocks", i, got.Load())
			}
		}()
	}
	wg.Wait()
	c := counting.Counts().Sub(before)
	// 120 blocks, 7.5 MiB. Alone each host would read it all; together
	// they read about one copy, plus manifests.
	if c.BytesOut > int64(len(data))*3/2 {
		t.Fatalf("fleet read %d bytes from the store for a %d byte image", c.BytesOut, len(data))
	}
	t.Logf("store: %d gets, %d bytes for a %d byte image on 3 hosts", c.Gets, c.BytesOut, len(data))
}

func TestBadPeerDataIsRejected(t *testing.T) {
	c, _ := cache.Open(t.TempDir(), 1<<30, 0)
	good := bytes.Repeat([]byte("x"), 4096)
	id := image.Sum(good)
	c.Put(id, bytes.Repeat([]byte("y"), 4096)) // a corrupt cache entry under good's ID
	srv := httptest.NewServer(&Server{Cache: c})
	defer srv.Close()
	r := &Ring{Self: "me", Hosts: []string{srv.URL}}
	if _, err := r.GetMany(context.Background(), []fetch.Want{{ID: id, Len: 4096, Loc: repo.Loc{Pack: "p"}}}); err == nil {
		t.Fatal("accepted a block that fails its checksum")
	}
}
