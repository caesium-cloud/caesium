package run

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// post_idempotency_test.go drives the real Post handler through echo against a
// test database and asserts on the status, headers and JSON a retrying client
// (a Temporal activity, the CLI) actually reads.

type postFixture struct {
	db       *gorm.DB
	mu       sync.Mutex
	launched []uuid.UUID
}

func newPostFixture(t *testing.T) *postFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	f := &postFixture{db: db}

	origJob, origStart, origFind, origLaunch := postGetJob, postStartRun, postFindIdempotentStart, postLaunchRun
	postGetJob = func(_ context.Context, id uuid.UUID) (*models.Job, error) {
		var j models.Job
		if err := db.First(&j, "id = ?", id).Error; err != nil {
			return nil, err
		}
		return &j, nil
	}
	store := runstorage.NewStore(db)
	postStartRun = func(ctx context.Context, jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, error) {
		return store.StartWithResult(context.WithoutCancel(ctx), jobID, nil, opts...)
	}
	postFindIdempotentStart = func(ctx context.Context, jobID uuid.UUID, opts ...runstorage.StartOption) (runstorage.StartResult, bool, error) {
		return store.FindIdempotentStart(context.WithoutCancel(ctx), jobID, opts...)
	}
	postLaunchRun = func(_ *models.Job, r *runstorage.JobRun) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.launched = append(f.launched, r.ID)
	}
	t.Cleanup(func() {
		postGetJob, postStartRun, postFindIdempotentStart, postLaunchRun = origJob, origStart, origFind, origLaunch
	})
	return f
}

func (f *postFixture) job(t *testing.T, strategy string) uuid.UUID {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, f.db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}).Error)
	raw, err := json.Marshal(&jobdef.Concurrency{MaxRuns: 1, Strategy: strategy})
	require.NoError(t, err)
	id := uuid.New()
	require.NoError(t, f.db.Create(&models.Job{
		ID: id, Alias: "post-" + uuid.NewString()[:8], TriggerID: triggerID,
		Concurrency: datatypes.JSON(raw), CreatedAt: now, UpdatedAt: now,
	}).Error)
	return id
}

func (f *postFixture) launches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.launched)
}

type postResponse struct {
	status   int
	replayed string
	body     map[string]any
}

func (f *postFixture) post(t *testing.T, jobID uuid.UUID, key, body string) (postResponse, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}})
	if err := Post(c); err != nil {
		return postResponse{}, err
	}
	out := postResponse{status: rec.Code, replayed: rec.Header().Get(IdempotentReplayedHeader)}
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out.body), rec.Body.String())
	}
	return out, nil
}

func requireHTTPStatus(t *testing.T, err error, status int) {
	t.Helper()
	var httpErr *echo.HTTPError
	require.ErrorAs(t, err, &httpErr)
	require.Equal(t, status, httpErr.Code, err.Error())
}

func TestPostRunIdempotencyKeyReplaysCreatedRun(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)

	first, err := f.post(t, jobID, "wf-1/act-1", `{"params":{"region":"eu"}}`)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, first.status)
	require.Empty(t, first.replayed)
	require.Equal(t, "created", first.body["outcome"])
	runID, _ := first.body["id"].(string)
	require.NotEmpty(t, runID, "a created start still returns the run itself")
	require.Equal(t, "running", first.body["status"])

	second, err := f.post(t, jobID, "wf-1/act-1", `{"params":{"region":"eu"}}`)
	require.NoError(t, err, "a retry must not hit the concurrency limit its own first attempt holds")
	require.Equal(t, http.StatusAccepted, second.status)
	require.Equal(t, "true", second.replayed)
	require.Equal(t, runID, second.body["id"])
	require.Equal(t, 1, f.launches(), "a replayed start must not launch the run again")

	_, err = f.post(t, jobID, "wf-1/act-1", `{"params":{"region":"us"}}`)
	requireHTTPStatus(t, err, http.StatusUnprocessableEntity)
}

func TestPostRunRejectsInvalidIdempotencyKey(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)

	_, err := f.post(t, jobID, strings.Repeat("k", runstorage.MaxIdempotencyKeyLength+1), "")
	requireHTTPStatus(t, err, http.StatusBadRequest)
	require.Zero(t, f.launches())
}

func TestPostRunReportsQueuedAndSkippedOutcomes(t *testing.T) {
	f := newPostFixture(t)

	queueJob := f.job(t, jobdef.ConcurrencyStrategyQueue)
	_, err := f.post(t, queueJob, "", "")
	require.NoError(t, err)
	queued, err := f.post(t, queueJob, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, queued.status)
	require.Equal(t, "queued", queued.body["outcome"])
	require.Equal(t, queueJob.String(), queued.body["job_id"])
	require.NotEmpty(t, queued.body["queue_id"])
	require.NotContains(t, queued.body, "id", "a start that created no run must not look like a run")

	skipJob := f.job(t, jobdef.ConcurrencyStrategySkip)
	_, err = f.post(t, skipJob, "", "")
	require.NoError(t, err)
	skipped, err := f.post(t, skipJob, "", "")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, skipped.status)
	require.Equal(t, "skipped", skipped.body["outcome"])
	require.Equal(t, "max_concurrency", skipped.body["reason"])

	failJob := f.job(t, jobdef.ConcurrencyStrategyFail)
	_, err = f.post(t, failJob, "", "")
	require.NoError(t, err)
	_, err = f.post(t, failJob, "", "")
	requireHTTPStatus(t, err, http.StatusConflict)
}

func TestPostRunPausedJobStillAnswersRecordedKey(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)

	first, err := f.post(t, jobID, "before-pause", "")
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("paused", true).Error)

	retry, err := f.post(t, jobID, "before-pause", "")
	require.NoError(t, err, "a retry of a start admitted before the pause must get its original answer")
	require.Equal(t, "true", retry.replayed)
	require.Equal(t, first.body["id"], retry.body["id"])

	_, err = f.post(t, jobID, "after-pause", "")
	requireHTTPStatus(t, err, http.StatusConflict)
	_, err = f.post(t, jobID, "", "")
	requireHTTPStatus(t, err, http.StatusConflict)
}
