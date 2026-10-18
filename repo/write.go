package repo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/Rakshit-gen/thaw/image"
)

// FileSpec is one file to put into a snapshot.
type FileSpec struct {
	Name  string // mem, disk, state
	Path  string
	Block int // block size; 64 KiB if zero
	// Order lists blocks to pack first, in this order. The rest follow by
	// offset. Packing a snapshot's hot blocks in the order a restore touches
	// them lets prefetch read them with a few long ranged GETs.
	Order []int
	// Reader, if set, is read instead of opening Path. Size must be set too.
	Reader io.ReaderAt
	Size   int64
	// Base and Dirty describe a fork. Blocks Dirty says are unchanged since
	// Base take their ID from Base without being read or hashed, so a fork
	// of a 1 GiB VM that wrote 30 MiB reads 30 MiB. Reader must still be
	// able to produce any block, in case Base's copy is being collected.
	Base  *image.Manifest
	Dirty func(block int) bool
}

// WriteOptions tune a write.
type WriteOptions struct {
	PackSize int  // target pack size; 8 MiB if zero
	Compress bool // deflate blocks that shrink by an eighth or more
	Parallel int  // workers for hashing and compressing; GOMAXPROCS if zero
	Uploads  int  // packs uploaded at once; 8 if zero
}

// WriteStats says what a write did.
type WriteStats struct {
	Blocks    int           // blocks in all files
	Zero      int           // all-zero blocks, not stored
	Reused    int           // blocks already in the store
	New       int           // blocks uploaded
	NewBytes  int64         // raw bytes of new blocks
	Stored    int64         // bytes uploaded after compression
	Packs     int           // packs uploaded
	Hashing   time.Duration // reading and hashing every block
	Uploading time.Duration // compressing and uploading new blocks, after hashing
	Total     time.Duration
}

// ErrTooSlow means a write took longer than the repo's Grace, so blocks it
// planned to reuse may have been collected. Writing again is safe.
var ErrTooSlow = errors.New("repo: write took longer than the GC grace period")

// NewID returns a snapshot ID that sorts by creation time.
func NewID() string {
	var b [3]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405.000") + "-" + hex.EncodeToString(b[:])
}

type blockOut struct {
	id    image.ID
	pack  string // filled when the pack is sealed
	off   uint32
	len   uint32
	flags uint8
	zero  bool
}

type pendingPack struct {
	buf  []byte
	ents []indexEntry
	// blocks in this pack: which file slot to fill once the pack has an ID.
	slots []*blockOut
}

