package env

import (
	"os"
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

func (s *EnvTestSuite) TestProcessInvalidTypeFailure() {
	s.Require().NoError(os.Setenv("CAESIUM_PORT", "not_a_port"))
	assert.NotNil(s.T(), Process())
}

func (s *EnvTestSuite) TestProcessInvalidLogLevelFailure() {
	s.Require().NoError(os.Setenv("CAESIUM_LOG_LEVEL", "bogus"))
	assert.NotNil(s.T(), Process())
}

func TestEnvTestSuite(t *testing.T) {
	suite.Run(t, new(EnvTestSuite))
}
