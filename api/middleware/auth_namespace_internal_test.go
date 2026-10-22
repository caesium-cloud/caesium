package middleware

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// countingDB opens a test database that counts every read the namespace
// lookups could issue.
func countingDB(t *testing.T) (*gorm.DB, *atomic.Int64) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	var reads atomic.Int64
	hook := func(*gorm.DB) { reads.Add(1) }
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:count_query", hook))
	require.NoError(t, db.Callback().Row().Before("gorm:row").Register("test:count_row", hook))
	require.NoError(t, db.Callback().Raw().Before("gorm:raw").Register("test:count_raw", hook))
	return db, &reads
}

// routeContext builds a request routed to path with every parameter a route
// can carry set to a fresh value, so a resolver (if one ran) would find no row.
func routeContext(method, path, body string) *echo.Context {
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), method, "/oracle", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	c := e.NewContext(req, httptest.NewRecorder())
	pv := echo.PathValues{
		{Name: "id", Value: uuid.NewString()},
		{Name: "run_id", Value: uuid.NewString()},
		{Name: "backfill_id", Value: uuid.NewString()},
		{Name: "queue_id", Value: uuid.NewString()},
		{Name: "approval_id", Value: uuid.NewString()},
		{Name: "ns", Value: "_"},
		{Name: "name", Value: "orders"},
	}
	c.InitializeRoute(&echo.RouteInfo{Path: path, Method: method}, &pv)
	return c
}

var allRoles = []models.Role{models.RoleViewer, models.RoleRunner, models.RoleOperator, models.RoleAdmin}

// TestNoPolicyDecisionsMatchClusterRoleOracle is the no-policy equivalence
// proof. With no policy file every principal holds its role at "*". For every
// route in the table and every role, the namespace-aware decision must be
// exactly master's HasRole(role, required) — same allow/deny, same denial
// reason, no pending marker, no namespace metadata — and it must not read the
// database at all. (internal/auth's TestRoutePolicyRolesMatchMaster proves
// every route's required role is master's.)
func TestNoPolicyDecisionsMatchClusterRoleOracle(t *testing.T) {
	db, reads := countingDB(t)
	svc := auth.NewService(db)
	reads.Store(0)

	checked := 0
	for key, rp := range auth.RoutePolicies() {
		method, path, ok := strings.Cut(key, " ")
		require.True(t, ok, key)
		for _, role := range allRoles {
			principal := &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: policy.Grants{"*": role}, Subject: "csk_oracle"}
			c := routeContext(method, path, `{"namespace":"*"}`)

			decision, err := authorizeRoute(c, svc, principal, key, rp)
			require.NotNil(t, decision, key)
			if auth.HasRole(role, rp.Role) {
				require.NoError(t, err, "%s as %s at *: master allows", key, role)
				require.Nil(t, decision.Namespaces, "%s: an unrestricted decision resolves nothing", key)
				_, restricted := authz.AllowedNamespaces(c.Request().Context())
				require.False(t, restricted, key)
			} else {
				denial, isDenial := err.(*routeDenial)
				require.True(t, isDenial, "%s as %s at *: master denies insufficient_role, got %v", key, role, err)
				require.Equal(t, "insufficient_role", denial.reason, key)
				require.Equal(t, rp.Role, denial.required, key)
				require.Empty(t, denial.pending, "%s: a cluster principal never sees a pending marker", key)
				require.False(t, confinedPrincipal(principal), "%s: no namespace metadata for a cluster principal", key)
			}
			checked++
		}
	}
	require.Equal(t, int64(0), reads.Load(), "the authorisation path must not read the database for a principal holding its role at *")
	require.Equal(t, len(auth.RoutePolicies())*len(allRoles), checked)
}

