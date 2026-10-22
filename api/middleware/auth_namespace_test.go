package middleware_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// nsFixture is a two-namespace catalog (marketing, finance) for driving the
// namespace-aware middleware end to end: real keys and sessions, real rows,
// the real resolvers, and a recording handler in place of the controller.
type nsFixture struct {
	db      *gorm.DB
	svc     *auth.Service
	auditor *auth.AuditLogger
	deps    authmw.AuthDeps

	mktJob, finJob, mktSharedJob, movedJob uuid.UUID
	mktRun, finRun, movedRun               uuid.UUID
	mktBackfill, finBackfill               uuid.UUID
	sharedTrigger, mktTrigger, idleTrigger uuid.UUID
	mktIncident, finIncident               uuid.UUID
	mktChannel, finChannel, sharedChannel  uuid.UUID
	mktPolicy, sharedPolicy                uuid.UUID
	mktProfile, finProfile, sharedProfile  uuid.UUID
	salesHold, mktHold                     uuid.UUID

	// replayer is a session whose policy grants runner in finance and viewer
	// in marketing (a single key cannot hold two roles).
	replayer cred
}

type cred struct {
	bearer string
	cookie string
	csrf   string
}

const (
	mktJobID = "aaaa1111-0000-4000-8000-000000000001"
	finJobID = "aaaa2222-0000-4000-8000-000000000002"
)

func newNSFixture(t *testing.T) *nsFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing: {}
  finance: {}
bindings:
  - subjects: {groups: [replayers]}
    role: runner
    namespaces: [finance]
  - subjects: {groups: [replayers]}
    role: viewer
    namespaces: [marketing]
