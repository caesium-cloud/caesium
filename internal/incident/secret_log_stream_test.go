package incident

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExactValueStreamScrubberHandlesChunkBoundariesAndAllKnownValues(t *testing.T) {
	s := NewExactValueStreamScrubber([]string{"abc", "42", "secret", "abc", ""}, 1024)
	for _, chunk := range []string{"normal ab", "c numeric=4", "2 word=sec", "ret again=abc\n"} {
		_, err := s.Write([]byte(chunk))
		require.NoError(t, err)
		text, _ := s.Snapshot()
		require.NotContains(t, text, "abc")
		require.NotContains(t, text, "42")
		require.NotContains(t, text, "secret")
	}
	require.NoError(t, s.Close())
	text, truncated := s.Snapshot()
	require.False(t, truncated)
	require.Equal(t, "normal [REDACTED] numeric=[REDACTED] word=[REDACTED] again=[REDACTED]\n", text)
}

func TestExactValueStreamScrubberScrubsBeforeSnapshotLimit(t *testing.T) {
	canary := "fake-canary-value"
	s := NewExactValueStreamScrubber([]string{canary}, 64)
	_, err := s.Write([]byte(strings.Repeat("x", 35) + canary))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	text, truncated := s.Snapshot()
	require.True(t, truncated)
	require.NotContains(t, text, canary)
	require.Contains(t, text, TruncatedLogMarker)
	require.LessOrEqual(t, len(text), 64)
}

func TestExactValueStreamScrubberStopsScanningAfterSnapshotCap(t *testing.T) {
	values := make([]string, 0, 200)
	for i := range 200 {
		values = append(values, strings.Repeat("secret", i+1))
	}
	s := NewExactValueStreamScrubber(values, 64)
	_, err := s.Write([]byte(strings.Repeat("ordinary-output-", 20)))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	_, truncated := s.Snapshot()
	require.True(t, truncated)
	version := s.Version()
	_, err = s.Write([]byte(strings.Repeat("unbounded-tail-", 10_000)))
	require.NoError(t, err)
	require.Empty(t, s.pending)
	require.Equal(t, version, s.Version(), "discarded bytes after the cap cannot change public output")
}

func TestExactValueStreamScrubberAbortDropsPossibleSecretPrefix(t *testing.T) {
	s := NewExactValueStreamScrubber([]string{"abcdef"}, 1024)
	_, err := s.Write([]byte("safe\nabc"))
	require.NoError(t, err)
	s.Abort()
	require.NoError(t, s.Close())
	text, _ := s.Snapshot()
	require.Equal(t, "saf", text)
	require.NotContains(t, text, "abc")
}

func TestExactValueScrubberLeavesUnrelatedHighEntropyAndSecretURI(t *testing.T) {
	s := NewExactValueStreamScrubber([]string{"ci-user:ci-pass"}, 1024)
	logText := "ref=secret://env/CAESIUM_IT_REGISTRY_CREDS sha=AKIAJ83HFKD9SLXMZ7Q2b8Xy1pQ9rT4"
	_, err := s.Write([]byte(logText))
	require.NoError(t, err)
	require.NoError(t, s.Close())
	text, truncated := s.Snapshot()
	require.False(t, truncated)
	require.Equal(t, logText, text)
}

func TestExactValueStreamScrubberPrefersOverlappingValueAndDoesNotAccumulate(t *testing.T) {
	first := NewExactValueStreamScrubber([]string{"abc", "abcdef", "abcdef"}, 1024)
	_, err := first.Write([]byte("abcdef abc abcdef"))
	require.NoError(t, err)
	require.NoError(t, first.Close())
	text, _ := first.Snapshot()
	require.Equal(t, "[REDACTED] [REDACTED] [REDACTED]", text)

	rotated := NewExactValueStreamScrubber([]string{"new-value"}, 1024)
	_, err = rotated.Write([]byte("old=abcdef new=new-value"))
	require.NoError(t, err)
	require.NoError(t, rotated.Close())
	text, _ = rotated.Snapshot()
	require.Equal(t, "old=abcdef new=[REDACTED]", text,
		"a new task-local scrubber must not retain a prior attempt's value")
}

func TestExactValueStreamScrubberGeneratedTextCannotReintroduceKnownValues(t *testing.T) {
	t.Run("replacement collision", func(t *testing.T) {
		s := NewExactValueStreamScrubber([]string{"REDACTED"}, 1024)
		_, err := s.Write([]byte("value=REDACTED\n"))
		require.NoError(t, err)
		require.NoError(t, s.Close())
		text, truncated := s.Snapshot()
		require.False(t, truncated)
		require.Equal(t, "value=\n", text)
		require.NotContains(t, text, "REDACTED")
	})

	t.Run("truncation marker collision", func(t *testing.T) {
		s := NewExactValueStreamScrubber([]string{"caesium", "truncated"}, 64)
		_, err := s.Write([]byte(strings.Repeat("x", 100)))
		require.NoError(t, err)
		require.NoError(t, s.Close())
		text, truncated := s.Snapshot()
		require.True(t, truncated)
		require.NotContains(t, text, "caesium")
		require.NotContains(t, text, "truncated")
		require.Contains(t, text, "[: log ]",
			"the truthful annotation remains visible after known values are stripped")
		require.LessOrEqual(t, len(text), 64)
	})
}
