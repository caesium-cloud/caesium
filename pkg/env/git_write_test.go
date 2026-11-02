package env

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func validWriteCredential() GitWriteCredential {
	return GitWriteCredential{
		SourceID:   "orders",
		Forge:      GitForgeGitHub,
		APIBaseURL: "https://api.github.com",
		TokenRef:   "secret://env/FORGE_TOKEN",
		Namespaces: []string{"marketing"},
	}
}

func TestGitWriteCredentialsDecode(t *testing.T) {
	var credentials GitWriteCredentials
	require.NoError(t, credentials.Decode(`[{
		"repo": "https://github.com/acme/jobs.git",
		"forge": "github",
		"api_base_url": "https://ghe.example.com/api/v3",
		"token": "literal",
		"namespaces": ["marketing", "finance"]
	}]`))
	require.Len(t, credentials, 1)
	require.Equal(t, "https://github.com/acme/jobs.git", credentials[0].Repo)
	require.Equal(t, []string{"marketing", "finance"}, credentials[0].Namespaces)
	require.NoError(t, credentials.Validate())

	require.NoError(t, credentials.Decode("  "))
	require.Empty(t, credentials)
}

// A misspelt key must not read as an absent one: "namespace" would otherwise
// leave a grant looking unscoped, or a token_ref silently unset.
func TestGitWriteCredentialsDecodeRefusesUnknownKeys(t *testing.T) {
	var credentials GitWriteCredentials
	require.ErrorContains(t, credentials.Decode(`[{"source_id":"a","namespace":["x"]}]`), "unknown field")
}

func TestGitWriteCredentialsValidate(t *testing.T) {
	cases := map[string]func(*GitWriteCredential){
		"no key":             func(c *GitWriteCredential) { c.SourceID = "" },
		"unknown forge":      func(c *GitWriteCredential) { c.Forge = "gitlab" },
		"no base url":        func(c *GitWriteCredential) { c.APIBaseURL = "" },
		"no token":           func(c *GitWriteCredential) { c.TokenRef = "" },
		"both tokens":        func(c *GitWriteCredential) { c.Token = "t" },
		"ref is not secret":  func(c *GitWriteCredential) { c.TokenRef = "env/FORGE_TOKEN" },
		"no namespaces":      func(c *GitWriteCredential) { c.Namespaces = nil },
		"empty namespaces":   func(c *GitWriteCredential) { c.Namespaces = []string{} },
		"wildcard namespace": func(c *GitWriteCredential) { c.Namespaces = []string{"*"} },
		"blank namespace":    func(c *GitWriteCredential) { c.Namespaces = []string{""} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			credential := validWriteCredential()
			mutate(&credential)
			err := GitWriteCredentials{credential}.Validate()
			require.ErrorContains(t, err, "CAESIUM_GIT_WRITE_CREDENTIALS entry 0")
		})
	}
	require.NoError(t, GitWriteCredentials{validWriteCredential()}.Validate())
}

func TestValidateForgeAPIBaseURL(t *testing.T) {
	for _, accepted := range []string{
		"https://api.github.com",
		"https://ghe.example.com/api/v3",
		"http://127.0.0.1:8123",
		"http://localhost:8123",
	} {
		require.NoError(t, ValidateForgeAPIBaseURL(accepted), accepted)
	}
	for _, refused := range []string{
		"http://api.github.com",
		"http://10.0.0.1:8080",
		"ftp://api.github.com",
		"api.github.com",
		"https://user:pass@api.github.com",
		"https://api.github.com?x=1",
		"https://api.github.com#frag",
	} {
		require.Error(t, ValidateForgeAPIBaseURL(refused), refused)
	}
}

// envconfig's ParseError echoes the raw value, which holds a literal token.
func TestProcessHidesTheRawWriteCredentialValue(t *testing.T) {
	t.Setenv("CAESIUM_GIT_WRITE_CREDENTIALS", `[{"token":"ghp_tokenvalue",`)
	t.Cleanup(func() { t.Setenv("CAESIUM_GIT_WRITE_CREDENTIALS", ""); _ = Process() })

	err := Process()
	require.ErrorContains(t, err, "CAESIUM_GIT_WRITE_CREDENTIALS")
	require.NotContains(t, err.Error(), "ghp_tokenvalue")
}

func TestProcessRefusesAnUnsafeWriteCredential(t *testing.T) {
	t.Setenv("CAESIUM_GIT_WRITE_CREDENTIALS",
		`[{"source_id":"a","forge":"github","api_base_url":"https://api.github.com","token":"t"}]`)
	t.Cleanup(func() { t.Setenv("CAESIUM_GIT_WRITE_CREDENTIALS", ""); _ = Process() })

	require.ErrorContains(t, Process(), "namespaces must list at least one namespace")
}
