//go:build integration

package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// authPolicyPathEnv names the runner-side path of the policy file the auth
// lane's server serves (justfile integration-up-agent bind-mounts its
// directory read-only into the server; integration-test-agent passes this).
const authPolicyPathEnv = "CAESIUM_IT_AUTH_POLICY_PATH"

// authPolicyPollWait bounds how long a scenario waits for the lane's poller
// (CAESIUM_AUTH_POLICY_RELOAD_INTERVAL=1s) to pick up a rewritten file.
const authPolicyPollWait = 15 * time.Second

type policyShowDoc struct {
	Source      string   `json:"source"`
	Path        string   `json:"path"`
	Revision    string   `json:"revision"`
	DefaultRole string   `json:"default_role"`
	Warnings    []string `json:"warnings"`
	LastReload  *struct {
		Trigger  string `json:"trigger"`
		OK       bool   `json:"ok"`
		Changed  bool   `json:"changed"`
		Revision string `json:"revision"`
		Error    string `json:"error"`
	} `json:"last_reload"`
	Policy struct {
		Bindings []struct {
			Subjects struct {
				Groups []string `json:"groups"`
			} `json:"subjects"`
			Role       string   `json:"role"`
			Namespaces []string `json:"namespaces"`
		} `json:"bindings"`
	} `json:"policy"`
}

type policyReloadDoc struct {
	Changed          bool   `json:"changed"`
	Revision         string `json:"revision"`
	PreviousRevision string `json:"previous_revision"`
}

type policyReloadFailureDoc struct {
	Message  string `json:"message"`
	Revision string `json:"revision"`
}

type policyLintDoc struct {
	Path       string   `json:"path"`
	Valid      bool     `json:"valid"`
	Revision   string   `json:"revision"`
	Error      string   `json:"error"`
	Warnings   []string `json:"warnings"`
	Namespaces []string `json:"namespaces"`
	Bindings   int      `json:"bindings"`
}

type policyAuditRow struct {
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	ResourceID string          `json:"resource_id"`
	SourceIP   string          `json:"source_ip"`
	Outcome    string          `json:"outcome"`
	Metadata   json.RawMessage `json:"metadata"`
}

type policyAuditMetadata struct {
	Trigger          string `json:"trigger"`
	Revision         string `json:"revision"`
	PreviousRevision string `json:"previous_revision"`
	Changed          bool   `json:"changed"`
	Error            string `json:"error"`
}

func (r policyAuditRow) meta(s *IntegrationTestSuite) policyAuditMetadata {
	s.T().Helper()
	var m policyAuditMetadata
	s.Require().NoError(json.Unmarshal(r.Metadata, &m), "audit metadata: %s", r.Metadata)
	return m
}

