package env

import (
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
