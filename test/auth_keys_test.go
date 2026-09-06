//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Stream C2 — the key-management surface, driven through its REAL surfaces on
// the auth-enabled lane: the `caesium auth` CLI binary (stdout captured
// SEPARATELY from stderr, per CLAUDE.md) and the live REST endpoints bound by
// api/rest/bind/bind.go `bindAuth`.
//
// Everything here needs a server with CAESIUM_AUTH_MODE=api-key, so every
// scenario opens with requireAuthLane() (test/auth_lane_test.go): off the auth
// lane it skips, on the lane it asserts the runner really carries the auth env
// rather than skipping silently.

// apiKeyMetadata mirrors the models.APIKey fields the auth CLI prints under
// "Key metadata:" and the REST endpoints serialise.
type apiKeyMetadata struct {
	ID          string     `json:"id"`
	KeyPrefix   string     `json:"key_prefix"`
	Description string     `json:"description"`
	Role        string     `json:"role"`
	CreatedBy   string     `json:"created_by"`
	ExpiresAt   *time.Time `json:"expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	Scope       *struct {
		Jobs []string `json:"jobs"`
	} `json:"scope"`
}

// createdAPIKey pairs a freshly minted key's plaintext with its metadata. The
// plaintext is shown exactly once, so every caller has to capture it here.
type createdAPIKey struct {
	Plaintext string
	Meta      apiKeyMetadata
}

// createAPIKeyCLI mints a key through the real `caesium auth key create`
// binary and returns the plaintext plus its metadata. extraArgs carries the
// role/scope/description flags for the case at hand; --server is appended here.
//
// The admin credential comes from CAESIUM_API_KEY in the runner environment
// (the auth lane exports the bootstrap admin key there), which is also the
// documented preference over --api-key. Stdout is captured on its own so the
// parsed output cannot be polluted by log lines or the --api-key warning.
func (s *IntegrationTestSuite) createAPIKeyCLI(extraArgs ...string) createdAPIKey {
	s.T().Helper()

	s.Require().NotEmpty(s.authAPIKey,
		"the auth lane must export an admin key (CAESIUM_API_KEY / CAESIUM_AUTH_ADMIN_KEY) to the runner")

	args := append([]string{"auth", "key", "create"}, extraArgs...)
	args = append(args, "--server", s.caesiumURL)

	stdout, err := s.runCLIStdout(args...)
	s.Require().NoError(err, "caesium auth key create failed:\n%s", stdout)
	return s.parseCreatedKey(stdout)
}

// parseCreatedKey decodes the record `auth key create` and `auth key rotate`
// write to stdout.
//
// It unmarshals the WHOLE stream with no stripping, on purpose: stdout must be
// exactly one JSON object. An earlier version scanned for a `csk_` token and
// sliced from the first `{`, which would have kept passing if the commands
// went back to interleaving prose into stdout — the reviewer's point on #391.
func (s *IntegrationTestSuite) parseCreatedKey(stdout string) createdAPIKey {
	s.T().Helper()

	var resp struct {
		Key    string         `json:"key"`
		APIKey apiKeyMetadata `json:"api_key"`
	}
	s.Require().NoError(json.Unmarshal([]byte(stdout), &resp),
		"auth key stdout must be exactly one JSON object, with no human framing:\n%s", stdout)
	s.Require().NotEmpty(resp.Key, "the response must carry the plaintext key:\n%s", stdout)
	s.Require().True(strings.HasPrefix(resp.Key, "csk_"),
		"the plaintext key must be a caesium key, got %q", resp.Key)
	s.Require().NotEmpty(resp.APIKey.ID, "the response must carry the key id:\n%s", stdout)
	return createdAPIKey{Plaintext: resp.Key, Meta: resp.APIKey}
}

// requestWithKey issues a request authenticated with an explicit bearer token,
// bypassing the suite's admin credential. s.doRequest cannot be used for this:
// it injects the admin key and there is no way to hand it a different one.
func (s *IntegrationTestSuite) requestWithKey(method, path, token string, body io.Reader) (int, string) {
	s.T().Helper()

	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, body)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()

	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}

// TestAuthKeyLifecycleCLI drives create → list → rotate → revoke through the
// `caesium auth key` binary and proves each step at the API: the created key
// authenticates, the rotated key authenticates, the old key survives its grace
// window, and the revoked key is rejected.
func (s *IntegrationTestSuite) TestAuthKeyLifecycleCLI() {
	s.requireAuthLane()

	description := fmt.Sprintf("auth-lane lifecycle key %d", time.Now().UnixNano())
	created := s.createAPIKeyCLI("--role", "viewer", "--description", description)

	s.Equal("viewer", created.Meta.Role)
	s.Equal(description, created.Meta.Description)
	s.Require().NotEmpty(created.Meta.KeyPrefix)
	s.True(
		strings.HasPrefix(created.Plaintext, created.Meta.KeyPrefix),
		"the printed plaintext %q must carry the key prefix %q", created.Plaintext, created.Meta.KeyPrefix,
	)

	// The key works: a viewer may list jobs.
	status, body := s.requestWithKey(http.MethodGet, "/v1/jobs", created.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status, body)

	// `auth key list` shows it. --api-key is exercised here (rather than the
	// CAESIUM_API_KEY env the other steps use) so the flag path is covered; its
	// "visible in process listings" warning must land on STDERR, leaving stdout
	// parseable as JSON.
	listOut, listErr, err := s.runCLISeparate(
		"auth", "key", "list",
		"--server", s.caesiumURL,
		"--api-key", s.authAPIKey,
	)
	s.Require().NoError(err, "caesium auth key list failed:\nstdout: %s\nstderr: %s", listOut, listErr)
	s.Contains(listErr, "--api-key is visible in process listings",
		"the --api-key warning belongs on stderr")
	s.NotContains(listOut, "--api-key is visible in process listings",
		"the --api-key warning must not contaminate machine-readable stdout")

	var listed []apiKeyMetadata
	s.Require().NoError(json.Unmarshal([]byte(listOut), &listed),
		"auth key list stdout must be exactly one JSON array, with no human framing:\n%s", listOut)
	s.True(containsAPIKeyID(listed, created.Meta.ID),
		"key %s must appear in `auth key list`", created.Meta.ID)
	s.NotContains(listOut, `"key_hash"`, "the stored key hash must never be serialised")

	// Rotate: a new key is issued and the old one keeps a grace window.
	rotateOut, err := s.runCLIStdout(
		"auth", "key", "rotate",
		"--id", created.Meta.ID,
		"--grace-period", "1m",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth key rotate failed:\n%s", rotateOut)

	rotated := s.parseCreatedKey(rotateOut)
	s.NotEqual(created.Meta.ID, rotated.Meta.ID, "rotation must mint a NEW key id")
	s.NotEqual(created.Plaintext, rotated.Plaintext)
	s.Equal("viewer", rotated.Meta.Role, "rotation preserves the role")

	status, body = s.requestWithKey(http.MethodGet, "/v1/jobs", rotated.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status, body)

	// The rotated-out key is still inside its 1m grace window.
	status, body = s.requestWithKey(http.MethodGet, "/v1/jobs", created.Plaintext, nil)
	s.Equal(http.StatusOK, status, "the rotated-out key must survive its grace period: %s", body)

	// Revoke the new key: it is rejected immediately.
	revokeOut, err := s.runCLIStdout(
		"auth", "key", "revoke",
		"--id", rotated.Meta.ID,
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth key revoke failed:\n%s", revokeOut)

	var revoked struct {
		Status string `json:"status"`
	}
	s.Require().NoError(json.Unmarshal([]byte(revokeOut), &revoked),
		"auth key revoke stdout must be exactly one JSON object:\n%s", revokeOut)
	s.Equal("revoked", revoked.Status)

	status, body = s.requestWithKey(http.MethodGet, "/v1/jobs", rotated.Plaintext, nil)
	s.Equal(http.StatusUnauthorized, status,
		"a revoked key must be rejected by a subsequent GET /v1/jobs: %s", body)
}

// TestAuthAuditCLI drives `caesium auth audit --action api_key.revoke` and
// asserts the revoke it just performed shows up in the audit spine.
func (s *IntegrationTestSuite) TestAuthAuditCLI() {
	s.requireAuthLane()

	created := s.createAPIKeyCLI(
		"--role", "viewer",
		"--description", fmt.Sprintf("auth-lane audit key %d", time.Now().UnixNano()),
	)

	revokeOut, err := s.runCLIStdout(
		"auth", "key", "revoke",
		"--id", created.Meta.ID,
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth key revoke failed:\n%s", revokeOut)

	auditOut, auditErr, err := s.runCLISeparate(
		"auth", "audit",
		"--action", "api_key.revoke",
		"--limit", "200",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth audit failed:\nstdout: %s\nstderr: %s", auditOut, auditErr)

	var entries []auditEntry
	s.Require().NoError(json.Unmarshal([]byte(auditOut), &entries),
		"caesium auth audit stdout must be exactly one JSON array (log or framing contamination?):\n%s", auditOut)
	s.Require().NotEmpty(entries)

	var found *auditEntry
	for i := range entries {
		s.Equal("api_key.revoke", entries[i].Action, "--action must filter the query server-side")
		if entries[i].ResourceID == created.Meta.ID {
			found = &entries[i]
		}
	}
	s.Require().NotNil(found, "the revoke of key %s must appear in the audit log:\n%s", created.Meta.ID, auditOut)
	s.Equal("api_key", found.ResourceType)
	s.Equal("success", found.Outcome)
}

type auditEntry struct {
	ID           string    `json:"id"`
	Timestamp    time.Time `json:"timestamp"`
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Outcome      string    `json:"outcome"`
}

// TestAuthKeysREST hits the key-management REST surface directly — the same
// endpoints the CLI wraps — including the role gate that keeps a viewer from
// minting keys.
func (s *IntegrationTestSuite) TestAuthKeysREST() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey,
		"the auth lane must export an admin key (CAESIUM_API_KEY / CAESIUM_AUTH_ADMIN_KEY) to the runner")

	description := fmt.Sprintf("auth-lane rest key %d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"role":"viewer","description":%q}`, description)

	status, body := s.requestWithKey(http.MethodPost, "/v1/auth/keys", s.authAPIKey, strings.NewReader(payload))
	s.Require().Equal(http.StatusCreated, status, body)

	var createResp struct {
		Key    string         `json:"key"`
		APIKey apiKeyMetadata `json:"api_key"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &createResp))
	s.Require().NotEmpty(createResp.Key)
	s.Require().NotEmpty(createResp.APIKey.ID)
	s.Equal("viewer", createResp.APIKey.Role)
	s.NotContains(body, `"key_hash"`, "the stored hash must never leave the server")

	// GET /v1/auth/keys lists it.
	status, body = s.requestWithKey(http.MethodGet, "/v1/auth/keys", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var listed []apiKeyMetadata
	s.Require().NoError(json.Unmarshal([]byte(body), &listed))
	s.True(containsAPIKeyID(listed, createResp.APIKey.ID),
		"key %s must appear in GET /v1/auth/keys", createResp.APIKey.ID)

	// A viewer-role key may NOT mint keys (RBAC: POST /v1/auth/keys is admin).
	status, body = s.requestWithKey(
		http.MethodPost, "/v1/auth/keys", createResp.Key,
		strings.NewReader(`{"role":"viewer","description":"escalation attempt"}`),
	)
	s.Equal(http.StatusForbidden, status,
		"a viewer-role key must be denied POST /v1/auth/keys: %s", body)

	// Rotate through REST.
	status, body = s.requestWithKey(
		http.MethodPost,
		fmt.Sprintf("/v1/auth/keys/%s/rotate", createResp.APIKey.ID),
		s.authAPIKey,
		strings.NewReader(`{"grace_period":"1m"}`),
	)
	s.Require().Equal(http.StatusCreated, status, body)
	var rotateResp struct {
		Key    string         `json:"key"`
		APIKey apiKeyMetadata `json:"api_key"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &rotateResp))
	s.NotEqual(createResp.APIKey.ID, rotateResp.APIKey.ID)

	status, body = s.requestWithKey(http.MethodGet, "/v1/jobs", rotateResp.Key, nil)
	s.Require().Equal(http.StatusOK, status, body)

	// Revoke through REST.
	status, body = s.requestWithKey(
		http.MethodPost,
		fmt.Sprintf("/v1/auth/keys/%s/revoke", rotateResp.APIKey.ID),
		s.authAPIKey,
		nil,
	)
	s.Require().Equal(http.StatusOK, status, body)
	s.Contains(body, "revoked")

	status, body = s.requestWithKey(http.MethodGet, "/v1/jobs", rotateResp.Key, nil)
	s.Equal(http.StatusUnauthorized, status, "a revoked key must be rejected: %s", body)

	// Revoking an unknown key is a 404, not a silent success.
	status, body = s.requestWithKey(
		http.MethodPost,
		"/v1/auth/keys/00000000-0000-0000-0000-000000000000/revoke",
		s.authAPIKey,
		nil,
	)
	s.Equal(http.StatusNotFound, status, body)

	// GET /v1/auth/audit records the revoke.
	status, body = s.requestWithKey(
		http.MethodGet,
		"/v1/auth/audit?action=api_key.revoke&limit=200",
		s.authAPIKey,
		nil,
	)
	s.Require().Equal(http.StatusOK, status, body)
	var entries []auditEntry
	s.Require().NoError(json.Unmarshal([]byte(body), &entries))
	s.True(auditContainsResource(entries, rotateResp.APIKey.ID),
		"the REST revoke of %s must be audited:\n%s", rotateResp.APIKey.ID, body)
}

