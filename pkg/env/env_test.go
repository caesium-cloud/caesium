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
	assert.Equal(s.T(), 5*time.Second, Variables().JoinInterval)
}

func (s *EnvTestSuite) TestProcessInvalidTypeFailure() {
	os.Setenv("CAESIUM_PORT", "not_a_port")
	assert.NotNil(s.T(), Process())
}

func (s *EnvTestSuite) TestProcessInvalidLogLevelFailure() {
	os.Setenv("CAESIUM_LOGLEVEL", "bogus")
	assert.NotNil(s.T(), Process())
}

func TestEnvTestSuite(t *testing.T) {
	suite.Run(t, new(EnvTestSuite))
}