// TestIdentityRoutesPassEmptyGrants: an authenticated principal with no grant
// at all may still ask who it is.
func TestIdentityRoutesPassEmptyGrants(t *testing.T) {
	db, reads := countingDB(t)
	svc := auth.NewService(db)
	reads.Store(0)
	empty := &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: policy.Grants{}, Subject: "csk_empty"}
	for key, rp := range auth.RoutePolicies() {
		method, path, _ := strings.Cut(key, " ")
		decision, err := authorizeRoute(routeContext(method, path, ""), svc, empty, key, rp)
		if rp.Class == auth.RouteClassIdentity {
			require.NoError(t, err, key)
			require.Equal(t, "", decision.AuditNamespace(), key)
			continue
		}
		_, isDenial := err.(*routeDenial)
		require.True(t, isDenial, "%s must deny empty grants, got %v", key, err)
	}
	require.Equal(t, int64(0), reads.Load(), "empty grants are denied before any lookup")
}

// TestConfinedPreCheckDeniesBeforeResolution: a confined principal whose best
// grant is below the route's role is denied before any lookup, body parse or
// 404 — the same 403 master returns.
func TestConfinedPreCheckDeniesBeforeResolution(t *testing.T) {
	db, reads := countingDB(t)
	svc := auth.NewService(db)
	reads.Store(0)
	viewer := &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: policy.Grants{"marketing": models.RoleViewer}, Subject: "csk_mkt_viewer"}
	for key, rp := range auth.RoutePolicies() {
		if rp.Class == auth.RouteClassIdentity || models.RoleLevel(rp.Role) <= models.RoleLevel(models.RoleViewer) {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		_, err := authorizeRoute(routeContext(method, path, "{not json"), svc, viewer, key, rp)
		denial, isDenial := err.(*routeDenial)
		require.True(t, isDenial, "%s: want insufficient_role before resolution, got %v", key, err)
		require.Equal(t, "insufficient_role", denial.reason)
		require.Empty(t, denial.pending, "%s: the role pre-check comes before the pending marker", key)
	}
	require.Equal(t, int64(0), reads.Load())
}

// TestConfinedPendingAndClusterRoutesDeny: pending and cluster routes are
// cluster-only for a confined principal whose grant reaches the route's role.
func TestConfinedPendingAndClusterRoutesDeny(t *testing.T) {
	db, reads := countingDB(t)
	svc := auth.NewService(db)
	reads.Store(0)
	admin := &auth.Principal{Kind: auth.PrincipalAPIKey, Grants: policy.Grants{"marketing": models.RoleAdmin}, Subject: "csk_mkt_admin"}
	for key, rp := range auth.RoutePolicies() {
		if !rp.Pending() && rp.Class != auth.RouteClassCluster {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		_, err := authorizeRoute(routeContext(method, path, ""), svc, admin, key, rp)
		denial, isDenial := err.(*routeDenial)
		require.True(t, isDenial, "%s: want a denial, got %v", key, err)
		require.Equal(t, rp.PendingItem, denial.pending, key)
	}
	require.Equal(t, int64(0), reads.Load())
}

// TestConfinedCollectionAllowedSetUsesRouteRole: the allowed set is computed
// for the route's role, not just viewer, and an empty set denies.
func TestConfinedCollectionAllowedSetUsesRouteRole(t *testing.T) {
	p := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleAdmin, "finance": models.RoleViewer, "ops": models.RoleOperator}}
	require.Equal(t, []string{"finance", "marketing", "ops"}, NamespacesGranting(p, models.RoleViewer))
	require.Equal(t, []string{"marketing", "ops"}, NamespacesGranting(p, models.RoleOperator))
	require.Equal(t, []string{"marketing"}, NamespacesGranting(p, models.RoleAdmin))

	live := auth.RoutePolicy{Role: models.RoleAdmin, Class: auth.RouteClassCollection}
	decision, err := authorizeRoute(routeContext(http.MethodGet, "/v1/fake", ""), nil, p, "GET /v1/fake", live)
	require.NoError(t, err)
	require.Equal(t, []string{"marketing"}, decision.Namespaces)
	require.Equal(t, "*", decision.AuditNamespace())

	viewerOnly := &auth.Principal{Grants: policy.Grants{"finance": models.RoleViewer}}
	_, err = authorizeRoute(routeContext(http.MethodGet, "/v1/fake", ""), nil, viewerOnly, "GET /v1/fake", live)
	require.IsType(t, &routeDenial{}, err)
}

