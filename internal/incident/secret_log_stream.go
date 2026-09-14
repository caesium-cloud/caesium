package incident

import (
	"bytes"
	"sort"
)

// TruncatedLogMarker makes a live capped stream truthful even when its HTTP
// headers were committed before the cap was reached. Space for it is reserved
// inside the configured snapshot limit.
const TruncatedLogMarker = "\n[caesium: log truncated]\n"

// ExactValueStreamScrubber incrementally removes known resolved secret values
// while retaining only a bounded, safe snapshot. It holds back enough raw bytes
// to recognize a value split across arbitrary runtime chunks; raw bytes are
// never exposed by Snapshot.
type ExactValueStreamScrubber struct {
	values      [][]byte
	maxValue    int
	replacement []byte
	marker      []byte
	pending     []byte
	output      []byte
	limit       int
	truncated   bool
	version     uint64
}

func NewExactValueStreamScrubber(secretValues []string, limit int) *ExactValueStreamScrubber {
	values := normalizedSecretValues(secretValues, func(string) bool { return true })
	byteValues := make([][]byte, 0, len(values))
	maxValue := 0
	for _, value := range values {
		b := []byte(value)
		byteValues = append(byteValues, b)
		if len(b) > maxValue {
			maxValue = len(b)
		}
	}
	// normalizedSecretValues already sorts longest-first. Keep this local sort
	// explicit because prefix selection is security-sensitive.
	sort.SliceStable(byteValues, func(i, j int) bool { return len(byteValues[i]) > len(byteValues[j]) })

	replacement := []byte(Redacted)
	for _, value := range byteValues {
		if bytes.Contains(replacement, value) {
			replacement = nil
			break
		}
	}
	marker := []byte(TruncatedLogMarker)
	for {
		before := len(marker)
		for _, value := range byteValues {
			marker = bytes.ReplaceAll(marker, value, nil)
		}
		if len(marker) == before {
			break
		}
	}

	return &ExactValueStreamScrubber{
		values: byteValues, maxValue: maxValue, replacement: replacement, marker: marker, limit: limit,
	}
}

func (s *ExactValueStreamScrubber) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.pending = append(s.pending, p...)
	s.process(false)
	return len(p), nil
}

// Close flushes the final held-back bytes after the source stream reaches EOF.
func (s *ExactValueStreamScrubber) Close() error {
	s.process(true)
	return nil
}

// Abort discards bytes held only for cross-chunk matching. A read error or
// forced stream close leaves their continuation unknown, so emitting them
// could reveal a prefix of a secret.
func (s *ExactValueStreamScrubber) Abort() {
	s.pending = nil
}

func (s *ExactValueStreamScrubber) Snapshot() (string, bool) {
	if !s.truncated {
		return string(s.output), false
	}
	marker := string(s.marker)
	if s.limit > 0 && len(marker) > s.limit-len(s.output) {
		marker = marker[:max(s.limit-len(s.output), 0)]
	}
	return string(s.output) + marker, true
}

// Version changes only when the externally visible snapshot changes. Callers
// can cheaply deduplicate persistence after the output cap is reached.
func (s *ExactValueStreamScrubber) Version() uint64 { return s.version }

func (s *ExactValueStreamScrubber) process(final bool) {
	hold := s.maxValue - 1
	if hold < 0 {
		hold = 0
	}
	for len(s.pending) > 0 && (final || len(s.pending) > hold) {
		matched := false
		for _, value := range s.values {
			if len(s.pending) >= len(value) && bytes.HasPrefix(s.pending, value) {
				s.appendOutput(s.replacement)
				s.pending = s.pending[len(value):]
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		s.appendOutput(s.pending[:1])
		s.pending = s.pending[1:]
	}
}

func (s *ExactValueStreamScrubber) appendOutput(p []byte) {
	if s.limit <= 0 {
		s.output = append(s.output, p...)
		return
	}
	dataLimit := max(s.limit-len(s.marker), 0)
	remaining := dataLimit - len(s.output)
	if remaining <= 0 {
		if len(p) > 0 && !s.truncated {
			s.truncated = true
			s.version++
		}
		return
	}
	if len(p) > remaining {
		s.output = append(s.output, p[:remaining]...)
		s.truncated = true
		s.version++
		return
	}
	s.output = append(s.output, p...)
	if len(p) > 0 {
		s.version++
	}
}