// TestAuthPolicyReload drives the policy file reload through the CLI against
// the live auth-lane server: grants resolve per request from the file, a
// rewritten file swaps on `caesium auth policy reload`, an invalid one is
// rejected with the last good revision kept, and every API attempt is audited.
func (s *IntegrationTestSuite) TestAuthPolicyReload() {
	s.requireAuthLane()
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() {
		s.restoreAuthPolicy(path, original)
		if _, _, err := s.runCLISeparate("auth", "policy", "reload", "--server", s.caesiumURL); err != nil {
			s.T().Errorf("restore the auth-lane policy revision: %v", err)
		}
	})

	cookie := s.seedPolicySessionUser("iaa-reload-viewers")
	show := runCLIJSON[policyShowDoc](s, "auth", "policy", "show", "--json", "--server", s.caesiumURL)
	s.Equal(policy.SourceFile, show.Source)
	s.Equal(policy.Hash(original), show.Revision, "the revision is the SHA-256 of the served file's bytes")
	s.Require().NotEmpty(show.Policy.Bindings)
	s.Equal([]string{"iaa-reload-viewers"}, show.Policy.Bindings[0].Subjects.Groups)

	status, body := s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Require().Equal(http.StatusOK, status, "the file binds the session's group to viewer at *: %s", body)

	// Revoke: bind a different group, reload, and the very next request is
	// refused (grants resolve per request against the new revision).
	revoked := []byte(strings.Replace(string(original), "iaa-reload-viewers", "iaa-reload-nobody", 1))
	s.Require().NotEqual(string(original), string(revoked))
	s.writeAuthPolicy(path, revoked)
	reload := runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	// The 1s poller may have swapped first, in which case the API reload
	// reports changed=false; either way the revoked revision is in force.
	s.Equal(policy.Hash(revoked), reload.Revision)
	status, body = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Require().Equal(http.StatusForbidden, status, "the revoked binding must stop granting: %s", body)

	// Invalid: rejected, the revoked revision stays in force.
	invalid := []byte(fmt.Sprintf("apiVersion: v1\nkind: AccessPolicy\nbindings:\n  - subjects: {groups: [iaa-reload-viewers]}\n    role: superuser\n    namespaces: [\"*\"]\n# %s\n", uuid.NewString()))
	s.writeAuthPolicy(path, invalid)
	stdout, stderr, err := s.runCLISeparate("auth", "policy", "reload", "--server", s.caesiumURL)
	s.Require().Error(err, "an invalid policy file must fail the reload; stdout: %s", stdout)
	s.requireExitCode(err, 1)
	s.Contains(stderr, `invalid role "superuser"`)
	s.Contains(stderr, "revision "+policy.Hash(revoked)+" stays in force")
	s.Empty(strings.TrimSpace(stdout), "without --json a rejected reload prints nothing on stdout")

	failure, _ := runCLIJSONFailure[policyReloadFailureDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Equal(policy.Hash(revoked), failure.Revision)
	s.Contains(failure.Message, `invalid role "superuser"`)

	show = runCLIJSON[policyShowDoc](s, "auth", "policy", "show", "--json", "--server", s.caesiumURL)
	s.Equal(policy.Hash(revoked), show.Revision, "an invalid file never replaces the last good revision")
	s.Require().NotNil(show.LastReload)
	s.False(show.LastReload.OK)
	s.Equal(policy.Hash(invalid), show.LastReload.Revision)
	s.Contains(show.LastReload.Error, `invalid role "superuser"`)
	status, _ = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Equal(http.StatusForbidden, status)

	// Both API attempts are audited as the calling key; the poller adds at
	// most one row of its own for the same bad file.
	rows := s.policyAuditRows(iauth.ActionPolicyReloadFailed, policy.Hash(invalid))
	var api []policyAuditRow
	for _, row := range rows {
		if row.meta(s).Trigger == policy.TriggerAPI {
			api = append(api, row)
		}
	}
	s.Require().Len(api, 2, "one policy.reload_failed row per API attempt: %+v", rows)
	s.Require().GreaterOrEqual(len(s.authAPIKey), 13)
	for _, row := range api {
		s.Equal(s.authAPIKey[:13], row.Actor, "an API reload audits the caller's key prefix")
		s.Equal(iauth.OutcomeError, row.Outcome)
		s.NotEmpty(row.SourceIP)
		meta := row.meta(s)
		s.Contains(meta.Error, `invalid role "superuser"`)
		s.Equal(policy.Hash(revoked), meta.PreviousRevision)
	}
	s.LessOrEqual(len(rows)-len(api), 1, "the poller reports one bad file once: %+v", rows)
	s.NotEmpty(s.policyAuditRows(iauth.ActionPolicyReloaded, policy.Hash(revoked)), "the swap to the revoked revision is audited")

	// Restore: the original binding grants again.
	s.writeAuthPolicy(path, original)
	reload = runCLIJSON[policyReloadDoc](s, "auth", "policy", "reload", "--json", "--server", s.caesiumURL)
	s.Equal(policy.Hash(original), reload.Revision)
	status, body = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Equal(http.StatusOK, status, "%s", body)
}