func TestRequirementsSatisfied(t *testing.T) {
	p := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleViewer}}
	require.True(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleRunner, "marketing")}))
	require.False(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleRunner, "marketing", "finance")}))
	require.True(t, requirementsSatisfied(p, []nsRequirement{{Namespaces: []string{"marketing", "finance"}, Role: models.RoleRunner, Any: true}}))
	require.False(t, requirementsSatisfied(p, []nsRequirement{{Namespaces: []string{"ops", "finance"}, Role: models.RoleRunner, Any: true}}))
	require.False(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleViewer)}), "no owner ⇒ cluster-only")
	require.False(t, requirementsSatisfied(p, []nsRequirement{{Role: models.RoleViewer, Any: true}}))
	require.False(t, requirementsSatisfied(p, nil))
	require.False(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleViewer, "*")}), "writing a * object needs the role at *")
	require.True(t, requirementsSatisfied(p, []nsRequirement{{Namespaces: []string{"*"}, Role: models.RoleViewer, AnyGrant: true}}))
	require.False(t, requirementsSatisfied(p, []nsRequirement{{Namespaces: []string{"*"}, Role: models.RoleAdmin, AnyGrant: true}}))
	// Mixed roles (replay): runner in the job's namespace AND viewer in the run's.
	require.True(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleRunner, "marketing"), every(models.RoleViewer, "finance")}))
	require.False(t, requirementsSatisfied(p, []nsRequirement{every(models.RoleRunner, "finance"), every(models.RoleViewer, "marketing")}))
	// "" is the default namespace, never the cluster role.
	clusterAdminNoDefault := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleAdmin}}
	require.False(t, requirementsSatisfied(clusterAdminNoDefault, []nsRequirement{every(models.RoleViewer, "")}))
}

// eofTracker records whether the client's body was read to the end.
type eofTracker struct {
	r   io.Reader
	eof bool
}

func (e *eofTracker) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if errors.Is(err, io.EOF) {
		e.eof = true
	}
	return n, err
}

// TestConfinedBodyIsBufferedBeforeResolution: a confined principal's body is
// read to the end before the namespace decision (so a client cannot hold the
// request open past authorisation), and the handler still reads all of it.
func TestConfinedBodyIsBufferedBeforeResolution(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	now := time.Now().UTC()
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "buffered", Namespace: "marketing", CreatedAt: now, UpdatedAt: now}).Error)

	body := `{"params":{"region":"eu"}}`
	src := &eofTracker{r: strings.NewReader(body)}
	e := echo.New()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs/"+jobID.String()+"/run", src)
	req.ContentLength = -1 // streamed: length unknown until the client finishes
	c := e.NewContext(req, httptest.NewRecorder())
	pv := echo.PathValues{{Name: "id", Value: jobID.String()}}
	c.InitializeRoute(&echo.RouteInfo{Path: "/v1/jobs/:id/run", Method: http.MethodPost}, &pv)

	p := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleRunner}}
	_, err := authorizeRoute(c, svc, p, "POST /v1/jobs/:id/run", auth.RoutePolicies()["POST /v1/jobs/:id/run"])
	require.NoError(t, err)
	require.True(t, src.eof, "the body must be fully read before the decision")
	got, err := io.ReadAll(c.Request().Body)
	require.NoError(t, err)
	require.Equal(t, body, string(got))

	big := &eofTracker{r: strings.NewReader(strings.Repeat("x", confinedBodyBufferLimit+1))}
	req = httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/jobs/"+jobID.String()+"/run", big)
	req.ContentLength = -1
	c = e.NewContext(req, httptest.NewRecorder())
	c.InitializeRoute(&echo.RouteInfo{Path: "/v1/jobs/:id/run", Method: http.MethodPost}, &pv)
	_, err = authorizeRoute(c, svc, p, "POST /v1/jobs/:id/run", auth.RoutePolicies()["POST /v1/jobs/:id/run"])
	require.Equal(t, http.StatusRequestEntityTooLarge, echo.StatusCode(err))
}

