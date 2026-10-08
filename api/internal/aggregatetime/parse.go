// Package aggregatetime parses timestamps returned by database aggregates.
package aggregatetime

import "time"

// Parse accepts the timestamp layouts emitted by supported databases and
// returns UTC. Callers retain their own whitespace and malformed-value policy.
func Parse(raw string) (*time.Time, bool) {
	layouts := [...]string{
		time.RFC3339Nano,
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05.999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, raw); err == nil {
			utc := parsed.UTC()
			return &utc, true
		}
		if parsed, err := time.ParseInLocation(layout, raw, time.UTC); err == nil {
			utc := parsed.UTC()
			return &utc, true
		}
	}
	return nil, false
}
