package env

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/suite"
)

type EnvTestSuite struct {
	suite.Suite
}

func (s *EnvTestSuite) TestProcess() {
	assert.Nil(s.T(), Process())
	assert.NotNil(s.T(), Variables())
	assert.Equal(s.T(), "info", Variables().LogLevel)
	assert.Equal(s.T(), 4, Variables().DatabaseMaxOpenConns)
	assert.Equal(s.T(), 2, Variables().DatabaseMaxIdleConns)
	assert.Equal(s.T(), 1, Variables().DatabaseShards)
	assert.Equal(s.T(), 3, Variables().DatabaseVoters)
	assert.Equal(s.T(), 3, Variables().DatabaseStandbys)
	assert.Equal(s.T(), DefaultDatabaseSnapshotThreshold, Variables().DatabaseSnapshotThreshold)
	assert.Equal(s.T(), DefaultDatabaseSnapshotTrailing, Variables().DatabaseSnapshotTrailing)
	assert.Equal(s.T(), 1024, Variables().DatabaseSnapshotThreshold)
	assert.Equal(s.T(), 2048, Variables().DatabaseSnapshotTrailing)
	assert.Equal(s.T(), 15*time.Second, Variables().WorkerPollInterval)
	assert.Equal(s.T(), "full", Variables().WakeupFanoutMode)
	assert.Equal(s.T(), 8*time.Hour, Variables().AuthSessionIdleTTL)
	assert.Equal(s.T(), 24*time.Hour, Variables().AuthSessionAbsoluteTTL)
	assert.Equal(s.T(), "caesium_session", Variables().AuthSessionCookieName)
	assert.Equal(s.T(), "openid profile email groups", Variables().AuthOIDCScopes)
	assert.Equal(s.T(), "groups", Variables().AuthOIDCGroupsClaim)
	assert.Equal(s.T(), "groups", Variables().AuthSAMLGroupsAttribute)
	assert.Equal(s.T(), "(uid={username})", Variables().AuthLDAPUserFilter)
	assert.Empty(s.T(), Variables().AuthLDAPGroupFilter)
	assert.Equal(s.T(), "cn", Variables().AuthLDAPGroupAttribute)
	assert.Equal(s.T(), "uid", Variables().AuthLDAPUsernameAttribute)
	assert.Equal(s.T(), "mail", Variables().AuthLDAPEmailAttribute)
	assert.Equal(s.T(), "displayName", Variables().AuthLDAPDisplayNameAttribute)
	assert.Equal(s.T(), 10*time.Second, Variables().AuthLDAPTimeout)
	assert.False(s.T(), Variables().SSOEnabled())
	assert.Empty(s.T(), Variables().AuthPolicyFile)
	assert.Equal(s.T(), 10*time.Second, Variables().AuthPolicyReloadInterval)
}

func (s *EnvTestSuite) TestAuthPolicyFileConflictsWithRoleMapping() {
	// The file is never read here: a path that does not exist still passes,
	// and only the configuration conflict fails.
	s.T().Setenv("CAESIUM_AUTH_POLICY_FILE", "/nonexistent/policy.yaml")
	s.Require().NoError(Process())
	s.Equal("/nonexistent/policy.yaml", Variables().AuthPolicyFile)

	s.T().Setenv("CAESIUM_AUTH_ROLE_MAPPING", "admins=admin")
	err := Process()
	s.Require().Error(err)
	s.Contains(err.Error(), "CAESIUM_AUTH_POLICY_FILE")
	s.Contains(err.Error(), "CAESIUM_AUTH_ROLE_MAPPING")

	// A blank mapping is unset, as NewRoleMapper treats it.
	s.T().Setenv("CAESIUM_AUTH_ROLE_MAPPING", "  ")
	s.Require().NoError(Process())

	// Without a file the mapping alone is today's configuration.
	s.T().Setenv("CAESIUM_AUTH_POLICY_FILE", "")
	s.T().Setenv("CAESIUM_AUTH_ROLE_MAPPING", "admins=admin")
	s.Require().NoError(Process())
}

