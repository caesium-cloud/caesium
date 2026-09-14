package run

import (
	"io"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/incident"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

const secretLogSnapshotInterval = 100 * time.Millisecond

// SecretLogCollector is an io.Writer placed beside the raw marker parser. Raw
// bytes reach the parser unchanged; only exact-value-scrubbed, bounded snapshots
// reach storage. The collector is owned by one execution attempt and discarded
// when that attempt ends.
type SecretLogCollector struct {
	mu              sync.Mutex
	store           *Store
	runID           uuid.UUID
	taskRef         uuid.UUID
	fence           SecretLogFence
	scrub           *incident.ExactValueStreamScrubber
	persistSnapshot func(*TaskLogSnapshot) error
	now             func() time.Time
	lastText        string
	lastTruncated   bool
	lastVersion     uint64
	lastAttemptedAt time.Time
	err             error
	aborted         bool
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
	defer c.mu.Unlock()
	n, _ := c.scrub.Write(p)
	c.persist(false)
	// Snapshot persistence is observability, not marker transport. Preserve the
	// parser's raw stream even if a transient database write failed.
	return n, nil
}

func (c *SecretLogCollector) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.aborted {
		_ = c.scrub.Close()
	}
	c.persist(true)
	return c.err
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
	c.persist(true)
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

func (c *SecretLogCollector) persist(force bool) {
	if c.persistSnapshot == nil {
		return
	}
	version := c.scrub.Version()
	if version == c.lastVersion {
		return
	}
	now := c.now()
	if !force && !c.lastAttemptedAt.IsZero() && now.Sub(c.lastAttemptedAt) < secretLogSnapshotInterval {
		return
	}
	text, truncated := c.scrub.Snapshot()
	if text == "" && !truncated {
		return
	}
	if text == c.lastText && truncated == c.lastTruncated {
		return
	}
	c.lastAttemptedAt = now
	if err := c.persistSnapshot(&TaskLogSnapshot{
		Text: text, Truncated: truncated,
	}); err != nil {
		if c.err == nil {
			c.err = err
		}
		return
	}
	c.lastText = text
	c.lastTruncated = truncated
	c.lastVersion = version
}
