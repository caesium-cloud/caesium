//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// TestAuthNamespacesRoute drives GET /v1/auth/namespaces (identity-and-access
// F3) over live HTTP: the declared namespaces and their settings for any
// cluster viewer, never the policy's bindings.
//
//   - no credentials: 401;
//   - a namespace-confined key and a job-scoped key: 403 (cluster route);
//   - a cluster viewer: 200 with the declared set. The agent lane mounts
//     two-namespaces.yaml (default, marketing, finance); the coverage auth
//     server mounts no policy file and declares `default` only.
func (s *IntegrationTestSuite) TestAuthNamespacesRoute() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey, "the auth lane must export an admin key to the runner")

	req, err := http.NewRequestWithContext(s.T().Context(), http.MethodGet, s.caesiumURL+"/v1/auth/namespaces", nil)
	s.Require().NoError(err)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	_ = resp.Body.Close()
	s.Equal(http.StatusUnauthorized, resp.StatusCode, "no credentials")

	stamp := time.Now().UnixNano()
	// A namespace-valid key confined to marketing holds no cluster role.
	confined := s.createAPIKeyCLI("--role", "viewer", "--namespace", "marketing",
		"--description", fmt.Sprintf("namespaces route confined %d", stamp))
	status, body := s.requestWithKey(http.MethodGet, "/v1/auth/namespaces", confined.Plaintext, nil)
	s.Equal(http.StatusForbidden, status, "a namespace-confined key is denied the cluster route: %s", body)

	alias := fmt.Sprintf("auth-namespaces-scoped-%d", stamp)
	s.applyScopedJobDefinition(s.authAPIKey, alias, false)
	scoped := s.createAPIKeyCLI("--role", "viewer", "--scope-jobs", alias,
		"--description", fmt.Sprintf("namespaces route scoped %d", stamp))
	status, body = s.requestWithKey(http.MethodGet, "/v1/auth/namespaces", scoped.Plaintext, nil)
	s.Equal(http.StatusForbidden, status, "a job-scoped key is denied the cluster route: %s", body)

	viewer := s.createAPIKeyCLI("--role", "viewer",
		"--description", fmt.Sprintf("namespaces route viewer %d", stamp))
	status, body = s.requestWithKey(http.MethodGet, "/v1/auth/namespaces", viewer.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status, body)

	var view struct {
		Source     string `json:"source"`
		Revision   string `json:"revision"`
		Namespaces []struct {
			Name    string `json:"name"`
			Secrets *struct {
				Allow []string `json:"allow"`
			} `json:"secrets"`
		} `json:"namespaces"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &view), body)
	names := make([]string, 0, len(view.Namespaces))
	for _, ns := range view.Namespaces {
		names = append(names, ns.Name)
	}
	s.Require().NotEmpty(names)
	s.Equal("default", names[0], "default is always declared and listed first")

	for _, leak := range []string{`"bindings"`, `"subjects"`, `"groups"`, "iaa-reload-viewers", "marketing-eng", "finance-eng"} {
		s.NotContains(body, leak, "the namespaces route must never expose bindings")
	}

	if strings.TrimSpace(os.Getenv(authPolicyPathEnv)) != "" {
		s.Equal([]string{"default", "finance", "marketing"}, names)
		s.NotEmpty(view.Revision, "a file-backed policy reports its revision")
		for _, ns := range view.Namespaces {
			if ns.Name == "marketing" {
				s.Require().NotNil(ns.Secrets)
				s.Equal([]string{"env/MARKETING_*"}, ns.Secrets.Allow)
			}
		}
		s.NotContains(body, "policy.yaml", "the policy file path is not exposed")
	} else {
		s.Equal([]string{"default"}, names, "no policy file: only the default namespace")
		s.Empty(view.Revision)
	}
}
