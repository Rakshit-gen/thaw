package objstore

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 talks to S3 or anything that speaks its API (MinIO, R2, Ceph RGW) with
// path-style requests signed with SigV4. It covers the five calls thaw uses
// and nothing else.
type S3 struct {
	endpoint *url.URL
	bucket   string
	region   string
	access   string
	secret   string
	client   *http.Client
}

// S3Config names a bucket and the credentials to reach it.
type S3Config struct {
	Endpoint  string // e.g. http://127.0.0.1:9000
	Bucket    string
	Region    string // us-east-1 if empty
	AccessKey string
	SecretKey string
	// MaxConns caps idle connections kept per host. Reusing connections
	// avoids a TCP and TLS handshake per request, which matters when most
	// requests are small ranged reads.
	MaxConns int
}

// NewS3 returns a client for one bucket. It does not create the bucket.
func NewS3(c S3Config) (*S3, error) {
	u, err := url.Parse(c.Endpoint)
	if err != nil {
		return nil, err
	}
	if c.Region == "" {
		c.Region = "us-east-1"
	}
	if c.MaxConns == 0 {
		c.MaxConns = 64
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:        c.MaxConns,
		MaxIdleConnsPerHost: c.MaxConns,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  true,
	}
	return &S3{
		endpoint: u, bucket: c.Bucket, region: c.Region,
		access: c.AccessKey, secret: c.SecretKey,
		client: &http.Client{Transport: tr, Timeout: 60 * time.Second},
	}, nil
}

// CreateBucket makes the bucket if it doesn't exist yet.
func (s *S3) CreateBucket(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodPut, "", nil, nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 200 || bytes.Contains(body, []byte("BucketAlreadyOwnedByYou")) {
		return nil
	}
	return fmt.Errorf("s3: create bucket: %s: %s", resp.Status, firstLine(body))
}

func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	resp, err := s.do(ctx, http.MethodPut, key, nil, nil, data)
	if err != nil {
		return err
	}
	return drain(resp, 200)
}

func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		io.Copy(io.Discard, resp.Body)
		return nil, ErrNotFound
	}
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("s3: get %s: %s: %s", key, resp.Status, firstLine(b))
	}
	return io.ReadAll(resp.Body)
}

func (s *S3) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	h := http.Header{"Range": {fmt.Sprintf("bytes=%d-%d", off, off+n-1)}}
	resp, err := s.do(ctx, http.MethodGet, key, nil, h, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		io.Copy(io.Discard, resp.Body)
		return nil, ErrNotFound
	}
	if resp.StatusCode != 206 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("s3: get %s range %d+%d: %s: %s", key, off, n, resp.Status, firstLine(b))
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return nil, fmt.Errorf("s3: get %s range %d+%d: %w", key, off, n, err)
	}
	return buf, nil
}

type listResult struct {
	Contents []struct {
		Key          string
		Size         int64
		LastModified time.Time
	}
	IsTruncated           bool
	NextContinuationToken string
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		resp, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("s3: list %q: %s: %s", prefix, resp.Status, firstLine(body))
		}
		var r listResult
		if err := xml.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("s3: list %q: %w", prefix, err)
		}
		for _, c := range r.Contents {
			out = append(out, Object{Key: c.Key, Size: c.Size, Modified: c.LastModified})
		}
		if !r.IsTruncated {
			return out, nil
		}
		token = r.NextContinuationToken
	}
}

func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	return drain(resp, 204, 200, 404)
}

func (s *S3) do(ctx context.Context, method, key string, q url.Values, h http.Header, body []byte) (*http.Response, error) {
	u := *s.endpoint
	u.Path = "/" + s.bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.ContentLength = int64(len(body))
	s.sign(req, body, time.Now().UTC())
	return s.client.Do(req)
}

// sign adds a SigV4 Authorization header. See
// https://docs.aws.amazon.com/IAM/latest/UserGuide/create-signed-request.html
func (s *S3) sign(req *http.Request, body []byte, now time.Time) {
	amzDate := now.Format("20060102T150405Z")
	day := now.Format("20060102")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("x-amz-content-sha256", payload)
	req.Header.Set("Host", req.URL.Host)

	names := make([]string, 0, len(req.Header))
	for k := range req.Header {
		names = append(names, strings.ToLower(k))
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, n := range names {
		v := req.Header.Get(n)
		if n == "host" {
			v = req.URL.Host
		}
		canonHeaders.WriteString(n + ":" + strings.TrimSpace(v) + "\n")
	}
	signed := strings.Join(names, ";")
	canon := strings.Join([]string{
		req.Method,
		uriEncode(req.URL.Path, false),
		req.URL.RawQuery,
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")
	scope := day + "/" + s.region + "/s3/aws4_request"
	ch := sha256.Sum256([]byte(canon))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(ch[:])
	k := hmacSHA([]byte("AWS4"+s.secret), day)
	k = hmacSHA(k, s.region)
	k = hmacSHA(k, "s3")
	k = hmacSHA(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+s.access+"/"+scope+
		", SignedHeaders="+signed+", Signature="+sig)
}

func hmacSHA(key []byte, msg string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

// canonicalQuery sorts and encodes query parameters the way SigV4 wants.
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		for _, v := range q[k] {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is SigV4's encoding: unreserved characters stay, everything else
// is %XX, and '/' is kept in paths.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func drain(resp *http.Response, ok ...int) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, c := range ok {
		if resp.StatusCode == c {
			return nil
		}
	}
	return fmt.Errorf("s3: %s %s: %s: %s", resp.Request.Method, resp.Request.URL.Path, resp.Status, firstLine(body))
}

func firstLine(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		b = b[:i]
	}
	if len(b) > 200 {
		b = b[:200]
	}
	return string(b)
}
