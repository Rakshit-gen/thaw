// Package cache keeps blocks on a host's local disk, by content ID, for
// every VM on the host to share. Twenty forks of one snapshot fault in the
// same blocks; the first fetch puts them here and the other nineteen read
// them from disk.
//
// The cache is a log: blocks are appended to segment files and an in-memory
// map says where each one is. When the cache is over capacity the oldest
// segment goes, except that blocks read since they were written get one
// more pass by being copied to the head first (the clock algorithm, a
// segment at a time). A restart rebuilds the map by scanning the segments,
// stopping at the first torn or corrupt record of each.
package cache

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Rakshit-gen/thaw/image"
)

const (
	recMagic  = 0x74686177 // "thaw"
	headerLen = 4 + 16 + 4 + 4
)

type entry struct {
	seg  int
	off  int64 // of the data, after the header
	n    int32
	used atomic.Bool // read since written or last moved
}

type segment struct {
	id   int
	f    *os.File
	size int64
	ids  []image.ID // blocks written here, in order; some may have moved on
}

// Stats counts what the cache did.
type Stats struct {
	Hits, Misses     int64
	Puts, Evicted    int64
	Moved            int64 // blocks given a second pass during eviction
	Bytes, Capacity  int64
	Blocks, Segments int
}

// Cache is a block cache on local disk. It is safe for concurrent use.
type Cache struct {
	dir      string
	capacity int64
	segSize  int64

	mu    sync.Mutex
	index map[image.ID]*entry
	segs  map[int]*segment
	order []int // segment IDs, oldest first
	head  *segment
	bytes int64

	hits, misses, puts, evicted, moved atomic.Int64
}

// Open opens or creates a cache in dir holding up to capacity bytes, in
// segments of segSize bytes (64 MiB if zero).
func Open(dir string, capacity, segSize int64) (*Cache, error) {
	if segSize == 0 {
		segSize = 64 << 20
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, capacity: capacity, segSize: segSize, index: map[image.ID]*entry{}, segs: map[int]*segment{}}
	names, err := filepath.Glob(filepath.Join(dir, "seg-*.log"))
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, n := range names {
		id, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(n), "seg-"), ".log"))
		if err == nil {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		if err := c.load(id); err != nil {
			return nil, err
		}
	}
	if err := c.roll(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Cache) segPath(id int) string { return filepath.Join(c.dir, fmt.Sprintf("seg-%08d.log", id)) }

