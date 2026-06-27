package event

import (
	"context"
	"testing"
	"time"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
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

func openEventRouterTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(models.All...))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

func eventRouterTestTrigger(t *testing.T, alias string) *models.Trigger {
	t.Helper()
	trigger := &models.Trigger{
		ID:    uuid.New(),
		Alias: alias,
		Type:  models.TriggerTypeEvent,
		Configuration: `{
			"events":[{"type":"webhook.*","source":"github"}],
			"paramMapping":{"branch":"$.ref"}
		}`,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	require.NoError(t, trigger.ApplyDerivedFields())
	return trigger
}