func (s *EnvTestSuite) TestAuthPolicyReloadIntervalValidatedOnlyWithFile() {
	for _, interval := range []string{"0s", "-1s"} {
		s.T().Setenv("CAESIUM_AUTH_POLICY_RELOAD_INTERVAL", interval)
		s.T().Setenv("CAESIUM_AUTH_POLICY_FILE", "")
		s.Require().NoError(Process(), "no file: the interval %s is ignored", interval)

		s.T().Setenv("CAESIUM_AUTH_POLICY_FILE", "/etc/caesium/policy.yaml")
		err := Process()
		s.Require().Error(err, "interval %s with a file", interval)
		s.Contains(err.Error(), "CAESIUM_AUTH_POLICY_RELOAD_INTERVAL must be greater than 0")
	}

	s.T().Setenv("CAESIUM_AUTH_POLICY_RELOAD_INTERVAL", "250ms")
	s.Require().NoError(Process())
	s.Equal(250*time.Millisecond, Variables().AuthPolicyReloadInterval)

	s.T().Setenv("CAESIUM_AUTH_POLICY_RELOAD_INTERVAL", "soon")
	err := Process()
	s.Require().Error(err)
	s.Contains(err.Error(), "CAESIUM_AUTH_POLICY_RELOAD_INTERVAL")
}

func (s *EnvTestSuite) TestAuthCLIKeyTTLDefaultAndBounds() {
	s.Require().NoError(Process())
	s.Equal(12*time.Hour, Variables().AuthCLIKeyTTL)

	for _, ok := range []string{"1m", "168h"} {
		s.T().Setenv("CAESIUM_AUTH_CLI_KEY_TTL", ok)
		s.Require().NoError(Process(), "a TTL of %s is within (0, 7d]", ok)
	}
	s.Equal(MaxAuthCLIKeyTTL, Variables().AuthCLIKeyTTL)

	for _, bad := range []string{"0s", "-1h", "169h"} {
		s.T().Setenv("CAESIUM_AUTH_CLI_KEY_TTL", bad)
		err := Process()
		s.Require().Error(err, "a TTL of %s must be refused", bad)
		s.Contains(err.Error(), "CAESIUM_AUTH_CLI_KEY_TTL")
	}

	s.T().Setenv("CAESIUM_AUTH_CLI_KEY_TTL", "later")
	err := Process()
	s.Require().Error(err)
	s.Contains(err.Error(), "AUTH_CLI_KEY_TTL")
}

func (s *EnvTestSuite) TestAuthGroupRefreshDefaultsAndBounds() {
	s.Require().NoError(Process())
	vars := Variables()
	s.Equal(15*time.Minute, vars.AuthGroupRefreshInterval)
	s.Equal(24*time.Hour, vars.AuthGroupMaxStaleness)
	s.Equal(5*time.Second, vars.AuthGroupRefreshTimeout)
	s.Equal(8*time.Hour, vars.AuthSAMLSessionAbsoluteTTL)
	s.Empty(vars.AuthLDAPCAFile)

	for _, bad := range []struct{ name, value, want string }{
		{"CAESIUM_AUTH_GROUP_REFRESH_INTERVAL", "0s", "CAESIUM_AUTH_GROUP_REFRESH_INTERVAL"},
		{"CAESIUM_AUTH_GROUP_MAX_STALENESS", "10m", "CAESIUM_AUTH_GROUP_MAX_STALENESS"},
		{"CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT", "0s", "CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT"},
		{"CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT", "15m", "CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT"},
		{"CAESIUM_AUTH_SAML_SESSION_ABSOLUTE_TTL", "-1h", "CAESIUM_AUTH_SAML_SESSION_ABSOLUTE_TTL"},
	} {
		s.Run(bad.name+"="+bad.value, func() {
			s.T().Setenv(bad.name, bad.value)
			err := Process()
			s.Require().Error(err)
			s.Contains(err.Error(), bad.want)
		})
	}
	s.Require().NoError(Process())

	s.T().Setenv("CAESIUM_AUTH_GROUP_REFRESH_INTERVAL", "1m")
	s.T().Setenv("CAESIUM_AUTH_GROUP_MAX_STALENESS", "1m")
	s.T().Setenv("CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT", "2s")
	s.T().Setenv("CAESIUM_AUTH_LDAP_CA_FILE", "/etc/caesium/ldap-ca.pem")
	s.Require().NoError(Process(), "the bound may equal the interval")
	s.Equal("/etc/caesium/ldap-ca.pem", Variables().AuthLDAPCAFile)
}

