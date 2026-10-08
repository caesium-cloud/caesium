package aggregatetime

import (
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want time.Time
	}{
		{"RFC3339 offset", "2025-01-02T03:04:05.123456789+02:00", time.Date(2025, 1, 2, 1, 4, 5, 123456789, time.UTC)},
		{"SQL offset", "2025-01-02 03:04:05.123456789-07:00", time.Date(2025, 1, 2, 10, 4, 5, 123456789, time.UTC)},
		{"SQL nanoseconds", "2025-01-02 03:04:05.123456789", time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC)},
		{"SQL microseconds", "2025-01-02 03:04:05.123456", time.Date(2025, 1, 2, 3, 4, 5, 123456000, time.UTC)},
		{"SQL milliseconds", "2025-01-02 03:04:05.123", time.Date(2025, 1, 2, 3, 4, 5, 123000000, time.UTC)},
		{"SQL seconds", "2025-01-02 03:04:05", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"RFC3339 UTC", "2025-01-02T03:04:05Z", time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)},
		{"empty", "", time.Time{}},
		{"blank", " \t\n", time.Time{}},
		{"padded timestamp", " 2025-01-02T03:04:05Z ", time.Time{}},
		{"malformed", "not-a-timestamp", time.Time{}},
		{"date only", "2025-01-02", time.Time{}},
		{"named timezone", "2025-01-02 03:04:05 UTC", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Parse(tc.raw)
			if tc.want.IsZero() {
				if ok || got != nil {
					t.Fatalf("Parse(%q) = %v, %v; want nil, false", tc.raw, got, ok)
				}
				return
			}
			if !ok || got == nil || !got.Equal(tc.want) || got.Location() != time.UTC {
				t.Fatalf("Parse(%q) = %v, %v; want %v in UTC", tc.raw, got, ok, tc.want)
			}
		})
	}
}
