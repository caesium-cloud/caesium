//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// TestJobLintServerFlagTargetsExplicitURL exercises the shipped CLI against
// two distinguishable HTTP targets. It keeps the spaced and equals spellings
// from silently falling back to the default localhost server.
func (s *IntegrationTestSuite) TestJobLintServerFlagTargetsExplicitURL() {
	help, helpErr, helpRunErr := s.runCLISeparate("job", "lint", "--help")
	s.Require().NoError(helpRunErr, helpErr)
	s.NotContains(help, "__caesium_job_lint_bare_server__")
	s.Contains(help, "http://localhost:8080")

	path := s.writeLintServerFlagManifest()
	first, firstRequests := newLintFlagServer("first-target")
	defer first.Close()
	second, secondRequests := newLintFlagServer("second-target")
	defer second.Close()

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "spaced", args: []string{"--server", first.URL}, want: "first-target"},
		{name: "equals", args: []string{"--server=" + second.URL}, want: "second-target"},
	} {
		s.Run(tc.name, func() {
			args := append([]string{"job", "lint", "--path", path}, tc.args...)
			args = append(args, "--json")
			stdout, stderr, err := s.runCLISeparate(args...)
			s.Require().NoError(err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
			s.Contains(stdout, tc.want)
			s.True(json.Valid([]byte(stdout)), "--json stdout must be valid JSON: %s", stdout)
		})
	}

	s.Equal(int32(1), firstRequests.features.Load(), "the spaced form must request features from its selected server")
	s.Equal(int32(1), firstRequests.lint.Load(), "the spaced form must post lint to its selected server")
	s.Equal(int32(1), secondRequests.features.Load(), "the equals form must request features from its selected server")
	s.Equal(int32(1), secondRequests.lint.Load(), "the equals form must post lint to its selected server")
	s.Zero(firstRequests.invalid.Load())
	s.Zero(secondRequests.invalid.Load())

	// The documented no-value spelling continues to select the default server.
	stdout, stderr, err := s.runCLISeparate("job", "lint", "--path", path, "--server", "--json")
	s.Require().NoError(err, "stdout:\n%s\nstderr:\n%s", stdout, stderr)
	s.NotEmpty(strings.TrimSpace(stdout))
	s.True(json.Valid([]byte(stdout)), "bare --server --json stdout must be valid JSON: %s", stdout)

	for _, args := range [][]string{
		{"job", "lint", "--path", path, "unexpected"},
		{"job", "lint", "--path", path, "--server=http://localhost:8080", "https://different.example"},
		{"job", "lint", "--path", path, "--server", "--", "unexpected"},
	} {
		stdout, stderr, err = s.runCLISeparate(args...)
		s.Require().Error(err)
		s.Empty(strings.TrimSpace(stdout))
		s.Contains(stderr, "unexpected positional argument(s)")
		s.NotContains(stderr, "__caesium_job_lint_bare_server__")
	}
}

type lintFlagRequests struct {
	features atomic.Int32
	lint     atomic.Int32
	invalid  atomic.Int32
}

func newLintFlagServer(marker string) (*httptest.Server, *lintFlagRequests) {
	requests := new(lintFlagRequests)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/system/features":
			if r.Method != http.MethodGet {
				requests.invalid.Add(1)
				http.Error(w, "features must be GET", http.StatusMethodNotAllowed)
				return
			}
			requests.features.Add(1)
			_, _ = w.Write([]byte(`{"contract_enforcement_enabled":true}`))
		case "/v1/jobdefs/lint":
			if r.Method != http.MethodPost {
				requests.invalid.Add(1)
				http.Error(w, "lint must be POST", http.StatusMethodNotAllowed)
				return
			}
			requests.lint.Add(1)
			_, _ = fmt.Fprintf(w, `{"errors":[],"warnings":[{"message":%q}],"summary":{"steps":1},"contracts":{"breaking":[],"warnings":[],"edges":0}}`, marker)
		default:
			requests.invalid.Add(1)
			http.NotFound(w, r)
		}
	}))
	return server, requests
}

func (s *IntegrationTestSuite) writeLintServerFlagManifest() string {
	s.T().Helper()
	dir := s.T().TempDir()
	path := filepath.Join(dir, "lint-server-flag.job.yaml")
	manifest := `apiVersion: v1
kind: Job
metadata: {alias: lint-server-flag}
trigger: {type: cron, configuration: {cron: "0 2 * * *"}}
steps: [{name: verify, image: alpine:3.23, command: ["sh", "-c", "true"]}]
`
	s.Require().NoError(os.WriteFile(path, []byte(manifest), 0o644))
	return path
}