// Write uploads files as a new snapshot and commits it. snap supplies ID,
// Parent, Env and Meta; Created and Files are filled in.
func (r *Repo) Write(ctx context.Context, snap Snapshot, files []FileSpec, opt WriteOptions) (*Snapshot, WriteStats, error) {
	start := time.Now()
	var st WriteStats
	if opt.PackSize == 0 {
		opt.PackSize = 8 << 20
	}
	if opt.Parallel == 0 {
		opt.Parallel = runtime.GOMAXPROCS(0)
	}
	if opt.Uploads == 0 {
		opt.Uploads = 8
	}
	if snap.ID == "" {
		snap.ID = NewID()
	}

	// The lookup: which packs GC has condemned, then which blocks exist.
	// Everything we reuse must be committed within Grace of this moment.
	lookup := time.Now()
	tombs, err := r.tombstones(ctx)
	if err != nil {
		return nil, st, err
	}
	if err := r.LoadIndex(ctx, tombs); err != nil {
		return nil, st, err
	}

	type openFile struct {
		spec FileSpec
		ra   io.ReaderAt
		size int64
		outs []blockOut
		f    *os.File
	}
	var ofs []*openFile
	defer func() {
		for _, of := range ofs {
			if of.f != nil {
				of.f.Close()
			}
		}
	}()
	for _, fs := range files {
		of := &openFile{spec: fs, ra: fs.Reader, size: fs.Size}
		if of.spec.Block == 0 {
			of.spec.Block = 64 << 10
		}
		if of.spec.Block%4096 != 0 {
			return nil, st, fmt.Errorf("repo: %s: block size %d is not a multiple of 4096", fs.Name, of.spec.Block)
		}
		if of.ra == nil {
			f, err := os.Open(fs.Path)
			if err != nil {
				return nil, st, err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return nil, st, err
			}
			of.f, of.ra, of.size = f, f, info.Size()
		}
		n := int((of.size + int64(of.spec.Block) - 1) / int64(of.spec.Block))
		of.outs = make([]blockOut, n)
		ofs = append(ofs, of)
		st.Blocks += n
	}

	// Pass 1: hash every block in parallel.
	hashStart := time.Now()
	for _, of := range ofs {
		base := of.spec.Base
		if base != nil && (of.spec.Dirty == nil || base.Block != of.spec.Block || base.Size != of.size) {
			base = nil
		}
		if err := parallel(ctx, len(of.outs), opt.Parallel, func(i int, buf []byte) error {
			if base != nil && !of.spec.Dirty(i) {
				if ref := base.Refs[i]; ref.Zero() {
					of.outs[i].zero = true
				} else {
					of.outs[i].id = ref.ID
				}
				return nil
			}
			b, err := readBlock(of.ra, of.size, of.spec.Block, i, buf)
			if err != nil {
				return err
			}
			if image.IsZero(b) {
				of.outs[i].zero = true
				return nil
			}
			of.outs[i].id = image.Sum(b)
			return nil
		}, of.spec.Block); err != nil {
			return nil, st, err
		}
	}
	st.Hashing = time.Since(hashStart)

	// Pass 2: pack new blocks in order, compressing in parallel batches and
	// uploading sealed packs in the background.
	upStart := time.Now()
	var (
		upWG   sync.WaitGroup
		upSem  = make(chan struct{}, opt.Uploads)
		upMu   sync.Mutex
		upErr  error
		mine   = map[image.ID]*blockOut{} // new blocks in this write
		cur    = &pendingPack{}
		sealed []*pendingPack
	)
	seal := func() {
		if len(cur.ents) == 0 {
			return
		}
		p := cur
		cur = &pendingPack{}
		// Pack IDs are random, never derived from content: a pack GC is
		// about to delete must not share a name with a fresh upload.
		var raw [16]byte
		rand.Read(raw[:])
		id := hex.EncodeToString(raw[:])
		for _, s := range p.slots {
			s.pack = id
		}
		sealed = append(sealed, p)
		st.Packs++
		upWG.Add(1)
		upSem <- struct{}{}
		go func() {
			defer upWG.Done()
			defer func() { <-upSem }()
			err := r.st.Put(ctx, "packs/"+id, p.buf)
			if err == nil {
				err = r.st.Put(ctx, "index/"+id, encodeIndex(p.ents))
			}
			if err != nil {
				upMu.Lock()
				if upErr == nil {
					upErr = fmt.Errorf("repo: upload pack %s: %w", id, err)
				}
				upMu.Unlock()
				return
			}
			r.mu.Lock()
			for _, e := range p.ents {
				if _, have := r.index[e.id]; !have {
					r.index[e.id] = Loc{Pack: id, Off: e.off, Len: e.len, Flags: e.flags}
				}
			}
			r.seen[id] = true
			r.mu.Unlock()
		}()
	}

	for _, of := range ofs {
		order := blockOrder(of.spec.Order, len(of.outs))
		// Resolve reuse first, collect what's new.
		var todo []int
		for _, i := range order {
			o := &of.outs[i]
			if o.zero {
				st.Zero++
				continue
			}
			if l, ok := r.lookupLocked(o.id); ok {
				o.pack, o.off, o.len, o.flags = l.Pack, l.Off, l.Len, l.Flags
				st.Reused++
				continue
			}
			if m := mine[o.id]; m != nil {
				// The same content twice in this write: point at the first
				// copy once its pack is sealed.
				o.pack = "" // resolved below
				st.Reused++
				continue
			}
			mine[o.id] = o
			todo = append(todo, i)
		}
		const batch = 256
		for b0 := 0; b0 < len(todo); b0 += batch {
			idx := todo[b0:min(b0+batch, len(todo))]
			stored := make([][]byte, len(idx))
			flags := make([]uint8, len(idx))
			err := parallel(ctx, len(idx), opt.Parallel, func(k int, buf []byte) error {
				b, err := readBlock(of.ra, of.size, of.spec.Block, idx[k], buf)
				if err != nil {
					return err
				}
				if opt.Compress {
					s, f := image.Compress(b)
					if f == 0 {
						s = append([]byte(nil), b...)
					}
					stored[k], flags[k] = s, f
				} else {
					stored[k] = append([]byte(nil), b...)
				}
				return nil
			}, of.spec.Block)
			if err != nil {
				return nil, st, err
			}
			for k, i := range idx {
				o := &of.outs[i]
				o.off = uint32(len(cur.buf))
				o.len = uint32(len(stored[k]))
				o.flags = flags[k]
				cur.buf = append(cur.buf, stored[k]...)
				cur.ents = append(cur.ents, indexEntry{id: o.id, off: o.off, len: o.len, flags: o.flags})
				cur.slots = append(cur.slots, o)
				st.New++
				st.NewBytes += int64(blockLen(of.size, of.spec.Block, i))
				st.Stored += int64(o.len)
				if len(cur.buf) >= opt.PackSize {
					seal()
				}
			}
		}
	}
	seal()
	upWG.Wait()
	if upErr != nil {
		return nil, st, upErr
	}
	st.Uploading = time.Since(upStart)

	// Duplicates inside this write point at the first copy.
	for _, of := range ofs {
		for i := range of.outs {
			o := &of.outs[i]
			if !o.zero && o.pack == "" {
				m := mine[o.id]
				o.pack, o.off, o.len, o.flags = m.pack, m.off, m.len, m.flags
			}
		}
	}

	// Commit: manifests, then the record. Refuse if we took too long.
	if time.Since(lookup) > r.Grace-r.Grace/10 {
		return nil, st, ErrTooSlow
	}
	snap.Created = time.Now().UTC()
	snap.Files = map[string]FileInfo{}
	for _, of := range ofs {
		m := &image.Manifest{Size: of.size, Block: of.spec.Block}
		packIdx := map[string]uint32{}
		m.Refs = make([]image.Ref, len(of.outs))
		var newBytes int64
		for i, o := range of.outs {
			if o.zero {
				m.Refs[i] = image.Ref{Flags: image.FlagZero}
				continue
			}
			pi, ok := packIdx[o.pack]
			if !ok {
				pi = uint32(len(m.Packs))
				packIdx[o.pack] = pi
				m.Packs = append(m.Packs, o.pack)
			}
			m.Refs[i] = image.Ref{ID: o.id, Pack: pi, Off: o.off, Len: o.len, Flags: o.flags}
			if mine[o.id] == &of.outs[i] {
				newBytes += int64(blockLen(of.size, of.spec.Block, i))
			}
		}
		key := "manifests/" + snap.ID + "/" + of.spec.Name
		if err := r.st.Put(ctx, key, m.Encode()); err != nil {
			return nil, st, err
		}
		_, stored := m.Stored()
		snap.Files[of.spec.Name] = FileInfo{Manifest: key, Size: of.size, Block: of.spec.Block, Stored: stored, New: newBytes}
	}
	rec, _ := json.MarshalIndent(snap, "", "  ")
	if time.Since(lookup) > r.Grace-r.Grace/10 {
		return nil, st, ErrTooSlow
	}
	if err := r.st.Put(ctx, "snapshots/"+snap.ID+".json", rec); err != nil {
		return nil, st, err
	}
	st.Total = time.Since(start)
	return &snap, st, nil
}

