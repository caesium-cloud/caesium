package outputdiff

import (
	"strings"
	"testing"
)

func TestCompareDetectsAddedRemovedAndChangedDeterministically(t *testing.T) {
	diff := Compare(
		map[string]string{
			"b_removed": "old",
			"c_same":    "same",
			"d_changed": "before",
		},
		map[string]string{
			"a_added":   "new",
			"c_same":    "same",
			"d_changed": "after",
		},
	)

	if diff.Empty() {
		t.Fatal("diff.Empty() = true, want false")
	}
	if got, want := diff.Added, []Entry{{Key: "a_added", Value: "new"}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Added = %#v, want %#v", got, want)
	}
	if got, want := diff.Removed, []Entry{{Key: "b_removed", Value: "old"}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Removed = %#v, want %#v", got, want)
	}
	if got, want := diff.Changed, []Change{{Key: "d_changed", Recorded: "before", Reproduced: "after"}}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("Changed = %#v, want %#v", got, want)
	}

	rendered := diff.Render()
	firstChanged := strings.Index(rendered, "d_changed")
	firstRemoved := strings.Index(rendered, "b_removed")
	firstAdded := strings.Index(rendered, "a_added")
	if !(firstChanged > 0 && firstRemoved > firstChanged && firstAdded > firstRemoved) {
		t.Fatalf("Render() order is not deterministic changed/removed/added:\n%s", rendered)
	}
}

func TestCompareEmpty(t *testing.T) {
	diff := Compare(
		map[string]string{"rows": "7"},
		map[string]string{"rows": "7"},
	)

	if !diff.Empty() {
		t.Fatalf("diff.Empty() = false, diff = %#v", diff)
	}
	if got, want := diff.Render(), "Output diff vs recorded: match\n"; got != want {
		t.Fatalf("Render() = %q, want %q", got, want)
	}
}
