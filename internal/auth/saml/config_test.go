package saml

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeRequiresSingleMetadataSource(t *testing.T) {
	base := Config{
		PublicBaseURL: "https://app.example.com",
		CookieSecret:  []byte(strings.Repeat("x", 32)),
		ReplayCache:   fakeReplayCache{},
	}

	_, err := base.normalize()
	require.ErrorContains(t, err, "metadata")

	base.IDPMetadataURL = "https://idp.example.com/metadata"
	base.IDPMetadataXML = minimalIDPMetadata
	_, err = base.normalize()
	require.ErrorContains(t, err, "only one")
}

func TestNormalizeRejectsInsecureMetadataURL(t *testing.T) {
	_, err := (Config{
		IDPMetadataURL: "http://idp.example.com/metadata",
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	}).normalize()

	require.ErrorContains(t, err, "must use https")
}

func TestNormalizeDerivesServiceURLs(t *testing.T) {
	cfg, err := (Config{
		IDPMetadataURL: "https://idp.example.com/metadata",
		PublicBaseURL:  "https://app.example.com/base",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		ReplayCache:    fakeReplayCache{},
	}).normalize()
	require.NoError(t, err)

	require.Equal(t, "https://app.example.com/base/auth/sso/saml/acs", cfg.ACSURL)
	require.Equal(t, "https://app.example.com/base/auth/sso/saml/metadata", cfg.MetadataURL)
	require.Equal(t, cfg.MetadataURL, cfg.SPEntityID)
	require.Equal(t, DefaultGroupsAttribute, cfg.GroupsAttribute)
	require.Equal(t, DefaultStateCookieName, cfg.StateCookieName)
	require.Equal(t, DefaultStateTTL, cfg.StateTTL)
}

func TestNormalizeRequiresReplayCache(t *testing.T) {
	_, err := (Config{
		IDPMetadataURL: "https://idp.example.com/metadata",
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
	}).normalize()

	require.ErrorContains(t, err, "replay cache")
}

func TestNewFetchesHTTPSMetadata(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write([]byte(minimalIDPMetadata))
	}))
	t.Cleanup(server.Close)

	provider, err := New(t.Context(), Config{
		IDPMetadataURL: server.URL,
		PublicBaseURL:  "https://app.example.com",
		CookieSecret:   []byte(strings.Repeat("x", 32)),
		HTTPClient:     server.Client(),
		ReplayCache:    fakeReplayCache{},
	})

	require.NoError(t, err)
	require.Equal(t, "https://idp.example.com/metadata", provider.serviceProvider.IDPMetadata.EntityID)
}

func TestDefaultMetadataHTTPClientHasTimeout(t *testing.T) {
	require.Equal(t, DefaultMetadataFetchTimeout, defaultMetadataHTTPClient.Timeout)
}

type fakeReplayCache struct{}

func (fakeReplayCache) Record(_ context.Context, _, _ string, _ time.Time) error {
	return nil
}
