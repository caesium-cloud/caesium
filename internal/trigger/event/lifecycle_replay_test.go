package event

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type lifecycleReplayFixture struct {
	db       *gorm.DB
	router   *Router
	listener *models.Job
	event    eventstore.Event
	adopted  atomic.Int32
	launched chan struct{}
}

func newLifecycleReplayFixture(t *testing.T) *lifecycleReplayFixture {
	t.Helper()
	f := &lifecycleReplayFixture{db: openEventRouterTestDB(t), launched: make(chan struct{}, 16)}
	trigger := eventRouterTestTriggerConfig(t, "replay-listener", `{
		"events":[{"type":"run_*","source":"caesium","filter":{"job_alias":"replay-source"}}]
	}`)
	sourceTrigger := &models.Trigger{ID: uuid.New(), Alias: "replay-source-trigger", Type: models.TriggerTypeCron,
		Configuration: `{"expression":"0 0 31 2 *"}`, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	require.NoError(t, f.db.Create([]*models.Trigger{trigger, sourceTrigger}).Error)
	now := time.Now().UTC()
	source := &models.Job{ID: uuid.New(), Alias: "replay-source", Namespace: "marketing", TriggerID: sourceTrigger.ID, CreatedAt: now, UpdatedAt: now}
	f.listener = &models.Job{ID: uuid.New(), Alias: "replay-listener", Namespace: "marketing", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, f.db.Create([]*models.Job{source, f.listener}).Error)
	sourceRun := &models.JobRun{ID: uuid.New(), JobID: source.ID, Namespace: "marketing", Status: "succeeded", StartedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, f.db.Create(sourceRun).Error)
	f.event = eventstore.Event{Sequence: 389, Type: eventstore.TypeRunCompleted, JobID: source.ID, RunID: sourceRun.ID,
		Payload: []byte(`{"job_alias":"replay-source","status":"succeeded"}`)}
	f.router = f.newRouter(t)
	return f
}

func (f *lifecycleReplayFixture) newRouter(t *testing.T) *Router {
	t.Helper()
	r := NewRouter(f.db,
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error {
			f.launched <- struct{}{}
			return nil
		})),
		withStartedRunAdopter(func(uuid.UUID) { f.adopted.Add(1) }),
	)
	require.NoError(t, r.Reload(t.Context()))
	return r
}

func (f *lifecycleReplayFixture) count(t *testing.T, model any, query string, args ...any) int64 {
	t.Helper()
	var count int64
	require.NoError(t, f.db.Model(model).Where(query, args...).Count(&count).Error)
	return count
}

