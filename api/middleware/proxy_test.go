package middleware

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseTrustedProxyRangesStrict(t *testing.T) {
	ranges, err := ParseTrustedProxyRangesStrict("127.0.0.1, 10.0.0.0/24")
	require.NoError(t, err)
	require.Len(t, ranges, 2)
}

func TestParseTrustedProxyRangesStrictRejectsInvalidEntries(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "invalid only", raw: "not-a-cidr"},
		{name: "mixed valid and invalid", raw: "127.0.0.1, not-a-cidr"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ranges, err := ParseTrustedProxyRangesStrict(tt.raw)
			require.Error(t, err)
			require.Nil(t, ranges)
		})
	}
}