func (s *EnvTestSuite) TestSSOEnabled() {
	env := Environment{AuthOIDCEnabled: true}
	assert.True(s.T(), env.SSOEnabled())
	env = Environment{AuthSAMLEnabled: true}
	assert.True(s.T(), env.SSOEnabled())
	env = Environment{AuthLDAPEnabled: true}
	assert.True(s.T(), env.SSOEnabled())
	assert.False(s.T(), Environment{}.SSOEnabled())
}

func (s *EnvTestSuite) TestOIDCEnvironmentOverrides() {
	s.T().Setenv("CAESIUM_AUTH_OIDC_ENABLED", "true")
	s.T().Setenv("CAESIUM_AUTH_OIDC_ISSUER_URL", "https://idp.example.com")
	s.T().Setenv("CAESIUM_AUTH_OIDC_CLIENT_ID", "caesium")
	s.T().Setenv("CAESIUM_AUTH_OIDC_CLIENT_SECRET", "secret")
	s.T().Setenv("CAESIUM_AUTH_OIDC_SCOPES", "openid email")
	s.T().Setenv("CAESIUM_AUTH_OIDC_GROUPS_CLAIM", "roles")
	s.T().Setenv("CAESIUM_AUTH_OIDC_REDIRECT_URL", "https://app.example.com/auth/sso/oidc/callback")

	s.Require().NoError(Process())
	assert.True(s.T(), Variables().AuthOIDCEnabled)
	assert.True(s.T(), Variables().SSOEnabled())
	assert.Equal(s.T(), "https://idp.example.com", Variables().AuthOIDCIssuerURL)
	assert.Equal(s.T(), "caesium", Variables().AuthOIDCClientID)
	assert.Equal(s.T(), "secret", Variables().AuthOIDCClientSecret)
	assert.Equal(s.T(), "openid email", Variables().AuthOIDCScopes)
	assert.Equal(s.T(), "roles", Variables().AuthOIDCGroupsClaim)
	assert.Equal(s.T(), "https://app.example.com/auth/sso/oidc/callback", Variables().AuthOIDCRedirectURL)
}

func (s *EnvTestSuite) TestSAMLEnvironmentOverrides() {
	s.T().Setenv("CAESIUM_AUTH_SAML_ENABLED", "true")
	s.T().Setenv("CAESIUM_AUTH_SAML_IDP_METADATA_URL", "https://idp.example.com/metadata")
	s.T().Setenv("CAESIUM_AUTH_SAML_IDP_METADATA_XML", "<EntityDescriptor/>")
	s.T().Setenv("CAESIUM_AUTH_SAML_IDP_METADATA_FILE", "/etc/caesium/idp.xml")
	s.T().Setenv("CAESIUM_AUTH_SAML_SP_ENTITY_ID", "https://app.example.com/saml/metadata")
	s.T().Setenv("CAESIUM_AUTH_SAML_SP_CERT", "/etc/caesium/sp.crt")
	s.T().Setenv("CAESIUM_AUTH_SAML_SP_KEY", "/etc/caesium/sp.key")
	s.T().Setenv("CAESIUM_AUTH_SAML_ACS_URL", "https://app.example.com/auth/sso/saml/acs")
	s.T().Setenv("CAESIUM_AUTH_SAML_METADATA_URL", "https://app.example.com/auth/sso/saml/metadata")
	s.T().Setenv("CAESIUM_AUTH_SAML_GROUPS_ATTRIBUTE", "memberOf")

	s.Require().NoError(Process())
	assert.True(s.T(), Variables().AuthSAMLEnabled)
	assert.True(s.T(), Variables().SSOEnabled())
	assert.Equal(s.T(), "https://idp.example.com/metadata", Variables().AuthSAMLIDPMetadataURL)
	assert.Equal(s.T(), "<EntityDescriptor/>", Variables().AuthSAMLIDPMetadataXML)
	assert.Equal(s.T(), "/etc/caesium/idp.xml", Variables().AuthSAMLIDPMetadataFile)
	assert.Equal(s.T(), "https://app.example.com/saml/metadata", Variables().AuthSAMLSPEntityID)
	assert.Equal(s.T(), "/etc/caesium/sp.crt", Variables().AuthSAMLSPCert)
	assert.Equal(s.T(), "/etc/caesium/sp.key", Variables().AuthSAMLSPKey)
	assert.Equal(s.T(), "https://app.example.com/auth/sso/saml/acs", Variables().AuthSAMLACSURL)
	assert.Equal(s.T(), "https://app.example.com/auth/sso/saml/metadata", Variables().AuthSAMLMetadataURL)
	assert.Equal(s.T(), "memberOf", Variables().AuthSAMLGroupsAttribute)
}

