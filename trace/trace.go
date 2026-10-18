// Package trace records what a restore touched and when: the first time
// each block of guest memory or disk was needed, measured from the moment
// the restore started.
//
// Traces are what thaw learns from. A restore that prefetches still has to
// leave an exact trace, or the next plan would learn from its own guesses;
// see restore.Session for how staged blocks keep traces honest.
package trace

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// Kind says how a touched block was served.
type Kind uint8

const (
	Demand Kind = iota // fetched while the guest waited
	Staged             // already fetched by prefetch, served from memory
	Cached             // in the host cache
	Zero               // all zeros, nothing fetched
)

func (k Kind) String() string {
	switch k {
	case Demand:
		return "demand"
	case Staged:
		return "staged"
	case Cached:
		return "cached"
	case Zero:
		return "zero"
	}
	return fmt.Sprintf("kind(%d)", k)
}

// Event is the first touch of one block.
type Event struct {
	At    time.Duration // since the restore began
	File  uint8         // index into Trace.Files
	Block uint32
	Kind  Kind
	Wait  time.Duration // how long the guest waited for it
}

// Trace is one restore's record.
type Trace struct {
	Snapshot string            `json:"snapshot"`
	Task     string            `json:"task,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Files    []string          `json:"files"`  // e.g. ["mem", "disk"]
	Blocks   []int             `json:"blocks"` // block size per file
	Started  time.Time         `json:"started"`
	Marks    map[string]int64  `json:"marks,omitempty"` // named moments, ns since start
	Meta     map[string]string `json:"meta,omitempty"`
	Events   []Event           `json:"-"`
}

// Recorder collects first touches during a restore. Safe for concurrent use.
type Recorder struct {
	mu    sync.Mutex
	t     Trace
	start time.Time
	seen  []map[uint32]bool
}

// NewRecorder starts recording for a snapshot's files.
func NewRecorder(snapshot string, files []string, blocks []int) *Recorder {
	r := &Recorder{start: time.Now()}
	r.t = Trace{Snapshot: snapshot, Files: files, Blocks: blocks, Started: r.start.UTC(), Marks: map[string]int64{}, Meta: map[string]string{}}
	for range files {
		r.seen = append(r.seen, map[uint32]bool{})
	}
	return r
}

// Start is when the restore began.
func (r *Recorder) Start() time.Time { return r.start }

// Elapsed is the time since the restore began.
func (r *Recorder) Elapsed() time.Duration { return time.Since(r.start) }

// Touch records a block's first touch; later touches are ignored. at is
// when the guest asked for the block (Elapsed at the time) and wait is how
// long it then waited. It reports whether this was the first touch.
func (r *Recorder) Touch(file int, block uint32, kind Kind, at, wait time.Duration) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen[file][block] {
		return false
	}
	r.seen[file][block] = true
	r.t.Events = append(r.t.Events, Event{At: at, File: uint8(file), Block: block, Kind: kind, Wait: wait})
	return true
}

// Mark names the current moment, e.g. "resumed" or "answered".
func (r *Recorder) Mark(name string) time.Duration {
	at := time.Since(r.start)
	r.mu.Lock()
	r.t.Marks[name] = int64(at)
	r.mu.Unlock()
	return at
}

// Set records a piece of metadata.
func (r *Recorder) Set(k, v string) {
	r.mu.Lock()
	r.t.Meta[k] = v
	r.mu.Unlock()
}

// Trace returns a copy of what was recorded so far, events in time order.
func (r *Recorder) Trace() Trace {
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.t
	t.Events = append([]Event(nil), r.t.Events...)
	t.Marks = map[string]int64{}
	for k, v := range r.t.Marks {
		t.Marks[k] = v
	}
	t.Meta = map[string]string{}
	for k, v := range r.t.Meta {
		t.Meta[k] = v
	}
	sort.SliceStable(t.Events, func(i, j int) bool { return t.Events[i].At < t.Events[j].At })
	return t
}

// Mark returns a named moment of a finished trace.
func (t *Trace) Mark(name string) (time.Duration, bool) {
	v, ok := t.Marks[name]
	return time.Duration(v), ok
}

const magic = "THAWTRC1"

// Encode writes the trace: a JSON header then fixed-size events.
func (t *Trace) Encode() []byte {
	var b bytes.Buffer
	b.WriteString(magic)
	head, _ := json.Marshal(t)
	binary.Write(&b, binary.LittleEndian, uint32(len(head)))
	b.Write(head)
	binary.Write(&b, binary.LittleEndian, uint32(len(t.Events)))
	var e [22]byte
	for _, ev := range t.Events {
		binary.LittleEndian.PutUint64(e[0:], uint64(ev.At))
		e[8] = ev.File
		binary.LittleEndian.PutUint32(e[9:], ev.Block)
		e[13] = byte(ev.Kind)
		binary.LittleEndian.PutUint64(e[14:], uint64(ev.Wait))
		b.Write(e[:])
	}
	return b.Bytes()
}

// Decode reads what Encode wrote.
func Decode(data []byte) (*Trace, error) {
	r := bytes.NewReader(data)
	head := make([]byte, len(magic))
	if _, err := io.ReadFull(r, head); err != nil || string(head) != magic {
		return nil, errors.New("trace: not a trace")
	}
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil || int(n) > r.Len() {
		return nil, errors.New("trace: short header")
	}
	js := make([]byte, n)
	io.ReadFull(r, js)
	var t Trace
	if err := json.Unmarshal(js, &t); err != nil {
		return nil, fmt.Errorf("trace: header: %w", err)
	}
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
		return nil, errors.New("trace: short")
	}
	if r.Len() != int(n)*22 {
		return nil, errors.New("trace: wrong length")
	}
	t.Events = make([]Event, n)
	var e [22]byte
	for i := range t.Events {
		io.ReadFull(r, e[:])
		t.Events[i] = Event{
			At:    time.Duration(binary.LittleEndian.Uint64(e[0:])),
			File:  e[8],
			Block: binary.LittleEndian.Uint32(e[9:]),
			Kind:  Kind(e[13]),
			Wait:  time.Duration(binary.LittleEndian.Uint64(e[14:])),
		}
		if int(t.Events[i].File) >= len(t.Files) {
			return nil, fmt.Errorf("trace: event names file %d of %d", t.Events[i].File, len(t.Files))
		}
	}
	return &t, nil
}

// Name is a storage name for a trace that sorts by time.
func (t *Trace) Name() string {
	return t.Started.Format("20060102-150405.000000")
}
