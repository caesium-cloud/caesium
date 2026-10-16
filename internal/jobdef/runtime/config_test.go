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

func (s *ConfigSuite) buildWatches(raw string) ([]Watch, error) {
	sources, err := s.decodeSources(raw)
	s.Require().NoError(err)
	return BuildGitWatches(env.Environment{
		JobdefGitEnabled:  true,
		JobdefGitInterval: time.Minute,
		JobdefGitSources:  sources,
	}, nil)
}

func (s *ConfigSuite) TestBuildGitWatchesCopiesNamespaces() {
	watches, err := s.buildWatches(`[
		{"url":"https://example.com/a.git","source_id":"a","namespaces":["marketing","default","marketing"]},
		{"url":"https://example.com/b.git","source_id":"b"}
	]`)
	s.Require().NoError(err)
	s.Require().Len(watches, 2)
	s.Equal([]string{"default", "marketing"}, watches[0].Source.Namespaces)
	s.Nil(watches[1].Source.Namespaces)
}

func (s *ConfigSuite) TestBuildGitWatchesRejectsInvalidNamespaces() {
	for _, namespaces := range []string{`[]`, `[""]`, `["default",null]`, `["*"]`, `["Marketing"]`, `["a_b"]`, `[" marketing"]`} {
		_, err := s.buildWatches(`[{"url":"https://example.com/a.git","source_id":"a","namespaces":` + namespaces + `}]`)
		s.Require().Error(err, namespaces)
		s.Contains(err.Error(), "jobdef git source 0", namespaces)
		s.Contains(err.Error(), "omit the field for an unrestricted source", namespaces)
	}
}

func (s *ConfigSuite) TestBuildGitWatchesRequiresSourceIDForNamespaces() {
	for _, sourceID := range []string{``, `,"source_id":"  "`} {
		_, err := s.buildWatches(`[{"url":"https://example.com/a.git","namespaces":["default"]` + sourceID + `}]`)
		s.Require().Error(err, sourceID)
		s.Contains(err.Error(), "jobdef git source 0: namespaces requires a non-empty source_id")
	}
}

func (s *ConfigSuite) TestBuildGitWatchesRejectsSharedSourceIDWithNamespaces() {
	_, err := s.buildWatches(`[
		{"url":"https://example.com/a.git","source_id":"a","namespaces":["default"]},
		{"url":"https://example.com/b.git","source_id":"a"}
	]`)
	s.Require().Error(err)
	s.Contains(err.Error(), `jobdef git sources 0 and 1 share source_id "a"`)

	_, err = s.buildWatches(`[
		{"url":"https://example.com/z.git","source_id":"z"},
		{"url":"https://example.com/a.git","source_id":"a"},
		{"url":"https://example.com/b.git","source_id":"a ","namespaces":["default"]}
	]`)
	s.Require().Error(err)
	s.Contains(err.Error(), `jobdef git sources 1 and 2 share source_id "a"`)

	// Two unrestricted sources sharing an id keep working (compat).
	watches, err := s.buildWatches(`[
		{"url":"https://example.com/a.git","source_id":"a"},
		{"url":"https://example.com/b.git","source_id":"a"}
	]`)
	s.Require().NoError(err)
	s.Len(watches, 2)
}

func (s *ConfigSuite) decodeSources(raw string) (env.GitSources, error) {
	var sources env.GitSources
	if err := sources.Decode(raw); err != nil {
		return nil, err
	}
	return sources, nil
}
