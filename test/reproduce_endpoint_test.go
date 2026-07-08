//go:build integration

package test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	reproducesvc "github.com/caesium-cloud/caesium/api/rest/service/reproduce"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type reproduceDescriptorResponse struct {
	TaskRunID  string            `json:"task_run_id"`
	Status     string            `json:"status"`
	Result     string            `json:"result"`
	Output     map[string]string `json:"output"`
	ReplaySafe bool              `json:"replay_safe"`
	LogExcerpt struct {
		Path string `json:"path"`
	} `json:"log_excerpt"`
	Descriptor json.RawMessage `json:"descriptor"`
}

type reproduceDescriptorPayload struct {
	SchemaVersion int `json:"schemaVersion"`
	Baseline      struct {
		TaskID   string `json:"taskId"`
		TaskName string `json:"taskName"`
	} `json:"baseline"`
	Runtime struct {
		Image               string `json:"image"`
		ResolvedImageDigest string `json:"resolvedImageDigest"`
	} `json:"runtime"`
	Cache struct {
		PinDigests bool `json:"pinDigests"`
	} `json:"cache"`
	Run struct {
		Params map[string]string `json:"params"`
	} `json:"run"`
	DAG struct {
		PredecessorOutputs map[string]map[string]string `json:"predecessorOutputs"`
	} `json:"dag"`
	ContainerSpec struct {
		Env map[string]string `json:"env"`
	} `json:"containerSpec"`
}

