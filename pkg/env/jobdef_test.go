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