// runCLIWithEnv runs the CLI with extra environment entries appended to the
// inherited environment (later entries win), keeping stdout and stderr apart.
// It exists so a scenario can run the binary with CAESIUM_API_KEY *cleared* and
// prove the unauthenticated path is refused.
func (s *IntegrationTestSuite) runCLIWithEnv(extraEnv []string, args ...string) (string, string, error) {
	s.T().Helper()

	cmd := exec.CommandContext(s.T().Context(), s.cliPath, args...)
	cmd.Dir = s.projectRoot
	cmd.Env = append(os.Environ(), extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

// TestAuthJobApplyCLI covers the deploy verb the README and CLAUDE.md both
// present as THE way to ship a job: `caesium job apply --path … --server …`.
//
// It sent no Authorization header at all (`cmd/job/apply.go` sendApplyRequest),
// so against any server with CAESIUM_AUTH_MODE=api-key it answered 401 — the
// auth surface could not be true end to end while the primary write command
// could not authenticate. `job lint --server` already resolved a key through
// cliutil.ResolveAPIKey; apply now uses the same helper, and this scenario is
// what keeps it that way.
func (s *IntegrationTestSuite) TestAuthJobApplyCLI() {
	s.requireAuthLane()
	s.Require().NotEmpty(s.authAPIKey,
		"the auth lane must export an admin key (CAESIUM_API_KEY / CAESIUM_AUTH_ADMIN_KEY) to the runner")

	alias := fmt.Sprintf("auth-job-apply-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`
apiVersion: v1
kind: Job
metadata:
  alias: %s
trigger:
  type: cron
  configuration:
    cron: "0 2 * * *"
steps:
  - name: run
    image: alpine:3.23
    command: ["sh", "-c", "echo apply-ok"]
`, alias)

	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	// Without a credential the apply must be REFUSED, not silently applied.
	stdout, stderr, err := s.runCLIWithEnv(
		[]string{"CAESIUM_API_KEY=", "CAESIUM_AUTH_ADMIN_KEY="},
		"job", "apply", "--path", dir, "--server", s.caesiumURL,
	)
	s.Require().Error(err,
		"an unauthenticated `job apply` must fail on an auth-enabled server:\nstdout: %s\nstderr: %s", stdout, stderr)
	s.Contains(stderr+stdout, "apply failed")

	// The job must not exist yet — a refused apply writes nothing.
	status, body := s.requestWithKey(http.MethodGet, "/v1/jobs", s.authAPIKey, nil)
	s.Require().Equal(http.StatusOK, status, body)
	s.NotContains(body, alias, "a refused apply must not have created the job")

	// With the admin key from the environment the apply succeeds.
	stdout, stderr, err = s.runCLISeparate("job", "apply", "--path", dir, "--server", s.caesiumURL)
	s.Require().NoError(err,
		"caesium job apply must authenticate with CAESIUM_API_KEY:\nstdout: %s\nstderr: %s", stdout, stderr)
	s.Contains(stdout, "Applied 1 job definition(s)")

	applied := s.requireJobByAlias(alias)
	s.Require().NotNil(applied)

	// And the --api-key flag reaches the same place, with its "visible in
	// process listings" warning on stderr rather than stdout.
	stdout, stderr, err = s.runCLIWithEnv(
		[]string{"CAESIUM_API_KEY=", "CAESIUM_AUTH_ADMIN_KEY="},
		"job", "apply", "--path", dir, "--server", s.caesiumURL, "--api-key", s.authAPIKey,
	)
	s.Require().NoError(err, "--api-key must authenticate too:\nstdout: %s\nstderr: %s", stdout, stderr)
	s.Contains(stdout, "Applied 1 job definition(s)")
	s.Contains(stderr, "--api-key is visible in process listings")
	s.NotContains(stdout, "--api-key is visible in process listings")
}

func containsAPIKeyID(keys []apiKeyMetadata, id string) bool {
	for _, key := range keys {
		if key.ID == id {
			return true
		}
	}
	return false
}

func auditContainsResource(entries []auditEntry, resourceID string) bool {
	for _, entry := range entries {
		if entry.ResourceID == resourceID {
			return true
		}
	}
	return false
}
