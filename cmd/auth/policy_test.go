package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

const cliPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing: {}
bindings:
  - subjects: {groups: [marketing-eng], users: [alice@example.com]}
    role: operator
    namespaces: [marketing]
`

func runPolicyCmd(t *testing.T, leaf *cobra.Command) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	err := leaf.RunE(cmd, nil)
	return stdout.String(), stderr.String(), err
}

func writeCLIPolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

func TestPolicyLintValid(t *testing.T) {
	t.Cleanup(func() { policyLintPath, policyLintJSON = "", false })
	policyLintPath = writeCLIPolicy(t, cliPolicy)

	stdout, stderr, err := runPolicyCmd(t, policyLintCmd)
	require.NoError(t, err)
	require.Contains(t, stdout, "is valid: revision "+policy.Hash([]byte(cliPolicy)))
	require.Contains(t, stdout, "2 namespace(s) (default, marketing), 1 binding(s)")
	require.Contains(t, stderr, `warning: namespace "marketing" has no secret rules`)
	require.Contains(t, stderr, "warning: bindings[0].subjects.users: only OIDC logins")

	policyLintJSON = true
	stdout, stderr, err = runPolicyCmd(t, policyLintCmd)
	require.NoError(t, err)
	var result policyLintResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), "stdout is exactly one JSON document: %q", stdout)
	require.True(t, result.Valid)
	require.Equal(t, policy.Hash([]byte(cliPolicy)), result.Revision)
	require.Equal(t, []string{"default", "marketing"}, result.Namespaces)
	require.Equal(t, 1, result.Bindings)
	require.Len(t, result.Warnings, 2)
	require.Empty(t, result.Error)
	require.Contains(t, stderr, "warning: ", "warnings stay on stderr with --json")
}

func TestPolicyLintInvalid(t *testing.T) {
	t.Cleanup(func() { policyLintPath, policyLintJSON = "", false })
	policyLintPath = writeCLIPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nbindings:\n  - subjects: {groups: [g]}\n    role: root\n    namespaces: [\"*\"]\n")

	stdout, _, err := runPolicyCmd(t, policyLintCmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), `invalid role "root"`)
	require.Contains(t, err.Error(), policyLintPath)
	require.Empty(t, stdout)

	policyLintJSON = true
	stdout, _, err = runPolicyCmd(t, policyLintCmd)
	require.Error(t, err, "an invalid file exits 1 with --json too")
	var result policyLintResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &result), "stdout is exactly one JSON document: %q", stdout)
	require.False(t, result.Valid)
	require.Contains(t, result.Error, `invalid role "root"`)
	require.NotEmpty(t, result.Revision)

	policyLintPath = filepath.Join(t.TempDir(), "absent.yaml")
	stdout, _, err = runPolicyCmd(t, policyLintCmd)
	require.ErrorContains(t, err, "no such file")
	var missing policyLintResult
	require.NoError(t, json.Unmarshal([]byte(stdout), &missing))
	require.False(t, missing.Valid)
	require.Empty(t, missing.Revision)
	require.Contains(t, missing.Error, "no such file")

	policyLintPath = ""
	_, _, err = runPolicyCmd(t, policyLintCmd)
	require.ErrorContains(t, err, "--path is required")
}

func TestPolicyShow(t *testing.T) {
	t.Cleanup(func() { policyShowServer, policyShowAPIKey, policyShowJSON = "http://localhost:8080", "", false })
	var gotAuth, gotPath, gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath, gotMethod = r.Header.Get("Authorization"), r.URL.Path, r.Method
		_, _ = w.Write([]byte(`{"source":"file","path":"/etc/caesium/policy.yaml","revision":"abc123","loaded_at":"2026-10-15T10:00:00Z",
			"default_role":"","warnings":["w1"],"last_reload":{"at":"2026-10-15T10:01:00Z","trigger":"poll","ok":false,"changed":false,"revision":"bad","error":"boom"},
			"policy":{"apiVersion":"v1","kind":"AccessPolicy","namespaces":{"default":{}},"bindings":[{"subjects":{"groups":["g"]},"role":"viewer","namespaces":["*"]}]}}`))
	}))
	defer server.Close()
	policyShowServer = server.URL + "/"
	t.Setenv("CAESIUM_API_KEY", "admin-token")

	stdout, _, err := runPolicyCmd(t, policyShowCmd)
	require.NoError(t, err)
	require.Equal(t, "Bearer admin-token", gotAuth)
	require.Equal(t, "/v1/auth/policy", gotPath)
	require.Equal(t, http.MethodGet, gotMethod)
	require.Contains(t, stdout, "source:       file (/etc/caesium/policy.yaml)")
	require.Contains(t, stdout, "revision:     abc123")
	require.Contains(t, stdout, "FAILED (poll): boom")
	require.Contains(t, stdout, `- groups ["g"] -> viewer @ *`)
	require.Contains(t, stdout, "  - w1")

	policyShowJSON = true
	stdout, stderr, err := runPolicyCmd(t, policyShowCmd)
	require.NoError(t, err)
	require.Empty(t, stderr)
	var view map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &view))
	require.Equal(t, "abc123", view["revision"])
}

func TestPolicyShowSurfacesHTTPErrors(t *testing.T) {
	t.Cleanup(func() { policyShowServer, policyShowJSON = "http://localhost:8080", false })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"insufficient permissions"}`))
	}))
	defer server.Close()
	policyShowServer, policyShowJSON = server.URL, true
	stdout, _, err := runPolicyCmd(t, policyShowCmd)
	require.ErrorContains(t, err, "policy show failed (403)")
	require.Empty(t, stdout)
}

