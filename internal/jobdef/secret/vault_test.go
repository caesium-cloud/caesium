package secret

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	vault "github.com/hashicorp/vault/api"
	"github.com/stretchr/testify/suite"
)

type fakeLogical struct {
	response     *vault.Secret
	responses    map[string]*vault.Secret
	err          error
	lastPath     string
	paths        []string
	dataRequests []logicalDataRequest
}

type logicalDataRequest struct {
	path string
	data map[string][]string
}

func (f *fakeLogical) ReadWithContext(_ context.Context, path string) (*vault.Secret, error) {
	f.lastPath = path
	f.paths = append(f.paths, path)
	if f.responses != nil {
		return f.responses[path], f.err
	}
	return f.response, f.err
}

func (f *fakeLogical) ReadWithDataWithContext(_ context.Context, path string, data map[string][]string) (*vault.Secret, error) {
	copied := make(map[string][]string, len(data))
	for k, values := range data {
		copied[k] = append([]string(nil), values...)
	}
	f.dataRequests = append(f.dataRequests, logicalDataRequest{path: path, data: copied})
	if f.responses != nil {
		return f.responses[path], f.err
	}
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

func (s *VaultResolverSuite) TestResolveWithIdentityKVv2PinsVersionAndHMACsValue() {
	keyring, err := NewIdentityKeyring("k1", map[string][]byte{"k1": []byte("server-key")})
	s.Require().NoError(err)
	rest := &fakeLogical{responses: map[string]*vault.Secret{
		"secret/data/path": {
			Data: map[string]any{
				"data":     map[string]any{"token": "abc"},
				"metadata": map[string]any{"version": 7},
			},
		},
	}}
	r := NewVaultResolverWithLogicalAndKeyring(rest, keyring)

	value, identity, err := r.ResolveWithIdentity(context.Background(), "secret://vault/secret/data/path?field=token")
	s.Require().NoError(err)
	s.Equal("abc", value)
	s.Equal([]string{"secret/data/path"}, rest.paths)
	s.Require().Len(rest.dataRequests, 1)
	s.Equal("secret/data/path", rest.dataRequests[0].path)
	s.Equal([]string{"7"}, rest.dataRequests[0].data["version"])
	s.True(identity.Verifiable)
	s.Equal("vault", identity.Provider)
	s.Equal("7", identity.Version)
	s.Equal("k1", identity.KeyID)

	mac := hmac.New(sha256.New, []byte("server-key"))
	_, _ = mac.Write([]byte("abc"))
	s.Equal(hex.EncodeToString(mac.Sum(nil)), identity.HMACSHA256)
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
