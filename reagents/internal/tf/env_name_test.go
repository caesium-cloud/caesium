package tf

import "testing"

func TestNormalizeEnvName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"", ""}, {"a.b-c", "A_B_C"}, {"Mixed_Case", "MIXED_CASE"}, {"é.δ-ü", "É_Δ_Ü"},
	} {
		if got := NormalizeEnvName(tc.name); got != tc.want {
			t.Errorf("NormalizeEnvName(%q) = %q, want %q", tc.name, got, tc.want)
		}
	}
}