func (s *EnvTestSuite) TestLDAPEnvironmentOverrides() {
	s.T().Setenv("CAESIUM_AUTH_LDAP_ENABLED", "true")
	s.T().Setenv("CAESIUM_AUTH_LDAP_URL", "ldap://ldap.example.com:389")
	s.T().Setenv("CAESIUM_AUTH_LDAP_START_TLS", "true")
	s.T().Setenv("CAESIUM_AUTH_LDAP_BIND_DN", "cn=caesium,ou=svc,dc=example,dc=com")
	s.T().Setenv("CAESIUM_AUTH_LDAP_BIND_PASSWORD", "secret")
	s.T().Setenv("CAESIUM_AUTH_LDAP_USER_BASE_DN", "ou=users,dc=example,dc=com")
	s.T().Setenv("CAESIUM_AUTH_LDAP_USER_FILTER", "(sAMAccountName={username})")
	s.T().Setenv("CAESIUM_AUTH_LDAP_GROUP_BASE_DN", "ou=groups,dc=example,dc=com")
	s.T().Setenv("CAESIUM_AUTH_LDAP_GROUP_FILTER", "(member={dn})")
	s.T().Setenv("CAESIUM_AUTH_LDAP_GROUP_ATTRIBUTE", "dn")
	s.T().Setenv("CAESIUM_AUTH_LDAP_USERNAME_ATTRIBUTE", "sAMAccountName")
	s.T().Setenv("CAESIUM_AUTH_LDAP_EMAIL_ATTRIBUTE", "userPrincipalName")
	s.T().Setenv("CAESIUM_AUTH_LDAP_DISPLAY_NAME_ATTRIBUTE", "cn")
	s.T().Setenv("CAESIUM_AUTH_LDAP_TIMEOUT", "15s")

	s.Require().NoError(Process())
	assert.True(s.T(), Variables().AuthLDAPEnabled)
	assert.True(s.T(), Variables().SSOEnabled())
	assert.Equal(s.T(), "ldap://ldap.example.com:389", Variables().AuthLDAPURL)
	assert.True(s.T(), Variables().AuthLDAPStartTLS)
	assert.Equal(s.T(), "cn=caesium,ou=svc,dc=example,dc=com", Variables().AuthLDAPBindDN)
	assert.Equal(s.T(), "secret", Variables().AuthLDAPBindPassword)
	assert.Equal(s.T(), "ou=users,dc=example,dc=com", Variables().AuthLDAPUserBaseDN)
	assert.Equal(s.T(), "(sAMAccountName={username})", Variables().AuthLDAPUserFilter)
	assert.Equal(s.T(), "ou=groups,dc=example,dc=com", Variables().AuthLDAPGroupBaseDN)
	assert.Equal(s.T(), "(member={dn})", Variables().AuthLDAPGroupFilter)
	assert.Equal(s.T(), "dn", Variables().AuthLDAPGroupAttribute)
	assert.Equal(s.T(), "sAMAccountName", Variables().AuthLDAPUsernameAttribute)
	assert.Equal(s.T(), "userPrincipalName", Variables().AuthLDAPEmailAttribute)
	assert.Equal(s.T(), "cn", Variables().AuthLDAPDisplayNameAttribute)
	assert.Equal(s.T(), 15*time.Second, Variables().AuthLDAPTimeout)
}

