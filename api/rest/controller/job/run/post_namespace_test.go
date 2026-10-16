package run

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// postAs drives Post with an authenticated principal in the context, as the
// auth middleware leaves it.
func (f *postFixture) postAs(t *testing.T, principal *iauth.Principal, jobID uuid.UUID, key string) (postResponse, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(supervisedRequestContext(t), http.MethodPost, "/", strings.NewReader(""))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	if key != "" {
		req.Header.Set(IdempotencyKeyHeader, key)
	}
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: jobID.String()}})
	if principal != nil {
		c.Set(authmw.ContextKeyPrincipal, principal)
	}
	if err := Post(c); err != nil {
		return postResponse{}, err
	}
	out := postResponse{status: rec.Code, replayed: rec.Header().Get(IdempotentReplayedHeader)}
	if rec.Body.Len() > 0 {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out.body), rec.Body.String())
	}
	return out, nil
}

func confined(grants policy.Grants) *iauth.Principal {
	return &iauth.Principal{Kind: iauth.PrincipalAPIKey, Grants: grants, Subject: "csk_confined"}
}

// TestPostRunReplayedRunRequiresViewerInItsNamespace is D9: an idempotent
// retry answers with the run recorded for (job, key). After the job moved
// from marketing to finance, that run is still marketing's: a caller confined
// to finance must not receive it.
func TestPostRunReplayedRunRequiresViewerInItsNamespace(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "marketing").Error)

	first, err := f.postAs(t, confined(policy.Grants{"marketing": models.RoleRunner}), jobID, "move-1")
	require.NoError(t, err)
	runID := first.body["id"]
	require.Equal(t, "marketing", first.body["namespace"])

	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "finance").Error)

	_, err = f.postAs(t, confined(policy.Grants{"finance": models.RoleRunner}), jobID, "move-1")
	var denial *authz.Denial
	require.True(t, errors.As(err, &denial), "a finance-only caller must not receive the marketing run: %v", err)
	require.Equal(t, []string{"marketing"}, denial.Namespaces)
	require.Equal(t, models.RoleViewer, denial.Required)

	allowed, err := f.postAs(t, confined(policy.Grants{"finance": models.RoleRunner, "marketing": models.RoleViewer}), jobID, "move-1")
	require.NoError(t, err)
	require.Equal(t, "true", allowed.replayed)
	require.Equal(t, runID, allowed.body["id"])

	// Unrestricted callers and auth-off servers are unchanged.
	cluster, err := f.postAs(t, confined(policy.Grants{"*": models.RoleRunner}), jobID, "move-1")
	require.NoError(t, err)
	require.Equal(t, runID, cluster.body["id"])
	anonymous, err := f.post(t, jobID, "move-1", "")
	require.NoError(t, err)
	require.Equal(t, runID, anonymous.body["id"])
	require.Equal(t, 1, f.launches())
}

// TestPostRunAdmissionRechecksNamespace: the middleware authorised the job's
// namespace before the handler ran; if the job moved in between (a slow
// request body, a concurrent apply), admission re-checks the confined caller's
// grant against the job row it actually starts from and refuses without
// writing a run. Unrestricted callers get no guard.
func TestPostRunAdmissionRechecksNamespace(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)
	// Authorised while in marketing; now in finance.
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "finance").Error)

	_, err := f.postAs(t, confined(policy.Grants{"marketing": models.RoleRunner}), jobID, "")
	var denial *authz.Denial
	require.True(t, errors.As(err, &denial), "%v", err)
	require.Equal(t, []string{"finance"}, denial.Namespaces)
	require.Equal(t, models.RoleRunner, denial.Required)
	var runs int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Count(&runs).Error)
	require.Zero(t, runs)
	require.Zero(t, f.launches())

	created, err := f.postAs(t, confined(policy.Grants{"finance": models.RoleRunner}), jobID, "")
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, created.status)
	require.Equal(t, "finance", created.body["namespace"])
	require.Nil(t, admissionNamespaceGuard(echo.New().NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil), httptest.NewRecorder())),
		"auth off: no guard")
}

// TestPostRunReplayedPausedJobRunRequiresViewer covers the paused-job branch,
// which answers recorded keys through the same writer.
func TestPostRunReplayedPausedJobRunRequiresViewer(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "marketing").Error)
	_, err := f.post(t, jobID, "paused-1", "")
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Updates(map[string]any{"namespace": "finance", "paused": true}).Error)

	_, err = f.postAs(t, confined(policy.Grants{"finance": models.RoleRunner}), jobID, "paused-1")
	var denial *authz.Denial
	require.True(t, errors.As(err, &denial), "%v", err)
}

// TestAuthorizeReplayedStartByRunID covers outcomes that carry only a run id
// (a dataset-hold skip) and those that carry none (queued, dropped).
func TestAuthorizeReplayedStartByRunID(t *testing.T) {
	runID := uuid.New()
	orig := postReplayedRunNamespace
	t.Cleanup(func() { postReplayedRunNamespace = orig })
	lookup := map[uuid.UUID]string{runID: "marketing"}
	postReplayedRunNamespace = func(_ context.Context, id uuid.UUID) (string, error) {
		if ns, ok := lookup[id]; ok {
			return ns, nil
		}
		return "", gorm.ErrRecordNotFound
	}
	finance := confined(policy.Grants{"finance": models.RoleRunner})
	ctx := func(p *iauth.Principal) *echo.Context {
		c := echo.New().NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil), httptest.NewRecorder())
		c.Set(authmw.ContextKeyPrincipal, p)
		return c
	}

	skipped := runstorage.StartResult{Outcome: runstorage.StartOutcomeSkipped, RunID: &runID, Replayed: true}
	var denial *authz.Denial
	require.True(t, errors.As(authorizeReplayedStart(ctx(finance), skipped), &denial))
	require.NoError(t, authorizeReplayedStart(ctx(confined(policy.Grants{"marketing": models.RoleViewer, "finance": models.RoleRunner})), skipped))

	gone := uuid.New()
	require.True(t, errors.As(authorizeReplayedStart(ctx(finance),
		runstorage.StartResult{Outcome: runstorage.StartOutcomeSkipped, RunID: &gone, Replayed: true}), &denial),
		"a recorded run that no longer exists cannot be proven readable")

	queueID := uuid.New()
	require.NoError(t, authorizeReplayedStart(ctx(finance),
		runstorage.StartResult{Outcome: runstorage.StartOutcomeQueued, QueueID: &queueID, Replayed: true}),
		"a queued start names no run")

	postReplayedRunNamespace = func(context.Context, uuid.UUID) (string, error) { return "", errors.New("db down") }
	err := authorizeReplayedStart(ctx(finance), skipped)
	require.Equal(t, http.StatusInternalServerError, echo.StatusCode(err), "a lookup failure fails closed")
}
