package runtime

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/stretchr/testify/suite"
)

type ConfigSuite struct {
	suite.Suite
}

func TestConfigSuite(t *testing.T) {
	suite.Run(t, new(ConfigSuite))
}

func (s *ConfigSuite) TestBuildSecretResolverProviders() {
	vars := env.Environment{
		JobdefSecretsEnableEnv:        true,
		JobdefSecretsEnableKubernetes: true,
		JobdefSecretsKubeNamespace:    "jobs",
		JobdefSecretsVaultAddress:     "https://vault.example.com",
		JobdefSecretsVaultToken:       "token",
		JobdefSecretsVaultSkipVerify:  true,
	}

	resolver, err := BuildSecretResolver(vars)
	s.Require().NoError(err)
	providers := resolver.Providers()
	expected := map[string]struct{}{"env": {}, "k8s": {}, "kubernetes": {}, "vault": {}}
	s.Len(providers, len(expected))
	for _, p := range providers {
		_, ok := expected[p]
		s.True(ok, "unexpected provider %q", p)
	}
}

func (s *ConfigSuite) TestBuildGitWatches() {
	sources, err := s.decodeSources(`[
		{"url":"https://example.com/repo.git","source_id":"primary","interval":"3m","once":true,
		  "auth":{"username":"user","password_ref":"secret://env/PASS"}}
	]`)
	s.Require().NoError(err)

	vars := env.Environment{
		JobdefGitEnabled:       true,
		JobdefGitInterval:      time.Minute,
		JobdefGitOnce:          false,
		JobdefGitSources:       sources,
		JobdefSecretsEnableEnv: true,
	}

	resolver, err := BuildSecretResolver(vars)
	s.Require().NoError(err)

	watches, err := BuildGitWatches(vars, resolver)
	s.Require().NoError(err)
	s.Len(watches, 1)

	watch := watches[0]
	s.Equal("https://example.com/repo.git", watch.Source.URL)
	s.Equal("3m0s", watch.Interval.String())
	s.True(watch.Once)
	s.Require().NotNil(watch.Source.Auth)
	s.Equal("secret://env/PASS", watch.Source.Auth.PasswordRef)
}

func (s *ConfigSuite) decodeSources(raw string) (env.GitSources, error) {
	var sources env.GitSources
	if err := sources.Decode(raw); err != nil {
		return nil, err
	}
	return sources, nil
}
