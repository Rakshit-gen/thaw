package objstore

import (
	"fmt"
	"net/url"
	"strings"
)

// ParseS3URL reads http(s)://ACCESS:SECRET@host:port/bucket.
func ParseS3URL(raw string) (S3Config, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return S3Config{}, err
	}
	bucket := strings.Trim(u.Path, "/")
	if bucket == "" || strings.Contains(bucket, "/") {
		return S3Config{}, fmt.Errorf("objstore: %q: want one bucket name in the path", raw)
	}
	c := S3Config{Endpoint: u.Scheme + "://" + u.Host, Bucket: bucket}
	if u.User != nil {
		c.AccessKey = u.User.Username()
		c.SecretKey, _ = u.User.Password()
	}
	return c, nil
}

// Open returns a store for a location: a directory path, or an http(s) URL
// for S3 or MinIO in the form ParseS3URL reads. An S3 bucket is created if
// it doesn't exist.
func Open(loc string) (Store, error) {
	if !strings.HasPrefix(loc, "http://") && !strings.HasPrefix(loc, "https://") {
		return NewDir(loc)
	}
	cfg, err := ParseS3URL(loc)
	if err != nil {
		return nil, err
	}
	s, err := NewS3(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}