func (s *EnvTestSuite) TestAgentRemediationRequiresAuthMode() {
	// D1 security precondition: enabling the remediation feature under the default
	// CAESIUM_AUTH_MODE=none (no auth middleware) must fail, so the tier-3 approval
	// routes are never reachable without authentication.
	s.T().Setenv("CAESIUM_AGENT_REMEDIATION_ENABLED", "true")
	err := Process()
	s.Require().Error(err)
	assert.Contains(s.T(), err.Error(), "CAESIUM_AGENT_REMEDIATION_ENABLED")

	// With an active auth mode the precondition is satisfied.
	s.T().Setenv("CAESIUM_AUTH_MODE", "api-key")
	assert.NoError(s.T(), Process())
}

func (s *EnvTestSuite) TestAgentRemediationSatisfiedBySSO() {
	// An SSO provider is an active auth mode even when CAESIUM_AUTH_MODE=none.
	s.T().Setenv("CAESIUM_AGENT_REMEDIATION_ENABLED", "true")
	s.T().Setenv("CAESIUM_AUTH_OIDC_ENABLED", "true")
	assert.NoError(s.T(), Process())
}

func (s *EnvTestSuite) TestContractDeprecationWindowValidatedOnlyWhenEnforcementEnabled() {
	s.T().Setenv("CAESIUM_CONTRACT_DEPRECATION_WINDOW", "0s")
	assert.NoError(s.T(), Process())

	s.T().Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	err := Process()
	s.Require().Error(err)
	assert.Contains(s.T(), err.Error(), "CAESIUM_CONTRACT_DEPRECATION_WINDOW")
}

func (s *EnvTestSuite) TestDatabaseSnapshotParamsOverrides() {
	s.T().Setenv("CAESIUM_DATABASE_SNAPSHOT_THRESHOLD", "512")
	s.T().Setenv("CAESIUM_DATABASE_SNAPSHOT_TRAILING", "512")
	s.Require().NoError(Process())
	assert.Equal(s.T(), 512, Variables().DatabaseSnapshotThreshold)
	assert.Equal(s.T(), 512, Variables().DatabaseSnapshotTrailing)
}

func (s *EnvTestSuite) TestDatabaseSnapshotParamsValidation() {
	for _, tc := range []struct {
		name, threshold, trailing, want string
	}{
		{"zero threshold", "0", "2048", "CAESIUM_DATABASE_SNAPSHOT_THRESHOLD must be between 1 and"},
		{"negative threshold", "-5", "2048", "CAESIUM_DATABASE_SNAPSHOT_THRESHOLD must be between 1 and"},
		{"threshold beyond C unsigned", "4294967296", "4294967296", "CAESIUM_DATABASE_SNAPSHOT_THRESHOLD must be between 1 and"},
		{"trailing below dqlite floor", "1", "3", "CAESIUM_DATABASE_SNAPSHOT_TRAILING must be between 4 and"},
		{"trailing below threshold", "1024", "512", "CAESIUM_DATABASE_SNAPSHOT_TRAILING (512) must be greater than or equal to CAESIUM_DATABASE_SNAPSHOT_THRESHOLD (1024)"},
	} {
		s.Run(tc.name, func() {
			s.T().Setenv("CAESIUM_DATABASE_SNAPSHOT_THRESHOLD", tc.threshold)
			s.T().Setenv("CAESIUM_DATABASE_SNAPSHOT_TRAILING", tc.trailing)
			err := Process()
			s.Require().Error(err)
			assert.Contains(s.T(), err.Error(), tc.want)
		})
	}
}

func (s *EnvTestSuite) TestDatabaseSnapshotParamsIgnoredForPostgres() {
	s.T().Setenv("CAESIUM_DATABASE_TYPE", "postgres")
	s.T().Setenv("CAESIUM_DATABASE_SNAPSHOT_THRESHOLD", "0")
	assert.NoError(s.T(), Process())
}

func (s *EnvTestSuite) TestProcessInvalidTypeFailure() {
	s.T().Setenv("CAESIUM_PORT", "not_a_port")
	assert.NotNil(s.T(), Process())
}

func (s *EnvTestSuite) TestProcessInvalidLogLevelFailure() {
	s.T().Setenv("CAESIUM_LOG_LEVEL", "bogus")
	assert.NotNil(s.T(), Process())
}

func TestEnvTestSuite(t *testing.T) {
	suite.Run(t, new(EnvTestSuite))
}

