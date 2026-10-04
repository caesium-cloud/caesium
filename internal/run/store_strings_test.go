package run

import "testing"

func TestFirstNonEmptyPreservesRawFallbackSelection(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{nil, ""}, {[]string{"", " \t\n", "\u2003"}, ""},
		{[]string{"", "  alias \n", "fallback"}, "  alias \n"},
		{[]string{"\t", "", "branch", "task"}, "branch"},
		{[]string{"  runtime ", "branch", "task"}, "  runtime "},
	} {
		if got := firstNonEmpty(tc.values...); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.values, got, tc.want)
		}
	}
}
