package jobdef

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/eventmatch"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestValidateTriggerChainsRejectsBatchCycle(t *testing.T) {
	t.Parallel()

	defs := []schema.Definition{
		triggerChainDefinition("chain-a", "chain-b"),
		triggerChainDefinition("chain-b", "chain-a"),
	}

	err := ValidateTriggerChains(context.Background(), nil, defs)
	require.ErrorIs(t, err, ErrTriggerChainCycle)
	require.Contains(t, err.Error(), "chain-a -> chain-b -> chain-a")
}

func TestValidateTriggerChainsIncludesExistingDBTriggers(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	existingTrigger := triggerCycleModel(t, "chain-a", "chain-b")
	require.NoError(t, db.Create(existingTrigger).Error)
	createTriggerCycleJob(t, db, "chain-a", existingTrigger.ID)

	err := ValidateTriggerChains(context.Background(), db, []schema.Definition{
		triggerChainDefinition("chain-b", "chain-a"),
	})
	require.ErrorIs(t, err, ErrTriggerChainCycle)
	require.Contains(t, err.Error(), "chain-a -> chain-b -> chain-a")
}

func TestValidateTriggerChainsRejectsJobIDScopedCycle(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	existingTrigger := triggerCycleModel(t, "chain-a", "chain-b")
	require.NoError(t, db.Create(existingTrigger).Error)
	upstreamID := createTriggerCycleJob(t, db, "chain-a", existingTrigger.ID)

	err := ValidateTriggerChains(context.Background(), db, []schema.Definition{
		triggerChainDefinitionWithFilter("chain-b", map[string]any{"job_id": upstreamID.String()}),
	})
	require.ErrorIs(t, err, ErrTriggerChainCycle)
	require.Contains(t, err.Error(), "chain-a -> chain-b -> chain-a")
}

func TestValidateTriggerChainsUpdateSupersedesPersistedTriggerByJobID(t *testing.T) {
	t.Parallel()

	t.Run("rejects cycle introduced by update", func(t *testing.T) {
		t.Parallel()

		db := openTriggerCycleTestDB(t)
		staleBTrigger := triggerCycleCronModel(t, "job-b")
		require.NoError(t, db.Create(staleBTrigger).Error)
		jobBID := createTriggerCycleJob(t, db, "job-b", staleBTrigger.ID)

		jobATrigger := triggerCycleModelWithConfig(t, "job-a", map[string]any{
			"events": []any{
				map[string]any{
					"type":   "run_completed",
					"source": "caesium",
					"filter": map[string]any{"job_id": jobBID.String()},
				},
			},
		})
		require.NoError(t, db.Create(jobATrigger).Error)
		createTriggerCycleJob(t, db, "job-a", jobATrigger.ID)

		err := ValidateTriggerChains(context.Background(), db, []schema.Definition{
			triggerChainDefinition("job-b", "job-a"),
		})
		require.ErrorIs(t, err, ErrTriggerChainCycle)
		require.Contains(t, err.Error(), "job-a -> job-b -> job-a")
	})

	t.Run("allows non-cyclic update without stale persisted edge", func(t *testing.T) {
		t.Parallel()

		db := openTriggerCycleTestDB(t)
		staleBTrigger := triggerCycleModel(t, "job-b", "job-a")
		require.NoError(t, db.Create(staleBTrigger).Error)
		jobBID := createTriggerCycleJob(t, db, "job-b", staleBTrigger.ID)

		jobATrigger := triggerCycleModelWithConfig(t, "job-a", map[string]any{
			"events": []any{
				map[string]any{
					"type":   "run_completed",
					"source": "caesium",
					"filter": map[string]any{"job_id": jobBID.String()},
				},
			},
		})
		require.NoError(t, db.Create(jobATrigger).Error)
		createTriggerCycleJob(t, db, "job-a", jobATrigger.ID)

		err := ValidateTriggerChains(context.Background(), db, []schema.Definition{
			triggerChainCronDefinition("job-b"),
		})
		require.NoError(t, err)
	})
}

func TestValidateTriggerChainsAllowsJobIDScopedNonCycle(t *testing.T) {
	t.Parallel()

	db := openTriggerCycleTestDB(t)
	upstreamTrigger := triggerCycleModelWithConfig(t, "chain-a", map[string]any{
		"events": []any{
			map[string]any{
				"type":   "webhook.*",
				"source": "github",
			},
		},
	})
	require.NoError(t, db.Create(upstreamTrigger).Error)
	upstreamID := createTriggerCycleJob(t, db, "chain-a", upstreamTrigger.ID)

	err := ValidateTriggerChains(context.Background(), db, []schema.Definition{
		triggerChainDefinitionWithFilter("chain-b", map[string]any{"job_id": upstreamID.String()}),
	})
	require.NoError(t, err)
}

