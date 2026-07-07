package event

import (
	"context"
	"errors"
	"testing"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	eventstore "github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestRouterRoutePersistsEventAndMatches(t *testing.T) {
	t.Parallel()

	db := openEventRouterTestDB(t)
	triggerStart := eventRouterTestTrigger(t, "start")
	triggerPaused := eventRouterTestTrigger(t, "paused")
	triggerEmpty := eventRouterTestTrigger(t, "empty")
	require.NoError(t, db.Create([]*models.Trigger{triggerStart, triggerPaused, triggerEmpty}).Error)

	activeJob := &models.Job{
		ID:        uuid.New(),
		Alias:     "active-job",
		TriggerID: triggerStart.ID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	pausedJob := &models.Job{
		ID:        uuid.New(),
		Alias:     "paused-job",
		TriggerID: triggerPaused.ID,
		Paused:    true,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create([]*models.Job{activeJob, pausedJob}).Error)

	router := NewRouter(db,
		WithTriggerLister(func(context.Context) (models.Triggers, error) {
			return models.Triggers{triggerStart, triggerPaused, triggerEmpty}, nil
		}),
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error {
			return nil
		})),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, router.Reload(context.Background()))

	result, err := router.Route(context.Background(), &models.IngestedEvent{
		Type:   "webhook.github",
		Source: "github",
		Data:   datatypes.JSON(`{"ref":"refs/heads/main"}`),
	})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, result.EventID)
	require.Equal(t, "webhook.github", result.EventType)
	require.Equal(t, "github", result.Source)
	require.Len(t, result.MatchedTriggers, 3)

	byTrigger := make(map[uuid.UUID]TriggerRouteResult)
	for _, match := range result.MatchedTriggers {
		byTrigger[match.TriggerID] = match
	}

	startResult := byTrigger[triggerStart.ID]
	require.Len(t, startResult.RunsStarted, 1)
	require.False(t, startResult.Skipped)
	require.Empty(t, startResult.SkipReason)
	require.Empty(t, startResult.Error)

	pausedResult := byTrigger[triggerPaused.ID]
	require.Empty(t, pausedResult.RunsStarted)
	require.True(t, pausedResult.Skipped)
	require.Contains(t, pausedResult.SkipReason, "job paused")
	require.Empty(t, pausedResult.Error)

	emptyResult := byTrigger[triggerEmpty.ID]
	require.Empty(t, emptyResult.RunsStarted)
	require.True(t, emptyResult.Skipped)
	require.Equal(t, "no jobs registered for trigger", emptyResult.SkipReason)
	require.Empty(t, emptyResult.Error)

	var storedEvent models.IngestedEvent
	require.NoError(t, db.First(&storedEvent, "id = ?", result.EventID).Error)
	require.Equal(t, "webhook.github", storedEvent.Type)

	var matchRows []models.EventTriggerMatch
	require.NoError(t, db.Order("trigger_id").Find(&matchRows, "event_id = ?", result.EventID).Error)
	require.Len(t, matchRows, 3)

	var run models.JobRun
	require.NoError(t, db.First(&run, "id = ?", startResult.RunsStarted[0]).Error)
	require.Equal(t, activeJob.ID, run.JobID)
	require.Equal(t, triggerStart.ID, run.TriggerID)
	require.JSONEq(t, `{"branch":"refs/heads/main"}`, string(run.Params))
}

func TestRouterRouteAfterSharedTriggerJobDeleteFiresSibling(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := openEventRouterTestDB(t)
	trigger := eventRouterTestTrigger(t, "shared")
	require.NoError(t, db.Create(trigger).Error)

	jobSvc := jsvc.ServiceWithDatabase(ctx, db)
	deletedJob, err := jobSvc.Create(&jsvc.CreateRequest{TriggerID: trigger.ID, Alias: "shared-deleted"})
	require.NoError(t, err)
	siblingJob, err := jobSvc.Create(&jsvc.CreateRequest{TriggerID: trigger.ID, Alias: "shared-sibling"})
	require.NoError(t, err)

	router := NewRouter(db,
		WithEventTriggerOptions(WithRunJob(func(context.Context, *models.Job, map[string]string) error {
			return nil
		})),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, router.Reload(ctx))

	require.NoError(t, jobSvc.Delete(deletedJob.ID))

	var storedTrigger models.Trigger
	require.NoError(t, db.First(&storedTrigger, "id = ?", trigger.ID).Error)

	result, err := router.Route(ctx, &models.IngestedEvent{
		Type:   "webhook.github",
		Source: "github",
		Data:   datatypes.JSON(`{"ref":"refs/heads/main"}`),
	})
	require.NoError(t, err)
	require.Len(t, result.MatchedTriggers, 1)

	routeResult := result.MatchedTriggers[0]
	require.Equal(t, trigger.ID, routeResult.TriggerID)
	require.Len(t, routeResult.RunsStarted, 1)
	require.False(t, routeResult.Skipped)
	require.Empty(t, routeResult.Error)

	var run models.JobRun
	require.NoError(t, db.First(&run, "id = ?", routeResult.RunsStarted[0]).Error)
	require.Equal(t, siblingJob.ID, run.JobID)
	require.Equal(t, trigger.ID, run.TriggerID)

	require.NoError(t, jobSvc.Delete(siblingJob.ID))
	require.ErrorIs(t, db.First(&storedTrigger, "id = ?", trigger.ID).Error, gorm.ErrRecordNotFound)
	require.NoError(t, db.Unscoped().First(&storedTrigger, "id = ?", trigger.ID).Error)
	require.True(t, storedTrigger.DeletedAt.Valid)
}

