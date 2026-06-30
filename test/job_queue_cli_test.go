//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
)

type queueCLIItem struct {
	ID         string            `json:"id"`
	Position   int               `json:"position"`
	Priority   int               `json:"priority"`
	Params     map[string]string `json:"params,omitempty"`
	EnqueuedAt string            `json:"enqueued_at"`
}

func (s *IntegrationTestSuite) TestJobQueueCLIListsPendingRun() {
	alias := fmt.Sprintf("e2e-job-queue-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(jobQueueManifest(alias))
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	firstRunID := s.postJobQueueRun(job.ID, "low", map[string]string{"lane": "active"})
	s.Require().NotEmpty(firstRunID)
	secondRunID := s.postJobQueueRun(job.ID, "high", map[string]string{"lane": "queued"})
	s.Empty(secondRunID, "queued admission should return 202 with no created run")

	var stdout, stderr string
	var err error
	queueListed := s.Eventually(func() bool {
		stdout, stderr, err = s.runCLISeparate("job", "queue", alias, "--server", s.caesiumURL)
		return err == nil && strings.Contains(stdout, "lane=queued")
	}, 10*time.Second, 250*time.Millisecond, "job queue CLI should list the pending queued run")
	// stderr may carry the live server's debug-level logs; assert only that no WARN/ERROR
	// diagnostics surfaced — the clean-stdout check below is the real machine-output gate.
	s.NotContains(stderr, `"level":"warn"`, "job queue should surface no warnings on success")
	s.NotContains(stderr, `"level":"error"`, "job queue should surface no errors on success")
	s.NotContains(stdout, `"level":"`, "job queue stdout should not contain structured logs")
	s.Require().True(queueListed, "job queue CLI should list the pending queued run")
	s.Require().NoError(err, "caesium job queue failed:\nstdout=%s\nstderr=%s", stdout, stderr)

	lines := nonEmptyLines(stdout)
	s.Require().GreaterOrEqual(len(lines), 2, "queue table should have a header and row:\n%s", stdout)
	s.Equal([]string{"POSITION", "PRIORITY", "ENQUEUED_AT", "PARAMS"}, strings.Fields(lines[0]))
	fields := strings.Fields(lines[1])
	s.Require().GreaterOrEqual(len(fields), 4, "queue row should be parseable:\n%s", lines[1])
	s.Equal("1", fields[0])
	s.Equal("high", fields[1])
	s.Contains(fields[3], "lane=queued")

	jsonOut, jsonErr, jsonCmdErr := s.runCLISeparate("job", "queue", alias, "--json", "--server", s.caesiumURL)
	s.Require().NoError(jsonCmdErr, "caesium job queue --json failed:\nstdout=%s\nstderr=%s", jsonOut, jsonErr)
	s.NotContains(jsonErr, `"level":"warn"`, "job queue --json should surface no warnings on success")
	s.NotContains(jsonErr, `"level":"error"`, "job queue --json should surface no errors on success")
	s.Require().True(json.Valid([]byte(jsonOut)), "job queue --json stdout was not clean JSON:\n%s", jsonOut)
	var rows []queueCLIItem
	s.Require().NoError(json.Unmarshal([]byte(jsonOut), &rows))
	s.Require().Len(rows, 1)
	s.Require().NotEmpty(rows[0].ID)
	s.Require().NoError(uuid.Validate(rows[0].ID))
	s.Equal(1, rows[0].Position)
	s.Equal(3, rows[0].Priority)
	s.Equal("queued", rows[0].Params["lane"])
	s.NotEmpty(rows[0].EnqueuedAt)
}

func TestJobQueueRouteAllowsInScopeScopedKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	now := time.Now().UTC()
	triggerID := uuid.New()
	jobID := uuid.New()
	requireNoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)
	requireNoError(t, db.Create(&models.Job{
		ID:        jobID,
		Alias:     "queue-scope-alpha",
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)

	svc := iauth.NewService(db)
	auditor := iauth.NewAuditLogger(db)
	limiter := iauth.NewRateLimiter(10, time.Minute)
	inScopeKey := createScopedQueueKey(t, svc, "queue-scope-alpha")
	outOfScopeKey := createScopedQueueKey(t, svc, "queue-scope-beta")

	status, err := callJobQueueScopedRoute(t, svc, auditor, limiter, inScopeKey, jobID)
	requireNoError(t, err)
	if status != http.StatusOK {
		t.Fatalf("expected in-scope job queue read to succeed, got %d", status)
	}

	_, err = callJobQueueScopedRoute(t, svc, auditor, limiter, outOfScopeKey, jobID)
	if err == nil {
		t.Fatalf("expected out-of-scope job queue read to be denied")
	}
	he, ok := err.(*echo.HTTPError)
	if !ok || he.Code != http.StatusForbidden {
		t.Fatalf("expected out-of-scope job queue read to return 403, got %#v", err)
	}
}

func (s *IntegrationTestSuite) postJobQueueRun(jobID, priority string, params map[string]string) string {
	s.T().Helper()

	body, err := json.Marshal(map[string]any{
		"priority": priority,
		"params":   params,
	})
	s.Require().NoError(err)

	resp, err := s.doJSONRequest(http.MethodPost, fmt.Sprintf("%s/v1/jobs/%s/run", s.caesiumURL, jobID), bytes.NewReader(body))
	s.Require().NoError(err)
	defer resp.Body.Close()
	s.Require().Equal(http.StatusAccepted, resp.StatusCode)

	var run struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		return ""
	}
	return run.ID
}

func jobQueueManifest(alias string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  concurrency:
    maxRuns: 1
    strategy: queue
trigger:
  type: cron
  configuration:
    cron: "0 0 1 1 *"
steps:
  - name: hold
    image: alpine:3.23
    command: ["sh", "-c", "sleep 30"]
`, alias)
}

func nonEmptyLines(s string) []string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func createScopedQueueKey(t *testing.T, svc *iauth.Service, alias string) string {
	t.Helper()
	resp, err := svc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		Scope:     &models.KeyScope{Jobs: []string{alias}},
		CreatedBy: "job-queue-test",
	})
	requireNoError(t, err)
	return resp.Plaintext
}

func callJobQueueScopedRoute(
	t *testing.T,
	svc *iauth.Service,
	auditor *iauth.AuditLogger,
	limiter *iauth.RateLimiter,
	key string,
	jobID uuid.UUID,
) (int, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/v1/jobs/"+jobID.String()+"/queue", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	pv := echo.PathValues{{Name: "id", Value: jobID.String()}}
	c.InitializeRoute(&echo.RouteInfo{Path: "/v1/jobs/:id/queue", Method: http.MethodGet}, &pv)

	handler := authmw.Auth(authmw.AuthDeps{
		Service: svc,
		Auditor: auditor,
		Limiter: limiter,
	})(func(c *echo.Context) error {
		return c.String(http.StatusOK, "ok")
	})
	err := handler(c)
	return rec.Code, err
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
