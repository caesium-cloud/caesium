package secret

import (
	"context"
	"testing"

	"github.com/stretchr/testify/suite"
)

type stubResolver struct {
	value string
	err   error
	ref   string
}

func (s *stubResolver) Resolve(_ context.Context, ref string) (string, error) {
	s.ref = ref
	if s.err != nil {
		return "", s.err
	}
	return s.value, nil
}

type MultiResolverSuite struct {
	suite.Suite
}

func TestMultiResolverSuite(t *testing.T) {
	suite.Run(t, new(MultiResolverSuite))
}

func (s *MultiResolverSuite) TestDispatch() {
	stub := &stubResolver{value: "secret"}
	multi := NewMultiResolver(map[string]Resolver{"env": stub})
	value, err := multi.Resolve(context.Background(), "secret://env/FOO")
	s.Require().NoError(err)
	s.Equal("secret", value)
	s.Equal("secret://env/FOO", stub.ref)
}

func (s *MultiResolverSuite) TestMissingProvider() {
	multi := NewMultiResolver(nil)
	_, err := multi.Resolve(context.Background(), "secret://vault/foo")
	s.Require().Error(err)
}

func (s *MultiResolverSuite) TestEmptyReference() {
	multi := NewMultiResolver(nil)
	_, err := multi.Resolve(context.Background(), " ")
	s.Require().Error(err)
}
