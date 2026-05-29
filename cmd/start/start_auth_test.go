package start

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHasTrustedProxyTLSPath(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty", raw: "", want: false},
		{name: "blank entries", raw: " , ", want: false},
		{name: "ip", raw: "127.0.0.1", want: true},
		{name: "cidr", raw: "10.0.0.0/24", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hasTrustedProxyTLSPath(tt.raw)
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestHasTrustedProxyTLSPathRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid only", raw: "not-a-cidr"},
		{name: "mixed valid and invalid", raw: "127.0.0.1, not-a-cidr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := hasTrustedProxyTLSPath(tt.raw)
			require.Error(t, err)
			require.False(t, got)
		})
	}
}
