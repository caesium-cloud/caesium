package secret

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
)

type EnvResolverSuite struct {
	suite.Suite
	resolver *EnvResolver
}

func TestEnvResolverSuite(t *testing.T) {
	suite.Run(t, new(EnvResolverSuite))
}

func (s *EnvResolverSuite) SetupTest() {
	s.resolver = NewEnvResolver()
}

func (s *EnvResolverSuite) TestResolveDirectVariable() {
	s.T().Setenv("EXAMPLE_TOKEN", "abc123")
	value, err := s.resolver.Resolve(context.Background(), "secret://env/EXAMPLE_TOKEN")
	s.Require().NoError(err)
	s.Equal("abc123", value)
}

func (s *EnvResolverSuite) TestResolveSegmentsJoin() {
	s.T().Setenv("APP_DB_PASSWORD", "s3cr3t")
	value, err := s.resolver.Resolve(context.Background(), "secret://env/APP/DB/PASSWORD")
	s.Require().NoError(err)
	s.Equal("s3cr3t", value)
}

func (s *EnvResolverSuite) TestResolveQueryNameOverride() {
	s.T().Setenv("CUSTOM_NAME", "value")
	value, err := s.resolver.Resolve(context.Background(), "secret://env/foo/bar?name=CUSTOM_NAME")
	s.Require().NoError(err)
	s.Equal("value", value)
}

func (s *EnvResolverSuite) TestMissingEnvVariableFails() {
	_, err := s.resolver.Resolve(context.Background(), "secret://env/MISSING_VAR")
	s.Require().Error(err)
}
