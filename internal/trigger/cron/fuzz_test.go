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
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			// Not valid JSON — skip, we only fuzz the expression extraction logic.
			return
		}
		// Must not panic
		_, _ = extractExpression(cfg)
	})
}

func FuzzExtractLocation(f *testing.F) {
	f.Add(`{"timezone": "UTC"}`)
	f.Add(`{"timezone": "America/New_York"}`)
	f.Add(`{"timezone": ""}`)
	f.Add(`{"timezone": "invalid/zone"}`)
	f.Add(`{}`)
	f.Fuzz(func(t *testing.T, data string) {
		var cfg map[string]interface{}
		if err := json.Unmarshal([]byte(data), &cfg); err != nil {
			return
		}
		// Must not panic
		_, _ = extractLocation(cfg)
	})
}
