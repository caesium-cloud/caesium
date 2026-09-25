//go:build integration

package cluster

import (
	"strings"
	"testing"
)

func TestQueryUUIDCanonicalizesSQLCellsAndRejectsInvalidIdentity(t *testing.T) {
	const canonical = "e2a55b78-4f0e-4903-a9eb-36a3ff647959"
	for _, raw := range []string{canonical, strings.ReplaceAll(canonical, "-", ""), strings.ToUpper(canonical)} {
		got, err := queryUUID(raw)
		if err != nil || got != canonical {
			t.Fatalf("queryUUID(%q) = %q, %v; want %q", raw, got, err, canonical)
		}
	}
	for _, bad := range []any{nil, "", "not-a-uuid", 42} {
		if got, err := queryUUID(bad); err == nil {
			t.Fatalf("queryUUID(%v) accepted ambiguous identity %q", bad, got)
		}
	}
}
