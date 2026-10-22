package authz_test

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/stretchr/testify/require"
)

func TestParseNamespaceNarrow(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    string
		narrow  bool
		wantErr bool
	}{
		{raw: ""},
		{raw: "*"},
		{raw: "marketing", want: "marketing", narrow: true},
		{raw: "default", want: "default", narrow: true},
		{raw: " marketing", wantErr: true},
		{raw: "Marketing", wantErr: true},
		{raw: "a_b", wantErr: true},
	} {
		got, narrow, err := authz.ParseNamespaceNarrow(tc.raw)
		if tc.wantErr {
			require.Error(t, err, tc.raw)
			continue
		}
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.want, got, tc.raw)
		require.Equal(t, tc.narrow, narrow, tc.raw)
	}
}