// --- collection guard -------------------------------------------------------

func guardKey(t *testing.T, db *gorm.DB, svc *auth.Service, namespaces string) string {
	t.Helper()
	created, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)
	if namespaces != "" {
		require.NoError(t, db.Model(&models.APIKey{}).Where("id = ?", created.Key.ID).
			Update("namespaces", datatypes.JSON(namespaces)).Error)
	}
	return created.Plaintext
}

// TestCollectionGuardCatchesUnfilteredHandler proves the test-only guard
// bites: a live collection route whose handler forgets the namespace filter
// fails for a confined principal; one that filters passes and sees exactly
// the allowed set.
func TestCollectionGuardCatchesUnfilteredHandler(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	deps := AuthDeps{
		Service: svc, Auditor: auth.NewAuditLogger(db), Limiter: auth.NewRateLimiter(100, time.Minute),
		RequireCollectionFilter: true,
	}
	const route = "/v1/fake/collection"
	prev := lookupRoutePolicy
	lookupRoutePolicy = func(method, path string) (auth.RoutePolicy, bool) {
		if method == http.MethodGet && path == route {
			return auth.RoutePolicy{Role: models.RoleViewer, Class: auth.RouteClassCollection}, true
		}
		return prev(method, path)
	}
	t.Cleanup(func() { lookupRoutePolicy = prev })

	confined := guardKey(t, db, svc, `["marketing"]`)
	cluster := guardKey(t, db, svc, "")

	serve := func(key string, deps AuthDeps, handler echo.HandlerFunc) (int, error) {
		e := echo.New()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, route, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.InitializeRoute(&echo.RouteInfo{Path: route, Method: http.MethodGet}, &echo.PathValues{})
		err := Auth(deps)(handler)(c)
		return rec.Code, err
	}

	forgetful := func(c *echo.Context) error {
		var jobs []models.Job
		if err := db.WithContext(c.Request().Context()).Find(&jobs).Error; err != nil {
			return err
		}
		return c.JSON(http.StatusOK, jobs)
	}
	var seen []string
	filtering := func(c *echo.Context) error {
		allowed, restricted := authz.AllowedNamespaces(c.Request().Context())
		if restricted {
			seen = allowed
		}
		var jobs []models.Job
		if err := authz.NamespaceFilter(db.WithContext(c.Request().Context()), "jobs.namespace").Find(&jobs).Error; err != nil {
			return err
		}
		require.Equal(t, allowed, c.Get(authz.ContextKeyAllowedNamespaces).([]string))
		return c.JSON(http.StatusOK, jobs)
	}
	rejecting := func(c *echo.Context) error {
		return c.String(http.StatusBadRequest, "bad")
	}

	_, err := serve(confined, deps, forgetful)
	require.ErrorIs(t, err, errCollectionUnfiltered, "the guard must catch a collection handler that forgot the filter")

	code, err := serve(confined, deps, filtering)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"marketing"}, seen)

	_, err = serve(confined, deps, rejecting)
	require.NoError(t, err, "an error response returned no rows")

	code, err = serve(cluster, deps, forgetful)
	require.NoError(t, err, "an unrestricted principal has nothing to filter")
	require.Equal(t, http.StatusOK, code)

	production := deps
	production.RequireCollectionFilter = false
	_, err = serve(confined, production, forgetful)
	require.NoError(t, err, "the guard is test-only")
}

