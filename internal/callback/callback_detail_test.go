package callback

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedCallbackRun creates a completed job run plus a notification callback
// pointing at target, and returns the job id and the run id.
func seedCallbackRun(t *testing.T, db *gorm.DB, target string) (uuid.UUID, uuid.UUID) {
	t.Helper()

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "detail-" + uuid.NewString(),
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "detail-job-" + uuid.NewString(),
		TriggerID: trig.ID,
	}
	require.NoError(t, db.Create(job).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NoError(t, store.Complete(runEntry.ID, nil))

	cb := &models.Callback{
		ID:            uuid.New(),
		JobID:         job.ID,
		Type:          models.CallbackTypeNotification,
		Configuration: `{"url":"` + target + `"}`,
	}
	require.NoError(t, db.Create(cb).Error)

	return job.ID, runEntry.ID
}

func callbackRunRows(t *testing.T, db *gorm.DB, runID uuid.UUID) []models.CallbackRun {
	t.Helper()
	var rows []models.CallbackRun
	require.NoError(t, db.Where("job_run_id = ?", runID).Order("started_at asc").Find(&rows).Error)
	return rows
}

// A 2xx delivery records the status the target answered with, its body, and a
// retry count of zero: the run-completion dispatch is attempt one.
func TestCallbackRunRecordsSuccessDetail(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("queued for delivery"))
	}))
	defer server.Close()

	jobID, runID := seedCallbackRun(t, db, server.URL)

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())
	require.NoError(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

	rows := callbackRunRows(t, db, runID)
	require.Len(t, rows, 1)
	require.Equal(t, models.CallbackRunStatusSucceeded, rows[0].Status)
	require.Equal(t, http.StatusAccepted, rows[0].HTTPStatus)
	require.Equal(t, "queued for delivery", rows[0].ResponseBody)
	require.Equal(t, 0, rows[0].RetryCount)

	// The API DTO must carry the same detail; the run detail page reads it from
	// there, not from the model.
	runState, err := run.NewStore(db).Get(runID)
	require.NoError(t, err)
	require.Len(t, runState.Callbacks, 1)
	require.Equal(t, http.StatusAccepted, runState.Callbacks[0].HTTPStatus)
	require.Equal(t, "queued for delivery", runState.Callbacks[0].ResponseBody)
	require.Equal(t, 0, runState.Callbacks[0].RetryCount)
}

// A 4xx/5xx records the rejecting status and the body the target explained
// itself with — the distinction between a permanent rejection and a transient
// network failure that the flat error string could not express.
func TestCallbackRunRecordsFailureDetail(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "client error", status: http.StatusUnprocessableEntity, body: `{"error":"unknown job alias"}`},
		{name: "server error", status: http.StatusServiceUnavailable, body: "receiver is draining"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()

			jobID, runID := seedCallbackRun(t, db, server.URL)

			dispatcher := NewDispatcher(db)
			dispatcher.WithHTTPClient(server.Client())
			require.Error(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

			rows := callbackRunRows(t, db, runID)
			require.Len(t, rows, 1)
			require.Equal(t, models.CallbackRunStatusFailed, rows[0].Status)
			require.Equal(t, tc.status, rows[0].HTTPStatus)
			require.Equal(t, tc.body, rows[0].ResponseBody)
			require.Equal(t, 0, rows[0].RetryCount)
			require.Contains(t, rows[0].Error, tc.body)
		})
	}
}

