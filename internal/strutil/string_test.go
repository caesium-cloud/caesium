package strutil

import "testing"

func TestFirstNonBlank(t *testing.T) {
	for _, tc := range []struct {
		values []string
		want   string
	}{
		{nil, ""}, {[]string{"", " ", "\t\n"}, ""},
		{[]string{" ", " first ", "second"}, " first "},
		{[]string{"\u2003", "\tbytes\n"}, "\tbytes\n"},
	} {
		if got := FirstNonBlank(tc.values...); got != tc.want {
			t.Errorf("FirstNonBlank(%q) = %q, want %q", tc.values, got, tc.want)
		}
	}
}
