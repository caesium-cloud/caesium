package event

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type scopeFixture struct {
	db                                 *gorm.DB
	router                             *Router
	shared, finOnly, empty             *models.Trigger
	mktJob, finJob, finOnlyJob, padJob *models.Job
}

// newScopeFixture: a "shared" trigger serving a marketing job, a finance job
// and a marketing job stored as "mkt-job " (a distinct alias); a finance-only
// trigger; and a jobless trigger. All match webhook events from github.
func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	f := &scopeFixture{db: openEventRouterTestDB(t)}
	f.shared = eventRouterTestTrigger(t, "shared")
	f.finOnly = eventRouterTestTrigger(t, "fin-only")
	f.empty = eventRouterTestTrigger(t, "empty")
	require.NoError(t, f.db.Create([]*models.Trigger{f.shared, f.finOnly, f.empty}).Error)
	now := time.Now().UTC()
	job := func(alias, namespace string, trigger uuid.UUID) *models.Job {
		return &models.Job{ID: uuid.New(), Alias: alias, Namespace: namespace, TriggerID: trigger, CreatedAt: now, UpdatedAt: now}
	}
	f.mktJob = job("mkt-job", "marketing", f.shared.ID)
	f.finJob = job("fin-job", "finance", f.shared.ID)
	f.padJob = job("mkt-job ", "marketing", f.shared.ID)
	f.finOnlyJob = job("fin-only-job", "finance", f.finOnly.ID)
	require.NoError(t, f.db.Create([]*models.Job{f.mktJob, f.finJob, f.padJob, f.finOnlyJob}).Error)
	f.router = NewRouter(f.db,
		WithTriggerLister(func(context.Context) (models.Triggers, error) {
			return models.Triggers{f.shared, f.finOnly, f.empty}, nil
		}),
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error { return nil })),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, f.router.Reload(context.Background()))
	return f
}

func scopeTestEvent() *models.IngestedEvent {
	return &models.IngestedEvent{Type: "webhook.github", Source: "github", Data: datatypes.JSON(`{"ref":"main"}`)}
}

func (f *scopeFixture) runsOf(t *testing.T, jobID uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Count(&n).Error)
	return n
}

func (f *scopeFixture) matchRows(t *testing.T, eventID uuid.UUID) map[uuid.UUID]models.EventTriggerMatch {
	t.Helper()
	var rows []models.EventTriggerMatch
	require.NoError(t, f.db.Find(&rows, "event_id = ?", eventID).Error)
	out := map[uuid.UUID]models.EventTriggerMatch{}
	for _, row := range rows {
		out[row.TriggerID] = row
	}
	return out
}

// TestRouteDeliveryScopeConfinesJobs (A6): a scoped Route delivers only to
// the jobs inside the scope (exact aliases), omits a trigger whose every job
// was withheld (no result, no match row), and keeps a jobless trigger's match
// and skip reason exactly as an unscoped Route does.
func TestRouteDeliveryScopeConfinesJobs(t *testing.T) {
	f := newScopeFixture(t)
	scope := jobdef.NewTenancyScope([]string{"marketing"}, []string{"mkt-job"})
	result, err := f.router.Route(context.Background(), scopeTestEvent(), WithDeliveryScope(scope))
	require.NoError(t, err)

	require.Equal(t, []RoutedJob{{JobID: f.mktJob.ID, Namespace: "marketing"}}, result.Delivered)
	require.ElementsMatch(t, []RoutedJob{
		{JobID: f.finJob.ID, Namespace: "finance"},
		{JobID: f.padJob.ID, Namespace: "marketing"},
		{JobID: f.finOnlyJob.ID, Namespace: "finance"},
	}, result.Withheld)

	matched := map[uuid.UUID]TriggerRouteResult{}
	for _, m := range result.MatchedTriggers {
		matched[m.TriggerID] = m
	}
	require.Len(t, matched, 2, "the finance-only trigger leaves no trace")
	require.Len(t, matched[f.shared.ID].RunsStarted, 1)
	require.Equal(t, "no jobs registered for trigger", matched[f.empty.ID].SkipReason)
	rows := f.matchRows(t, result.EventID)
	require.Len(t, rows, 2)
	require.NotContains(t, rows, f.finOnly.ID)

	require.EqualValues(t, 1, f.runsOf(t, f.mktJob.ID))
	for _, j := range []*models.Job{f.finJob, f.padJob, f.finOnlyJob} {
		require.EqualValues(t, 0, f.runsOf(t, j.ID), j.Alias)
	}
}

// TestRouteWithoutScopeUnchanged: a nil scope (an unconfined caller, auth
// off) delivers to every job and withholds nothing.
func TestRouteWithoutScopeUnchanged(t *testing.T) {
	f := newScopeFixture(t)
	result, err := f.router.Route(context.Background(), scopeTestEvent(), WithDeliveryScope(nil))
	require.NoError(t, err)
	require.Len(t, result.MatchedTriggers, 3)
	require.Len(t, result.Delivered, 4)
	require.Empty(t, result.Withheld)
	require.Len(t, f.matchRows(t, result.EventID), 3)
}