// TestAuthPolicyPollReload is the same shape driven by the poller alone: no
// API reload is issued. A changed file is picked up within the bound, an
// invalid one keeps the last good revision and is audited exactly once.
func (s *IntegrationTestSuite) TestAuthPolicyPollReload() {
	s.requireAuthLane()
	path, original := s.authPolicyFixture()
	s.T().Cleanup(func() {
		s.restoreAuthPolicy(path, original)
		if !s.waitForPolicyRevision(policy.Hash(original)) {
			s.T().Errorf("the poller did not restore the auth-lane policy within %s", authPolicyPollWait)
		}
	})

	cookie := s.seedPolicySessionUser("iaa-poll-viewers")
	status, body := s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Require().Equal(http.StatusForbidden, status, "the lane fixture does not bind iaa-poll-viewers: %s", body)

	granted := []byte(fmt.Sprintf("apiVersion: v1\nkind: AccessPolicy\nbindings:\n  - subjects: {groups: [iaa-poll-viewers]}\n    role: viewer\n    namespaces: [\"*\"]\n# %s\n", uuid.NewString()))
	s.writeAuthPolicy(path, granted)
	s.Require().True(s.waitForPolicyRevision(policy.Hash(granted)), "the poller must pick up the changed file within %s", authPolicyPollWait)
	status, body = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Require().Equal(http.StatusOK, status, "the polled revision grants the session: %s", body)

	swaps := s.policyAuditRows(iauth.ActionPolicyReloaded, policy.Hash(granted))
	s.Require().Len(swaps, 1, "%+v", swaps)
	s.Equal(iauth.ActorPolicyLoader, swaps[0].Actor)
	s.Equal(policy.TriggerPoll, swaps[0].meta(s).Trigger)
	s.True(swaps[0].meta(s).Changed)

	invalid := []byte(fmt.Sprintf("apiVersion: v1\nkind: AccessPolicy\nbindings: [%q]\n", uuid.NewString()))
	s.writeAuthPolicy(path, invalid)
	deadline := time.Now().Add(authPolicyPollWait)
	var show policyShowDoc
	for {
		show = runCLIJSON[policyShowDoc](s, "auth", "policy", "show", "--json", "--server", s.caesiumURL)
		if show.LastReload != nil && !show.LastReload.OK && show.LastReload.Revision == policy.Hash(invalid) {
			break
		}
		s.Require().True(time.Now().Before(deadline), "the poller did not report the invalid file within %s: %+v", authPolicyPollWait, show.LastReload)
		time.Sleep(250 * time.Millisecond)
	}
	s.Equal(policy.TriggerPoll, show.LastReload.Trigger)
	s.Contains(show.LastReload.Error, "expected a mapping")
	s.Equal(policy.Hash(granted), show.Revision, "the poller keeps the last good revision")
	status, _ = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Equal(http.StatusOK, status)

	// Several more intervals pass with the same bad file: still one row.
	time.Sleep(4 * time.Second)
	failed := s.policyAuditRows(iauth.ActionPolicyReloadFailed, policy.Hash(invalid))
	s.Require().Len(failed, 1, "one policy.reload_failed per distinct bad file, not one per interval: %+v", failed)
	s.Equal(iauth.ActorPolicyLoader, failed[0].Actor)
	s.Equal(policy.TriggerPoll, failed[0].meta(s).Trigger)
	s.Equal(policy.Hash(granted), failed[0].meta(s).PreviousRevision)

	s.restoreAuthPolicy(path, original)
	s.Require().True(s.waitForPolicyRevision(policy.Hash(original)), "the poller must pick up the restored file")
	status, _ = s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Equal(http.StatusForbidden, status, "the restored file no longer binds iaa-poll-viewers")
}

// TestAuthPolicyLintOffline drives `caesium auth policy lint`, which needs no
// server: valid, warning-only and invalid fixtures, with --json stdout held to
// exactly one JSON document and warnings kept on stderr.
func (s *IntegrationTestSuite) TestAuthPolicyLintOffline() {
	dir := filepath.Join(s.projectRoot, "test", "testdata", "policy")

	validPath := filepath.Join(dir, "lint-valid.yaml")
	validBytes, err := os.ReadFile(validPath)
	s.Require().NoError(err)
	stdout, stderr, err := s.runCLISeparate("auth", "policy", "lint", "--path", validPath, "--json")
	s.Require().NoError(err, "stderr: %s", stderr)
	var valid policyLintDoc
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &valid))
	s.True(valid.Valid)
	s.Equal(validPath, valid.Path)
	s.Equal(policy.Hash(validBytes), valid.Revision)
	s.Equal([]string{"default", "marketing"}, valid.Namespaces)
	s.Equal(2, valid.Bindings)
	s.Empty(valid.Warnings)
	s.NotContains(stderr, "warning:")

	warnPath := filepath.Join(dir, "lint-warnings.yaml")
	stdout, stderr, err = s.runCLISeparate("auth", "policy", "lint", "--path", warnPath, "--json")
	s.Require().NoError(err, "warnings are not errors; stderr: %s", stderr)
	var warned policyLintDoc
	s.Require().NoError(decodeCLIJSONStdout([]byte(stdout), &warned), "warnings must not leak onto stdout")
	s.True(warned.Valid)
	s.Len(warned.Warnings, 2)
	s.Contains(stderr, `warning: namespace "finance" has no secret rules; all secrets are allowed`)
	s.Contains(stderr, "warning: bindings[0].subjects.users: only OIDC logins with email_verified=true")

	invalidPath := filepath.Join(dir, "lint-invalid.yaml")
	invalid, stderr := runCLIJSONFailure[policyLintDoc](s, "auth", "policy", "lint", "--path", invalidPath, "--json")
	s.False(invalid.Valid)
	s.Contains(invalid.Error, `undeclared namespace "finance"`)
	s.Contains(stderr, `undeclared namespace "finance"`)

	stdout, stderr, err = s.runCLISeparate("auth", "policy", "lint", "--path", invalidPath)
	s.Require().Error(err)
	s.requireExitCode(err, 1)
	s.Empty(strings.TrimSpace(stdout))
	s.Contains(stderr, `undeclared namespace "finance"`)

	// The human summary for a valid file goes to stdout.
	stdout, _, err = s.runCLISeparate("auth", "policy", "lint", "--path", validPath)
	s.Require().NoError(err)
	s.Contains(stdout, "is valid: revision "+policy.Hash(validBytes))
}

