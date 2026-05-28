package ldap

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNormalizeDefaults(t *testing.T) {
	cfg, err := baseConfig().normalize()
	require.NoError(t, err)

	require.Equal(t, DefaultUsernameAttribute, cfg.UsernameAttribute)
	require.Equal(t, DefaultEmailAttribute, cfg.EmailAttribute)
	require.Equal(t, DefaultDisplayNameAttribute, cfg.DisplayNameAttribute)
	require.Equal(t, DefaultGroupAttribute, cfg.GroupAttribute)
	require.Equal(t, DefaultTimeout, cfg.Timeout)
	require.Equal(t, DefaultUserFilter, cfg.UserFilter)
}

func TestNormalizeRequiresSecureLDAP(t *testing.T) {
	cfg := baseConfig()
	cfg.URL = "ldap://dc.example.com:389"

	_, err := cfg.normalize()
	require.ErrorIs(t, err, ErrInvalidConfig)
	require.ErrorContains(t, err, "requires StartTLS")

	cfg.StartTLS = true
	_, err = cfg.normalize()
	require.NoError(t, err)

}

func TestNormalizeRejectsAnonymousServiceBind(t *testing.T) {
	cfg := baseConfig()
	cfg.BindDN = ""
	_, err := cfg.normalize()
	require.ErrorContains(t, err, "bind DN")

	cfg = baseConfig()
	cfg.BindPassword = ""
	_, err = cfg.normalize()
	require.ErrorContains(t, err, "bind password")
}

func TestNormalizeValidatesFilters(t *testing.T) {
	cfg := baseConfig()
	cfg.UserFilter = "(uid=*)"
	_, err := cfg.normalize()
	require.ErrorContains(t, err, "user filter")

	cfg = baseConfig()
	cfg.GroupBaseDN = "ou=Groups,dc=example,dc=com"
	cfg.GroupFilter = "(member=%s)"
	_, err = cfg.normalize()
	require.NoError(t, err)

	cfg.GroupFilter = "(member=%s)(memberUid={username})"
	_, err = cfg.normalize()
	require.ErrorContains(t, err, "must not mix")

	cfg.GroupFilter = "(objectClass=groupOfNames)"
	_, err = cfg.normalize()
	require.ErrorContains(t, err, "group filter")
}

func TestNormalizeDefaultsGroupFilterWhenBaseIsConfigured(t *testing.T) {
	cfg := baseConfig()
	cfg.GroupBaseDN = "ou=Groups,dc=example,dc=com"
	got, err := cfg.normalize()
	require.NoError(t, err)
	require.Equal(t, DefaultGroupFilter, got.GroupFilter)

	cfg = baseConfig()
	cfg.GroupFilter = "(member=%s)"
	_, err = cfg.normalize()
	require.ErrorContains(t, err, "configured together")
}

func baseConfig() Config {
	return Config{
		URL:          "ldaps://dc.example.com:636",
		BindDN:       "cn=caesium,ou=Services,dc=example,dc=com",
		BindPassword: strings.Repeat("x", 16),
		UserBaseDN:   "ou=People,dc=example,dc=com",
	}
}