// TestRouteDeliveryScopeGuardsAdmission: a job moved out of the scope after
// the router listed it (a concurrent move committed under read committed) is
// refused inside its admission transaction; no run is created for it.
func TestRouteDeliveryScopeGuardsAdmission(t *testing.T) {
	f := newScopeFixture(t)
	afterDeliveryList = func(tx *gorm.DB) {
		require.NoError(t, tx.Model(&models.Job{}).Where("id = ?", f.mktJob.ID).Update("namespace", "finance").Error)
	}
	t.Cleanup(func() { afterDeliveryList = nil })

	result, err := f.router.Route(context.Background(), scopeTestEvent(), WithDeliveryScope(jobdef.NewTenancyScope([]string{"marketing"}, nil)))
	require.NoError(t, err)
	var shared TriggerRouteResult
	for _, m := range result.MatchedTriggers {
		if m.TriggerID == f.shared.ID {
			shared = m
		}
	}
	require.Contains(t, shared.Error, ErrDeliveryOutOfScope.Error())
	require.EqualValues(t, 0, f.runsOf(t, f.mktJob.ID), "the moved job is not admitted")
}

// TestRouteDeliveryScopeOmitsForeignTriggerRejectedEarly: a trigger whose
// every job is outside the scope leaves no trace even when firing it would
// stop before listing its jobs (a rejected trigger depth): no result, no
// match row, nothing counted as delivered.
func TestRouteDeliveryScopeOmitsForeignTriggerRejectedEarly(t *testing.T) {
	db := openEventRouterTestDB(t)
	now := time.Now().UTC()
	listener := eventRouterTestTriggerConfig(t, "depth-listener", `{"events":[{"type":"run_completed","source":"caesium"}]}`)
	require.NoError(t, db.Create(listener).Error)
	require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: "fin-depth", Namespace: "finance", TriggerID: listener.ID, CreatedAt: now, UpdatedAt: now}).Error)
	router := NewRouter(db,
		WithTriggerLister(func(context.Context) (models.Triggers, error) { return models.Triggers{listener}, nil }),
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error { return nil }), WithMaxTriggerDepth(1)),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, router.Reload(context.Background()))
	evt := func() *models.IngestedEvent {
		return &models.IngestedEvent{Type: "run_completed", Source: "caesium", Data: datatypes.JSON(`{"_trigger_depth":"5"}`)}
	}

	scoped, err := router.Route(context.Background(), evt(), WithDeliveryScope(jobdef.NewTenancyScope([]string{"marketing"}, nil)))
	require.NoError(t, err)
	require.Empty(t, scoped.MatchedTriggers)
	require.Empty(t, scoped.Delivered)
	require.Len(t, scoped.Withheld, 1)
	var rows int64
	require.NoError(t, db.Model(&models.EventTriggerMatch{}).Where("event_id = ?", scoped.EventID).Count(&rows).Error)
	require.Zero(t, rows)

	// Unscoped, the depth rejection is recorded exactly as before.
	unscoped, err := router.Route(context.Background(), evt())
	require.NoError(t, err)
	require.Len(t, unscoped.MatchedTriggers, 1)
	require.Contains(t, unscoped.MatchedTriggers[0].Error, ErrTriggerChainDepthExceeded.Error())
	require.Empty(t, unscoped.Delivered, "a trigger rejected before its jobs offered the event to none")
}

func installPolicyFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default: {}\n  marketing: {}\n  finance: {}\nbindings: []\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
}

// TestLifecycleBridgeScopedOnlyWithPolicyFile (A6): while a policy file is
// loaded, a run's lifecycle event reaches only event triggers' jobs in the
// run's persisted namespace; with no policy file the fan-out is unchanged;
// a run that cannot be resolved delivers nothing.
func TestLifecycleBridgeScopedOnlyWithPolicyFile(t *testing.T) {
	db := openEventRouterTestDB(t)
	now := time.Now().UTC()
	listener := eventRouterTestTriggerConfig(t, "lifecycle-listener", `{"events":[{"type":"run_completed","source":"caesium"}]}`)
	require.NoError(t, db.Create(listener).Error)
	mkt := &models.Job{ID: uuid.New(), Alias: "mkt-listener", Namespace: "marketing", TriggerID: listener.ID, CreatedAt: now, UpdatedAt: now}
	fin := &models.Job{ID: uuid.New(), Alias: "fin-listener", Namespace: "finance", TriggerID: listener.ID, CreatedAt: now, UpdatedAt: now}
	source := &models.Job{ID: uuid.New(), Alias: "mkt-source", Namespace: "marketing", TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create([]*models.Job{mkt, fin, source}).Error)
	sourceRun := &models.JobRun{ID: uuid.New(), JobID: source.ID, Namespace: "marketing", Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(sourceRun).Error)
	router := NewRouter(db,
		WithTriggerLister(func(context.Context) (models.Triggers, error) { return models.Triggers{listener}, nil }),
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error { return nil })),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, router.Reload(context.Background()))
	runs := func(jobID uuid.UUID) int64 {
		var n int64
		require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", jobID).Count(&n).Error)
		return n
	}
	completed := func(runID uuid.UUID) eventstore.Event {
		return eventstore.Event{Type: eventstore.TypeRunCompleted, JobID: source.ID, RunID: runID, Timestamp: now}
	}

	previous := policy.ProcessHolder()
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	router.routeLifecycleEvent(context.Background(), completed(sourceRun.ID))
	require.EqualValues(t, 1, runs(mkt.ID), "no policy file: every listener")
	require.EqualValues(t, 1, runs(fin.ID), "no policy file: every listener")

	installPolicyFile(t)
	router.routeLifecycleEvent(context.Background(), completed(sourceRun.ID))
	require.EqualValues(t, 2, runs(mkt.ID), "the run's own namespace")
	require.EqualValues(t, 1, runs(fin.ID), "never another namespace")

	router.routeLifecycleEvent(context.Background(), completed(uuid.New()))
	require.EqualValues(t, 2, runs(mkt.ID), "an unresolvable run delivers nothing")
	require.EqualValues(t, 1, runs(fin.ID))
}
