// Package objstore is the narrow slice of an object store that thaw needs:
// whole-object puts, whole and ranged gets, listing by prefix and deletes.
// S3 and MinIO speak it over HTTP; a directory speaks it for tests and for
// running on one machine.
package objstore

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("objstore: not found")

// Object is one entry from a listing.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Store is an object store. Implementations must be safe for concurrent use
// and give read-after-write consistency for puts, deletes and listings, which
// S3 has guaranteed since December 2020. GC depends on it.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	// GetRange reads n bytes starting at off. Reading past the end is an error.
	GetRange(ctx context.Context, key string, off, n int64) ([]byte, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
}
