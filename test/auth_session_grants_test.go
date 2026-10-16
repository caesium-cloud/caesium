//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// TestAuthSessionCookieWithoutGrantsIsRefused proves, against the running
// server, that a cookie session's access comes from grants resolved per
// request through the policy source initAuth installed, not from the role
// stored on the user row.
//
// The auth lane runs CAESIUM_AUTH_MODE=api-key with no SSO provider, so
// initAuth still installs a SessionStore (every AuthDeps site gets it), and
// the policy source is the lane's policy file
// (test/testdata/policy/auth-lane-reload.yaml), which binds no group this user
// holds: a valid cookie reaches PrincipalFromUser, resolves no grants, and must
// be refused. The seeded user stores role=admin and groups that would have
// mapped under any env mapping, so the old frozen-role behaviour would have
// answered 200 here.
//
// The session is minted with the real SessionStore over the live catalog, keyed
// with the lane's CAESIUM_AUTH_KEY_HASH_SECRET, exactly as a login would.
func (s *IntegrationTestSuite) TestAuthSessionCookieWithoutGrantsIsRefused() {
	s.requireAuthLane()
	vars := env.Variables()
	s.Require().NotEmpty(vars.AuthKeyHashSecret, "the auth lane runner must carry CAESIUM_AUTH_KEY_HASH_SECRET")

	conn := s.openIntegrationCatalogGorm()
	now := time.Now().UTC()
	user := &models.User{
		ID:            uuid.New(),
		Issuer:        "integration-seeded",
		Subject:       fmt.Sprintf("session-grants-%d", now.UnixNano()),
		Email:         fmt.Sprintf("session-grants-%d@example.com", now.UnixNano()),
		Groups:        datatypes.JSON(`["caesium-admins"]`),
		Role:          models.RoleAdmin, // the stale, frozen role: it must never grant
		EmailVerified: true,
		CreatedAt:     now,
		LastLoginAt:   &now,
	}
	s.Require().NoError(conn.Create(user).Error)
	defer func() {
		s.NoError(conn.Exec(`DELETE FROM sessions WHERE user_id = ?`, user.ID).Error)
		s.NoError(conn.Exec(`DELETE FROM users WHERE id = ?`, user.ID).Error)
	}()

	sessions := iauth.NewSessionStore(conn, iauth.WithSessionHashSecret(vars.AuthKeyHashSecret))
	token, _, err := sessions.Create(s.T().Context(), iauth.CreateSessionRequest{
		UserID:     user.ID,
		AuthMethod: "oidc",
		SourceIP:   "127.0.0.1",
		UserAgent:  "integration",
	})
	s.Require().NoError(err)
	cookie := &http.Cookie{Name: vars.AuthSessionCookieName, Value: token}

	// The session authenticates (401 would mean it was not even recognised) but
	// carries no grants, so a cluster route is refused.
	status, body := s.requestWithCookie(http.MethodGet, "/v1/atoms", cookie)
	s.Require().Equal(http.StatusForbidden, status,
		"a grant-less cookie session must be refused, not honoured with its stored role: %s", body)

	auditOut, auditErr, err := s.runCLISeparate(
		"auth", "audit",
		"--action", "auth.denied",
		"--actor", user.Email,
		"--limit", "50",
		"--server", s.caesiumURL,
	)
	s.Require().NoError(err, "caesium auth audit failed:\nstdout: %s\nstderr: %s", auditOut, auditErr)
	var denials []struct {
		Actor    string          `json:"actor"`
		Outcome  string          `json:"outcome"`
		Metadata json.RawMessage `json:"metadata"`
	}
	s.Require().NoError(json.Unmarshal([]byte(auditOut), &denials),
		"caesium auth audit stdout must be exactly one JSON array:\n%s", auditOut)
	s.Require().Len(denials, 1, "exactly one denial for the seeded session:\n%s", auditOut)
	s.Equal("denied", denials[0].Outcome)
	var metadata struct {
		KeyRole string `json:"key_role"`
		Reason  string `json:"reason"`
		Path    string `json:"path"`
	}
	s.Require().NoError(json.Unmarshal(denials[0].Metadata, &metadata))
	s.Equal("", metadata.KeyRole, "the session resolves no cluster role")
	s.Equal("insufficient_role", metadata.Reason)
	s.Equal("/v1/atoms", metadata.Path)

	// whoami still requires viewer at "*" (identity routes are classified
	// later), so it is refused the same way.
	status, body = s.requestWithCookie(http.MethodGet, "/auth/whoami", cookie)
	s.Equal(http.StatusForbidden, status, "whoami: %s", body)
}

// requestWithCookie issues an unauthenticated-by-header request that presents
// only the given session cookie.
func (s *IntegrationTestSuite) requestWithCookie(method, path string, cookie *http.Cookie) (int, string) {
	s.T().Helper()
	req, err := http.NewRequestWithContext(s.T().Context(), method, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	req.AddCookie(cookie)
	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}
