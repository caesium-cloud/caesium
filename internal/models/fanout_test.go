package models

import "testing"

func TestIsFannedGroup(t *testing.T) {
	for _, tc := range []struct {
		count int
		value string
		want  bool
	}{{0, "", false}, {0, "p", false}, {1, "", false}, {1, "p", true}, {2, "", true}, {2, "p", true}} {
		if got := IsFannedGroup(tc.count, tc.value); got != tc.want {
			t.Errorf("(%d,%q)=%t, want %t", tc.count, tc.value, got, tc.want)
		}
	}
}
