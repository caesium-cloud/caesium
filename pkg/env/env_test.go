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
