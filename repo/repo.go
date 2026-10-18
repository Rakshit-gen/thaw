// Package repo keeps snapshots in an object store.
//
// Layout of the bucket:
//
//	snapshots/<id>.json        the snapshot record; writing it is the commit
//	manifests/<id>/<file>      one manifest per file (mem, disk, state)
//	packs/<pack>               block bytes, many blocks per object
//	index/<pack>               which blocks a pack holds, for dedup
//	traces/<id>/<trace>        what a restore of the snapshot touched, in order
//	tomb/<pack>                GC has condemned the pack (see gc.go)
//
// Everything except snapshot records and tombstones is written once and
// never changed, so it can be cached on local disk forever.
package repo

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/image"
	"github.com/Rakshit-gen/thaw/objstore"
)

// Snapshot is one saved VM: its memory, disk and device state.
type Snapshot struct {
	ID      string              `json:"id"`
	Parent  string              `json:"parent,omitempty"`
	Env     string              `json:"env"`
	Created time.Time           `json:"created"`
	Files   map[string]FileInfo `json:"files"`
	Meta    map[string]string   `json:"meta,omitempty"`
}

// FileInfo describes one file of a snapshot.
type FileInfo struct {
	Manifest string `json:"manifest"` // object key
	Size     int64  `json:"size"`
	Block    int    `json:"block"`
	Stored   int64  `json:"stored"` // raw bytes in non-zero blocks
	New      int64  `json:"new"`    // raw bytes this snapshot uploaded itself
}

// Loc is where a block's stored bytes are.
type Loc struct {
	Pack  string
	Off   uint32
	Len   uint32
	Flags uint8
}

// Repo is a snapshot store on top of an object store.
type Repo struct {
	st    objstore.Store
	local string // local cache of immutable objects; "" for none
	// Grace bounds how long a writer may take between looking up which
	// blocks already exist and committing. GC never deletes a pack within
	// Grace of condemning it. See gc.go.
	Grace time.Duration

	mu    sync.Mutex
	index map[image.ID]Loc
	seen  map[string]bool // packs whose index is loaded
}

// Open returns a repo. local is a directory for caching immutable objects;
// it may be empty.
func Open(st objstore.Store, local string) *Repo {
	return &Repo{st: st, local: local, Grace: 10 * time.Minute, index: map[image.ID]Loc{}, seen: map[string]bool{}}
}

// Store is the underlying object store.
func (r *Repo) Store() objstore.Store { return r.st }

// getImmutable reads an object that never changes, through the local cache.
func (r *Repo) getImmutable(ctx context.Context, key string) ([]byte, error) {
	var p string
	if r.local != "" {
		p = filepath.Join(r.local, "objects", filepath.FromSlash(key))
		if b, err := os.ReadFile(p); err == nil {
			return b, nil
		}
	}
	b, err := r.st.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if p != "" {
		os.MkdirAll(filepath.Dir(p), 0o755)
		tmp := p + ".tmp"
		if os.WriteFile(tmp, b, 0o644) == nil {
			os.Rename(tmp, p)
		}
	}
	return b, nil
}

// Snapshot reads one snapshot record.
func (r *Repo) Snapshot(ctx context.Context, id string) (*Snapshot, error) {
	b, err := r.st.Get(ctx, "snapshots/"+id+".json")
	if errors.Is(err, objstore.ErrNotFound) {
		return nil, fmt.Errorf("repo: no snapshot %q", id)
	}
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("repo: snapshot %s: %w", id, err)
	}
	return &s, nil
}