func TestPolicyReload(t *testing.T) {
	t.Cleanup(func() { policyReloadServer, policyReloadAPIKey, policyReloadJSON = "http://localhost:8080", "", false })
	status := http.StatusOK
	body := `{"changed":true,"revision":"new","previous_revision":"old","loaded_at":"2026-10-15T10:00:00Z","warnings":["w1"]}`
	var gotMethod, gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	policyReloadServer = server.URL
	t.Setenv("CAESIUM_API_KEY", "admin-token")

	stdout, stderr, err := runPolicyCmd(t, policyReloadCmd)
	require.NoError(t, err)
	require.Equal(t, http.MethodPost, gotMethod)
	require.Equal(t, "/v1/auth/policy/reload", gotPath)
	require.Equal(t, "access policy reloaded: revision new (was old)\n", stdout)
	require.Equal(t, "warning: w1\n", stderr)

	body = `{"changed":false,"revision":"same","previous_revision":"same","warnings":[]}`
	stdout, _, err = runPolicyCmd(t, policyReloadCmd)
	require.NoError(t, err)
	require.Equal(t, "access policy unchanged: revision same\n", stdout)

	policyReloadJSON = true
	stdout, _, err = runPolicyCmd(t, policyReloadCmd)
	require.NoError(t, err)
	var ok map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &ok))
	require.Equal(t, "same", ok["revision"])

	// A rejected file: exit 1, the retained revision in the error, and the
	// failure document on stdout with --json.
	status, body = http.StatusUnprocessableEntity, `{"message":"access policy file /p.yaml: bad","revision":"kept"}`
	stdout, _, err = runPolicyCmd(t, policyReloadCmd)
	require.Error(t, err)
	require.Contains(t, err.Error(), "revision kept stays in force")
	require.Contains(t, err.Error(), "access policy file /p.yaml: bad")
	var failure map[string]any
	require.NoError(t, json.Unmarshal([]byte(stdout), &failure))
	require.Equal(t, "kept", failure["revision"])

	policyReloadJSON = false
	stdout, _, err = runPolicyCmd(t, policyReloadCmd)
	require.Error(t, err)
	require.Empty(t, stdout)

	// No file configured (409) and authorization failures carry no document.
	for _, code := range []int{http.StatusConflict, http.StatusForbidden} {
		status, body = code, `{"message":"no access policy file is configured"}`
		policyReloadJSON = true
		stdout, _, err = runPolicyCmd(t, policyReloadCmd)
		require.ErrorContains(t, err, "policy reload failed")
		require.Empty(t, stdout)
	}
}

func TestPolicyLeafCommandsRejectArguments(t *testing.T) {
	for _, leaf := range []*cobra.Command{policyLintCmd, policyShowCmd, policyReloadCmd} {
		require.Error(t, leaf.Args(leaf, []string{"unexpected"}), leaf.Name())
		require.NoError(t, leaf.Args(leaf, nil), leaf.Name())
	}
	names := map[string]bool{}
	for _, sub := range policyCmd.Commands() {
		names[sub.Name()] = true
	}
	require.Equal(t, map[string]bool{"lint": true, "show": true, "reload": true}, names)
	require.Contains(t, Cmd.Commands(), policyCmd)
}
