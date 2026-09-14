package run

import (
	"errors"
	"io"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

const secretLogSnapshotInterval = 100 * time.Millisecond

const (
	SecretLogDrainTimeout = 5 * time.Second
	SecretLogAbortTimeout = time.Second
)

// SecretLogCollector is an io.Writer placed beside the raw marker parser. Raw
// bytes reach the parser unchanged; only exact-value-scrubbed, bounded snapshots
// reach storage. The collector is owned by one execution attempt and discarded
// when that attempt ends.
type SecretLogCollector struct {
	mu                sync.Mutex
	persistMu         sync.Mutex
	store             *Store
	runID             uuid.UUID
	taskRef           uuid.UUID
	fence             SecretLogFence
	scrub             *incident.ExactValueStreamScrubber
	persistSnapshot   func(*TaskLogSnapshot) error
	now               func() time.Time
	lastText          string
	lastTruncated     bool
	lastVersion       uint64
	lastAttemptedAt   time.Time
	err               error
	aborted           bool
	closed            bool
	permanentError    bool
	nextSequence      uint64
	completedSequence uint64
}

type secretLogPersistRequest struct {
	sequence uint64
	version  uint64
	snapshot *TaskLogSnapshot
}

// SecretLogCaptureResult reports both marker parsing and sanitized snapshot
// persistence from one live task log stream.
type SecretLogCaptureResult struct {
	Markers    *pkgtask.Markers
	Err        error
	PersistErr error
}

func StartSecretTaskLogCapture(logs io.ReadCloser, collector *SecretLogCollector, maxRefBytes int64, maxPartitions int) <-chan SecretLogCaptureResult {
	results := make(chan SecretLogCaptureResult, 1)
	go func() {
		markers, err := CaptureSecretTaskLogs(logs, collector, maxRefBytes, maxPartitions)
		results <- SecretLogCaptureResult{Markers: markers, Err: err, PersistErr: collector.Err()}
	}()
	return results
}

// DrainSecretTaskLogCapture bounds shutdown of a live log stream. A timeout is
// a task failure because structured output or partition markers may remain in
// unread bytes even when the container itself exited successfully.
func DrainSecretTaskLogCapture(results <-chan SecretLogCaptureResult, collector *SecretLogCollector, logs io.ReadCloser, drainTimeout, abortTimeout time.Duration) (SecretLogCaptureResult, bool) {
	select {
	case result := <-results:
		return result, false
	case <-time.After(drainTimeout):
		collector.Abort()
		_ = logs.Close()
		select {
		case result := <-results:
			return result, true
		case <-time.After(abortTimeout):
			return SecretLogCaptureResult{Err: errors.New("timed out draining scrubbed task log")}, true
		}
	}
}

// CaptureSecretTaskLogs parses the original stream while the tee persists only
// scrubbed text. A zero snapshot limit on CaptureMarkers is deliberate: keeping
// a second raw snapshot would reintroduce the value and could truncate through
// it before redaction.
func CaptureSecretTaskLogs(logs io.ReadCloser, collector *SecretLogCollector, maxRefBytes int64, maxPartitions int) (*pkgtask.Markers, error) {
	defer func() { _ = logs.Close() }()
	markers, err := pkgtask.CaptureMarkersWithLimits(io.TeeReader(logs, collector), 0, maxRefBytes, maxPartitions)
	if err != nil {
		collector.Abort()
	} else {
		_ = collector.Close()
	}
	if markers != nil {
		snapshot := collector.Snapshot()
		markers.LogText = snapshot.Text
		markers.LogTruncated = snapshot.Truncated
	}
	return markers, err
}

func NewSecretLogCollector(store *Store, runID, taskRef uuid.UUID, fence SecretLogFence, values []string, limit int) *SecretLogCollector {
	c := &SecretLogCollector{
		store: store, runID: runID, taskRef: taskRef, fence: fence,
		scrub: incident.NewExactValueStreamScrubber(values, limit), now: time.Now,
	}
	if store != nil {
		c.persistSnapshot = func(snapshot *TaskLogSnapshot) error {
			return store.SaveSecretTaskLogSnapshot(runID, taskRef, fence, snapshot)
		}
	}
	return c
}

func (c *SecretLogCollector) Write(p []byte) (int, error) {
	c.mu.Lock()
	n, _ := c.scrub.Write(p)
	req := c.preparePersistLocked(false)
	c.mu.Unlock()
	c.persist(req)
	// Snapshot persistence is observability, not marker transport. Preserve the
	// parser's raw stream even if a transient database write failed.
	return n, nil
}

func (c *SecretLogCollector) Close() error {
	c.mu.Lock()
	if !c.aborted && !c.closed {
		_ = c.scrub.Close()
	}
	c.closed = true
	req := c.preparePersistLocked(true)
	c.mu.Unlock()
	c.persist(req)
	return c.Err()
}

// Abort prevents an incomplete held-back suffix from ever being flushed. It is
// safe to call concurrently with the capture goroutine and idempotent.
func (c *SecretLogCollector) Abort() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.aborted {
		return
	}
	c.aborted = true
	c.scrub.Abort()
}

func (c *SecretLogCollector) Snapshot() *TaskLogSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	text, truncated := c.scrub.Snapshot()
	return &TaskLogSnapshot{Text: text, Truncated: truncated}
}

func (c *SecretLogCollector) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *SecretLogCollector) preparePersistLocked(force bool) *secretLogPersistRequest {
	if c.persistSnapshot == nil || c.permanentError || c.aborted {
		return nil
	}
	version := c.scrub.Version()
	if version == c.lastVersion {
		return nil
	}
	now := c.now()
	if !force && !c.lastAttemptedAt.IsZero() && now.Sub(c.lastAttemptedAt) < secretLogSnapshotInterval {
		return nil
	}
	text, truncated := c.scrub.Snapshot()
	if text == "" && !truncated {
		return nil
	}
	if text == c.lastText && truncated == c.lastTruncated {
		return nil
	}
	c.lastAttemptedAt = now
	c.nextSequence++
	return &secretLogPersistRequest{
		sequence: c.nextSequence,
		version:  version,
		snapshot: &TaskLogSnapshot{Text: text, Truncated: truncated},
	}
}

func (c *SecretLogCollector) persist(req *secretLogPersistRequest) {
	if req == nil {
		return
	}
	// Serialize DB writes separately from scrubber state. Abort can always take
	// c.mu immediately, while sequence checks keep a delayed older request from
	// replacing a newer snapshot within the same execution generation.
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	if c.aborted || c.permanentError || req.sequence <= c.completedSequence {
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()

	err := c.persistSnapshot(req.snapshot)
	c.mu.Lock()
	defer c.mu.Unlock()
	if req.sequence <= c.completedSequence {
		return
	}
	c.completedSequence = req.sequence
	if err != nil {
		c.err = err
		if errors.Is(err, ErrTaskClaimMismatch) {
			c.permanentError = true
		}
		return
	}
	c.err = nil
	c.lastText = req.snapshot.Text
	c.lastTruncated = req.snapshot.Truncated
	c.lastVersion = req.version
}
