package secret

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type FactorySuite struct {
	suite.Suite
}

func TestFactorySuite(t *testing.T) {
	suite.Run(t, new(FactorySuite))
}

func (s *FactorySuite) TestNewConfiguredResolverProvidesEnv() {
	resolver, err := NewConfiguredResolver(Config{EnableEnv: true})
	s.Require().NoError(err)
	providers := resolver.Providers()
	s.Equal([]string{providerEnv}, providers)
}