func TestLifecycleBridgeReplayAcrossRoutersAdmitsAndLaunchesOnce(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	f.router.routeLifecycleEvent(t.Context(), f.event)
	select {
	case <-f.launched:
	case <-time.After(time.Second):
		t.Fatal("first committed delivery did not launch")
	}
	// A second process/router has no shared in-memory seen set. Its duplicate
	// must be fenced by the same database transaction as the first admission.
	f.newRouter(t).routeLifecycleEvent(t.Context(), f.event)
	require.EqualValues(t, 1, f.count(t, &models.IngestedEvent{}, "id = ?", lifecycleIngestedEventID(f.event)))
	require.EqualValues(t, 1, f.count(t, &models.EventTriggerMatch{}, "event_id = ?", lifecycleIngestedEventID(f.event)))
	require.EqualValues(t, 1, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.Zero(t, f.count(t, &models.RunQueue{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 1, f.adopted.Load(), "replay must not adopt the first run again")
	require.Never(t, func() bool { return len(f.launched) != 0 }, 100*time.Millisecond, 10*time.Millisecond,
		"replay must not launch execution again")
}

func TestLifecycleReplayConcurrentRoutesShareAdmissionFence(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	routers := []*Router{f.router, f.newRouter(t)}
	start := make(chan struct{})
	errs := make(chan error, len(routers))
	var workers sync.WaitGroup
	for _, router := range routers {
		workers.Go(func() {
			ingested, err := router.lifecycleIngestedEvent(t.Context(), f.event)
			if err == nil {
				<-start
				_, err = router.Route(t.Context(), ingested, withLifecycleReplayDeduplication(f.event.Sequence))
			}
			errs <- err
		})
	}
	close(start)
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.EqualValues(t, 1, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 1, f.count(t, &models.EventTriggerMatch{}, "trigger_id = ?", f.listener.TriggerID))
	require.EqualValues(t, 1, f.adopted.Load())
}

func TestLifecycleReplayRollbackDoesNotConsumeAdmission(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	failure := errors.New("match persistence failed")
	const callback = "test:reject-lifecycle-match"
	require.NoError(t, f.db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "event_trigger_matches" {
			tx.AddError(failure)
		}
	}))
	t.Cleanup(func() { _ = f.db.Callback().Create().Remove(callback) })
	ingested, err := f.router.lifecycleIngestedEvent(t.Context(), f.event)
	require.NoError(t, err)
	_, err = f.router.Route(t.Context(), ingested, withLifecycleReplayDeduplication(f.event.Sequence))
	require.ErrorIs(t, err, failure)
	require.Zero(t, f.count(t, &models.IngestedEvent{}, "id = ?", ingested.ID))
	require.Zero(t, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.Zero(t, f.adopted.Load())
	require.Empty(t, f.launched, "an uncommitted routing outcome must never launch")
	require.NoError(t, f.db.Callback().Create().Remove(callback))
	f.router.routeLifecycleEvent(t.Context(), f.event)
	require.EqualValues(t, 1, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 1, f.adopted.Load())
}

func TestLifecycleReplayDoesNotEnqueueTwice(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	require.NoError(t, f.db.Model(f.listener).Update("concurrency", datatypes.JSON(`{"maxRuns":1,"strategy":"queue"}`)).Error)
	now := time.Now().UTC()
	holder := &models.JobRun{ID: uuid.New(), JobID: f.listener.ID, Namespace: "marketing", Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, f.db.Create(holder).Error)
	f.router.routeLifecycleEvent(t.Context(), f.event)
	f.newRouter(t).routeLifecycleEvent(t.Context(), f.event)
	require.EqualValues(t, 1, f.count(t, &models.RunQueue{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 1, f.count(t, &models.EventTriggerMatch{}, "event_id = ?", lifecycleIngestedEventID(f.event)))
	require.Zero(t, f.adopted.Load())
	require.Empty(t, f.launched)
}

func TestLifecycleReplayIdentitySeparatesDurableEvents(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	first := lifecycleIngestedEventID(f.event)
	require.NotEqual(t, uuid.Nil, first)
	changedPayload := f.event
	changedPayload.Payload = []byte(`{"job_alias":"renamed-source"}`)
	require.Equal(t, first, lifecycleIngestedEventID(changedPayload), "mutable enrichment must not change replay identity")
	for name, mutate := range map[string]func(*eventstore.Event){
		"sequence": func(e *eventstore.Event) { e.Sequence++ },
		"run":      func(e *eventstore.Event) { e.RunID = uuid.New() },
		"job":      func(e *eventstore.Event) { e.JobID = uuid.New() },
		"type":     func(e *eventstore.Event) { e.Type = eventstore.TypeRunTerminal },
	} {
		t.Run(name, func(t *testing.T) {
			evt := f.event
			mutate(&evt)
			require.NotEqual(t, first, lifecycleIngestedEventID(evt))
		})
	}
	// Identical payload with a different durable sequence or terminal type is
	// a new event, even when it names the same job and run.
	f.router.routeLifecycleEvent(t.Context(), f.event)
	differentSequence := f.event
	differentSequence.Sequence++
	f.router.routeLifecycleEvent(t.Context(), differentSequence)
	differentType := f.event
	differentType.Type = eventstore.TypeRunTerminal
	f.router.routeLifecycleEvent(t.Context(), differentType)
	require.EqualValues(t, 3, f.count(t, &models.IngestedEvent{}, "source = ?", "caesium"))
	require.EqualValues(t, 3, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 3, f.adopted.Load())
}

func TestLifecycleReplayPreservesPersistedNamespaceScope(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	installPolicyFile(t)
	now := time.Now().UTC()
	finance := &models.Job{ID: uuid.New(), Alias: "replay-finance-listener", Namespace: "finance",
		TriggerID: f.listener.TriggerID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, f.db.Create(finance).Error)
	f.router.routeLifecycleEvent(t.Context(), f.event)
	f.newRouter(t).routeLifecycleEvent(t.Context(), f.event)
	require.EqualValues(t, 1, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.Zero(t, f.count(t, &models.JobRun{}, "job_id = ?", finance.ID))
	require.Zero(t, f.count(t, &models.RunQueue{}, "job_id = ?", finance.ID))
	require.EqualValues(t, 1, f.count(t, &models.EventTriggerMatch{}, "event_id = ?", lifecycleIngestedEventID(f.event)))
}

func TestLifecycleLegacyAndExternalCopiesRemainIndependent(t *testing.T) {
	f := newLifecycleReplayFixture(t)
	legacy := f.event
	legacy.Sequence = 0
	require.Equal(t, uuid.Nil, lifecycleIngestedEventID(legacy))
	f.router.routeLifecycleEvent(t.Context(), legacy)
	f.router.routeLifecycleEvent(t.Context(), legacy)
	for range 2 {
		// External source/data do not opt a request into internal deduplication.
		_, err := f.router.Route(t.Context(), &models.IngestedEvent{
			Type: "run_completed", Source: "caesium", Data: datatypes.JSON(`{"job_alias":"replay-source","sequence":389}`),
		})
		require.NoError(t, err)
	}
	require.EqualValues(t, 4, f.count(t, &models.IngestedEvent{}, "source = ?", "caesium"))
	require.EqualValues(t, 4, f.count(t, &models.JobRun{}, "job_id = ?", f.listener.ID))
	require.EqualValues(t, 4, f.adopted.Load())
}