// load scans one segment into the index. A torn tail is cut off.
func (c *Cache) load(id int) error {
	f, err := os.OpenFile(c.segPath(id), os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	var off int64
	var ids []image.ID
	hdr := make([]byte, headerLen)
	for {
		if _, err := f.ReadAt(hdr, off); err != nil {
			break
		}
		if binary.LittleEndian.Uint32(hdr) != recMagic {
			break
		}
		var bid image.ID
		copy(bid[:], hdr[4:20])
		n := int64(binary.LittleEndian.Uint32(hdr[20:]))
		sum := binary.LittleEndian.Uint32(hdr[24:])
		data := make([]byte, n)
		if _, err := f.ReadAt(data, off+headerLen); err != nil || crc32.ChecksumIEEE(data) != sum {
			break
		}
		if old := c.index[bid]; old == nil {
			c.index[bid] = &entry{seg: id, off: off + headerLen, n: int32(n)}
		} else {
			old.seg, old.off = id, off+headerLen // a later copy wins
		}
		ids = append(ids, bid)
		off += headerLen + n
	}
	if err := f.Truncate(off); err != nil {
		f.Close()
		return err
	}
	s := &segment{id: id, f: f, size: off, ids: ids}
	c.segs[id] = s
	c.order = append(c.order, id)
	c.bytes += off
	return nil
}

// roll starts a new head segment. Called with mu held (or before use).
func (c *Cache) roll() error {
	id := 0
	if len(c.order) > 0 {
		id = c.order[len(c.order)-1] + 1
	}
	f, err := os.OpenFile(c.segPath(id), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	s := &segment{id: id, f: f}
	c.segs[id] = s
	c.order = append(c.order, id)
	c.head = s
	return nil
}

// Get returns a block, or false if the cache doesn't have it.
func (c *Cache) Get(id image.ID) ([]byte, bool) {
	c.mu.Lock()
	e := c.index[id]
	var f *os.File
	var off int64
	var n int32
	if e != nil {
		f, off, n = c.segs[e.seg].f, e.off, e.n
		e.used.Store(true)
	}
	c.mu.Unlock()
	if e == nil {
		c.misses.Add(1)
		return nil, false
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil {
		// The segment was evicted between the lookup and the read.
		c.misses.Add(1)
		return nil, false
	}
	c.hits.Add(1)
	return buf, true
}

// Has reports whether the cache holds a block, without counting a hit.
func (c *Cache) Has(id image.ID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.index[id] != nil
}

// Put adds a block. Adding one that's already there does nothing.
func (c *Cache) Put(id image.ID, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.index[id] != nil {
		return nil
	}
	if err := c.appendLocked(id, data); err != nil {
		return err
	}
	c.puts.Add(1)
	return c.evictLocked()
}

func (c *Cache) appendLocked(id image.ID, data []byte) error {
	if c.head.size+headerLen+int64(len(data)) > c.segSize && c.head.size > 0 {
		if err := c.roll(); err != nil {
			return err
		}
	}
	rec := make([]byte, headerLen+len(data))
	binary.LittleEndian.PutUint32(rec, recMagic)
	copy(rec[4:20], id[:])
	binary.LittleEndian.PutUint32(rec[20:], uint32(len(data)))
	binary.LittleEndian.PutUint32(rec[24:], crc32.ChecksumIEEE(data))
	copy(rec[headerLen:], data)
	if _, err := c.head.f.WriteAt(rec, c.head.size); err != nil {
		return err
	}
	c.index[id] = &entry{seg: c.head.id, off: c.head.size + headerLen, n: int32(len(data))}
	c.head.ids = append(c.head.ids, id)
	c.head.size += int64(len(rec))
	c.bytes += int64(len(rec))
	return nil
}

// evictLocked drops the oldest segments until the cache fits, moving
// recently used blocks forward first.
func (c *Cache) evictLocked() error {
	for c.bytes > c.capacity && len(c.order) > 1 {
		oldID := c.order[0]
		old := c.segs[oldID]
		if old == c.head {
			break
		}
		// Find what lives in this segment.
		var keep []image.ID
		var drop []image.ID
		for _, id := range old.ids {
			e := c.index[id]
			if e == nil || e.seg != oldID {
				continue // moved or evicted already
			}
			if e.used.Load() {
				keep = append(keep, id)
			} else {
				drop = append(drop, id)
			}
		}
		for _, id := range keep {
			e := c.index[id]
			buf := make([]byte, e.n)
			if _, err := old.f.ReadAt(buf, e.off); err != nil {
				drop = append(drop, id)
				continue
			}
			delete(c.index, id)
			if err := c.appendLocked(id, buf); err != nil {
				return err
			}
			c.moved.Add(1) // its used bit starts clear: one more pass only
		}
		for _, id := range drop {
			delete(c.index, id)
			c.evicted.Add(1)
		}
		c.order = c.order[1:]
		delete(c.segs, oldID)
		c.bytes -= old.size
		old.f.Close()
		os.Remove(c.segPath(oldID))
	}
	return nil
}

// Stats returns counters and sizes.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Stats{
		Hits: c.hits.Load(), Misses: c.misses.Load(), Puts: c.puts.Load(),
		Evicted: c.evicted.Load(), Moved: c.moved.Load(),
		Bytes: c.bytes, Capacity: c.capacity, Blocks: len(c.index), Segments: len(c.order),
	}
}

// Close closes the segment files.
func (c *Cache) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first error
	for _, s := range c.segs {
		if err := s.f.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Drop removes every block and segment.
func (c *Cache) Drop() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.segs {
		s.f.Close()
		if err := os.Remove(c.segPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	c.index = map[image.ID]*entry{}
	c.segs = map[int]*segment{}
	c.order = nil
	c.bytes = 0
	return c.roll()
}
