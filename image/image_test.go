package image

import (
	"bytes"
	"math/rand/v2"
	"testing"
)

func TestManifestRoundTrip(t *testing.T) {
	m := &Manifest{Size: 3*65536 + 100, Block: 65536, Packs: []string{"aa", "bb"}}
	m.Refs = []Ref{
		{ID: Sum([]byte("x")), Pack: 1, Off: 10, Len: 20, Flags: FlagFlate},
		{Flags: FlagZero},
		{ID: Sum([]byte("y")), Pack: 0, Off: 0, Len: 65536},
		{ID: Sum([]byte("z")), Pack: 1, Off: 30, Len: 100},
	}
	got, err := Decode(m.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != m.Size || got.Block != m.Block || len(got.Refs) != 4 || got.Refs[0] != m.Refs[0] || got.Packs[1] != "bb" {
		t.Fatalf("round trip lost something: %+v", got)
	}
	if got.BlockLen(3) != 100 || got.BlockLen(0) != 65536 {
		t.Fatalf("block lengths %d %d", got.BlockLen(3), got.BlockLen(0))
	}
	if n, b := got.Stored(); n != 3 || b != 2*65536+100 {
		t.Fatalf("stored %d %d", n, b)
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	m := &Manifest{Size: 65536, Block: 65536, Packs: nil, Refs: []Ref{{ID: Sum([]byte("x")), Pack: 3}}}
	if _, err := Decode(m.Encode()); err == nil {
		t.Fatal("a ref to a pack that isn't listed should be refused")
	}
	if _, err := Decode([]byte("not a manifest")); err == nil {
		t.Fatal("garbage should be refused")
	}
}

func TestCompressRoundTrip(t *testing.T) {
	text := bytes.Repeat([]byte("the guest page cache is full of text "), 2000)[:65536]
	noise := make([]byte, 65536)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range noise {
		noise[i] = byte(r.Uint32())
	}
	for _, b := range [][]byte{text, noise} {
		stored, flags := Compress(b)
		if bytes.Equal(b, noise) && flags != 0 {
			t.Fatal("random bytes should be stored raw")
		}
		if bytes.Equal(b, text) && (flags&FlagFlate == 0 || len(stored) > len(b)/4) {
			t.Fatalf("text should compress well, got %d bytes flags %d", len(stored), flags)
		}
		out, err := Expand(stored, flags, len(b), Sum(b))
		if err != nil || !bytes.Equal(out, b) {
			t.Fatalf("expand: %v", err)
		}
	}
	stored, flags := Compress(text)
	if _, err := Expand(stored, flags, len(text), Sum(noise)); err == nil {
		t.Fatal("a block under the wrong ID must fail its checksum")
	}
}

func TestIsZero(t *testing.T) {
	b := make([]byte, 3<<20)
	if !IsZero(b) {
		t.Fatal("zeros")
	}
	b[2<<20+5] = 1
	if IsZero(b) {
		t.Fatal("not zeros")
	}
}