func TestLifecycleBridgeRoutesRunCompletedWithJobAliasAndDepth(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db := openEventRouterTestDB(t)
	upstreamTrigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "upstream-cron",
		Type:          models.TriggerTypeCron,
		Configuration: `{"expression":"* * * * *"}`,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	downstreamTrigger := eventRouterTestTriggerConfig(t, "downstream-event", `{
		"events":[{"type":"run_completed","source":"caesium","filter":{"job_alias":"upstream-job"}}]
	}`)
	require.NoError(t, db.Create([]*models.Trigger{upstreamTrigger, downstreamTrigger}).Error)

	upstreamJob := &models.Job{
		ID:        uuid.New(),
		Alias:     "upstream-job",
		TriggerID: upstreamTrigger.ID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	downstreamJob := &models.Job{
		ID:        uuid.New(),
		Alias:     "downstream-job",
		TriggerID: downstreamTrigger.ID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, db.Create([]*models.Job{upstreamJob, downstreamJob}).Error)

	router := NewRouter(db,
		WithEventTriggerOptions(
			WithRunJob(func(context.Context, *models.Job, map[string]string) error {
				return nil
			}),
			WithMaxTriggerDepth(3),
		),
		withStartedRunAdopter(func(uuid.UUID) {}),
	)
	require.NoError(t, router.Reload(ctx))

	bus := eventstore.New()
	// Subscribe SYNCHRONOUSLY so the publish below can't race the bridge's
	// bus.Subscribe landing (an async StartLifecycleBridge occasionally loses
	// the single publish under -race). Then run the bridge loop in the
	// background and publish exactly once; Eventually only polls the DB.
	events, err := router.SubscribeLifecycleBridge(ctx, bus)
	require.NoError(t, err)
	errCh := make(chan error, 1)
	go func() {
		errCh <- router.RunLifecycleBridge(ctx, events)
	}()
	t.Cleanup(func() {
		cancel()
		// Bounded wait: a regression that stops the bridge honoring
		// cancellation should fail here in seconds with a clear message,
		// not hold the package open to the 600s go-test timeout. 5s gives
		// a loaded -race arm64 runner room to finish the in-flight route.
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("lifecycle bridge did not exit after context cancellation")
		}
	})

	runID := uuid.New()
	bus.Publish(eventstore.Event{
		Type:    eventstore.TypeRunCompleted,
		JobID:   upstreamJob.ID,
		RunID:   runID,
		Payload: []byte(`{"params":{"_trigger_depth":"1"}}`),
	})

	require.Eventually(t, func() bool {
		var count int64
		if err := db.Model(&models.JobRun{}).Where("job_id = ?", downstreamJob.ID).Count(&count).Error; err != nil {
			return false
		}
		return count > 0
	}, time.Second, 10*time.Millisecond)

	var run models.JobRun
	require.NoError(t, db.First(&run, "job_id = ?", downstreamJob.ID).Error)
	require.JSONEq(t, `{"_trigger_depth":"2"}`, string(run.Params))

	var ingested models.IngestedEvent
	require.NoError(t, db.First(&ingested, "type = ? AND source = ?", string(eventstore.TypeRunCompleted), "caesium").Error)
	require.JSONEq(t, `{
		"event_type":"run_completed",
		"job_alias":"upstream-job",
		"job_id":"`+upstreamJob.ID.String()+`",
		"run_id":"`+runID.String()+`",
		"_trigger_depth":"1",
		"params":{"_trigger_depth":"1"}
	}`, string(ingested.Data))
}

func TestEventTriggerFireRejectsDepthExceeded(t *testing.T) {
	t.Parallel()

	triggerModel := eventRouterTestTrigger(t, "depth")
	trigger, err := New(triggerModel,
		WithMaxTriggerDepth(1),
		WithListJobs(func(context.Context, string) (models.Jobs, error) {
			t.Fatal("list jobs should not be called after depth rejection")
			return nil, nil
		}),
	)
	require.NoError(t, err)

	_, err = trigger.FireWithParams(context.Background(), map[string]string{TriggerDepthParam: "1"})
	require.ErrorIs(t, err, ErrTriggerChainDepthExceeded)
	require.True(t, errors.Is(err, ErrTriggerChainDepthExceeded))
}

func openEventRouterTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	// Private in-memory DB (no cache=shared): shared-cache uses a process-wide
	// lock, so one test's concurrent bridge writes contend with — and can stall —
	// sibling t.Parallel() tests. A private in-memory DB is isolated per test.
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// Pin to a single connection so the private in-memory DB survives across
	// calls (a mode=memory DB lives only as long as its connection) and to
	// serialize the lifecycle-bridge test's concurrent Route writes with the
	// test's reads without contention.
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	sqlDB.SetConnMaxLifetime(0)
	sqlDB.SetConnMaxIdleTime(0)
	require.NoError(t, db.AutoMigrate(models.All...))
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func eventRouterTestTrigger(t *testing.T, alias string) *models.Trigger {
	return eventRouterTestTriggerConfig(t, alias, `{
		"events":[{"type":"webhook.*","source":"github"}],
		"paramMapping":{"branch":"$.ref"}
	}`)
}

func eventRouterTestTriggerConfig(t *testing.T, alias string, configuration string) *models.Trigger {
	t.Helper()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         alias,
		Type:          models.TriggerTypeEvent,
		Configuration: configuration,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	require.NoError(t, trigger.ApplyDerivedFields())
	return trigger
}
