package metricsutil

import (
	"fmt"
	"strings"
)

// MatchingCounterSamples selects raw sample values in encounter order using
// the existing lightweight metric-prefix and label-substring grammar.
func MatchingCounterSamples(text, name string, labels map[string]string) []string {
	var samples []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		matched := true
		for k, v := range labels {
			if !strings.Contains(series, fmt.Sprintf("%s=%q", k, v)) {
				matched = false
				break
			}
		}
		if matched {
			samples = append(samples, value)
		}
	}
	return samples
}