// TestDatasetReadResolverUsesAnyMode exercises the any-of mode B6 switched
// on for the dataset reads and the every-of mode the
// live writes use, on the same two-owner dataset.
func TestDatasetReadResolverUsesAnyMode(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}).Error)
	for alias, namespace := range map[string]string{"mkt-producer": "marketing", "fin-producer": "finance"} {
		jobID := uuid.New()
		require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: alias, Namespace: namespace, TriggerID: triggerID, CreatedAt: now, UpdatedAt: now}).Error)
		require.NoError(t, db.Create(&models.DatasetDeclaration{
			ID: uuid.New(), JobID: jobID, JobAlias: alias, Name: "orders", Direction: models.DatasetDirectionProduces,
			CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	marketing := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleRunner}}
	both := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleRunner, "finance": models.RoleRunner}}

	read := auth.RoutePolicy{Role: models.RoleViewer, Class: auth.RouteClassNamespaced} // the live B6 policy
	decision, err := authorizeRoute(routeContext(http.MethodGet, "/v1/datasets/:ns/:name", ""), svc, marketing, "GET /v1/datasets/:id/:id", read)
	require.NoError(t, err, "a read needs viewer in any owner namespace")
	require.Equal(t, []string{"finance", "marketing"}, decision.Namespaces)
	require.Equal(t, "*", decision.AuditNamespace())

	// The hold release resolves the held dataset's
	// producers and needs the role in every one.
	holdID := uuid.New()
	key := "orders/" + holdID.String()
	require.NoError(t, db.Create(&models.DatasetHold{ID: holdID, Name: "orders", Status: models.DatasetHoldStatusActive, ActiveKey: &key,
		HeldByJobID: uuid.New(), OpenedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	release := auth.RoutePolicy{Role: models.RoleOperator, Class: auth.RouteClassNamespaced} // the live B6 policy
	releaseCtx := func(id string) *echo.Context {
		c := routeContext(http.MethodPost, "/v1/datasets/holds/:id/release", "")
		pv := echo.PathValues{{Name: "id", Value: id}}
		c.InitializeRoute(&echo.RouteInfo{Path: "/v1/datasets/holds/:id/release", Method: http.MethodPost}, &pv)
		return c
	}
	operatorMkt := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator}}
	operatorBoth := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleOperator}}
	_, err = authorizeRoute(releaseCtx(holdID.String()), svc, operatorMkt, "POST /v1/datasets/holds/:id/release", release)
	require.IsType(t, &routeDenial{}, err)
	decision, err = authorizeRoute(releaseCtx(holdID.String()), svc, operatorBoth, "POST /v1/datasets/holds/:id/release", release)
	require.NoError(t, err)
	require.Equal(t, []string{"finance", "marketing"}, decision.Namespaces)
	_, err = authorizeRoute(releaseCtx(uuid.NewString()), svc, operatorBoth, "POST /v1/datasets/holds/:id/release", release)
	require.Equal(t, http.StatusNotFound, echo.StatusCode(err))
	_, err = authorizeRoute(releaseCtx("not-a-hold"), svc, operatorBoth, "POST /v1/datasets/holds/:id/release", release)
	require.Equal(t, http.StatusBadRequest, echo.StatusCode(err))

	advance := auth.RoutePolicy{Role: models.RoleRunner, Class: auth.RouteClassNamespaced} // the live B6 policy
	_, err = authorizeRoute(routeContext(http.MethodPost, "/v1/datasets/:ns/:name/advance", ""), svc, marketing, "POST /v1/datasets/:id/:id/advance", advance)
	require.IsType(t, &routeDenial{}, err, "a write needs the role in every owner namespace")
	_, err = authorizeRoute(routeContext(http.MethodPost, "/v1/datasets/:ns/:name/advance", ""), svc, both, "POST /v1/datasets/:id/:id/advance", advance)
	require.NoError(t, err)
}

