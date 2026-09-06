//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/google/uuid"
)

// Stream C1/C5 — the job-scoped principal, exercised against the LIVE auth
// lane rather than a hand-built echo instance.
//
// C1: GET /auth/whoami is IDENTITY, not resource access, so every authenticated
// API-key principal reaches it. C5: passing that gate grants nothing else — the
// allow/deny matrix below pins every branch of
// api/middleware/auth_scope.go `authorizeScope` that a job-scoped key can hit.

// scopedJobFixture is three real jobs plus an operator key scoped to two of
// them, minted through `caesium auth key create --scope-jobs`.
//
// aliasApplyOnly exists because an apply is refused with 409 while the job has
// a running run (internal/jobdef/importer.go ensureJobNotRunningTx), and the
// matrix deliberately starts runs of aliasInScope. Keeping the positive-apply
// target separate makes that row deterministic; it also proves a multi-alias
// scope really carries both aliases.
type scopedJobFixture struct {
	aliasInScope    string
	aliasApplyOnly  string
	aliasOutOfScope string
	jobInScope      *jobSummary
	jobOutOfScope   *jobSummary
	key             createdAPIKey
}

// newScopedJobFixture applies the fixture jobs with the admin credential and
// mints an operator-role key scoped to the two in-scope aliases.
//
// The key is minted at OPERATOR (not viewer) deliberately: the matrix has to
// reach POST /v1/jobs/:id/run (runner) and POST /v1/jobdefs/apply (operator),
// and a lower role would make those rows 403 on the RBAC role gate instead of
// on the scope gate — pinning the wrong thing entirely.
func (s *IntegrationTestSuite) newScopedJobFixture(label string) scopedJobFixture {
	s.T().Helper()

	stamp := time.Now().UnixNano()
	fixture := scopedJobFixture{
		aliasInScope:    fmt.Sprintf("auth-scoped-%s-in-%d", label, stamp),
		aliasApplyOnly:  fmt.Sprintf("auth-scoped-%s-apply-%d", label, stamp),
		aliasOutOfScope: fmt.Sprintf("auth-scoped-%s-out-%d", label, stamp),
	}

	s.applyScopedJobDefinition(s.authAPIKey, fixture.aliasInScope, false)
	s.applyScopedJobDefinition(s.authAPIKey, fixture.aliasApplyOnly, false)
	s.applyScopedJobDefinition(s.authAPIKey, fixture.aliasOutOfScope, false)

	fixture.jobInScope = s.requireJobByAlias(fixture.aliasInScope)
	fixture.jobOutOfScope = s.requireJobByAlias(fixture.aliasOutOfScope)

	fixture.key = s.createAPIKeyCLI(
		"--role", "operator",
		"--description", "auth-lane scoped key "+label,
		"--scope-jobs", fixture.aliasInScope+","+fixture.aliasApplyOnly,
	)
	s.Require().NotNil(fixture.key.Meta.Scope, "the minted key must carry a job scope")
	s.Require().ElementsMatch(
		[]string{fixture.aliasInScope, fixture.aliasApplyOnly},
		fixture.key.Meta.Scope.Jobs,
		"--scope-jobs must persist every alias it was given",
	)

	return fixture
}

// scopedJobDefinitionPayload renders the POST /v1/jobdefs/apply body for a
// single trivial job.
//
// The matrix drives this endpoint directly rather than through `caesium job
// apply` because it has to pin `parseApplyAliasesForScope`'s branches exactly —
// prune on/off and an out-of-scope alias in the body — which is a property of
// the REQUEST, not of the CLI. (`caesium job apply` does now authenticate; the
// CLI path is covered by TestAuthJobApplyCLI.)
func (s *IntegrationTestSuite) scopedJobDefinitionPayload(alias string, prune bool) string {
	return fmt.Sprintf(`{"definitions":[{
  "apiVersion": "v1",
  "kind": "Job",
  "metadata": {"alias": %q},
  "trigger": {"type": "cron", "configuration": {"cron": "0 2 * * *"}},
  "steps": [{"name": "run", "image": "alpine:3.23", "engine": %q, "command": ["sh", "-c", "echo scoped-ok"]}]
}], "prune": %t}`, alias, s.engineType, prune)
}

func (s *IntegrationTestSuite) applyScopedJobDefinition(token, alias string, prune bool) {
	s.T().Helper()

	status, body := s.requestWithKey(
		http.MethodPost, "/v1/jobdefs/apply", token,
		strings.NewReader(s.scopedJobDefinitionPayload(alias, prune)),
	)
	s.Require().Equal(http.StatusOK, status, "applying %s: %s", alias, body)
}