`))
	require.NoError(t, err)
	svc := auth.NewService(db, auth.WithPolicySource(&policy.Static{Policy: p}))
	auditor := auth.NewAuditLogger(db)
	sessions := auth.NewSessionStore(db)
	f := &nsFixture{
		db: db, svc: svc, auditor: auditor,
		deps: authmw.AuthDeps{
			Service: svc, Auditor: auditor, Limiter: auth.NewRateLimiter(1000, time.Minute),
			Sessions: sessions, CookieName: "caesium_session",
		},
	}
	now := time.Now().UTC()
	mk := func(v any) { t.Helper(); require.NoError(t, db.Create(v).Error) }

	f.sharedTrigger, f.mktTrigger, f.idleTrigger = uuid.New(), uuid.New(), uuid.New()
	movedTrigger := uuid.New()
	for _, id := range []uuid.UUID{f.sharedTrigger, f.mktTrigger, f.idleTrigger, movedTrigger} {
		mk(&models.Trigger{ID: id, Type: models.TriggerTypeHTTP, Configuration: `{}`, CreatedAt: now, UpdatedAt: now})
	}

	f.mktJob, f.finJob = uuid.MustParse(mktJobID), uuid.MustParse(finJobID)
	f.mktSharedJob, f.movedJob = uuid.New(), uuid.New()
	job := func(id uuid.UUID, alias, namespace string, trigger uuid.UUID) {
		mk(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: trigger, CreatedAt: now, UpdatedAt: now})
	}
	job(f.mktJob, "ns-mkt", "marketing", f.mktTrigger)
	job(f.finJob, "ns-fin", "finance", f.sharedTrigger)
	job(f.mktSharedJob, "ns-mkt-shared", "marketing", f.sharedTrigger)
	job(f.movedJob, "ns-moved", "finance", movedTrigger)

	run := func(jobID uuid.UUID, namespace string) uuid.UUID {
		id := uuid.New()
		mk(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, TriggerID: f.mktTrigger, Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktRun = run(f.mktJob, "marketing")
	f.finRun = run(f.finJob, "finance")
	// The moved job's history stayed in marketing when it moved to finance.
	f.movedRun = run(f.movedJob, "marketing")

	backfill := func(jobID uuid.UUID, namespace string) uuid.UUID {
		id := uuid.New()
		mk(&models.Backfill{ID: id, JobID: jobID, Namespace: namespace, Status: models.BackfillStatusRunning, Start: now, End: now.Add(time.Hour),
			MaxConcurrent: 1, Reprocess: models.ReprocessNone, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktBackfill = backfill(f.mktJob, "marketing")
	f.finBackfill = backfill(f.finJob, "finance")

	incident := func(jobID uuid.UUID, namespace string) uuid.UUID {
		id := uuid.New()
		mk(&models.Incident{ID: id, Namespace: namespace, JobID: jobID, Class: "image_pull", Status: models.IncidentStatus("open"),
			DedupeKey: id.String(), OpenedAt: now, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktIncident = incident(f.mktJob, "marketing")
	f.finIncident = incident(f.finJob, "finance")

	channel := func(namespace string) uuid.UUID {
		id := uuid.New()
		mk(&models.NotificationChannel{ID: id, Namespace: namespace, Name: "ch-" + id.String()[:8], Type: models.ChannelTypeWebhook,
			Config: datatypes.JSON(`{}`), Enabled: true, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktChannel, f.finChannel, f.sharedChannel = channel("marketing"), channel("finance"), channel("*")
	notifPolicy := func(namespace string, channelID uuid.UUID) uuid.UUID {
		id := uuid.New()
		mk(&models.NotificationPolicy{ID: id, Namespace: namespace, Name: "pol-" + id.String()[:8], ChannelID: channelID,
			EventTypes: datatypes.JSON(`["run_failed"]`), Enabled: true, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktPolicy, f.sharedPolicy = notifPolicy("marketing", f.mktChannel), notifPolicy("*", f.sharedChannel)
	profile := func(namespace string) uuid.UUID {
		id := uuid.New()
		mk(&models.AgentProfile{ID: id, Namespace: namespace, Name: "prof-" + id.String()[:8], Image: "busybox:1.36.1",
			Engine: models.AtomEngine("docker"), CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.mktProfile, f.finProfile, f.sharedProfile = profile("marketing"), profile("finance"), profile("*")

	declare := func(jobID uuid.UUID, alias, name, direction string) {
		mk(&models.DatasetDeclaration{ID: uuid.New(), JobID: jobID, JobAlias: alias, Name: name, Direction: direction, CreatedAt: now, UpdatedAt: now})
	}
	declare(f.mktJob, "ns-mkt", "sales", models.DatasetDirectionProduces)
	declare(f.finJob, "ns-fin", "sales", models.DatasetDirectionProduces)
	declare(f.mktJob, "ns-mkt", "mkt-only", models.DatasetDirectionProduces)
	// A consumer does not own the dataset, nor does a job that declares an
	// external dataset as its source.
	declare(f.finJob, "ns-fin", "mkt-only", models.DatasetDirectionConsumes)
	declare(f.mktJob, "ns-mkt", "external", models.DatasetDirectionSource)
	declare(f.finJob, "ns-fin", "external", models.DatasetDirectionSource)
	hold := func(name string, jobID uuid.UUID) uuid.UUID {
		id := uuid.New()
		key := name + "-" + id.String()
		mk(&models.DatasetHold{ID: id, Name: name, Status: "active", ActiveKey: &key, HeldByJobID: jobID, OpenedAt: now, CreatedAt: now, UpdatedAt: now})
		return id
	}
	f.salesHold, f.mktHold = hold("sales", f.mktJob), hold("mkt-only", f.mktJob)

	user := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "replayer", Email: "replayer@example.com", EmailVerified: true,
		Groups: datatypes.JSON(`["replayers"]`), Role: models.RoleViewer, CreatedAt: now}
	mk(user)
	token, sess, err := sessions.Create(t.Context(), auth.CreateSessionRequest{UserID: user.ID})
	require.NoError(t, err)
	f.replayer = cred{cookie: token, csrf: sess.CSRFToken}
	return f
}

// key mints a key with role, then stores namespaces verbatim ("" keeps SQL
// NULL, i.e. the role cluster-wide) — the seeded state A5's minting replaces.
func (f *nsFixture) key(t *testing.T, role models.Role, namespaces string) cred {
	t.Helper()
	created, err := f.svc.CreateKey(&auth.CreateKeyRequest{Role: role, CreatedBy: "test"})
	require.NoError(t, err)
	if namespaces != "" {
		require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).
			Update("namespaces", datatypes.JSON(namespaces)).Error)
	}
	return cred{bearer: created.Plaintext}
}

type nsCall struct {
	method      string
	pattern     string
	params      echo.PathValues
	query       string
	body        string
	contentType string
}

type nsResult struct {
	status   int
	decision *authz.Decision
	body     string // the body the handler read
	message  string
}

func (f *nsFixture) do(t *testing.T, who cred, call nsCall) nsResult {
	t.Helper()
	target := "/test"
	if call.query != "" {
		target += "?" + call.query
	}
	var reader io.Reader = http.NoBody
	if call.body != "" {
		reader = strings.NewReader(call.body)
	}
	req := httptest.NewRequestWithContext(context.Background(), call.method, target, reader)
	if call.body != "" {
		ctype := call.contentType
		if ctype == "" {
			ctype = echo.MIMEApplicationJSON
		}
		req.Header.Set(echo.HeaderContentType, ctype)
	}
	if who.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+who.bearer)
	} else {
		req.AddCookie(&http.Cookie{Name: "caesium_session", Value: who.cookie})
		req.Header.Set("X-CSRF-Token", who.csrf)
	}
	var out nsResult
	rec, err := callMiddlewareWithDeps(t, f.deps, req, &echo.RouteInfo{Path: call.pattern, Method: call.method}, call.params, func(c *echo.Context) error {
		out.decision = authz.DecisionFrom(c)
		data, readErr := io.ReadAll(c.Request().Body)
		require.NoError(t, readErr)
		out.body = string(data)
		return c.String(http.StatusOK, "ok")
	})
	if err != nil {
		out.status = echo.StatusCode(err)
		require.NotZero(t, out.status, "unexpected error %v", err)
		if he, ok := err.(*echo.HTTPError); ok {
			out.message = he.Message
		}
		return out
	}
	out.status = rec.Code
	return out
}

func pv(pairs ...string) echo.PathValues {
	out := make(echo.PathValues, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, echo.PathValue{Name: pairs[i], Value: pairs[i+1]})
	}
	return out
}

func get(pattern string, params echo.PathValues) nsCall {
	return nsCall{method: http.MethodGet, pattern: pattern, params: params}
}

func post(pattern string, params echo.PathValues, body string) nsCall {
	return nsCall{method: http.MethodPost, pattern: pattern, params: params, body: body}
}

func (f *nsFixture) lastDenial(t *testing.T) models.AuditLog {
	t.Helper()
	var entry models.AuditLog
	require.NoError(t, f.db.Where("action = ?", auth.ActionAuthDenied).Order("timestamp DESC").First(&entry).Error)
	return entry
}

func TestNamespacedJobRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	clusterViewer := f.key(t, models.RoleViewer, "")
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)

	mkt, fin := f.mktJob.String(), f.finJob.String()
	for _, route := range []struct {
		call nsCall
	}{
		{get("/v1/jobs/:id/dag", pv("id", mkt))},
		{get("/v1/jobs/:id/tasks", pv("id", mkt))},
		{get("/v1/jobs/:id/queue", pv("id", mkt))},
		{post("/v1/jobs/:id/run", pv("id", mkt), "")},
		{nsCall{method: http.MethodPut, pattern: "/v1/jobs/:id/pause", params: pv("id", mkt)}},
		{nsCall{method: http.MethodDelete, pattern: "/v1/jobs/:id/cache/:task_name", params: pv("id", mkt, "task_name", "x")}},
	} {
		res := f.do(t, mktOperator, route.call)
		require.Equal(t, http.StatusOK, res.status, "%s %s on the marketing job", route.call.method, route.call.pattern)
		require.Equal(t, []string{"marketing"}, res.decision.Namespaces)

		finCall := route.call
		finCall.params = append(echo.PathValues{{Name: "id", Value: fin}}, route.call.params[1:]...)
		res = f.do(t, mktOperator, finCall)
		require.Equal(t, http.StatusForbidden, res.status, "%s %s on the finance job", route.call.method, route.call.pattern)
	}

	// The denial is audited against the target's namespace with the
	// namespaces checked.
	denial := f.lastDenial(t)
	require.Equal(t, "finance", denial.Namespace)
	requireAuditMetadata(t, denial, map[string]any{"reason": "insufficient_role", "required_role": "operator", "namespaces": []any{"finance"}})

	// A missing job is 404 and an unparseable id 400, as the handler answers.
	require.Equal(t, http.StatusNotFound, f.do(t, mktOperator, get("/v1/jobs/:id/dag", pv("id", uuid.NewString()))).status)
	require.Equal(t, http.StatusBadRequest, f.do(t, mktOperator, get("/v1/jobs/:id/dag", pv("id", "not-a-uuid"))).status)

	// D1: a principal whose grants never reach the route's role is denied
	// before resolution, so a missing target stays a 403 (master's answer).
	missing := pv("id", uuid.NewString())
	require.Equal(t, http.StatusForbidden, f.do(t, clusterViewer, post("/v1/jobs/:id/run", missing, "")).status)
	require.Equal(t, http.StatusForbidden, f.do(t, mktViewer, post("/v1/jobs/:id/run", missing, "")).status)
	denial = f.lastDenial(t)
	requireAuditMetadata(t, denial, map[string]any{"reason": "insufficient_role", "required_role": "runner"})

	// The cluster viewer reads any namespace exactly as before.
	require.Equal(t, http.StatusOK, f.do(t, clusterViewer, get("/v1/jobs/:id/dag", pv("id", fin))).status)
	require.Equal(t, http.StatusOK, f.do(t, clusterViewer, get("/v1/jobs/:id/dag", missing)).status,
		"an unrestricted principal reaches the handler: no resolver runs, so no resolver 404")
}

// TestNamespacedManifestPrefix: the manifest resolves the job by id or
// unique prefix (the resolver shared with GET /v1/jobs/:id, exercised in
// TestJobPrefixResolverMatchesHandler); the handler withholds a trigger
// shared with another namespace's job (B9).
func TestNamespacedManifestPrefix(t *testing.T) {
	f := newNSFixture(t)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	manifest := func(who cred, id string) nsResult {
		return f.do(t, who, get("/v1/jobs/:id/manifest", pv("id", id)))
	}
	for _, id := range []string{mktJobID, "aaaa1111"} {
		require.Equal(t, http.StatusOK, manifest(mktViewer, id).status, id)
	}
	require.Equal(t, http.StatusForbidden, manifest(mktViewer, finJobID).status)
	require.Equal(t, http.StatusOK, manifest(f.key(t, models.RoleViewer, ""), "aaaa1111").status, "a cluster viewer is unchanged")
}

func TestNamespacedRunRoutesRequireBothNamespaces(t *testing.T) {
	f := newNSFixture(t)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	finViewer := f.key(t, models.RoleViewer, `["finance"]`)
	both := f.key(t, models.RoleRunner, `["marketing","finance"]`)
	mktRunner := f.key(t, models.RoleRunner, `["marketing"]`)

	runGet := func(who cred, job, run uuid.UUID) int {
		return f.do(t, who, get("/v1/jobs/:id/runs/:run_id", pv("id", job.String(), "run_id", run.String()))).status
	}
	require.Equal(t, http.StatusOK, runGet(mktViewer, f.mktJob, f.mktRun))
	require.Equal(t, http.StatusForbidden, runGet(mktViewer, f.finJob, f.finRun))
	require.Equal(t, http.StatusNotFound, runGet(mktViewer, f.mktJob, f.finRun), "a run of another job under this job's path is 404")
	require.Equal(t, http.StatusNotFound, runGet(mktViewer, f.mktJob, uuid.New()))

	// The moved job lives in finance; its old run stayed in marketing.
	require.Equal(t, http.StatusForbidden, runGet(mktViewer, f.movedJob, f.movedRun), "the job's current namespace is required")
	require.Equal(t, http.StatusForbidden, runGet(finViewer, f.movedJob, f.movedRun), "the run's persisted namespace is required")
	require.Equal(t, http.StatusOK, runGet(both, f.movedJob, f.movedRun))
	denial := f.lastDenial(t)
	require.Equal(t, "*", denial.Namespace, "a decision spanning namespaces is audited as *")
	requireAuditMetadata(t, denial, map[string]any{"namespaces": []any{"finance", "marketing"}})

	retry := post("/v1/jobs/:id/runs/:run_id/retry", pv("id", f.movedJob.String(), "run_id", f.movedRun.String()), "")
	require.Equal(t, http.StatusForbidden, f.do(t, mktRunner, retry).status)
	require.Equal(t, http.StatusOK, f.do(t, both, retry).status)

	// Replay: runner in the job's current namespace AND viewer in the run's.
	replay := post("/v1/jobs/:id/runs/:run_id/replay", pv("id", f.movedJob.String(), "run_id", f.movedRun.String()), "")
	require.Equal(t, http.StatusOK, f.do(t, f.replayer, replay).status, "runner in finance + viewer in marketing")
	require.Equal(t, http.StatusForbidden, f.do(t, mktRunner, replay).status, "runner only where the baseline lives")
	// The replayer's viewer-only marketing grant cannot run there.
	require.Equal(t, http.StatusForbidden, f.do(t, f.replayer,
		post("/v1/jobs/:id/runs/:run_id/replay", pv("id", f.mktJob.String(), "run_id", f.mktRun.String()), "")).status)
}

// TestNamespacedBackfillRoutes: backfill create, read and cancel are live for
// confined principals (B4): a backfill deduplicates against its own
// namespace, so a confined caller acts on its own namespace's backfills, is
// refused another namespace's (audited against it), and a backfill of another
// job is 404. Unrestricted principals are unaffected.
func TestNamespacedBackfillRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	clusterOperator := f.key(t, models.RoleOperator, "")
	backfillParams := func(job, backfill uuid.UUID) echo.PathValues {
		return pv("id", job.String(), "backfill_id", backfill.String())
	}
	cancel := func(job, backfill uuid.UUID) nsCall {
		return nsCall{method: http.MethodPut, pattern: "/v1/jobs/:id/backfills/:backfill_id/cancel", params: backfillParams(job, backfill)}
	}
	for _, call := range []nsCall{
		post("/v1/jobs/:id/backfill", pv("id", f.mktJob.String()), `{}`),
		get("/v1/jobs/:id/backfills", pv("id", f.mktJob.String())),
		get("/v1/jobs/:id/backfills/:backfill_id", backfillParams(f.mktJob, f.mktBackfill)),
		cancel(f.mktJob, f.mktBackfill),
	} {
		require.Equal(t, http.StatusOK, f.do(t, mktOperator, call).status, call.pattern)
		require.Equal(t, http.StatusOK, f.do(t, clusterOperator, call).status, call.pattern)
	}
	for _, call := range []nsCall{
		post("/v1/jobs/:id/backfill", pv("id", f.finJob.String()), `{}`),
		get("/v1/jobs/:id/backfills/:backfill_id", backfillParams(f.finJob, f.finBackfill)),
		cancel(f.finJob, f.finBackfill),
	} {
		require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, call).status, call.pattern)
		denial := f.lastDenial(t)
		requireAuditMetadata(t, denial, map[string]any{"reason": "insufficient_role"})
		require.Equal(t, "finance", denial.Namespace, call.pattern)
		require.Equal(t, http.StatusOK, f.do(t, clusterOperator, call).status, call.pattern)
	}
	// A backfill of another job is the handler's 404.
	require.Equal(t, http.StatusNotFound, f.do(t, mktOperator, get("/v1/jobs/:id/backfills/:backfill_id", backfillParams(f.mktJob, f.finBackfill))).status)
}

// TestRunDiffResolvesBothRuns: GET /v1/jobs/:id/runs/diff needs viewer in the
// job's namespace and in both compared runs' persisted namespaces (B4), with
// the handler's own 400/404 for a malformed request or another job's run.
func TestRunDiffResolvesBothRuns(t *testing.T) {
	f := newNSFixture(t)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	finViewer := f.key(t, models.RoleViewer, `["finance"]`)
	bothViewer := f.key(t, models.RoleViewer, `["marketing","finance"]`)
	diff := func(job uuid.UUID, query string) nsCall {
		call := get("/v1/jobs/:id/runs/diff", pv("id", job.String()))
		call.query = query
		return call
	}
	same := "left=" + f.mktRun.String() + "&right=" + f.mktRun.String()
	require.Equal(t, http.StatusOK, f.do(t, mktViewer, diff(f.mktJob, same)).status)

	// The moved job is in finance; its run stayed in marketing.
	moved := "left=" + f.movedRun.String() + "&right=" + f.movedRun.String()
	require.Equal(t, http.StatusForbidden, f.do(t, finViewer, diff(f.movedJob, moved)).status, "the run's namespace is required")
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"reason": "insufficient_role"})
	require.Equal(t, http.StatusForbidden, f.do(t, mktViewer, diff(f.movedJob, moved)).status, "the job's namespace is required")
	require.Equal(t, http.StatusOK, f.do(t, bothViewer, diff(f.movedJob, moved)).status)

	require.Equal(t, http.StatusNotFound, f.do(t, mktViewer, diff(f.mktJob, "left="+f.mktRun.String()+"&right="+f.finRun.String())).status, "a run of another job")
	require.Equal(t, http.StatusBadRequest, f.do(t, mktViewer, diff(f.mktJob, "left="+f.mktRun.String())).status)
	require.Equal(t, http.StatusBadRequest, f.do(t, mktViewer, diff(f.mktJob, "left=nope&right="+f.mktRun.String())).status)
	require.Equal(t, http.StatusNotFound, f.do(t, mktViewer, diff(f.mktJob, "left="+f.mktRun.String()+"&right="+uuid.NewString())).status)
}

func TestNamespacedTriggerRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	both := f.key(t, models.RoleOperator, `["marketing","finance"]`)
	calls := func(trigger uuid.UUID) []nsCall {
		params := pv("id", trigger.String())
		return []nsCall{
			get("/v1/triggers/:id", params),
			post("/v1/triggers/:id/fire", params, ""),
		}
	}
	// Editing a trigger needs the role in every job it serves; the handler
	// re-checks that and the webhook path inside its transaction (B9).
	patch := nsCall{method: http.MethodPatch, pattern: "/v1/triggers/:id", params: pv("id", f.mktTrigger.String()), body: `{}`}
	require.Equal(t, http.StatusOK, f.do(t, mktOperator, patch).status)
	sharedPatch := nsCall{method: http.MethodPatch, pattern: "/v1/triggers/:id", params: pv("id", f.sharedTrigger.String()), body: `{}`}
	require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, sharedPatch).status)
	require.Equal(t, http.StatusOK, f.do(t, both, sharedPatch).status)
	for _, call := range calls(f.sharedTrigger) {
		require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, call).status, "a trigger shared with a finance job: %s", call.method)
		require.Equal(t, http.StatusOK, f.do(t, both, call).status)
	}
	for _, call := range calls(f.mktTrigger) {
		require.Equal(t, http.StatusOK, f.do(t, mktOperator, call).status, "a marketing-only trigger: %s", call.method)
	}
	for _, call := range calls(f.idleTrigger) {
		require.Equal(t, http.StatusForbidden, f.do(t, both, call).status, "a trigger serving no job is cluster-only: %s", call.method)
	}
	require.Equal(t, http.StatusNotFound, f.do(t, mktOperator, get("/v1/triggers/:id", pv("id", uuid.NewString()))).status)
	// Creating a trigger (no job yet) is cluster-only.
	require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, post("/v1/triggers", nil, `{}`)).status)
}

func TestNamespacedIncidentRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	// The detail carries the frozen agent allowlist (downstream aliases from
	// any namespace): cluster-only until A8.
	require.Equal(t, http.StatusForbidden, f.do(t, mktOperator, get("/v1/incidents/:id", pv("id", f.mktIncident.String()))).status)
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"pending": "A8"})
	require.Equal(t, "marketing", f.lastDenial(t).Namespace, "a pending denial is still audited against the incident's namespace")
	decide := func(verb string, incident uuid.UUID) int {
		return f.do(t, mktOperator, post("/v1/incidents/:id/approvals/:approval_id/"+verb,
			pv("id", incident.String(), "approval_id", uuid.NewString()), "")).status
	}
	require.Equal(t, http.StatusOK, decide("reject", f.mktIncident))
	require.Equal(t, http.StatusForbidden, decide("reject", f.finIncident))
	// Approving executes the action against the job, which may have moved:
	// cluster-only until A8 confines execution.
	require.Equal(t, http.StatusForbidden, decide("approve", f.mktIncident))
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"pending": "A8"})
	// The agent tool surface stays fail-closed for confined principals (A8).
	res := f.do(t, mktOperator, get("/v1/agent/incidents/:id/bundle", pv("id", f.mktIncident.String())))
	require.Equal(t, http.StatusForbidden, res.status)
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"pending": "A8"})
}

func TestNamespacedSharedObjects(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	clusterOperator := f.key(t, models.RoleOperator, "")

	// Shared objects (D4): reads need viewer in the object's namespace (any
	// grant for a "*" object); creates, edits and deletes need operator
	// there, and at "*" for a shared object. Lists are collections. The
	// create-body resolver is exercised in TestCreateBodyNamespaceResolver.
	cases := []struct {
		name   string
		who    cred
		call   nsCall
		status int
	}{
		{"read a * channel", mktViewer, get("/v1/notifications/channels/:id", pv("id", f.sharedChannel.String())), http.StatusOK},
		{"read a marketing channel", mktViewer, get("/v1/notifications/channels/:id", pv("id", f.mktChannel.String())), http.StatusOK},
		{"read a finance channel", mktViewer, get("/v1/notifications/channels/:id", pv("id", f.finChannel.String())), http.StatusForbidden},
		{"read a * policy", mktViewer, get("/v1/notifications/policies/:id", pv("id", f.sharedPolicy.String())), http.StatusOK},
		{"read a marketing profile", mktViewer, get("/v1/agentprofiles/:id", pv("id", f.mktProfile.String())), http.StatusOK},
		{"read a finance profile", mktViewer, get("/v1/agentprofiles/:id", pv("id", f.finProfile.String())), http.StatusForbidden},
		{"read a * profile", mktViewer, get("/v1/agentprofiles/:id", pv("id", f.sharedProfile.String())), http.StatusOK},
		{"missing object", mktViewer, get("/v1/agentprofiles/:id", pv("id", uuid.NewString())), http.StatusNotFound},
		{"delete a marketing channel", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/channels/:id", params: pv("id", f.mktChannel.String())}, http.StatusOK},
		{"delete a marketing policy", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/policies/:id", params: pv("id", f.mktPolicy.String())}, http.StatusOK},
		{"delete a finance channel", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/channels/:id", params: pv("id", f.finChannel.String())}, http.StatusForbidden},
		{"delete a * channel", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/channels/:id", params: pv("id", f.sharedChannel.String())}, http.StatusForbidden},
		{"cluster operator deletes a marketing channel", clusterOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/channels/:id", params: pv("id", f.mktChannel.String())}, http.StatusOK},
		{"cluster operator deletes a marketing policy", clusterOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/policies/:id", params: pv("id", f.mktPolicy.String())}, http.StatusOK},
		{"delete a marketing profile", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/agentprofiles/:id", params: pv("id", f.mktProfile.String())}, http.StatusOK},
		{"cluster operator deletes a marketing profile", clusterOperator, nsCall{method: http.MethodDelete, pattern: "/v1/agentprofiles/:id", params: pv("id", f.mktProfile.String())}, http.StatusOK},
		{"delete a * policy", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/policies/:id", params: pv("id", f.sharedPolicy.String())}, http.StatusForbidden},
		{"delete a finance profile", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/agentprofiles/:id", params: pv("id", f.finProfile.String())}, http.StatusForbidden},
		{"delete a * profile", mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/agentprofiles/:id", params: pv("id", f.sharedProfile.String())}, http.StatusForbidden},
		{"viewer deletes a marketing profile", mktViewer, nsCall{method: http.MethodDelete, pattern: "/v1/agentprofiles/:id", params: pv("id", f.mktProfile.String())}, http.StatusForbidden},
		{"patch a marketing profile", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/agentprofiles/:id", params: pv("id", f.mktProfile.String()), body: `{}`}, http.StatusOK},
		{"patch a marketing channel", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/notifications/channels/:id", params: pv("id", f.mktChannel.String()), body: `{}`}, http.StatusOK},
		{"patch a marketing policy", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/notifications/policies/:id", params: pv("id", f.mktPolicy.String()), body: `{}`}, http.StatusOK},
		{"patch a finance channel", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/notifications/channels/:id", params: pv("id", f.finChannel.String()), body: `{}`}, http.StatusForbidden},
		{"patch a * channel", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/notifications/channels/:id", params: pv("id", f.sharedChannel.String()), body: `{}`}, http.StatusForbidden},
		{"patch a * profile", mktOperator, nsCall{method: http.MethodPatch, pattern: "/v1/agentprofiles/:id", params: pv("id", f.sharedProfile.String()), body: `{}`}, http.StatusForbidden},
		{"profile create in marketing", mktOperator, post("/v1/agentprofiles", nil, `{"namespace":"marketing","name":"x"}`), http.StatusOK},
		{"channel create in marketing", mktOperator, post("/v1/notifications/channels", nil, `{"namespace":"marketing","name":"x"}`), http.StatusOK},
		{"policy create in marketing", mktOperator, post("/v1/notifications/policies", nil, `{"namespace":"marketing"}`), http.StatusOK},
		{"create with the namespace omitted is default", mktOperator, post("/v1/notifications/channels", nil, `{"name":"x"}`), http.StatusForbidden},
		{"channel create in finance", mktOperator, post("/v1/notifications/channels", nil, `{"namespace":"finance","name":"x"}`), http.StatusForbidden},
		{"shared channel create", mktOperator, post("/v1/notifications/channels", nil, `{"namespace":"*","name":"x"}`), http.StatusForbidden},
		{"shared profile create", mktOperator, post("/v1/agentprofiles", nil, `{"namespace":"*","name":"x"}`), http.StatusForbidden},
		{"shared policy create", mktOperator, post("/v1/notifications/policies", nil, `{"namespace":"*"}`), http.StatusForbidden},
		{"cluster operator creates shared", clusterOperator, post("/v1/notifications/channels", nil, `{"namespace":"*"}`), http.StatusOK},
		{"cluster operator sends a form (handler decides)", clusterOperator, nsCall{method: http.MethodPost, pattern: "/v1/agentprofiles", body: `namespace=*`, contentType: echo.MIMEApplicationForm}, http.StatusOK},
		{"confined create must be JSON", mktOperator, nsCall{method: http.MethodPost, pattern: "/v1/agentprofiles", body: `namespace=marketing`, contentType: echo.MIMEApplicationForm}, http.StatusUnsupportedMediaType},
		{"viewer creates (pre-check before body)", mktViewer, post("/v1/notifications/channels", nil, `{not json`), http.StatusForbidden},
		{"channel list", mktViewer, get("/v1/notifications/channels", nil), http.StatusOK},
		{"policy list", mktViewer, get("/v1/notifications/policies", nil), http.StatusOK},
		{"profile list", mktViewer, get("/v1/agentprofiles", nil), http.StatusOK},
	}
	for _, tc := range cases {
		res := f.do(t, tc.who, tc.call)
		require.Equal(t, tc.status, res.status, tc.name)
		if tc.status == http.StatusOK && tc.call.body != "" {
			require.Equal(t, tc.call.body, res.body, "%s: the handler must still read the whole body", tc.name)
		}
	}
	// A confined principal's list is a collection: the handler gets its
	// allowed namespaces (and adds "*" itself, authz.SharedObjectFilter).
	res := f.do(t, mktViewer, get("/v1/agentprofiles", nil))
	require.NotNil(t, res.decision)
	require.Equal(t, []string{"marketing"}, res.decision.Namespaces)
	require.False(t, res.decision.Unrestricted)

	// A denied write of each shared kind is audited against the object's
	// namespace, and names no pending item any more.
	for _, pattern := range []struct {
		path string
		id   uuid.UUID
	}{
		{"/v1/agentprofiles/:id", f.finProfile},
		{"/v1/notifications/channels/:id", f.finChannel},
	} {
		f.do(t, mktOperator, nsCall{method: http.MethodDelete, pattern: pattern.path, params: pv("id", pattern.id.String())})
		denial := f.lastDenial(t)
		requireAuditMetadata(t, denial, map[string]any{"reason": "insufficient_role"})
		require.NotContains(t, string(denial.Metadata), "pending", pattern.path)
		require.Equal(t, "finance", denial.Namespace, pattern.path)
	}
	// A * object denial is audited against *.
	f.do(t, mktOperator, nsCall{method: http.MethodDelete, pattern: "/v1/notifications/policies/:id", params: pv("id", f.sharedPolicy.String())})
	require.Equal(t, "*", f.lastDenial(t).Namespace)
}

func TestNamespacedDatasetRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktOperator := f.key(t, models.RoleOperator, `["marketing"]`)
	finViewer := f.key(t, models.RoleViewer, `["finance"]`)
	mktViewer := f.key(t, models.RoleViewer, `["marketing"]`)
	both := f.key(t, models.RoleOperator, `["marketing","finance"]`)
	advance := func(who cred, name string) int {
		return f.do(t, who, post("/v1/datasets/:ns/:name/advance", pv("ns", "_", "name", name), `{"watermark":"x"}`)).status
	}
	// B6: a write needs the role in every producer namespace; a dataset with
	// no declared producer (only a consumer, only source declarations, or
	// nothing at all) is cluster-only.
	require.Equal(t, http.StatusOK, advance(both, "sales"))
	require.Equal(t, http.StatusOK, advance(mktOperator, "mkt-only"), "a consumer in finance owns nothing")
	require.Equal(t, http.StatusForbidden, advance(mktOperator, "sales"), "finance co-produces sales")
	requireAuditMetadata(t, f.lastDenial(t), map[string]any{"pending": nil, "namespaces": []any{"finance", "marketing"}})
	for _, name := range []string{"orphan", "external"} {
		require.Equal(t, http.StatusForbidden, advance(both, name), name)
	}
	require.Equal(t, http.StatusOK, advance(f.key(t, models.RoleRunner, ""), "orphan"), "a cluster runner advances exactly as before")

	release := func(who cred, hold string) int {
		return f.do(t, who, post("/v1/datasets/holds/:id/release", pv("id", hold), `{"reason":"ok"}`)).status
	}
	require.Equal(t, http.StatusForbidden, release(mktOperator, f.salesHold.String()))
	require.Equal(t, http.StatusOK, release(both, f.salesHold.String()))
	require.Equal(t, http.StatusOK, release(mktOperator, f.mktHold.String()))
	require.Equal(t, http.StatusNotFound, release(both, uuid.NewString()))
	require.Equal(t, http.StatusBadRequest, release(both, "not-a-hold"))
	require.Equal(t, http.StatusOK, release(f.key(t, models.RoleOperator, ""), f.salesHold.String()))

	// Reads need viewer in any producer namespace. A dataset the caller may
	// not read and one that does not exist answer the same audited 403.
	for _, route := range []string{"/v1/datasets/:ns/:name", "/v1/datasets/:ns/:name/derivations", "/v1/datasets/:ns/:name/metrics"} {
		read := func(who cred, name string) nsResult {
			return f.do(t, who, get(route, pv("ns", "_", "name", name)))
		}
		require.Equal(t, http.StatusOK, read(mktViewer, "sales").status, route)
		require.Equal(t, http.StatusOK, read(finViewer, "sales").status, route)
		require.Equal(t, http.StatusOK, read(mktViewer, "mkt-only").status, route)
		foreign, missing := read(finViewer, "mkt-only"), read(finViewer, "no-such-dataset")
		require.Equal(t, http.StatusForbidden, foreign.status, route)
		require.Equal(t, foreign.status, missing.status, route)
		require.Equal(t, foreign.message, missing.message, route)
		require.Equal(t, http.StatusForbidden, read(mktViewer, "external").status, route)
	}
}

func TestNamespacedAuditQuery(t *testing.T) {
	f := newNSFixture(t)
	mktAdmin := f.key(t, models.RoleAdmin, `["marketing"]`)
	clusterAdmin := f.key(t, models.RoleAdmin, "")
	audit := func(who cred, query string) int {
		return f.do(t, who, nsCall{method: http.MethodGet, pattern: "/v1/auth/audit", query: query}).status
	}
	require.Equal(t, http.StatusOK, audit(mktAdmin, "namespace=marketing"))
	require.Equal(t, http.StatusForbidden, audit(mktAdmin, ""), "omitted ⇒ * ⇒ cluster admin")
	require.Equal(t, http.StatusForbidden, audit(mktAdmin, "namespace=finance"))
	require.Equal(t, http.StatusForbidden, audit(mktAdmin, "namespace=*"))
	require.Equal(t, http.StatusBadRequest, audit(mktAdmin, "namespace=Bad"))
	require.Equal(t, http.StatusOK, audit(clusterAdmin, ""))
	require.Equal(t, http.StatusOK, audit(clusterAdmin, "namespace=Bad"), "an unrestricted admin reaches the handler, which validates")
}

func TestNamespacedClusterAndPendingRoutes(t *testing.T) {
	f := newNSFixture(t)
	mktAdmin := f.key(t, models.RoleAdmin, `["marketing"]`)
	for _, call := range []nsCall{
		get("/v1/atoms", nil),
		get("/metrics", nil),
		get("/v1/system/nodes", nil),
		post("/v1/cache/prune", nil, ""),
	} {
		res := f.do(t, mktAdmin, call)
		require.Equal(t, http.StatusForbidden, res.status, call.pattern)
		entry := f.lastDenial(t)
		require.Equal(t, "*", entry.Namespace, call.pattern)
	}
	for _, tc := range []struct {
		call    nsCall
		pending string
	}{
		{post("/v1/events", nil, `{}`), "A6"},
		{get("/v1/lineage/impact", nil), "B5"},
	} {
		res := f.do(t, mktAdmin, tc.call)
		require.Equal(t, http.StatusForbidden, res.status, tc.call.pattern)
		requireAuditMetadata(t, f.lastDenial(t), map[string]any{"reason": "insufficient_role", "pending": tc.pending})
	}
	// Snapshot reads are live for confined principals (B7): allowed in the
	// job's namespace, and a foreign job's denial audits against its namespace.
	for _, route := range []string{"/v1/jobs/:id/blame", "/v1/jobs/:id/topology", "/v1/jobs/:id/topology/history"} {
		require.Equal(t, http.StatusOK, f.do(t, mktAdmin, get(route, pv("id", f.mktJob.String()))).status, route)
		require.Equal(t, http.StatusForbidden, f.do(t, mktAdmin, get(route, pv("id", f.finJob.String()))).status, route)
		require.Equal(t, "finance", f.lastDenial(t).Namespace, route)
	}

	// Identity routes pass any authenticated principal, even with no grants.
	empty := f.key(t, models.RoleViewer, `[]`)
	for _, who := range []cred{mktAdmin, empty} {
		require.Equal(t, http.StatusOK, f.do(t, who, get("/auth/whoami", nil)).status)
		require.Equal(t, http.StatusOK, f.do(t, who, get("/v1/system/features", nil)).status)
		require.Equal(t, http.StatusOK, f.do(t, who, post("/auth/logout", nil, "")).status)
	}
	require.Equal(t, http.StatusForbidden, f.do(t, empty, get("/v1/jobs/:id/dag", pv("id", f.mktJob.String()))).status)
}

// TestUnrestrictedAuditRowsCarryNamespace: an unrestricted principal's write
// is audited against the target's namespace (resolved best-effort), and a
// cluster route against "*" — the additive part of the no-policy guarantee.
func TestUnrestrictedAuditRowsCarryNamespace(t *testing.T) {
	f := newNSFixture(t)
	clusterOperator := f.key(t, models.RoleOperator, "")
	res := f.do(t, clusterOperator, nsCall{method: http.MethodPut, pattern: "/v1/jobs/:id/pause", params: pv("id", f.mktJob.String())})
	require.Equal(t, http.StatusOK, res.status)
	require.True(t, res.decision.Unrestricted)
	var entry models.AuditLog
	require.NoError(t, f.db.Where("action = ?", auth.ActionJobPause).First(&entry).Error)
	require.Equal(t, "marketing", entry.Namespace)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(entry.Metadata, &metadata))
	require.Equal(t, map[string]any{"method": "PUT", "path": "/v1/jobs/:id/pause"}, metadata,
		"the metadata keeps its pre-namespace shape; one namespace needs no list")

	// A write on the moved job's old run spans two namespaces: "*" plus the list.
	res = f.do(t, clusterOperator, post("/v1/jobs/:id/runs/:run_id/retry", pv("id", f.movedJob.String(), "run_id", f.movedRun.String()), ""))
	require.Equal(t, http.StatusOK, res.status)
	entry = models.AuditLog{}
	require.NoError(t, f.db.Where("action = ?", auth.ActionRunRetry).First(&entry).Error)
	require.Equal(t, "*", entry.Namespace)
	requireAuditMetadata(t, entry, map[string]any{"namespaces": []any{"finance", "marketing"}})

	// A best-effort lookup failure (missing job) leaves "" and changes nothing.
	missing := pv("id", uuid.NewString())
	res = f.do(t, clusterOperator, nsCall{method: http.MethodPut, pattern: "/v1/jobs/:id/unpause", params: missing})
	require.Equal(t, http.StatusOK, res.status, "the handler, not the audit lookup, answers for a missing job")
	entry = models.AuditLog{}
	require.NoError(t, f.db.Where("action = ?", auth.ActionJobUnpause).First(&entry).Error)
	require.Equal(t, "", entry.Namespace)

	// Cluster routes record "*"; reads never pay for a lookup.
	res = f.do(t, clusterOperator, get("/v1/jobs/:id/queue", pv("id", f.mktJob.String())))
	require.Equal(t, http.StatusOK, res.status)
	require.Nil(t, res.decision.Namespaces, "a read route does no audit lookup for an unrestricted principal")

	// ...but the handler, which loads the job anyway, notes its namespace for
	// the queue-read audit row.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+clusterOperator.bearer)
	_, err := callMiddlewareWithDeps(t, f.deps, req, &echo.RouteInfo{Path: "/v1/jobs/:id/queue", Method: http.MethodGet},
		pv("id", f.mktJob.String()), func(c *echo.Context) error {
			authz.NoteNamespace(c, "marketing")
			return c.JSON(http.StatusOK, []any{})
		})
	require.NoError(t, err)
	entry = models.AuditLog{}
	require.NoError(t, f.db.Where("action = ?", auth.ActionRunQueueRead).Order("timestamp DESC").First(&entry).Error)
	require.Equal(t, "marketing", entry.Namespace)
}

// TestNamespaceDenialFromHandlerIsAudited: a handler's *authz.Denial becomes
// the same audited 403 the middleware's own denials produce.
func TestNamespaceDenialFromHandlerIsAudited(t *testing.T) {
	f := newNSFixture(t)
	mktRunner := f.key(t, models.RoleRunner, `["marketing"]`)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/test", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer "+mktRunner.bearer)
	_, err := callMiddlewareWithDeps(t, f.deps, req, &echo.RouteInfo{Path: "/v1/jobs/:id/run", Method: http.MethodPost},
		pv("id", f.mktJob.String()), func(c *echo.Context) error {
			return authz.RequireRoleIn(authmw.GetPrincipal(c), models.RoleViewer, "finance")
		})
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok, "%v", err)
	require.Equal(t, http.StatusForbidden, he.Code)
	require.Equal(t, "insufficient permissions", he.Message)
	entry := f.lastDenial(t)
	require.Equal(t, "finance", entry.Namespace)
	requireAuditMetadata(t, entry, map[string]any{"reason": "insufficient_role", "required_role": "viewer", "namespaces": []any{"finance"}})
}