// TestBackfillResolver: one backfill resolves to the job's current namespace
// AND the backfill's own, and a backfill under another job's path is 404.
func TestBackfillResolver(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}).Error)
	job := func(alias, namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Job{ID: id, Alias: alias, Namespace: namespace, TriggerID: triggerID, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	backfill := func(jobID uuid.UUID, namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.Backfill{ID: id, JobID: jobID, Namespace: namespace, Status: models.BackfillStatusRunning,
			Start: now, End: now.Add(time.Hour), MaxConcurrent: 1, Reprocess: models.ReprocessNone, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	mktJob, movedJob := job("bf-mkt", "marketing"), job("bf-moved", "finance")
	mktBackfill := backfill(mktJob, "marketing")
	// Started while the job lived in marketing; the job has since moved.
	movedBackfill := backfill(movedJob, "marketing")

	marketing := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator}}
	finance := &auth.Principal{Grants: policy.Grants{"finance": models.RoleOperator}}
	both := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator, "finance": models.RoleOperator}}
	for _, route := range []struct {
		method, path, key string
		role              models.Role
	}{
		{http.MethodGet, "/v1/jobs/:id/backfills/:backfill_id", "GET /v1/jobs/:id/backfills/:id", models.RoleViewer},
		{http.MethodPut, "/v1/jobs/:id/backfills/:backfill_id/cancel", "PUT /v1/jobs/:id/backfills/:id/cancel", models.RoleOperator},
	} {
		live := auth.RoutePolicy{Role: route.role, Class: auth.RouteClassNamespaced}
		authorize := func(p *auth.Principal, jobID, backfillID string) (*authz.Decision, error) {
			c := routeContext(route.method, route.path, "")
			pv := echo.PathValues{{Name: "id", Value: jobID}, {Name: "backfill_id", Value: backfillID}}
			c.InitializeRoute(&echo.RouteInfo{Path: route.path, Method: route.method}, &pv)
			return authorizeRoute(c, svc, p, route.key, live)
		}

		decision, err := authorize(marketing, mktJob.String(), mktBackfill.String())
		require.NoError(t, err, route.key)
		require.Equal(t, []string{"marketing"}, decision.Namespaces, route.key)
		_, err = authorize(finance, mktJob.String(), mktBackfill.String())
		require.IsType(t, &routeDenial{}, err, route.key)

		// Under a moved job the role is needed in both namespaces.
		_, err = authorize(marketing, movedJob.String(), movedBackfill.String())
		require.IsType(t, &routeDenial{}, err, "%s: the job now lives in finance", route.key)
		_, err = authorize(finance, movedJob.String(), movedBackfill.String())
		require.IsType(t, &routeDenial{}, err, "%s: the backfill was started in marketing", route.key)
		decision, err = authorize(both, movedJob.String(), movedBackfill.String())
		require.NoError(t, err, route.key)
		require.Equal(t, []string{"finance", "marketing"}, decision.Namespaces, route.key)

		// Another job's backfill, an unknown backfill, an unknown job, and
		// malformed ids.
		_, err = authorize(both, mktJob.String(), movedBackfill.String())
		require.Equal(t, http.StatusNotFound, echo.StatusCode(err), "%s: a backfill of another job", route.key)
		_, err = authorize(both, mktJob.String(), uuid.NewString())
		require.Equal(t, http.StatusNotFound, echo.StatusCode(err), route.key)
		_, err = authorize(both, uuid.NewString(), mktBackfill.String())
		require.Equal(t, http.StatusNotFound, echo.StatusCode(err), route.key)
		_, err = authorize(both, mktJob.String(), "not-a-backfill")
		require.Equal(t, http.StatusBadRequest, echo.StatusCode(err), route.key)
		_, err = authorize(both, "not-a-job", mktBackfill.String())
		require.Equal(t, http.StatusBadRequest, echo.StatusCode(err), route.key)
	}
}

