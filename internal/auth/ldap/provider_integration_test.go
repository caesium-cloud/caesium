//go:build integration

package ldap

import (
	"os"
	"strconv"
	"strings"
	"testing"

	authpkg "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthenticateIntegration(t *testing.T) {
	cfg, username, password := integrationLDAPConfig(t)

	provider, err := New(cfg)
	require.NoError(t, err)
	identity, err := provider.Authenticate(t.Context(), username, password)
	require.NoError(t, err)
	require.Equal(t, ProviderName, identity.Issuer)
	require.NotEmpty(t, identity.Subject)

	requireOptionalEnvEqual(t, "CAESIUM_TEST_LDAP_EXPECTED_SUBJECT", identity.Subject)
	requireOptionalEnvEqual(t, "CAESIUM_TEST_LDAP_EXPECTED_EMAIL", identity.Email)
	requireOptionalEnvEqual(t, "CAESIUM_TEST_LDAP_EXPECTED_DISPLAY_NAME", identity.DisplayName)

	if expectedGroups := splitEnvList(os.Getenv("CAESIUM_TEST_LDAP_EXPECTED_GROUPS")); len(expectedGroups) > 0 {
		require.ElementsMatch(t, expectedGroups, identity.Groups)
	}

	if mapping := os.Getenv("CAESIUM_TEST_LDAP_ROLE_MAPPING"); strings.TrimSpace(mapping) != "" {
		mapper, err := authpkg.NewRoleMapper(mapping, "")
		require.NoError(t, err)
		role, ok := mapper.Resolve(identity.Groups)
		require.True(t, ok, "expected LDAP groups to resolve with CAESIUM_TEST_LDAP_ROLE_MAPPING")
		if expectedRole := strings.TrimSpace(os.Getenv("CAESIUM_TEST_LDAP_EXPECTED_ROLE")); expectedRole != "" {
			require.Equal(t, models.Role(expectedRole), role)
		}
	}
}

func integrationLDAPConfig(t *testing.T) (Config, string, string) {
	t.Helper()

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
	return cfg, username, password
}

func requireOptionalEnvEqual(t *testing.T, key string, got string) {
	t.Helper()

	if want := strings.TrimSpace(os.Getenv(key)); want != "" {
		require.Equal(t, want, got)
	}
}

func splitEnvList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := strings.TrimSpace(part); value != "" {
			out = append(out, value)
		}
	}
	return out
}
