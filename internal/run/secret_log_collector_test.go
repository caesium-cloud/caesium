package run

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type secretLogErrorReader struct {
	data []byte
	err  error
}

func (r *secretLogErrorReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func (*secretLogErrorReader) Close() error { return nil }

func TestSecretLogCollectorReadErrorDropsHeldSecretPrefix(t *testing.T) {
	collector := NewSecretLogCollector(nil, uuid.Nil, uuid.Nil, SecretLogFence{}, []string{"abcdef"}, 1024)
	logs := &secretLogErrorReader{data: []byte("safe-line\nabc"), err: errors.New("stream failed")}
	markers, err := CaptureSecretTaskLogs(logs, collector, 0, 0)
	require.ErrorContains(t, err, "stream failed")
	require.Nil(t, markers)
	snapshot := collector.Snapshot()
	require.Contains(t, snapshot.Text, "safe-")
	require.NotContains(t, snapshot.Text, "abc")
	require.NoError(t, collector.Close(), "Close after Abort must not flush the held prefix")
	require.NotContains(t, collector.Snapshot().Text, "abc")
}

func TestSecretLogCollectorCoalescesWritesAndStopsAfterCap(t *testing.T) {
	collector := NewSecretLogCollector(nil, uuid.Nil, uuid.Nil, SecretLogFence{}, []string{"fake-canary-value"}, 128)
	now := time.Unix(1000, 0)
	collector.now = func() time.Time { return now }
	writes := 0
	collector.persistSnapshot = func(*TaskLogSnapshot) error {
		writes++
		return nil
	}

	_, err := collector.Write([]byte(strings.Repeat("a", 40)))
	require.NoError(t, err)
	require.Equal(t, 1, writes, "the first safe delta should be visible immediately")
	for range 50 {
		_, err = collector.Write([]byte("b"))
		require.NoError(t, err)
	}
	require.Equal(t, 1, writes, "bursty chunks should be coalesced")

	now = now.Add(secretLogSnapshotInterval)
	_, err = collector.Write([]byte(strings.Repeat("c", 200)))
	require.NoError(t, err)
	require.Equal(t, 2, writes)
	snapshot := collector.Snapshot()
	require.True(t, snapshot.Truncated)
	require.Contains(t, snapshot.Text, "[caesium: log truncated]")

	for range 100 {
		now = now.Add(secretLogSnapshotInterval)
		_, err = collector.Write([]byte("ignored-after-cap"))
		require.NoError(t, err)
	}
	require.Equal(t, 2, writes, "an unchanged capped snapshot must not rewrite the database")
	require.NoError(t, collector.Close())
	require.Equal(t, 2, writes, "forced close still deduplicates an identical final snapshot")
}

func TestSecretLogCollectorAbortOnCancellationCannotFlushPrefix(t *testing.T) {
	collector := NewSecretLogCollector(nil, uuid.Nil, uuid.Nil, SecretLogFence{}, []string{"abcdef"}, 1024)
	_, err := io.Copy(collector, strings.NewReader("before\nabc"))
	require.NoError(t, err)
	collector.Abort()
	require.NoError(t, collector.Close())
	require.NotContains(t, collector.Snapshot().Text, "abc")
}

func TestSecretLogCollectorPreservesLogsWhenResolvedValueIsEmpty(t *testing.T) {
	collector := NewSecretLogCollector(nil, uuid.Nil, uuid.Nil, SecretLogFence{}, nil, 1024)
	markers, err := CaptureSecretTaskLogs(io.NopCloser(strings.NewReader("ordinary output\n")), collector, 0, 0)
	require.NoError(t, err)
	require.Equal(t, "ordinary output\n", markers.LogText,
		"a resolved secret reference with an empty value still uses the safe collector without dropping logs")
}

func TestSecretLogCollectorThrottlesFailedSnapshotWritesAndRetriesFinal(t *testing.T) {
	collector := NewSecretLogCollector(nil, uuid.Nil, uuid.Nil, SecretLogFence{}, []string{"canary"}, 1024)
	now := time.Unix(1000, 0)
	collector.now = func() time.Time { return now }
	writes := 0
	collector.persistSnapshot = func(*TaskLogSnapshot) error {
		writes++
		return errors.New("database unavailable")
	}

	_, err := collector.Write([]byte("first "))
	require.NoError(t, err)
	require.Equal(t, 1, writes)
	for range 50 {
		_, err = collector.Write([]byte("more "))
		require.NoError(t, err)
	}
	require.Equal(t, 1, writes, "a failed update must still start the coalescing interval")

	now = now.Add(secretLogSnapshotInterval)
	_, err = collector.Write([]byte("later"))
	require.NoError(t, err)
	require.Equal(t, 2, writes)

	require.ErrorContains(t, collector.Close(), "database unavailable")
	require.Equal(t, 3, writes, "the final close must retry despite the failed-write throttle")
}
