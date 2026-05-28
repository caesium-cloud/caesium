//go:build integration

package ldap

import (
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderAuthenticateIntegration(t *testing.T) {
	username := os.Getenv("CAESIUM_TEST_LDAP_USERNAME")
	password := os.Getenv("CAESIUM_TEST_LDAP_PASSWORD")
	cfg := Config{
		URL:                  os.Getenv("CAESIUM_TEST_LDAP_URL"),
		BindDN:               os.Getenv("CAESIUM_TEST_LDAP_BIND_DN"),
		BindPassword:         os.Getenv("CAESIUM_TEST_LDAP_BIND_PASSWORD"),
		UserBaseDN:           os.Getenv("CAESIUM_TEST_LDAP_USER_BASE_DN"),
		UserFilter:           os.Getenv("CAESIUM_TEST_LDAP_USER_FILTER"),
		GroupBaseDN:          os.Getenv("CAESIUM_TEST_LDAP_GROUP_BASE_DN"),
		GroupFilter:          os.Getenv("CAESIUM_TEST_LDAP_GROUP_FILTER"),
		UsernameAttribute:    os.Getenv("CAESIUM_TEST_LDAP_USERNAME_ATTRIBUTE"),
		EmailAttribute:       os.Getenv("CAESIUM_TEST_LDAP_EMAIL_ATTRIBUTE"),
		DisplayNameAttribute: os.Getenv("CAESIUM_TEST_LDAP_DISPLAY_NAME_ATTRIBUTE"),
		GroupAttribute:       os.Getenv("CAESIUM_TEST_LDAP_GROUP_ATTRIBUTE"),
	}
	cfg.StartTLS, _ = strconv.ParseBool(os.Getenv("CAESIUM_TEST_LDAP_START_TLS"))

	if cfg.URL == "" || cfg.BindDN == "" || cfg.BindPassword == "" || cfg.UserBaseDN == "" || username == "" || password == "" {
		t.Skip("set CAESIUM_TEST_LDAP_* env vars to run LDAP integration test")
	}

	provider, err := New(cfg)
	require.NoError(t, err)
	identity, err := provider.Authenticate(t.Context(), username, password)
	require.NoError(t, err)
	require.Equal(t, ProviderName, identity.Issuer)
	require.NotEmpty(t, identity.Subject)
}
