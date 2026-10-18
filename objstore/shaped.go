package objstore

import (
	"context"
	"math"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// Profile describes how far away an object store is: how long a request
// waits before its first byte, how fast one stream moves, and how fast the
// host's network link is overall.
//
// The numbers are a model, not a measurement of AWS. AWS's performance
// guide says small requests (under 512 KB) have "median latencies often in
// the tens of milliseconds" on S3 Standard, and that S3 Express One Zone is
// built for "single-digit millisecond latencies".
type Profile struct {
	Name string
	// FirstByte is the median time to first byte. Samples are log-normal
	// around it with spread Sigma, which gives a long right tail.
	FirstByte time.Duration
	Sigma     float64
	// StreamBps caps one request's transfer rate, in bytes per second.
	StreamBps float64
	// LinkBps caps all requests together, in bytes per second.
	LinkBps float64
}

// Profiles used by the benchmarks.
var (
	// S3Standard: 30 ms median first byte with a tail out past 100 ms, 90 MB/s
	// per stream, 10 Gbit/s for the host.
	S3Standard = Profile{Name: "s3-standard", FirstByte: 30 * time.Millisecond, Sigma: 0.55, StreamBps: 90e6, LinkBps: 1.25e9}
	// S3Express: 4 ms median first byte, same bandwidth.
	S3Express = Profile{Name: "s3-express", FirstByte: 4 * time.Millisecond, Sigma: 0.4, StreamBps: 90e6, LinkBps: 1.25e9}
	// Peer: another host's cache in the same zone.
	Peer = Profile{Name: "peer", FirstByte: 300 * time.Microsecond, Sigma: 0.3, StreamBps: 600e6, LinkBps: 1.25e9}
)

// ProfileByName returns one of the named profiles, or false.
func ProfileByName(name string) (Profile, bool) {
	for _, p := range []Profile{S3Standard, S3Express, Peer} {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}

// Link models the network path to a store: a log-normal wait for the
// first byte, a per-stream rate cap, and one shared link that all requests
// queue on. It only computes and sleeps; it moves no data.
type Link struct {
	p Profile

	mu       sync.Mutex
	rng      *rand.Rand
	linkFree time.Time // when the shared link finishes what's queued on it
}

// NewLink makes a link. seed makes the latency samples repeatable.
func NewLink(p Profile, seed uint64) *Link {
	return &Link{p: p, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Profile is the link's profile.
func (l *Link) Profile() Profile { return l.p }

func (l *Link) firstByte() time.Duration {
	l.mu.Lock()
	z := l.rng.NormFloat64()
	l.mu.Unlock()
	return time.Duration(float64(l.p.FirstByte) * math.Exp(l.p.Sigma*z))
}

// Wait sleeps until a request that started at start and moves n bytes
// would have finished.
func (l *Link) Wait(ctx context.Context, start time.Time, n int64) error {
	ready := start.Add(l.firstByte())
	streamDone := ready.Add(time.Duration(float64(n) / l.p.StreamBps * 1e9))
	l.mu.Lock()
	linkStart := ready
	if l.linkFree.After(linkStart) {
		linkStart = l.linkFree
	}
	linkDone := linkStart.Add(time.Duration(float64(n) / l.p.LinkBps * 1e9))
	l.linkFree = linkDone
	l.mu.Unlock()
	done := streamDone
	if linkDone.After(done) {
		done = linkDone
	}
	t := time.NewTimer(time.Until(done))
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Shaped delays every request on an underlying store as if the store were as
// far away as its profile says. Data still comes from the underlying store;
// only the timing is modelled. Puts are delayed the same way as gets.
type Shaped struct {
	Store
	link *Link
}

// NewShaped wraps s. seed makes the latency samples repeatable.
func NewShaped(s Store, p Profile, seed uint64) *Shaped {
	return &Shaped{Store: s, link: NewLink(p, seed)}
}

func (s *Shaped) wait(ctx context.Context, start time.Time, n int64) error {
	return s.link.Wait(ctx, start, n)
}

func (s *Shaped) Put(ctx context.Context, key string, data []byte) error {
	start := time.Now()
	if err := s.Store.Put(ctx, key, data); err != nil {
		return err
	}
	return s.wait(ctx, start, int64(len(data)))
}

func (s *Shaped) Get(ctx context.Context, key string) ([]byte, error) {
	start := time.Now()
	b, err := s.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	return b, s.wait(ctx, start, int64(len(b)))
}

func (s *Shaped) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	start := time.Now()
	b, err := s.Store.GetRange(ctx, key, off, n)
	if err != nil {
		return nil, err
	}
	return b, s.wait(ctx, start, n)
}

func (s *Shaped) List(ctx context.Context, prefix string) ([]Object, error) {
	start := time.Now()
	o, err := s.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	return o, s.wait(ctx, start, 0)
}

// Counts are what a store was asked to do, for working out request costs.
type Counts struct {
	Puts, Gets, Lists, Deletes int64
	BytesIn, BytesOut          int64 // in: uploaded, out: downloaded
}

// Counting wraps a store and counts its requests.
type Counting struct {
	Store
	puts, gets, lists, deletes atomic.Int64
	in, out                    atomic.Int64
}

// NewCounting wraps s.
func NewCounting(s Store) *Counting { return &Counting{Store: s} }

func (c *Counting) Put(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	c.in.Add(int64(len(data)))
	return c.Store.Put(ctx, key, data)
}

func (c *Counting) Get(ctx context.Context, key string) ([]byte, error) {
	c.gets.Add(1)
	b, err := c.Store.Get(ctx, key)
	c.out.Add(int64(len(b)))
	return b, err
}

func (c *Counting) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	c.gets.Add(1)
	b, err := c.Store.GetRange(ctx, key, off, n)
	c.out.Add(int64(len(b)))
	return b, err
}

func (c *Counting) List(ctx context.Context, prefix string) ([]Object, error) {
	c.lists.Add(1)
	return c.Store.List(ctx, prefix)
}

func (c *Counting) Delete(ctx context.Context, key string) error {
	c.deletes.Add(1)
	return c.Store.Delete(ctx, key)
}

// Counts returns what has been counted so far.
func (c *Counting) Counts() Counts {
	return Counts{
		Puts: c.puts.Load(), Gets: c.gets.Load(), Lists: c.lists.Load(), Deletes: c.deletes.Load(),
		BytesIn: c.in.Load(), BytesOut: c.out.Load(),
	}
}

// Sub returns c minus an earlier reading.
func (c Counts) Sub(o Counts) Counts {
	return Counts{
		Puts: c.Puts - o.Puts, Gets: c.Gets - o.Gets, Lists: c.Lists - o.Lists, Deletes: c.Deletes - o.Deletes,
		BytesIn: c.BytesIn - o.BytesIn, BytesOut: c.BytesOut - o.BytesOut,
	}
}

// Price is what a store charges, in US dollars.
type Price struct {
	PerThousandPut float64
	PerThousandGet float64
	PerGBMonth     float64
}

// S3StandardPrice is the us-east-1 list price for S3 Standard: $0.005 per
// 1000 PUT or LIST, $0.0004 per 1000 GET, $0.023 per GB-month for the first
// 50 TB. Transfer to EC2 in the same region is free.
var S3StandardPrice = Price{PerThousandPut: 0.005, PerThousandGet: 0.0004, PerGBMonth: 0.023}

// RequestCost is the request cost of c at price p.
func (c Counts) RequestCost(p Price) float64 {
	return float64(c.Puts+c.Lists)/1000*p.PerThousandPut + float64(c.Gets)/1000*p.PerThousandGet
}
