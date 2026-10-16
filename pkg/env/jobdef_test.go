package env

import (
	"testing"

	"github.com/stretchr/testify/suite"
)

type GitSourcesSuite struct {
	suite.Suite
}

func TestGitSourcesSuite(t *testing.T) {
	suite.Run(t, new(GitSourcesSuite))
}

func (s *GitSourcesSuite) TestDecodePopulatesFields() {
	var sources GitSources
	input := `[
		{"url":"https://example.com/repo.git","interval":"2m","once":true,
		"auth":{"username_ref":"secret://env/USER"},
		"ssh":{"known_hosts_ref":"secret://env/KH"}}
	]`

	s.Require().NoError(sources.Decode(input))
	s.Len(sources, 1)
	src := sources[0]
	s.Equal("https://example.com/repo.git", src.URL)
	s.Require().NotNil(src.Auth)
	s.Equal("secret://env/USER", src.Auth.UsernameRef)
	s.Require().NotNil(src.SSH)
	s.Equal("secret://env/KH", src.SSH.KnownHostsRef)

	d, err := src.IntervalDuration(0)
	s.Require().NoError(err)
	s.Equal("2m0s", d.String())
	s.True(src.OnceValue(false))
}

func (s *GitSourcesSuite) TestDecodeEmpty() {
	var sources GitSources
	s.Require().NoError(sources.Decode("  "))
	s.Empty(sources)
}

func (s *GitSourcesSuite) TestDecodeNamespacesAbsentIsNil() {
	var sources GitSources
	s.Require().NoError(sources.Decode(`[{"url":"x","source_id":"a"}]`))
	s.Require().Len(sources, 1)
	s.Nil(sources[0].Namespaces)
}

func (s *GitSourcesSuite) TestDecodeNamespacesList() {
	var sources GitSources
	s.Require().NoError(sources.Decode(`[{"url":"x","namespaces":["marketing","finance"]}]`))
	s.Require().Len(sources, 1)
	s.Equal([]string{"marketing", "finance"}, sources[0].Namespaces)
}

func (s *GitSourcesSuite) TestDecodeNamespacesEmptyStaysNonNil() {
	var sources GitSources
	s.Require().NoError(sources.Decode(`[{"url":"x","namespaces":[]}]`))
	s.Require().Len(sources, 1)
	s.NotNil(sources[0].Namespaces)
	s.Empty(sources[0].Namespaces)
}

func (s *GitSourcesSuite) TestDecodeRejectsSingularNamespaceKey() {
	for _, key := range []string{"namespace", "Namespace", "NAMESPACE"} {
		var sources GitSources
		err := sources.Decode(`[{"url":"x"},{"url":"y","` + key + `":["default"]}]`)
		s.Require().Error(err, key)
		s.Contains(err.Error(), "entry 1")
		s.Contains(err.Error(), `did you mean "namespaces"?`)
	}
}

func (s *GitSourcesSuite) TestDecodeRejectsNullNamespaces() {
	var sources GitSources
	err := sources.Decode(`[{"url":"x","namespaces":null}]`)
	s.Require().Error(err)
	s.Contains(err.Error(), "entry 0")
	s.Contains(err.Error(), "must be a non-empty list")

	// A later case-variant holding null would otherwise win and decode to an
	// unrestricted source.
	err = sources.Decode(`[{"url":"x","namespaces":["a"],"Namespaces":null}]`)
	s.Require().Error(err)
}

func (s *GitSourcesSuite) TestDecodeRejectsDuplicateNamespacesKeys() {
	var sources GitSources
	err := sources.Decode(`[{"url":"x","namespaces":["a"],"NAMESPACES":["b"]}]`)
	s.Require().Error(err)
	s.Contains(err.Error(), `duplicate "namespaces" keys`)
}

// A rejected source must not echo its inline credentials into the fatal
// "environment failure" log.
func (s *GitSourcesSuite) TestProcessRedactsGitSourcesValue() {
	for name, value := range map[string]string{
		"singular namespace key": `[{"url":"https://example.com/r.git","source_id":"a","namespace":["default"],"auth":{"username":"ci","password":"ghp_SECRET"}}]`,
		"malformed JSON":         `[{"url":"https://example.com/r.git","auth":{"username":"ci","password":"ghp_SECRET"}}`,
	} {
		s.T().Setenv("CAESIUM_JOBDEF_GIT_SOURCES", value)
		err := Process()
		s.Require().Error(err, name)
		s.Contains(err.Error(), "CAESIUM_JOBDEF_GIT_SOURCES", name)
		s.NotContains(err.Error(), "ghp_SECRET", name)
	}
}

func (s *GitSourcesSuite) TestDecodeKeepsUnknownKeysLenient() {
	var sources GitSources
	s.Require().NoError(sources.Decode(`[{"url":"x","future_field":true}]`))
	s.Require().Len(sources, 1)
}
