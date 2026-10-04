package enginekind

import "testing"

func TestIsSupported(t *testing.T) {
	tests := []struct {
		engine string
		want   bool
	}{
		{engine: "docker", want: true},
		{engine: "podman", want: true},
		{engine: "kubernetes", want: true},
		{engine: "Docker"},
		{engine: " docker"},
		{engine: "docker "},
		{engine: ""},
	}
	for _, tt := range tests {
		t.Run(tt.engine, func(t *testing.T) {
			if got := IsSupported(tt.engine); got != tt.want {
				t.Errorf("IsSupported(%q) = %v, want %v", tt.engine, got, tt.want)
			}
		})
	}
}
