package secret

import (
	"context"
	"errors"
	"testing"

	vault "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/suite"
)

type fakeLogical struct {
	response *vault.Secret
	err      error
	lastPath string
}

func (f *fakeLogical) ReadWithContext(_ context.Context, path string) (*vault.Secret, error) {
	f.lastPath = path
	return f.response, f.err
}

type VaultResolverSuite struct {
	suite.Suite
}

func TestVaultResolverSuite(t *testing.T) {
	suite.Run(t, new(VaultResolverSuite))
}

func (s *VaultResolverSuite) TestResolveKVv2() {
	rest := &fakeLogical{response: &vault.Secret{Data: map[string]any{
		"data": map[string]any{"token": "abc"},
	}}}
	r := NewVaultResolverWithLogical(rest)
	value, err := r.Resolve(context.Background(), "secret://vault/secret/data/path?field=token")
	s.Require().NoError(err)
	s.Equal("abc", value)
	s.Equal("secret/data/path", rest.lastPath)
}

func (s *VaultResolverSuite) TestResolveFieldAsSegment() {
	rest := &fakeLogical{response: &vault.Secret{Data: map[string]any{"password": "hunter2"}}}
	r := NewVaultResolverWithLogical(rest)
	value, err := r.Resolve(context.Background(), "secret://vault/secret/legacy/password")
	s.Require().NoError(err)
	s.Equal("hunter2", value)
	s.Equal("secret/legacy", rest.lastPath)
}

func (s *VaultResolverSuite) TestMissingFieldFails() {
	rest := &fakeLogical{response: &vault.Secret{Data: map[string]any{"data": map[string]any{}}}}
	r := NewVaultResolverWithLogical(rest)
	_, err := r.Resolve(context.Background(), "secret://vault/secret/data/path?field=missing")
	s.Require().Error(err)
}

func (s *VaultResolverSuite) TestMissingPathFails() {
	r := NewVaultResolverWithLogical(&fakeLogical{})
	_, err := r.Resolve(context.Background(), "secret://vault")
	s.Require().Error(err)
}

func (s *VaultResolverSuite) TestNewVaultResolverRequiresAddress() {
	_, err := NewVaultResolver(VaultConfig{})
	s.Require().Error(err)
}

func (s *VaultResolverSuite) TestNewVaultResolverTLSConfigError() {
	cfg := VaultConfig{Address: "https://vault.example.com", CACertPath: "/does/not/exist"}
	_, err := NewVaultResolver(cfg)
	s.Require().Error(err)
}

func (s *VaultResolverSuite) TestPropagatesReadError() {
	rest := &fakeLogical{err: errors.New("boom")}
	r := NewVaultResolverWithLogical(rest)
	_, err := r.Resolve(context.Background(), "secret://vault/secret/data/foo?field=bar")
	s.Require().Error(err)
}
