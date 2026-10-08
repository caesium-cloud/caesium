package testutil

import (
	"encoding/json"
	"testing"
)

// MustJSONBytes marshals a fixture and reports malformed fixtures at the caller.
func MustJSONBytes(tb testing.TB, value any) []byte {
	tb.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		tb.Fatalf("marshal JSON fixture: %v", err)
	}
	return data
}
