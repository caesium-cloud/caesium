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
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// postAs drives Post with an authenticated principal in the context, as the
// auth middleware leaves it.
func (f *postFixture) postAs(t *testing.T, principal *iauth.Principal, jobID uuid.UUID, key string) (postResponse, error) {
	t.Helper()
	return f.postAsBody(t, principal, jobID, key, "")
}

func (f *postFixture) postAsBody(t *testing.T, principal *iauth.Principal, jobID uuid.UUID, key, body string) (postResponse, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequestWithContext(supervisedRequestContext(t), http.MethodPost, "/", strings.NewReader(body))
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

// differentRequest changes only the priority, which the request fingerprint
// covers.
const differentRequest = `{"priority":"high"}`

// TestPostRunReplayedRunRequiresViewerInItsNamespace is D9: an idempotent
// retry answers with the run recorded for (job, key). After the job moved
// from marketing to finance, that run is still marketing's: a caller confined
// to finance must not receive it, and must not learn whether its request
// matches the recorded one (the record is authorised before the comparison,
// so a different request is the same 403, not a 422).
func TestPostRunReplayedRunRequiresViewerInItsNamespace(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategyFail)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "marketing").Error)

	first, err := f.postAs(t, confined(policy.Grants{"marketing": models.RoleRunner}), jobID, "move-1")
	require.NoError(t, err)
	runID := first.body["id"]
	require.Equal(t, "marketing", first.body["namespace"])

	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "finance").Error)

	financeOnly := confined(policy.Grants{"finance": models.RoleRunner})
	for _, body := range []string{"", differentRequest} {
		_, err = f.postAsBody(t, financeOnly, jobID, "move-1", body)
		var denial *authz.Denial
		require.True(t, errors.As(err, &denial), "body %q: a finance-only caller must not receive the marketing run: %v", body, err)
		require.Equal(t, []string{"marketing"}, denial.Namespaces)
		require.Equal(t, models.RoleViewer, denial.Required)
	}

	both := confined(policy.Grants{"finance": models.RoleRunner, "marketing": models.RoleViewer})
	allowed, err := f.postAs(t, both, jobID, "move-1")
	require.NoError(t, err)
	require.Equal(t, "true", allowed.replayed)
	require.Equal(t, runID, allowed.body["id"])
	_, err = f.postAsBody(t, both, jobID, "move-1", differentRequest)
	require.Equal(t, http.StatusUnprocessableEntity, echo.StatusCode(err), "a readable record still refuses a different request")

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

	for _, body := range []string{"", differentRequest} {
		_, err = f.postAsBody(t, confined(policy.Grants{"finance": models.RoleRunner}), jobID, "paused-1", body)
		var denial *authz.Denial
		require.True(t, errors.As(err, &denial), "body %q: %v", body, err)
		require.Equal(t, []string{"marketing"}, denial.Namespaces)
	}
	// A caller that can read the record gets its answer, or 422 for a
	// different request, as before.
	both := confined(policy.Grants{"finance": models.RoleRunner, "marketing": models.RoleViewer})
	replayed, err := f.postAs(t, both, jobID, "paused-1")
	require.NoError(t, err)
	require.Equal(t, "true", replayed.replayed)
	_, err = f.postAsBody(t, both, jobID, "paused-1", differentRequest)
	require.Equal(t, http.StatusUnprocessableEntity, echo.StatusCode(err))
}

// TestPostRunReplayedSkippedStartRequiresViewer: a skipped start names no run,
// but its record carries the namespace it was admitted in.
func TestPostRunReplayedSkippedStartRequiresViewer(t *testing.T) {
	f := newPostFixture(t)
	jobID := f.job(t, jobdef.ConcurrencyStrategySkip)
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "marketing").Error)
	_, err := f.post(t, jobID, "", "")
	require.NoError(t, err)
	skipped, err := f.post(t, jobID, "skip-1", "")
	require.NoError(t, err)
	require.Equal(t, "skipped", skipped.body["outcome"])
	require.NoError(t, f.db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", "finance").Error)

	for _, body := range []string{"", differentRequest} {
		_, err = f.postAsBody(t, confined(policy.Grants{"finance": models.RoleRunner}), jobID, "skip-1", body)
		var denial *authz.Denial
		require.True(t, errors.As(err, &denial), "body %q: %v", body, err)
		require.Equal(t, []string{"marketing"}, denial.Namespaces)
	}
}

// TestReplayNamespaceGuard pins the guard the controller hands the store: no
// guard for unrestricted callers or with auth off; otherwise viewer in every
// namespace the record is attributed to, and a record that cannot be
// attributed is refused.
func TestReplayNamespaceGuard(t *testing.T) {
	ctx := func(p *iauth.Principal) *echo.Context {
		c := echo.New().NewContext(httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil), httptest.NewRecorder())
		if p != nil {
			c.Set(authmw.ContextKeyPrincipal, p)
		}
		return c
	}
	require.Nil(t, replayNamespaceGuard(ctx(nil)), "auth off: no guard")
	require.Nil(t, replayNamespaceGuard(ctx(confined(policy.Grants{"*": models.RoleViewer}))), "cluster viewer: no guard")

	guard := replayNamespaceGuard(ctx(confined(policy.Grants{"finance": models.RoleRunner, "marketing": models.RoleViewer})))
	require.NotNil(t, guard)
	require.NoError(t, guard([]string{"marketing"}))
	require.NoError(t, guard([]string{"finance", "marketing"}))

	var denial *authz.Denial
	require.True(t, errors.As(guard([]string{"finance", "sales"}), &denial))
	require.Equal(t, []string{"finance", "sales"}, denial.Namespaces, "the denial names every namespace the check needed")
	require.Equal(t, models.RoleViewer, denial.Required)
	require.True(t, errors.As(guard(nil), &denial), "a record that cannot be attributed is refused")
	require.Empty(t, denial.Namespaces)
}