// Snapshots lists every committed snapshot, oldest first.
func (r *Repo) Snapshots(ctx context.Context) ([]*Snapshot, error) {
	objs, err := r.st.List(ctx, "snapshots/")
	if err != nil {
		return nil, err
	}
	var out []*Snapshot
	for _, o := range objs {
		id := strings.TrimSuffix(strings.TrimPrefix(o.Key, "snapshots/"), ".json")
		s, err := r.Snapshot(ctx, id)
		if err != nil {
			continue // deleted after we listed it
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out, nil
}

// Delete removes a snapshot record. Its manifests, traces and any packs
// nothing else uses are left for GC.
func (r *Repo) Delete(ctx context.Context, id string) error {
	return r.st.Delete(ctx, "snapshots/"+id+".json")
}

// Manifest reads the manifest of one file of a snapshot.
func (r *Repo) Manifest(ctx context.Context, s *Snapshot, file string) (*image.Manifest, error) {
	fi, ok := s.Files[file]
	if !ok {
		return nil, fmt.Errorf("repo: snapshot %s has no %q", s.ID, file)
	}
	b, err := r.getImmutable(ctx, fi.Manifest)
	if err != nil {
		return nil, fmt.Errorf("repo: manifest %s: %w", fi.Manifest, err)
	}
	return image.Decode(b)
}

// LocOf returns the location of a block of m.
func LocOf(m *image.Manifest, i int) Loc {
	ref := m.Refs[i]
	return Loc{Pack: m.Packs[ref.Pack], Off: ref.Off, Len: ref.Len, Flags: ref.Flags}
}

// ReadLoc fetches and checks one block. If its pack is gone (it was
// repacked) the block is looked up again by ID.
func (r *Repo) ReadLoc(ctx context.Context, loc Loc, id image.ID, n int) ([]byte, error) {
	b, err := r.st.GetRange(ctx, "packs/"+loc.Pack, int64(loc.Off), int64(loc.Len))
	if errors.Is(err, objstore.ErrNotFound) {
		nl, ok, lerr := r.Lookup(ctx, id, true)
		if lerr != nil {
			return nil, lerr
		}
		if !ok || nl.Pack == loc.Pack {
			return nil, fmt.Errorf("repo: block %s: pack %s is gone and no other copy is indexed", id, loc.Pack)
		}
		loc = nl
		b, err = r.st.GetRange(ctx, "packs/"+loc.Pack, int64(loc.Off), int64(loc.Len))
	}
	if err != nil {
		return nil, err
	}
	return image.Expand(b, loc.Flags, n, id)
}

// Lookup finds a block in the index. With refresh it first loads index
// objects it hasn't seen.
func (r *Repo) Lookup(ctx context.Context, id image.ID, refresh bool) (Loc, bool, error) {
	if refresh {
		if err := r.LoadIndex(ctx, nil); err != nil {
			return Loc{}, false, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.index[id]
	return l, ok, nil
}

// LoadIndex loads every pack index it hasn't loaded yet. Packs in skip are
// left out of the index so new snapshots don't reuse them.
func (r *Repo) LoadIndex(ctx context.Context, skip map[string]bool) error {
	objs, err := r.st.List(ctx, "index/")
	if err != nil {
		return err
	}
	var todo []string
	present := map[string]bool{}
	r.mu.Lock()
	for _, o := range objs {
		p := strings.TrimPrefix(o.Key, "index/")
		present[p] = true
		if !r.seen[p] {
			todo = append(todo, p)
		}
	}
	// Forget packs GC deleted since we last looked, or a long-lived client
	// would reuse blocks that are gone.
	for p := range r.seen {
		if !present[p] {
			delete(r.seen, p)
		}
	}
	for id, l := range r.index {
		if !present[l.Pack] {
			delete(r.index, id)
		}
	}
	r.mu.Unlock()
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	sem := make(chan struct{}, 16)
	for _, p := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			b, err := r.getImmutable(ctx, "index/"+p)
			if errors.Is(err, objstore.ErrNotFound) {
				return // collected after we listed it
			}
			if err == nil {
				var ents []indexEntry
				ents, err = decodeIndex(b)
				if err == nil {
					r.mu.Lock()
					for _, e := range ents {
						if _, have := r.index[e.id]; !have {
							r.index[e.id] = Loc{Pack: p, Off: e.off, Len: e.len, Flags: e.flags}
						}
					}
					r.seen[p] = true
					r.mu.Unlock()
				}
			}
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("repo: index %s: %w", p, err)
				}
				errMu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(skip) > 0 {
		r.mu.Lock()
		for id, l := range r.index {
			if skip[l.Pack] {
				delete(r.index, id)
			}
		}
		r.mu.Unlock()
	}
	return firstErr
}

// forgetPack drops a pack from the in-memory index, after GC deleted it.
func (r *Repo) forgetPack(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, l := range r.index {
		if l.Pack == p {
			delete(r.index, id)
		}
	}
	delete(r.seen, p)
}

type indexEntry struct {
	id    image.ID
	off   uint32
	len   uint32
	flags uint8
}

const indexMagic = "THAWIDX1"

func encodeIndex(ents []indexEntry) []byte {
	var b bytes.Buffer
	b.WriteString(indexMagic)
	binary.Write(&b, binary.LittleEndian, uint32(len(ents)))
	var e [25]byte
	for _, x := range ents {
		copy(e[:16], x.id[:])
		binary.LittleEndian.PutUint32(e[16:], x.off)
		binary.LittleEndian.PutUint32(e[20:], x.len)
		e[24] = x.flags
		b.Write(e[:])
	}
	return b.Bytes()
}

func decodeIndex(b []byte) ([]indexEntry, error) {
	if len(b) < len(indexMagic)+4 || string(b[:len(indexMagic)]) != indexMagic {
		return nil, errors.New("not an index")
	}
	b = b[len(indexMagic):]
	n := binary.LittleEndian.Uint32(b)
	b = b[4:]
	if len(b) != int(n)*25 {
		return nil, errors.New("index has the wrong length")
	}
	out := make([]indexEntry, n)
	for i := range out {
		e := b[i*25:]
		copy(out[i].id[:], e[:16])
		out[i].off = binary.LittleEndian.Uint32(e[16:])
		out[i].len = binary.LittleEndian.Uint32(e[20:])
		out[i].flags = e[24]
	}
	return out, nil
}

// PutTrace stores a restore trace for a snapshot.
func (r *Repo) PutTrace(ctx context.Context, snap, name string, data []byte) error {
	return r.st.Put(ctx, "traces/"+snap+"/"+name, data)
}

// Traces returns every trace stored for a snapshot, oldest first.
func (r *Repo) Traces(ctx context.Context, snap string) ([][]byte, error) {
	objs, err := r.st.List(ctx, "traces/"+snap+"/")
	if err != nil {
		return nil, err
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	var out [][]byte
	for _, o := range objs {
		b, err := r.getImmutable(ctx, o.Key)
		if errors.Is(err, objstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
