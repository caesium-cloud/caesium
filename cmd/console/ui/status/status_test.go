package status

import "testing"

func TestNormalize(t *testing.T) {
	got := Normalize("  Succeeded ")
	if got != Succeeded {
		t.Fatalf("Normalize()=%q, want %q", got, Succeeded)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "succeeded", in: Succeeded, want: true},
		{name: "failed", in: Failed, want: true},
		{name: "skipped", in: Skipped, want: true},
		{name: "running", in: Running, want: false},
		{name: "pending", in: Pending, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTerminal(tt.in); got != tt.want {
				t.Fatalf("IsTerminal(%q)=%t, want %t", tt.in, got, tt.want)
			}
		})
	}
}
