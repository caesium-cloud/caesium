package job

import (
	"testing"

	"github.com/google/uuid"
)

func TestNormalizeTaskFailurePolicy(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "default empty", in: "", want: taskFailurePolicyHalt},
		{name: "halt", in: "halt", want: taskFailurePolicyHalt},
		{name: "continue", in: "continue", want: taskFailurePolicyContinue},
		{name: "continue mixed case", in: " ConTinue ", want: taskFailurePolicyContinue},
		{name: "unknown", in: "explode", want: taskFailurePolicyHalt},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeTaskFailurePolicy(tt.in)
			if got != tt.want {
				t.Fatalf("normalizeTaskFailurePolicy(%q)=%q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestCollectDescendants(t *testing.T) {
	a := uuid.New()
	b := uuid.New()
	c := uuid.New()
	d := uuid.New()
	e := uuid.New()

	adjacency := map[uuid.UUID][]uuid.UUID{
		a: {b, c},
		b: {d},
		c: {d, e},
		d: {},
		e: {},
	}

	got := collectDescendants(adjacency, a)
	if len(got) != 4 {
		t.Fatalf("expected 4 descendants, got %d", len(got))
	}

	seen := make(map[uuid.UUID]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}

	for _, id := range []uuid.UUID{b, c, d, e} {
		if !seen[id] {
			t.Fatalf("missing descendant %s", id)
		}
	}
}