// TestCreateBodyNamespaceResolver: a shared-object create resolves the body
// namespace exactly as the handler's binder will read it (JSON only, the
// whole body, NormalizeObjectNamespace), refuses anything it could read
// differently, and leaves the whole body for the handler. The create routes
// are pending D4 today; this is the resolver D4 switches on.
func TestCreateBodyNamespaceResolver(t *testing.T) {
	live := auth.RoutePolicy{Role: models.RoleOperator, Class: auth.RouteClassNamespaced} // as D4 will leave it
	operator := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleOperator}}
	try := func(body, contentType string) (*echo.Context, error) {
		e := echo.New()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/agentprofiles", strings.NewReader(body))
		if contentType != "" {
			req.Header.Set(echo.HeaderContentType, contentType)
		}
		c := e.NewContext(req, httptest.NewRecorder())
		c.InitializeRoute(&echo.RouteInfo{Path: "/v1/agentprofiles", Method: http.MethodPost}, &echo.PathValues{})
		_, err := authorizeRoute(c, nil, operator, "POST /v1/agentprofiles", live)
		return c, err
	}
	for _, tc := range []struct {
		name, body, contentType string
		status                  int // 0: authorised; -1: denied
	}{
		{"in marketing", `{"namespace":"marketing","name":"x"}`, echo.MIMEApplicationJSON, 0},
		{"charset parameter", `{"namespace":"marketing"}`, "application/json; charset=utf-8", 0},
		{"shared", `{"namespace":"*"}`, echo.MIMEApplicationJSON, -1},
		{"finance", `{"namespace":"finance"}`, echo.MIMEApplicationJSON, -1},
		{"blank means default", `{"name":"x"}`, echo.MIMEApplicationJSON, -1},
		{"invalid namespace", `{"namespace":"Not A Label"}`, echo.MIMEApplicationJSON, http.StatusBadRequest},
		{"malformed", `{"namespace":`, echo.MIMEApplicationJSON, http.StatusBadRequest},
		{"trailing data", `{"namespace":"marketing"} {"namespace":"*"}`, echo.MIMEApplicationJSON, http.StatusBadRequest},
		{"form", `namespace=marketing`, echo.MIMEApplicationForm, http.StatusUnsupportedMediaType},
		{"JSON-looking form", `{"namespace":"marketing"}`, echo.MIMEApplicationForm, http.StatusUnsupportedMediaType},
		{"no content type", `{"namespace":"marketing"}`, "", http.StatusUnsupportedMediaType},
		{"oversized", `{"namespace":"marketing","pad":"` + strings.Repeat("x", 1<<20) + `"}`, echo.MIMEApplicationJSON, http.StatusRequestEntityTooLarge},
	} {
		c, err := try(tc.body, tc.contentType)
		switch tc.status {
		case 0:
			require.NoError(t, err, tc.name)
			got, readErr := io.ReadAll(c.Request().Body)
			require.NoError(t, readErr)
			require.Equal(t, tc.body, string(got), "%s: the handler reads the whole body", tc.name)
		case -1:
			require.IsType(t, &routeDenial{}, err, tc.name)
		default:
			require.Equal(t, tc.status, echo.StatusCode(err), tc.name)
		}
	}
}