// authPolicyFixture returns the served policy file's path and its current
// bytes. On the auth lane a missing path is a misconfigured lane, not a skip.
func (s *IntegrationTestSuite) authPolicyFixture() (string, []byte) {
	s.T().Helper()
	path := strings.TrimSpace(os.Getenv(authPolicyPathEnv))
	s.Require().NotEmpty(path, "the auth lane must pass %s to the runner (justfile integration-test-agent)", authPolicyPathEnv)
	original, err := os.ReadFile(path)
	s.Require().NoError(err, "read the auth-lane policy file")
	return path, original
}

// writeAuthPolicy replaces the served file atomically (temp file and rename in
// the same directory), as a ConfigMap update does, so the server never reads
// a half-written file.
func (s *IntegrationTestSuite) writeAuthPolicy(path string, data []byte) {
	s.T().Helper()
	s.Require().NoError(writePolicyFileAtomically(path, data))
}

func (s *IntegrationTestSuite) restoreAuthPolicy(path string, original []byte) {
	if err := writePolicyFileAtomically(path, original); err != nil {
		s.T().Errorf("restore the auth-lane policy file: %v", err)
	}
}

func writePolicyFileAtomically(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".policy-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// waitForPolicyRevision polls `caesium auth policy show --json` until the
// server reports revision, within authPolicyPollWait.
func (s *IntegrationTestSuite) waitForPolicyRevision(revision string) bool {
	s.T().Helper()
	deadline := time.Now().Add(authPolicyPollWait)
	for {
		stdout, err := s.runCLIStdout("auth", "policy", "show", "--json", "--server", s.caesiumURL)
		if err == nil {
			var show policyShowDoc
			if decodeCLIJSONStdout([]byte(stdout), &show) == nil && show.Revision == revision {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// policyAuditRows returns the audit rows for action whose resource is the
// given revision hash, read through `caesium auth audit` (always JSON).
func (s *IntegrationTestSuite) policyAuditRows(action, revision string) []policyAuditRow {
	s.T().Helper()
	rows := runCLIJSON[[]policyAuditRow](s, "auth", "audit", "--action", action, "--limit", "200", "--server", s.caesiumURL)
	var matched []policyAuditRow
	for _, row := range rows {
		s.Equal(action, row.Action)
		if row.ResourceID == revision {
			matched = append(matched, row)
		}
	}
	return matched
}

// seedPolicySessionUser seeds a user holding exactly group, and a real
// session cookie for it, through the live catalog (the pattern of
// TestAuthSessionCookieWithoutGrantsIsRefused). The stored role is admin and
// must never grant: access comes only from the policy file. The email is
// unverified, so only the group can match.
func (s *IntegrationTestSuite) seedPolicySessionUser(group string) *http.Cookie {
	s.T().Helper()
	vars := env.Variables()
	s.Require().NotEmpty(vars.AuthKeyHashSecret, "the auth lane runner must carry CAESIUM_AUTH_KEY_HASH_SECRET")

	groups, err := json.Marshal([]string{group})
	s.Require().NoError(err)
	conn := s.openIntegrationCatalogGorm()
	now := time.Now().UTC()
	user := &models.User{
		ID:          uuid.New(),
		Issuer:      "integration-seeded",
		Subject:     fmt.Sprintf("%s-%d", group, now.UnixNano()),
		Email:       fmt.Sprintf("%s-%d@example.com", group, now.UnixNano()),
		Groups:      datatypes.JSON(groups),
		Role:        models.RoleAdmin,
		CreatedAt:   now,
		LastLoginAt: &now,
	}
	s.Require().NoError(conn.Create(user).Error)
	s.T().Cleanup(func() {
		s.NoError(conn.Exec(`DELETE FROM sessions WHERE user_id = ?`, user.ID).Error)
		s.NoError(conn.Exec(`DELETE FROM users WHERE id = ?`, user.ID).Error)
	})

	sessions := iauth.NewSessionStore(conn, iauth.WithSessionHashSecret(vars.AuthKeyHashSecret))
	token, _, err := sessions.Create(s.T().Context(), iauth.CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: "oidc",
		SourceIP:   "127.0.0.1",
		UserAgent:  "integration",
	})
	s.Require().NoError(err)
	return &http.Cookie{Name: vars.AuthSessionCookieName, Value: token}
}

func (s *IntegrationTestSuite) requireExitCode(err error, want int) {
	s.T().Helper()
	var exitErr *exec.ExitError
	s.Require().True(errors.As(err, &exitErr), "expected a process exit error, got %v", err)
	s.Equal(want, exitErr.ExitCode())
}
