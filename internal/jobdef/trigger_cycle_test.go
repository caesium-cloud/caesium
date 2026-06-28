package jobdef

import (
	"context"
	"encoding/json"
	"testing"
	"time"

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