func (r *Repo) lookupLocked(id image.ID) (Loc, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.index[id]
	return l, ok
}

// updateRecord rewrites a committed snapshot's record. Repack uses it to
// point a file at a new manifest.
func (r *Repo) updateRecord(ctx context.Context, s *Snapshot) error {
	rec, _ := json.MarshalIndent(s, "", "  ")
	return r.st.Put(ctx, "snapshots/"+s.ID+".json", rec)
}

func blockOrder(first []int, n int) []int {
	out := make([]int, 0, n)
	done := make([]bool, n)
	for _, i := range first {
		if i >= 0 && i < n && !done[i] {
			done[i] = true
			out = append(out, i)
		}
	}
	for i := range n {
		if !done[i] {
			out = append(out, i)
		}
	}
	return out
}

func blockLen(size int64, block, i int) int {
	if end := int64(i+1) * int64(block); end > size {
		return int(size - int64(i)*int64(block))
	}
	return block
}

func readBlock(ra io.ReaderAt, size int64, block, i int, buf []byte) ([]byte, error) {
	n := blockLen(size, block, i)
	b := buf[:n]
	if _, err := ra.ReadAt(b, int64(i)*int64(block)); err != nil && !(errors.Is(err, io.EOF) && n > 0) {
		return nil, err
	}
	return b, nil
}

// parallel runs fn for 0..n-1 on w workers, each with its own buffer.
func parallel(ctx context.Context, n, w int, fn func(i int, buf []byte) error, bufSize int) error {
	if n == 0 {
		return nil
	}
	next := make(chan int)
	errc := make(chan error, w)
	var wg sync.WaitGroup
	for range min(w, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, bufSize)
			for i := range next {
				if err := fn(i, buf); err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	var err error
loop:
	for i := range n {
		select {
		case next <- i:
		case err = <-errc:
			break loop
		case <-ctx.Done():
			err = ctx.Err()
			break loop
		}
	}
	close(next)
	wg.Wait()
	if err == nil {
		select {
		case err = <-errc:
		default:
		}
	}
	return err
}
