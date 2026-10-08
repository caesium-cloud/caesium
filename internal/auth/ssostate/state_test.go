package ssostate

import (
	"encoding/base64"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateReturnTo(t *testing.T) {
	origin, err := url.Parse("https://example.com")
	require.NoError(t, err)
	invalid := errors.New("provider invalid return target")
	for _, tc := range []struct {
		in, want string
		bad      bool
	}{
		{"", "/", false}, {"  /a  ", "/a", false},
		{"/a%2Fb?q=a%20b#x%20y", "/a%2Fb?q=a%20b#x%20y", false},
		{"HTTPS://EXAMPLE.COM", "/", false},
		{"https://example.com?x=1#f", "/?x=1#f", false},
		{"//evil.test/x", "", true}, {`\evil.test`, "", true},
		{"relative", "", true}, {"%zz", "", true},
		{"https://evil.test/x", "", true}, {"http://example.com/x", "", true},
		{"https://example.com:443/x", "", true},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ValidateReturnTo(tc.in, origin, invalid)
			if tc.bad {
				require.ErrorIs(t, err, invalid)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.want, got)
		})
	}
	_, err = ValidateReturnTo("https://example.com", nil, invalid)
	require.ErrorIs(t, err, invalid)
}
func TestRandomURLSafe(t *testing.T) {
	a, err := RandomURLSafe(32)
	require.NoError(t, err)
	b, err := RandomURLSafe(32)
	require.NoError(t, err)
	require.NotEqual(t, a, b)
	raw, err := base64.RawURLEncoding.DecodeString(a)
	require.NoError(t, err)
	require.Len(t, raw, 32)
	require.NotContains(t, a, "=")
}
