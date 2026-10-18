package objstore

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestSigV4Example checks the signer against the worked example in the S3
// API reference ("Example: GET Object", signature calculations for the
// Authorization header).
func TestSigV4Example(t *testing.T) {
	s := &S3{region: "us-east-1", access: "AKIAIOSFODNN7EXAMPLE", secret: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	s.sign(req, nil, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func exercise(t *testing.T, s Store) {
	ctx := context.Background()
	if _, err := s.Get(ctx, "nope/x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing key: got %v, want ErrNotFound", err)
	}
	data := []byte("0123456789abcdef")
	for _, k := range []string{"a/one", "a/two", "b/three"} {
		if err := s.Put(ctx, k, data); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, "a/one")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("get: %q %v", got, err)
	}
	got, err = s.GetRange(ctx, "a/two", 4, 6)
	if err != nil || string(got) != "456789" {
		t.Fatalf("range: %q %v", got, err)
	}
	if _, err := s.GetRange(ctx, "a/two", 10, 10); err == nil {
		t.Fatal("range past the end should fail")
	}
	list, err := s.List(ctx, "a/")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Key != "a/one" || list[1].Key != "a/two" || list[0].Size != 16 {
		t.Fatalf("list: %+v", list)
	}
	if err := s.Delete(ctx, "a/one"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "a/one"); err != nil {
		t.Fatalf("deleting twice should be fine: %v", err)
	}
	if _, err := s.Get(ctx, "a/one"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestDir(t *testing.T) {
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	exercise(t, d)
	if err := d.Put(context.Background(), "../escape", nil); err == nil {
		t.Fatal("a key with .. should be refused")
	}
}

// TestS3 runs against a real endpoint when THAW_S3 is set, e.g.
// THAW_S3=http://minioadmin:minioadmin@127.0.0.1:9000/thaw-test
func TestS3(t *testing.T) {
	u := os.Getenv("THAW_S3")
	if u == "" {
		t.Skip("THAW_S3 not set")
	}
	cfg, err := ParseS3URL(u)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewS3(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBucket(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, o := range must(s.List(context.Background(), "")) {
		s.Delete(context.Background(), o.Key)
	}
	exercise(t, s)
	// Keys with characters SigV4 has to encode.
	k := "odd keys/with spaces+plus=eq"
	if err := s.Put(context.Background(), k, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if b, err := s.Get(context.Background(), k); err != nil || string(b) != "x" {
		t.Fatalf("odd key: %q %v", b, err)
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func TestShapedAddsLatency(t *testing.T) {
	d, _ := NewDir(t.TempDir())
	ctx := context.Background()
	d.Put(ctx, "k", make([]byte, 9_000_000))
	p := Profile{FirstByte: 20 * time.Millisecond, Sigma: 0, StreamBps: 90e6, LinkBps: 1e12}
	s := NewShaped(d, p, 1)
	start := time.Now()
	if _, err := s.GetRange(ctx, "k", 0, 9_000_000); err != nil {
		t.Fatal(err)
	}
	// 20 ms to first byte plus 9 MB at 90 MB/s = 120 ms.
	if el := time.Since(start); el < 115*time.Millisecond || el > 200*time.Millisecond {
		t.Fatalf("took %v, want about 120ms", el)
	}
}

func TestShapedSharesTheLink(t *testing.T) {
	d, _ := NewDir(t.TempDir())
	ctx := context.Background()
	d.Put(ctx, "k", make([]byte, 1_000_000))
	// Each stream could do 1 MB in 10 ms, but the link only does 100 MB/s,
	// so eight at once take about 80 ms.
	p := Profile{FirstByte: time.Millisecond, Sigma: 0, StreamBps: 100e6 * 8, LinkBps: 100e6}
	s := NewShaped(d, p, 1)
	start := time.Now()
	done := make(chan struct{})
	for range 8 {
		go func() { s.GetRange(ctx, "k", 0, 1_000_000); done <- struct{}{} }()
	}
	for range 8 {
		<-done
	}
	if el := time.Since(start); el < 75*time.Millisecond || el > 160*time.Millisecond {
		t.Fatalf("took %v, want about 80ms", el)
	}
}

func TestParseS3URL(t *testing.T) {
	c, err := ParseS3URL("https://AK:SK%2Fx@s3.example.com:9000/bucket")
	if err != nil {
		t.Fatal(err)
	}
	if c.Endpoint != "https://s3.example.com:9000" || c.Bucket != "bucket" || c.AccessKey != "AK" || c.SecretKey != "SK/x" {
		t.Fatalf("%+v", c)
	}
	if _, err := ParseS3URL("http://127.0.0.1:9000/"); err == nil || !strings.Contains(err.Error(), "bucket") {
		t.Fatalf("missing bucket: %v", err)
	}
}
