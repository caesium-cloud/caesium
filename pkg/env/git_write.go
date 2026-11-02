package env

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
)

// GitForgeGitHub is the forge kind of a GitHub or GitHub Enterprise API.
const GitForgeGitHub = "github"

// GitWriteCredentials are the forge write grants of the Git-PR route of
// apply_jobdef_patch, parsed from CAESIUM_GIT_WRITE_CREDENTIALS (a JSON array of
// GitWriteCredential). They are separate from the read-only credentials of
// CAESIUM_JOBDEF_GIT_SOURCES: a source configured for sync is not writable until
// an entry here grants it.
type GitWriteCredentials []GitWriteCredential

// GitWriteCredential is one write grant. It matches a job through SourceID, Repo
// or both (every key that is set must match), and only for the listed namespaces:
// a job outside Namespaces never uses the token, whatever its provenance says.
// Namespaces is required, because a token that writes to a repository must not
// reach every tenant of the server by default.
type GitWriteCredential struct {
	SourceID   string `json:"source_id,omitempty"`
	Repo       string `json:"repo,omitempty"`
	Forge      string `json:"forge"`
	APIBaseURL string `json:"api_base_url"`
	// Token is the literal token; TokenRef is a secret:// reference resolved each
	// time the token is used. Exactly one is set.
	Token      string   `json:"token,omitempty"`
	TokenRef   string   `json:"token_ref,omitempty"`
	Namespaces []string `json:"namespaces"`
}

// Decode implements envconfig.Decoder. Unknown keys are refused: a misspelt
// "namespaces" or "token_ref" must stop the server rather than read as absent.
func (g *GitWriteCredentials) Decode(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		*g = nil
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(value)))
	dec.DisallowUnknownFields()
	var credentials []GitWriteCredential
	if err := dec.Decode(&credentials); err != nil {
		return fmt.Errorf("decode git write credentials: %w", err)
	}
	*g = credentials
	return nil
}

// Validate refuses an entry the route could not use safely.
func (g GitWriteCredentials) Validate() error {
	for idx, credential := range g {
		if err := credential.validate(); err != nil {
			return fmt.Errorf("CAESIUM_GIT_WRITE_CREDENTIALS entry %d: %w", idx, err)
		}
	}
	return nil
}

func (c GitWriteCredential) validate() error {
	if strings.TrimSpace(c.SourceID) == "" && strings.TrimSpace(c.Repo) == "" {
		return errors.New("source_id or repo is required")
	}
	if c.Forge != GitForgeGitHub {
		return fmt.Errorf("forge %q must be %q", c.Forge, GitForgeGitHub)
	}
	if err := ValidateForgeAPIBaseURL(c.APIBaseURL); err != nil {
		return err
	}
	if (c.Token == "") == (strings.TrimSpace(c.TokenRef) == "") {
		return errors.New("exactly one of token and token_ref is required")
	}
	if ref := strings.TrimSpace(c.TokenRef); ref != "" && !strings.HasPrefix(strings.ToLower(ref), "secret://") {
		return errors.New("token_ref must be a secret:// reference")
	}
	if len(c.Namespaces) == 0 {
		return errors.New("namespaces must list at least one namespace")
	}
	if slices.ContainsFunc(c.Namespaces, func(ns string) bool {
		return ns == "" || jobdefschema.ValidateNamespace(ns) != nil
	}) {
		return errors.New("namespaces entries must be DNS labels; a wildcard is not accepted")
	}
	return nil
}

// ValidateForgeAPIBaseURL accepts an absolute API root with nothing a request
// should not inherit: no user info, query or fragment. A token crosses this URL,
// so plain http is limited to a loopback host.
func ValidateForgeAPIBaseURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return errors.New("api_base_url is not a URL")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("api_base_url must be an absolute URL with no user info, query or fragment")
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if ip := net.ParseIP(parsed.Hostname()); parsed.Hostname() == "localhost" || (ip != nil && ip.IsLoopback()) {
			return nil
		}
		return errors.New("api_base_url must use https outside a loopback host")
	default:
		return errors.New("api_base_url must use https")
	}
}