func (s *IntegrationTestSuite) TestReproduceDescriptorEndpointRoundTrip() {
	alias := fmt.Sprintf("e2e-reproduce-descriptor-%d", time.Now().UnixNano())
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  replaySafe: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: produce
    image: alpine:3.23
    cache: { pinDigests: true }
    command: ["sh","-c","echo '##caesium::output {\"rows\":\"7\",\"source\":\"raw\"}'"]
    next: transform
  - name: transform
    image: alpine:3.23
    cache: { pinDigests: true }
    env:
      LITERAL_ENV: fixture-literal
    command: ["sh","-c","test \"$LITERAL_ENV\" = fixture-literal; test \"$CAESIUM_PARAM_FLAVOR\" = vanilla; test \"$CAESIUM_OUTPUT_PRODUCE_ROWS\" = 7; echo '##caesium::output {\"clean\":\"yes\",\"rows\":\"7\"}'"]
    dependsOn: produce
`, alias)
	dir := s.writeJobManifest(manifest)
	defer os.RemoveAll(dir)

	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)
	job := s.requireJobByAlias(alias)
	s.Require().NotNil(job)

	runID := s.triggerRunWithParams(job.ID, map[string]string{"flavor": "vanilla"})
	s.Require().Equal("succeeded", s.awaitRun(job.ID, runID, runTimeout).Status)

	status, body := s.getReproduceDescriptor(job.ID, runID, "transform")
	s.Require().Equal(http.StatusOK, status, string(body))

	var resp reproduceDescriptorResponse
	s.Require().NoError(json.Unmarshal(body, &resp))
	s.Require().NoError(uuid.Validate(resp.TaskRunID))
	s.Equal("succeeded", resp.Status)
	s.Equal(map[string]string{"clean": "yes", "rows": "7"}, resp.Output)
	s.True(resp.ReplaySafe)
	s.Require().True(json.Valid(resp.Descriptor), "descriptor is not valid JSON: %s", resp.Descriptor)

	var desc reproduceDescriptorPayload
	s.Require().NoError(json.Unmarshal(resp.Descriptor, &desc))
	s.Equal(1, desc.SchemaVersion)
	s.Equal("transform", desc.Baseline.TaskName)
	s.Require().NoError(uuid.Validate(desc.Baseline.TaskID))
	s.Equal("alpine:3.23", desc.Runtime.Image)
	// Digest resolution is a docker-engine behavior: the podman and kubernetes
	// lanes resolve through engine paths that legitimately fall back to the
	// mutable tag (reproduce marks such pulls DEGRADED). Assert the recorded
	// digest only where the resolver actually runs.
	if s.engineType == "" || s.engineType == "docker" {
		s.NotEmpty(desc.Runtime.ResolvedImageDigest, "pinDigests fixture should record a resolved image digest")
		s.Contains(desc.Runtime.ResolvedImageDigest, "sha256:")
	} else {
		s.T().Logf("skipping resolved-digest assertion under CAESIUM_TEST_ENGINE=%s; digest recording is covered on the docker lane", s.engineType)
	}
	s.True(desc.Cache.PinDigests)
	s.Equal("fixture-literal", desc.ContainerSpec.Env["LITERAL_ENV"])
	s.Equal("vanilla", desc.Run.Params["flavor"])
	s.True(hasPredecessorOutput(desc.DAG.PredecessorOutputs, "rows", "7"),
		"descriptor should include predecessor rows output, got %+v", desc.DAG.PredecessorOutputs)
	s.True(hasPredecessorOutput(desc.DAG.PredecessorOutputs, "source", "raw"),
		"descriptor should include predecessor source output, got %+v", desc.DAG.PredecessorOutputs)
	s.Equal(fmt.Sprintf("/v1/jobs/%s/runs/%s/logs?task_id=%s", job.ID, runID, desc.Baseline.TaskID), resp.LogExcerpt.Path)

	uuidStatus, uuidBody := s.getReproduceDescriptor(job.ID, runID, desc.Baseline.TaskID)
	s.Require().Equal(http.StatusOK, uuidStatus, string(uuidBody))
	var uuidResp reproduceDescriptorResponse
	s.Require().NoError(json.Unmarshal(uuidBody, &uuidResp))
	s.Equal(resp.TaskRunID, uuidResp.TaskRunID)
	s.JSONEq(string(resp.Descriptor), string(uuidResp.Descriptor))

	if s.engineType == "kubernetes" {
		s.T().Logf("skipping absent-descriptor DB mutation under CAESIUM_TEST_ENGINE=%s; dqlite is not port-forward-reachable", s.engineType)
		return
	}

	catalogDB := s.openIntegrationCatalogDB()
	defer func() { s.Require().NoError(catalogDB.Close()) }()
	s.Require().NoError(clearTaskExecutionDescriptor(s.T().Context(), catalogDB, resp.TaskRunID))

	missingStatus, missingBody := s.getReproduceDescriptor(job.ID, runID, "transform")
	s.Equal(http.StatusNotFound, missingStatus, string(missingBody))
	s.Contains(string(missingBody), "descriptor unavailable")
}

func TestReproduceDescriptorRouteAllowsInScopeScopedKey(t *testing.T) {
	conn := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(conn) })

	jobID, runID, taskName := seedDescriptorScopeFixture(t, conn, "descriptor-scope-alpha")
	authSvc := iauth.NewService(conn)
	auditor := iauth.NewAuditLogger(conn)
	limiter := iauth.NewRateLimiter(10, time.Minute)
	inScopeKey := createScopedDescriptorKey(t, authSvc, "descriptor-scope-alpha")
	outOfScopeKey := createScopedDescriptorKey(t, authSvc, "descriptor-scope-beta")

	status, body, err := callDescriptorScopedRoute(t, conn, authSvc, auditor, limiter, inScopeKey, jobID, runID, taskName)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, `"descriptor"`)

	_, _, err = callDescriptorScopedRoute(t, conn, authSvc, auditor, limiter, outOfScopeKey, jobID, runID, taskName)
	require.Error(t, err)
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok, "expected echo HTTP error, got %#v", err)
	require.Equal(t, http.StatusForbidden, he.Code)
}

func (s *IntegrationTestSuite) getReproduceDescriptor(jobID, runID, task string) (int, []byte) {
	s.T().Helper()

	path := fmt.Sprintf(
		"%s/v1/jobs/%s/runs/%s/tasks/%s/descriptor",
		s.caesiumURL,
		url.PathEscape(jobID),
		url.PathEscape(runID),
		url.PathEscape(task),
	)
	resp, err := s.doRequest(http.MethodGet, path, nil)
	s.Require().NoError(err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	return resp.StatusCode, body
}

func hasPredecessorOutput(outputs map[string]map[string]string, key, value string) bool {
	for _, values := range outputs {
		if values[key] == value {
			return true
		}
	}
	return false
}

func clearTaskExecutionDescriptor(ctx context.Context, conn *sql.DB, taskRunID string) error {
	_, err := conn.ExecContext(ctx, `UPDATE task_runs SET execution_descriptor = NULL WHERE id = ?`, taskRunID)
	return err
}

func createScopedDescriptorKey(t *testing.T, svc *iauth.Service, alias string) string {
	t.Helper()

	resp, err := svc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		Scope:     &models.KeyScope{Jobs: []string{alias}},
		CreatedBy: "descriptor-test",
	})
	require.NoError(t, err)
	return resp.Plaintext
}

func callDescriptorScopedRoute(
	t *testing.T,
	conn *gorm.DB,
	authSvc *iauth.Service,
	auditor *iauth.AuditLogger,
	limiter *iauth.RateLimiter,
	key string,
	jobID uuid.UUID,
	runID uuid.UUID,
	task string,
) (int, string, error) {
	t.Helper()

	e := echo.New()
	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/v1/jobs/%s/runs/%s/tasks/%s/descriptor", jobID, runID, url.PathEscape(task)),
		nil,
	)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	pv := echo.PathValues{
		{Name: "id", Value: jobID.String()},
		{Name: "run_id", Value: runID.String()},
		{Name: "task", Value: task},
	}
	c.InitializeRoute(&echo.RouteInfo{
		Path:   "/v1/jobs/:id/runs/:run_id/tasks/:task/descriptor",
		Method: http.MethodGet,
	}, &pv)

	handler := authmw.Auth(authmw.AuthDeps{
		Service: authSvc,
		Auditor: auditor,
		Limiter: limiter,
	})(func(c *echo.Context) error {
		resp, err := reproducesvc.NewWithDatabase(c.Request().Context(), conn).Descriptor(runID, task)
		if err != nil {
			return err
		}
		if resp.JobID != jobID {
			return echo.ErrNotFound
		}
		return c.JSON(http.StatusOK, resp)
	})

	err := handler(c)
	return rec.Code, rec.Body.String(), err
}

func seedDescriptorScopeFixture(t *testing.T, conn *gorm.DB, alias string) (uuid.UUID, uuid.UUID, string) {
	t.Helper()

	now := time.Now().UTC()
	done := now.Add(time.Second)
	triggerID := uuid.New()
	jobID := uuid.New()
	runID := uuid.New()
	atomID := uuid.New()
	taskID := uuid.New()
	taskRunID := uuid.New()
	taskName := "transform"

	require.NoError(t, conn.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)
	require.NoError(t, conn.Create(&models.Job{
		ID:        jobID,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, conn.Create(&models.Atom{
		ID:        atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   `["sh","-c","echo ok"]`,
		Spec:      datatypes.JSON([]byte(`{}`)),
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, conn.Create(&models.Task{
		ID:        taskID,
		JobID:     jobID,
		AtomID:    atomID,
		Name:      taskName,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	require.NoError(t, conn.Create(&models.JobRun{
		ID:          runID,
		JobID:       jobID,
		TriggerID:   triggerID,
		TriggerType: string(models.TriggerTypeCron),
		Status:      "succeeded",
		StartedAt:   now,
		CompletedAt: &done,
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error)
	require.NoError(t, conn.Create(&models.TaskRun{
		ID:                  taskRunID,
		JobRunID:            runID,
		TaskID:              taskID,
		AtomID:              atomID,
		Engine:              models.AtomEngineDocker,
		Image:               "alpine:3.23",
		Command:             `["sh","-c","echo ok"]`,
		Status:              "succeeded",
		Result:              "success",
		Output:              datatypes.JSON([]byte(`{"clean":"yes"}`)),
		ReplaySafe:          true,
		ExecutionDescriptor: datatypes.JSON([]byte(`{"schemaVersion":1,"baseline":{"taskName":"transform"},"runtime":{"image":"alpine:3.23"}}`)),
		StartedAt:           &now,
		CompletedAt:         &done,
		CreatedAt:           now,
		UpdatedAt:           now,
	}).Error)

	return jobID, runID, taskName
}
