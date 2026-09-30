package cron

import (
	"encoding/json"
	"testing"
)

func FuzzExtractExpression(f *testing.F) {
	f.Add(`{"expression": "* * * * *"}`)
	f.Add(`{"cron": "0 9 * * 1-5"}`)
	f.Add(`{"schedule": "invalid"}`)
	f.Add(`{}`)
	f.Add(`{"expression": ""}`)
	f.Add(`not json at all`)
	f.Fuzz(func(t *testing.T, data string) {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			// Not valid JSON — skip, we only fuzz the expression extraction logic.
			return
		}

		expr, extractErr := extractExpression(cfg)

		// Determinism: a pure function over the same map must answer the same
		// way every time.
		expr2, extractErr2 := extractExpression(cfg)
		if expr != expr2 || (extractErr == nil) != (extractErr2 == nil) {
			t.Fatalf("extractExpression is nondeterministic for %v: (%q,%v) then (%q,%v)", cfg, expr, extractErr, expr2, extractErr2)
		}

		// Consistency with the real consumer: ParseSchedule (the function that
		// actually turns a trigger's stored JSON configuration into a live
		// cron.Schedule) calls extractExpression as its first decision on the
		// SAME configuration. A configuration this rejects must never reach a
		// live schedule through ParseSchedule — if it did, `caesium job apply`
		// could register a cron trigger whose expression field this function
		// says is missing/empty.
		_, _, scheduleErr := ParseSchedule(data)
		if extractErr != nil && scheduleErr == nil {
			t.Fatalf("extractExpression rejected the configuration (%v) but ParseSchedule accepted it: %q", extractErr, data)
		}
	})
}

func FuzzExtractLocation(f *testing.F) {
	f.Add(`{"timezone": "UTC"}`)
	f.Add(`{"timezone": "America/New_York"}`)
	f.Add(`{"timezone": ""}`)
	f.Add(`{"timezone": "invalid/zone"}`)
	f.Add(`{}`)
	f.Fuzz(func(t *testing.T, data string) {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			return
		}

		loc, extractErr := extractLocation(cfg)

		// Determinism: a pure function over the same map must answer the same
		// way every time.
		loc2, extractErr2 := extractLocation(cfg)
		if (extractErr == nil) != (extractErr2 == nil) {
			t.Fatalf("extractLocation nondeterministically changed its error-ness for %v: %v then %v", cfg, extractErr, extractErr2)
		}
		if extractErr == nil {
			name1, name2 := "", ""
			if loc != nil {
				name1 = loc.String()
			}
			if loc2 != nil {
				name2 = loc2.String()
			}
			if name1 != name2 {
				t.Fatalf("extractLocation is nondeterministic for %v: %q then %q", cfg, name1, name2)
			}
		}

		// Consistency with the real consumer: ParseSchedule calls
		// extractLocation on the SAME configuration right after
		// extractExpression. A timezone this rejects must never reach a live
		// schedule through ParseSchedule.
		_, _, scheduleErr := ParseSchedule(data)
		if extractErr != nil && scheduleErr == nil {
			t.Fatalf("extractLocation rejected the configuration (%v) but ParseSchedule accepted it: %q", extractErr, data)
		}
	})
}