// TestJobPrefixResolverMatchesHandler: the id-or-prefix routes resolve the job
// with models.FindJobByIDPrefix, the handlers' lookup, and answer as the
// handlers do: full id and unique prefix resolve, an ambiguous prefix is 409,
// an invalid one 400, an unmatched one 404.
func TestJobPrefixResolverMatchesHandler(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	now := time.Now().UTC()
	mktID := uuid.MustParse("aaaa1111-0000-4000-8000-000000000001")
	finID := uuid.MustParse("aaaa2222-0000-4000-8000-000000000002")
	require.NoError(t, db.Create(&models.Job{ID: mktID, Alias: "mkt", Namespace: "marketing", CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&models.Job{ID: finID, Alias: "fin", Namespace: "finance", CreatedAt: now, UpdatedAt: now}).Error)

	live := auth.RoutePolicy{Role: models.RoleViewer, Class: auth.RouteClassNamespaced} // as B2 will leave it
	viewer := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleViewer}}
	try := func(id string) error {
		c := routeContext(http.MethodGet, "/v1/jobs/:id/manifest", "")
		pv := echo.PathValues{{Name: "id", Value: id}}
		c.InitializeRoute(&echo.RouteInfo{Path: "/v1/jobs/:id/manifest", Method: http.MethodGet}, &pv)
		_, err := authorizeRoute(c, svc, viewer, "GET /v1/jobs/:id/manifest", live)
		return err
	}
	require.NoError(t, try(mktID.String()), "full id")
	require.NoError(t, try("aaaa1111"), "unique prefix")
	require.NoError(t, try("AAAA1111-0000"), "prefixes are case-insensitive like the handler")
	require.IsType(t, &routeDenial{}, try(finID.String()))
	require.IsType(t, &routeDenial{}, try("aaaa2222"))
	require.Equal(t, http.StatusConflict, echo.StatusCode(try("aaaa")), "ambiguous prefix")
	require.Equal(t, http.StatusBadRequest, echo.StatusCode(try("zz")), "invalid prefix")
	require.Equal(t, http.StatusNotFound, echo.StatusCode(try("ffff")), "no match")
}

// TestDatasetTenancyIsDeclaredProducersOnly (design §5.1): only a produces
// declaration owns a dataset. A consumer, or a job declaring an external
// source, owns nothing, and a dataset with no declared producer is
// cluster-only.
func TestDatasetTenancyIsDeclaredProducersOnly(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	svc := auth.NewService(db)
	now := time.Now().UTC()
	mkt, fin := uuid.New(), uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: mkt, Alias: "mkt", Namespace: "marketing", CreatedAt: now, UpdatedAt: now}).Error)
	require.NoError(t, db.Create(&models.Job{ID: fin, Alias: "fin", Namespace: "finance", CreatedAt: now, UpdatedAt: now}).Error)
	declare := func(job uuid.UUID, alias, name, direction string) {
		require.NoError(t, db.Create(&models.DatasetDeclaration{ID: uuid.New(), JobID: job, JobAlias: alias, Name: name, Direction: direction, CreatedAt: now, UpdatedAt: now}).Error)
	}
	declare(mkt, "mkt", "mkt-produced", models.DatasetDirectionProduces)
	declare(fin, "fin", "mkt-produced", models.DatasetDirectionConsumes)
	declare(mkt, "mkt", "external", models.DatasetDirectionSource)
	declare(mkt, "mkt", "consumed-only", models.DatasetDirectionConsumes)

	advance := auth.RoutePolicy{Role: models.RoleRunner, Class: auth.RouteClassNamespaced} // the live B6 policy
	runner := &auth.Principal{Grants: policy.Grants{"marketing": models.RoleRunner}}
	try := func(name string) error {
		c := routeContext(http.MethodPost, "/v1/datasets/:ns/:name/advance", "")
		pv := echo.PathValues{{Name: "ns", Value: "_"}, {Name: "name", Value: name}}
		c.InitializeRoute(&echo.RouteInfo{Path: "/v1/datasets/:ns/:name/advance", Method: http.MethodPost}, &pv)
		_, err := authorizeRoute(c, svc, runner, "POST /v1/datasets/:id/:id/advance", advance)
		return err
	}
	require.NoError(t, try("mkt-produced"), "the producer's namespace owns it; a finance consumer does not")
	for _, name := range []string{"external", "consumed-only", "never-declared"} {
		require.IsType(t, &routeDenial{}, try(name), "%s has no declared producer: cluster-only", name)
	}
	require.Equal(t, http.StatusBadRequest, echo.StatusCode(try(" ")), "a blank dataset name is the handler's 400")
}