func TestUnresolvedJobIDPatternRetainsFilterButNotAlias(t *testing.T) {
	missingJobID := uuid.NewString()
	patterns, err := eventmatch.ParseTriggerEventPatterns(map[string]any{
		"events": []any{map[string]any{
			"type":   "run_completed",
			"filter": map[string]any{"job_id": missingJobID},
		}},
	})
	require.NoError(t, err)
	require.Len(t, patterns, 1)
	require.Equal(t, missingJobID, patterns[0].Filter["job_id"])

	alias, scoped := triggerChainPatternSourceAlias(patterns[0], map[string]string{})
	require.True(t, scoped)
	require.Empty(t, alias)
}

func TestExistingJobIDsByAliasWrapperPreservesNilContextAndEmptyInput(t *testing.T) {
	var nilContext context.Context
	got, err := existingJobIDsByAlias(nilContext, nil, map[string]struct{}{"job": {}})
	require.NoError(t, err)
	require.Nil(t, got, "nil DB remains a no-op")

	db := openTriggerCycleTestDB(t)
	got, err = existingJobIDsByAlias(nilContext, db, nil)
	require.NoError(t, err)
	require.Nil(t, got, "empty aliases remain a no-op")

	trigger := triggerCycleCronModel(t, "job")
	require.NoError(t, db.Create(trigger).Error)
	jobID := createTriggerCycleJob(t, db, "job", trigger.ID)
	got, err = existingJobIDsByAlias(nilContext, db, map[string]struct{}{"job": {}})
	require.NoError(t, err, "nil context is normalized before the delegated query")
	require.Equal(t, map[string]uuid.UUID{"job": jobID}, got)
}

func TestValidateTriggerChainsRejectsUnfilteredLifecycleSelfCycle(t *testing.T) {
	t.Parallel()

	def := triggerChainDefinition("chain-self", "")
	def.Trigger.Configuration = map[string]any{
		"events": []any{
			map[string]any{
				"type":   "run_*",
				"source": "caesium",
			},
		},
	}

	err := ValidateTriggerChains(context.Background(), nil, []schema.Definition{def})
	require.ErrorIs(t, err, ErrTriggerChainCycle)
	require.Contains(t, err.Error(), "chain-self -> chain-self")
}

func triggerChainDefinition(alias, upstream string) schema.Definition {
	filter := map[string]any(nil)
	if upstream != "" {
		filter = map[string]any{"job_alias": upstream}
	}
	return triggerChainDefinitionWithFilter(alias, filter)
}

func triggerChainCronDefinition(alias string) schema.Definition {
	def := triggerChainDefinitionWithFilter(alias, nil)
	def.Trigger = schema.Trigger{
		Type:          schema.TriggerCron,
		Configuration: map[string]any{},
	}
	return def
}

func triggerChainDefinitionWithFilter(alias string, filter map[string]any) schema.Definition {
	pattern := map[string]any{
		"type":   "run_completed",
		"source": "caesium",
	}
	if len(filter) > 0 {
		pattern["filter"] = filter
	}
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias},
		Trigger: schema.Trigger{
			Type:          schema.TriggerEvent,
			Configuration: map[string]any{"events": []any{pattern}},
		},
		Steps: []schema.Step{{
			Name:    "run",
			Image:   "alpine:3.23",
			Command: []string{"sh", "-c", "echo ok"},
		}},
	}
}

func triggerCycleModel(t *testing.T, alias, upstream string) *models.Trigger {
	return triggerCycleModelWithConfig(t, alias, map[string]any{
		"events": []any{
			map[string]any{
				"type":   "run_completed",
				"source": "caesium",
				"filter": map[string]any{"job_alias": upstream},
			},
		},
	})
}

func triggerCycleModelWithConfig(t *testing.T, alias string, configuration map[string]any) *models.Trigger {
	t.Helper()
	cfg, err := json.Marshal(configuration)
	require.NoError(t, err)
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         alias,
		Type:          models.TriggerTypeEvent,
		Configuration: string(cfg),
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	require.NoError(t, trigger.ApplyDerivedFields())
	return trigger
}

func triggerCycleCronModel(t *testing.T, alias string) *models.Trigger {
	t.Helper()
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Alias:         alias,
		Type:          models.TriggerTypeCron,
		Configuration: `{}`,
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	require.NoError(t, trigger.ApplyDerivedFields())
	return trigger
}

func createTriggerCycleJob(t *testing.T, db *gorm.DB, alias string, triggerID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.Job{
		ID:        id,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}).Error)
	return id
}

func openTriggerCycleTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(models.All...))
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}