func (s *EnvTestSuite) TestResourceStatsGateAndSampleInterval() {
	s.T().Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "false")
	s.T().Setenv("CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL", "10s")
	s.Require().NoError(Process())
	s.False(Variables().ResourceStatsEnabled)
	s.Equal(10*time.Second, Variables().ResourceStatsSampleInterval)
	s.T().Setenv("CAESIUM_RESOURCE_STATS_ENABLED", "true")
	s.T().Setenv("CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL", "100ms")
	s.Require().NoError(Process())
	s.True(Variables().ResourceStatsEnabled)
	s.Equal(100*time.Millisecond, Variables().ResourceStatsSampleInterval)
	s.T().Setenv("CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL", "0")
	s.ErrorContains(Process(), "CAESIUM_RESOURCE_STATS_SAMPLE_INTERVAL must be greater than 0")
}

func (s *EnvTestSuite) TestConnectorsDisabledIgnoresConfigFile() {
	s.Require().NoError(Process())
	baseline := Variables()

	s.T().Setenv("CAESIUM_CONNECTORS_CONFIG_FILE", s.T().TempDir())
	s.T().Setenv("CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT", "previous-fingerprint")
	s.Require().NoError(Process())
	got := Variables()
	s.Equal(baseline.LogLevel, got.LogLevel)
	s.Equal(baseline.Port, got.Port)
	s.Equal(baseline.AuthMode, got.AuthMode)
	s.Equal(baseline.DatabaseMaxOpenConns, got.DatabaseMaxOpenConns)
	s.Equal(baseline.DatabaseVoters, got.DatabaseVoters)
	s.Equal(baseline.WorkerPollInterval, got.WorkerPollInterval)
	s.Equal(baseline.AuthSessionIdleTTL, got.AuthSessionIdleTTL)
	s.False(got.ConnectorsEnabled)
	s.False(got.SSOEnabled())
	s.Equal("previous-fingerprint", got.ConnectorsConfigPreviousFingerprint)

	s.T().Setenv("CAESIUM_CONNECTORS_ENABLED", "false")
	s.Require().NoError(Process())
	s.Equal(8080, Variables().Port)
	s.Equal("none", Variables().AuthMode)
}

func (s *EnvTestSuite) TestConnectorsEnabledRequiresAuth() {
	const planted = "planted-secret-VALUE/env-auth"
	path := filepath.Join(s.T().TempDir(), "connectors.yaml")
	s.Require().NoError(os.WriteFile(path, []byte("token: "+planted+"\n"), 0o600))
	s.T().Setenv("CAESIUM_CONNECTORS_ENABLED", "true")
	s.T().Setenv("CAESIUM_CONNECTORS_CONFIG_FILE", path)

	err := Process()
	s.Require().Error(err)
	s.Contains(err.Error(), "CAESIUM_CONNECTORS_ENABLED")
	s.NotContains(err.Error(), planted)

	s.T().Setenv("CAESIUM_AUTH_MODE", "none")
	err = Process()
	s.Require().Error(err)
	s.Contains(err.Error(), "CAESIUM_CONNECTORS_ENABLED")
	s.NotContains(err.Error(), planted)
}

func (s *EnvTestSuite) TestConnectorsEnabledDoesNotReadTheFile() {
	// Process runs for every command. A directory is not a readable config
	// file; startup is what reports that. The gate only checks auth and that
	// a path is set.
	s.T().Setenv("CAESIUM_CONNECTORS_ENABLED", "true")
	s.T().Setenv("CAESIUM_AUTH_MODE", "api-key")
	s.T().Setenv("CAESIUM_CONNECTORS_CONFIG_FILE", s.T().TempDir())
	s.T().Setenv("CAESIUM_CONNECTORS_CONFIG_PREVIOUS_FINGERPRINT", "stored-a")
	s.Require().NoError(Process())
	s.True(Variables().ConnectorsEnabled)
	s.Equal("stored-a", Variables().ConnectorsConfigPreviousFingerprint)
	s.Equal(8080, Variables().Port)

	s.T().Setenv("CAESIUM_AUTH_MODE", "none")
	s.T().Setenv("CAESIUM_AUTH_OIDC_ENABLED", "true")
	s.Require().NoError(Process())
	s.True(Variables().SSOEnabled())
}
