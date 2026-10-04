package oidc

import (
	"github.com/stretchr/testify/require"
	"net/url"
	"testing"
)

func TestReturnToPreservesProviderErrorIdentity(t *testing.T) {
	origin, err := url.Parse("https://example.com")
	require.NoError(t, err)
	p := &Provider{publicOrigin: origin}
	for _, target := range []string{"//evil.test/x", "https://evil.test/x", "%zz"} {
		_, err := p.validateReturnTo(target)
		require.ErrorIs(t, err, ErrInvalidReturnTo)
	}
	got, err := p.validateReturnTo("https://example.com/a%2Fb?q=1#x%20y")
	require.NoError(t, err)
	require.Equal(t, "/a%2Fb?q=1#x%20y", got)
}