// streamStatusWithKey issues a GET that may answer with an open SSE stream.
// The plain requestWithKey helper cannot be used for /v1/events: on a 200 the
// body never ends, so io.ReadAll would block until the test deadline.
func (s *IntegrationTestSuite) streamStatusWithKey(path, token string) (int, string) {
	s.T().Helper()

	ctx, cancel := context.WithTimeout(s.T().Context(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.caesiumURL+path, nil)
	s.Require().NoError(err)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	s.Require().NoError(err)
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		// A live stream: the status is the whole assertion, so do not read.
		return resp.StatusCode, ""
	}
	out, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, string(out)
}

// TestScopedKeyWhoamiAllowed is C1's positive scenario: a key minted with
// `caesium auth key create --scope-jobs` resolves its own principal through
// GET /auth/whoami, and the answer names the scope.
//
// Before C1, authorizeScope had no /auth/whoami case, so a scoped key fell to
// the trailing "insufficient permissions" deny — which is also why a scoped key
// could never complete the UI's api-key login.
func (s *IntegrationTestSuite) TestScopedKeyWhoamiAllowed() {
	s.requireAuthLane()

	alias := fmt.Sprintf("auth-scoped-whoami-%d", time.Now().UnixNano())
	s.applyScopedJobDefinition(s.authAPIKey, alias, false)

	scoped := s.createAPIKeyCLI(
		"--role", "viewer",
		"--description", "auth-lane whoami scoped key",
		"--scope-jobs", alias,
	)

	status, body := s.requestWithKey(http.MethodGet, "/auth/whoami", scoped.Plaintext, nil)
	s.Require().Equal(http.StatusOK, status,
		"a job-scoped key must be able to resolve its own principal: %s", body)

	var principal struct {
		Kind    string `json:"kind"`
		Subject string `json:"subject"`
		Role    string `json:"role"`
		Scope   *struct {
			Jobs []string `json:"jobs"`
		} `json:"scope"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &principal), body)
	s.Equal("api_key", principal.Kind)
	s.Equal("viewer", principal.Role)
	s.Equal(scoped.Meta.KeyPrefix, principal.Subject)
	s.Require().NotNil(principal.Scope, "whoami must name a scoped key's scope: %s", body)
	s.Equal([]string{alias}, principal.Scope.Jobs)

	// Identity only: the same key is still denied every cross-job route (the
	// full matrix lives in TestScopedKeyAllowDenyMatrix; this is the one-line
	// guard that the whoami allow did not become a general one).
	status, body = s.requestWithKey(http.MethodGet, "/v1/stats", scoped.Plaintext, nil)
	s.Equal(http.StatusForbidden, status,
		"the whoami allow must not widen a scoped key's reach: %s", body)
}

// TestScopedKeyAllowDenyMatrix is C5: one table over every authorizeScope
// branch a job-scoped principal can reach. Each row names the case it pins so a
// future edit to the switch cannot quietly drop a deny.
func (s *IntegrationTestSuite) TestScopedKeyAllowDenyMatrix() {
	s.requireAuthLane()

	fixture := s.newScopedJobFixture("matrix")
	token := fixture.key.Plaintext

	// A real run of the in-scope job, so the /v1/events run_id branch resolves
	// a genuine run→job→alias chain rather than a synthetic id.
	runID := s.triggerRun(fixture.jobInScope.ID)
	missingRunID := uuid.New().String()

	// ALLOW: GET /v1/jobs is filtered to the scoped aliases (the middleware
	// injects them; api/rest/controller/job/list.go applies them), so the
	// out-of-scope job must NOT be visible.
	status, body := s.requestWithKey(http.MethodGet, "/v1/jobs", token, nil)
	s.Require().Equal(http.StatusOK, status, body)
	var listed []struct {
		Alias string `json:"alias"`
	}
	s.Require().NoError(json.Unmarshal([]byte(body), &listed), body)
	s.Require().NotEmpty(listed, "the scoped list must contain the in-scope jobs")
	inScope := map[string]bool{fixture.aliasInScope: true, fixture.aliasApplyOnly: true}
	seen := make(map[string]bool, len(listed))
	for _, entry := range listed {
		s.Truef(inScope[entry.Alias],
			"GET /v1/jobs must be filtered to the key's scope, saw %q", entry.Alias)
		seen[entry.Alias] = true
	}
	s.True(seen[fixture.aliasInScope], "the scoped list must include %s", fixture.aliasInScope)
	s.False(seen[fixture.aliasOutOfScope], "the out-of-scope job must never be listed")

	rows := []struct {
		name    string
		pins    string
		method  string
		path    string
		body    string
		want    int
		message string
	}{
		{
			name:   "in-scope job read",
			pins:   `authorizeScope strings.HasPrefix(routePath, "/v1/jobs/:id") — alias in scope`,
			method: http.MethodGet,
			path:   "/v1/jobs/" + fixture.jobInScope.ID,
			want:   http.StatusOK,
		},
		{
			name:   "in-scope manual run",
			pins:   `authorizeScope "/v1/jobs/:id" prefix on a mutating route`,
			method: http.MethodPost,
			path:   "/v1/jobs/" + fixture.jobInScope.ID + "/run",
			want:   http.StatusAccepted,
		},
		{
			name:   "in-scope jobdef apply without prune",
			pins:   `authorizeScope case "/v1/jobdefs/apply" — every alias in scope`,
			method: http.MethodPost,
			path:   "/v1/jobdefs/apply",
			body:   s.scopedJobDefinitionPayload(fixture.aliasApplyOnly, false),
			want:   http.StatusOK,
		},
		{
			name:   "out-of-scope job read",
			pins:   `authorizeScope "/v1/jobs/:id" prefix — auth.CheckScope miss`,
			method: http.MethodGet,
			path:   "/v1/jobs/" + fixture.jobOutOfScope.ID,
			want:   http.StatusForbidden,
		},
		{
			name:   "out-of-scope manual run",
			pins:   `authorizeScope "/v1/jobs/:id" prefix — auth.CheckScope miss on a write`,
			method: http.MethodPost,
			path:   "/v1/jobs/" + fixture.jobOutOfScope.ID + "/run",
			want:   http.StatusForbidden,
		},
		{
			name:    "global lineage impact",
			pins:    `authorizeScope case "/v1/lineage/impact"`,
			method:  http.MethodGet,
			path:    "/v1/lineage/impact?namespace=caesium&name=missing",
			want:    http.StatusForbidden,
			message: authmw.LineageImpactScopedDenyMessage,
		},
		{
			name:    "global contracts graph",
			pins:    `authorizeScope case "/v1/contracts/graph"`,
			method:  http.MethodGet,
			path:    "/v1/contracts/graph",
			want:    http.StatusForbidden,
			message: authmw.ContractsGraphScopedDenyMessage,
		},
		{
			name:   "jobdef apply with prune",
			pins:   `authorizeScope case "/v1/jobdefs/apply" — prune is refused outright`,
			method: http.MethodPost,
			path:   "/v1/jobdefs/apply",
			body:   s.scopedJobDefinitionPayload(fixture.aliasApplyOnly, true),
			want:   http.StatusForbidden,
		},
		{
			name:   "jobdef apply naming an out-of-scope alias",
			pins:   `authorizeScope case "/v1/jobdefs/apply" — auth.CheckScope miss`,
			method: http.MethodPost,
			path:   "/v1/jobdefs/apply",
			body:   s.scopedJobDefinitionPayload(fixture.aliasOutOfScope, false),
			want:   http.StatusForbidden,
		},
		{
			name:   "unrelated global route",
			pins:   "authorizeScope trailing deny (no case, no /v1/jobs/:id prefix)",
			method: http.MethodGet,
			path:   "/v1/stats",
			want:   http.StatusForbidden,
		},
	}

	for _, row := range rows {
		s.Run(row.name, func() {
			var reader io.Reader
			if row.body != "" {
				reader = strings.NewReader(row.body)
			}
			status, body := s.requestWithKey(row.method, row.path, token, reader)
			s.Require().Equalf(row.want, status, "%s %s pins %s\nbody: %s",
				row.method, row.path, row.pins, body)
			if row.message != "" {
				s.Containsf(body, row.message, "%s must carry its specific deny reason", row.pins)
			}
		})
	}

	// The /v1/events rows are separated out: a 200 there is an OPEN SSE stream,
	// so the body must never be drained.
	s.Run("in-scope event stream", func() {
		status, body := s.streamStatusWithKey("/v1/events?run_id="+runID, token)
		s.Equalf(http.StatusOK, status,
			`pins authorizeScope case "/v1/events" — run resolves to an in-scope alias: %s`, body)
	})

	s.Run("event stream without run_id", func() {
		status, body := s.streamStatusWithKey("/v1/events", token)
		s.Require().Equalf(http.StatusForbidden, status,
			`pins authorizeScope case "/v1/events" — a scoped principal may not open the global stream: %s`, body)
		s.Contains(body, authmw.EventsScopedDenyMessage)
	})

	s.Run("event stream for an unknown run", func() {
		status, body := s.streamStatusWithKey("/v1/events?run_id="+missingRunID, token)
		s.Equalf(http.StatusNotFound, status,
			`pins authorizeScope case "/v1/events" — gorm.ErrRecordNotFound maps to 404, not 403: %s`, body)
	})
}
