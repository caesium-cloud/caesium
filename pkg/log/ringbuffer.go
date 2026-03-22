package log

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap/zapcore"
)

// LogEntry is a single structured log record stored in the ring buffer.
type LogEntry struct {
	Sequence  uint64          `json:"sequence"`
	Timestamp time.Time       `json:"ts"`
	Level     string          `json:"level"`
	Message   string          `json:"msg"`
	Caller    string          `json:"caller,omitempty"`
	Fields    json.RawMessage `json:"fields,omitempty"`
}

// RingBuffer is a bounded, thread-safe circular buffer of log entries that
// also implements zapcore.Core so it can be tee'd alongside the primary
// stdout core.
type RingBuffer struct {
	mu          sync.RWMutex
	entries     []LogEntry
	size        int
	head        int // next write position
	count       int
	seq         atomic.Uint64
	subscribers map[chan LogEntry]struct{}
	level       zapcore.Level
}

// NewRingBuffer creates a ring buffer that stores up to size entries.
func NewRingBuffer(size int, level zapcore.Level) *RingBuffer {
	return &RingBuffer{
		entries:     make([]LogEntry, size),
		size:        size,
		subscribers: make(map[chan LogEntry]struct{}),
		level:       level,
	}
}

// Snapshot returns a copy of all buffered entries in chronological order.
func (rb *RingBuffer) Snapshot() []LogEntry {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	if rb.count == 0 {
		return nil
	}

	out := make([]LogEntry, rb.count)
	if rb.count < rb.size {
		copy(out, rb.entries[:rb.count])
	} else {
		// Buffer has wrapped — read from head (oldest) to end, then start to head.
		n := copy(out, rb.entries[rb.head:])
		copy(out[n:], rb.entries[:rb.head])
	}
	return out
}

// Subscribe returns a channel that receives live log entries. The channel is
// closed when ctx is cancelled. Sends are non-blocking; entries are dropped
// if the subscriber cannot keep up.
func (rb *RingBuffer) Subscribe(ctx context.Context) <-chan LogEntry {
	ch := make(chan LogEntry, 256)

	rb.mu.Lock()
	rb.subscribers[ch] = struct{}{}
	rb.mu.Unlock()

	go func() {
		<-ctx.Done()
		rb.mu.Lock()
		delete(rb.subscribers, ch)
		close(ch)
		rb.mu.Unlock()
	}()

	return ch
}

// --- zapcore.Core implementation ---

func (rb *RingBuffer) Enabled(lvl zapcore.Level) bool {
	return lvl >= rb.level
}

func (rb *RingBuffer) With(fields []zapcore.Field) zapcore.Core {
	// The ring buffer is global; With() is a no-op. Fields are captured per-entry in Write().
	return rb
}

func (rb *RingBuffer) Check(entry zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if rb.Enabled(entry.Level) {
		ce = ce.AddCore(entry, rb)
	}
	return ce
}

func (rb *RingBuffer) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	le := LogEntry{
		Sequence:  rb.seq.Add(1),
		Timestamp: entry.Time,
		Level:     entry.Level.String(),
		Message:   entry.Message,
		Caller:    entry.Caller.TrimmedPath(),
	}

	if len(fields) > 0 {
		enc := zapcore.NewMapObjectEncoder()
		for _, f := range fields {
			f.AddTo(enc)
		}
		if data, err := json.Marshal(enc.Fields); err == nil {
			le.Fields = data
		}
	}

	rb.mu.Lock()
	rb.entries[rb.head] = le
	rb.head = (rb.head + 1) % rb.size
	if rb.count < rb.size {
		rb.count++
	}
	// Fan-out to subscribers (non-blocking).
	for ch := range rb.subscribers {
		select {
		case ch <- le:
		default:
		}
	}
	rb.mu.Unlock()

	return nil
}

func (rb *RingBuffer) Sync() error {
	return nil
}
