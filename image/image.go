// Package image describes a snapshot file (guest memory, a disk, the VM
// state) as a list of fixed-size blocks, each stored once by content.
//
// A block that is all zeros is not stored at all. Guest memory is mostly
// zeros: a 2 GiB VM that has touched 600 MB of it leaves the rest untouched,
// so the manifest says "zero" for those blocks and a restore never fetches
// them.
package image

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ID names a block by its content: the first 16 bytes of its SHA-256.
type ID [16]byte

// Sum returns the ID of b.
func Sum(b []byte) ID {
	s := sha256.Sum256(b)
	var id ID
	copy(id[:], s[:16])
	return id
}

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// ParseID reads the hex form.
func ParseID(s string) (ID, error) {
	var id ID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(id) {
		return id, fmt.Errorf("image: bad id %q", s)
	}
	copy(id[:], b)
	return id, nil
}

// Stored-block flags.
const (
	FlagZero  = 1 << 0 // all zeros, nothing stored
	FlagFlate = 1 << 1 // stored deflated
)

// Ref says where one block's bytes live.
type Ref struct {
	ID    ID
	Pack  uint32 // index into Manifest.Packs
	Off   uint32 // offset inside the pack
	Len   uint32 // stored length
	Flags uint8
}

// Zero reports whether the block is all zeros.
func (r Ref) Zero() bool { return r.Flags&FlagZero != 0 }

// Manifest lists the blocks of one file.
type Manifest struct {
	Size  int64    // file size in bytes
	Block int      // block size in bytes, a multiple of 4096
	Packs []string // pack IDs referenced by Refs
	Refs  []Ref
}

// Blocks is the number of blocks a file of the manifest's size has.
func (m *Manifest) Blocks() int { return int((m.Size + int64(m.Block) - 1) / int64(m.Block)) }

// BlockLen is the length of block i; only the last one can be short.
func (m *Manifest) BlockLen(i int) int {
	if end := int64(i+1) * int64(m.Block); end > m.Size {
		return int(m.Size - int64(i)*int64(m.Block))
	}
	return m.Block
}

// Stored is the number of non-zero blocks and their raw size.
func (m *Manifest) Stored() (blocks int, bytes int64) {
	for i, r := range m.Refs {
		if !r.Zero() {
			blocks++
			bytes += int64(m.BlockLen(i))
		}
	}
	return
}

const magic = "THAWMAN1"

// Encode writes the manifest in its binary form, deflated.
func (m *Manifest) Encode() []byte {
	var raw bytes.Buffer
	raw.WriteString(magic)
	binary.Write(&raw, binary.LittleEndian, uint64(m.Size))
	binary.Write(&raw, binary.LittleEndian, uint32(m.Block))
	binary.Write(&raw, binary.LittleEndian, uint32(len(m.Packs)))
	for _, p := range m.Packs {
		raw.WriteByte(byte(len(p)))
		raw.WriteString(p)
	}
	binary.Write(&raw, binary.LittleEndian, uint32(len(m.Refs)))
	var e [29]byte
	for _, r := range m.Refs {
		copy(e[:16], r.ID[:])
		binary.LittleEndian.PutUint32(e[16:], r.Pack)
		binary.LittleEndian.PutUint32(e[20:], r.Off)
		binary.LittleEndian.PutUint32(e[24:], r.Len)
		e[28] = r.Flags
		raw.Write(e[:])
	}
	var out bytes.Buffer
	w, _ := flate.NewWriter(&out, flate.BestSpeed)
	w.Write(raw.Bytes())
	w.Close()
	return out.Bytes()
}

// Decode reads what Encode wrote.
func Decode(b []byte) (*Manifest, error) {
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(b)))
	if err != nil {
		return nil, fmt.Errorf("image: manifest: %w", err)
	}
	r := bytes.NewReader(raw)
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(r, head); err != nil || string(head) != magic {
		return nil, errors.New("image: not a manifest")
	}
	var size uint64
	var block, npacks uint32
	binary.Read(r, binary.LittleEndian, &size)
	binary.Read(r, binary.LittleEndian, &block)
	if err := binary.Read(r, binary.LittleEndian, &npacks); err != nil {
		return nil, errors.New("image: short manifest")
	}
	m := &Manifest{Size: int64(size), Block: int(block)}
	if block == 0 || block%4096 != 0 {
		return nil, fmt.Errorf("image: bad block size %d", block)
	}
	for range npacks {
		n, err := r.ReadByte()
		if err != nil {
			return nil, errors.New("image: short manifest")
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(r, p); err != nil {
			return nil, errors.New("image: short manifest")
		}
		m.Packs = append(m.Packs, string(p))
	}
	var nrefs uint32
	if err := binary.Read(r, binary.LittleEndian, &nrefs); err != nil {
		return nil, errors.New("image: short manifest")
	}
	if int(nrefs) != m.Blocks() {
		return nil, fmt.Errorf("image: %d refs for %d blocks", nrefs, m.Blocks())
	}
	m.Refs = make([]Ref, nrefs)
	var e [29]byte
	for i := range m.Refs {
		if _, err := io.ReadFull(r, e[:]); err != nil {
			return nil, errors.New("image: short manifest")
		}
		ref := &m.Refs[i]
		copy(ref.ID[:], e[:16])
		ref.Pack = binary.LittleEndian.Uint32(e[16:])
		ref.Off = binary.LittleEndian.Uint32(e[20:])
		ref.Len = binary.LittleEndian.Uint32(e[24:])
		ref.Flags = e[28]
		if !ref.Zero() && int(ref.Pack) >= len(m.Packs) {
			return nil, fmt.Errorf("image: block %d names pack %d of %d", i, ref.Pack, len(m.Packs))
		}
	}
	return m, nil
}

var zeros = make([]byte, 1<<20)

// IsZero reports whether b is all zeros.
func IsZero(b []byte) bool {
	for len(b) > 0 {
		n := min(len(b), len(zeros))
		if !bytes.Equal(b[:n], zeros[:n]) {
			return false
		}
		b = b[n:]
	}
	return true
}

var flaters = sync.Pool{New: func() any {
	w, _ := flate.NewWriter(nil, flate.BestSpeed)
	return w
}}

// Compress deflates b and keeps the result only if it saves at least an
// eighth. It returns the bytes to store and their flags.
func Compress(b []byte) ([]byte, uint8) {
	var out bytes.Buffer
	out.Grow(len(b) / 2)
	w := flaters.Get().(*flate.Writer)
	w.Reset(&out)
	w.Write(b)
	w.Close()
	flaters.Put(w)
	if out.Len() > len(b)-len(b)/8 {
		return b, 0
	}
	return out.Bytes(), FlagFlate
}

// Expand turns stored bytes back into the block of length n, and checks it
// against its ID so corrupt or misplaced data never reaches a guest.
func Expand(stored []byte, flags uint8, n int, id ID) ([]byte, error) {
	out := stored
	if flags&FlagFlate != 0 {
		out = make([]byte, n)
		if _, err := io.ReadFull(flate.NewReader(bytes.NewReader(stored)), out); err != nil {
			return nil, fmt.Errorf("image: inflate %s: %w", id, err)
		}
	}
	if len(out) != n {
		return nil, fmt.Errorf("image: block %s is %d bytes, want %d", id, len(out), n)
	}
	if Sum(out) != id {
		return nil, fmt.Errorf("image: block %s fails its checksum", id)
	}
	return out, nil
}
