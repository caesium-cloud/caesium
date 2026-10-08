package authmode

import "testing"

func TestActive(t *testing.T) {
	for _, tc := range []struct {
		mode      string
		sso, want bool
	}{
		{"", false, false}, {" ", false, false}, {" NONE ", false, false},
		{"api-key", false, true}, {"unknown", false, true}, {"", true, true}, {"none", true, true},
	} {
		if got := Active(tc.mode, tc.sso); got != tc.want {
			t.Errorf("Active(%q, %v) = %v", tc.mode, tc.sso, got)
		}
	}
}