// A delivery that never reached the wire keeps HTTPStatus at 0, so "we could
// not reach you" stays distinguishable from "you answered an error".
func TestCallbackRunTransportFailureHasNoHTTPStatus(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client := server.Client()
	target := server.URL
	server.Close() // nothing is listening any more

	jobID, runID := seedCallbackRun(t, db, target)

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(client)
	require.Error(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

	rows := callbackRunRows(t, db, runID)
	require.Len(t, rows, 1)
	require.Equal(t, models.CallbackRunStatusFailed, rows[0].Status)
	require.Zero(t, rows[0].HTTPStatus)
	require.Empty(t, rows[0].ResponseBody)
	require.NotEmpty(t, rows[0].Error)
}

// Every retry writes its own row, and each row knows how many attempts came
// before it, so an operator reading the newest row sees the attempt count
// without reconstructing it from the others.
func TestCallbackRunRetryCountCountsPriorAttempts(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("still down"))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	jobID, runID := seedCallbackRun(t, db, server.URL)

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())

	require.Error(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))
	require.Error(t, dispatcher.RetryFailed(context.Background(), runID))
	require.NoError(t, dispatcher.RetryFailed(context.Background(), runID))

	rows := callbackRunRows(t, db, runID)
	require.Len(t, rows, 3)

	// Compared as a set: two rows written microseconds apart can tie on
	// started_at, and the ordinals are the claim under test, not the ordering.
	counts := make([]int, 0, len(rows))
	for _, row := range rows {
		counts = append(counts, row.RetryCount)
		if row.Status == models.CallbackRunStatusSucceeded {
			require.Equal(t, 2, row.RetryCount, "the delivery that finally landed was the third attempt")
			require.Equal(t, http.StatusOK, row.HTTPStatus)
		}
	}
	require.ElementsMatch(t, []int{0, 1, 2}, counts)
}

// The persisted body is bounded and scrubbed: a chatty target cannot push an
// unbounded blob through Raft, and one that echoes a credential back does not
// get it published on the run detail page.
func TestCallbackRunResponseBodyIsTruncatedAndScrubbed(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	const leaked = "AKIAIOSFODNN7EXAMPLEkeyMaterial42"
	body := "rejected: token " + leaked + " " + strings.Repeat("x", 8192)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	jobID, runID := seedCallbackRun(t, db, server.URL)

	dispatcher := NewDispatcher(db)
	dispatcher.WithHTTPClient(server.Client())
	require.Error(t, dispatcher.Dispatch(context.Background(), jobID, runID, nil))

	rows := callbackRunRows(t, db, runID)
	require.Len(t, rows, 1)
	require.Equal(t, http.StatusForbidden, rows[0].HTTPStatus)
	require.LessOrEqual(t, len(rows[0].ResponseBody), maxResponseBodyBytes)
	require.NotContains(t, rows[0].ResponseBody, leaked)
	require.Contains(t, rows[0].ResponseBody, incident.Redacted)
	require.NotContains(t, rows[0].Error, leaked)
}

// A handler that predates DetailedHandler still dispatches; it simply records
// no transport detail.
func TestPlainHandlerRecordsNoTransportDetail(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)

	const plainType = models.CallbackType("plain-detail-test")
	Register(plainType, handlerFunc(func(context.Context) error { return nil }))

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "plain-" + uuid.NewString(),
		Type:          models.TriggerTypeCron,
		Configuration: "{}",
	}
	require.NoError(t, db.Create(trig).Error)
	job := &models.Job{ID: uuid.New(), Alias: "plain-job-" + uuid.NewString(), TriggerID: trig.ID}
	require.NoError(t, db.Create(job).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID, nil)
	require.NoError(t, err)
	require.NoError(t, store.Complete(runEntry.ID, nil))

	require.NoError(t, db.Create(&models.Callback{
		ID:            uuid.New(),
		JobID:         job.ID,
		Type:          plainType,
		Configuration: `{}`,
	}).Error)

	require.NoError(t, NewDispatcher(db).Dispatch(context.Background(), job.ID, runEntry.ID, nil))

	rows := callbackRunRows(t, db, runEntry.ID)
	require.Len(t, rows, 1)
	require.Equal(t, models.CallbackRunStatusSucceeded, rows[0].Status)
	require.Zero(t, rows[0].HTTPStatus)
	require.Empty(t, rows[0].ResponseBody)
	require.Equal(t, 0, rows[0].RetryCount)
}

type handlerFunc func(ctx context.Context) error

func (f handlerFunc) Handle(ctx context.Context, _ json.RawMessage, _ Metadata) error {
	return f(ctx)
}
