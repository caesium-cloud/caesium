package diff

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestCompareProducesCreatesUpdatesDeletes(t *testing.T) {
	desired := map[string]JobSpec{
		"new": {
			Alias: "new",
		},
		"shared": {
			Alias:   "shared",
			Trigger: TriggerSpec{Type: "cron", Configuration: map[string]any{"cron": "* * * * *"}},
		},
	}
	actual := map[string]JobSpec{
		"shared": {
			Alias:   "shared",
			Trigger: TriggerSpec{Type: "cron", Configuration: map[string]any{"cron": "0 * * * *"}},
		},
		"stale": {Alias: "stale"},
	}

	diff := Compare(desired, actual)

	require.Len(t, diff.Creates, 1)
	require.Equal(t, "new", diff.Creates[0].Alias)

	require.Len(t, diff.Deletes, 1)
	require.Equal(t, "stale", diff.Deletes[0].Alias)

	require.Len(t, diff.Updates, 1)
	require.Equal(t, "shared", diff.Updates[0].Alias)
	require.NotEmpty(t, diff.Updates[0].Diff)
}

func TestLoadDatabaseSpecsMatchesDefinition(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	def := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata: schema.Metadata{
			Alias:       "csv-to-parquet",
			Labels:      map[string]string{"team": "data"},
			Annotations: map[string]string{"owner": "etl"},
		},
		Trigger: schema.Trigger{
			Type: schema.TriggerCron,
			Configuration: map[string]any{
				"cron":     "0 * * * *",
				"timezone": "UTC",
			},
		},
		Callbacks: []schema.Callback{{
			Type:          schema.CallbackNotification,
			Configuration: map[string]any{"url": "https://example"},
		}},
		Steps: []schema.Step{{
			Name:    "list",
			Engine:  schema.EngineDocker,
			Image:   "busybox:1.36.1",
			Command: []string{"sh", "-c", "echo list"},
		}},
	}
	insertDiffDefinition(t, db, def)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	desired := map[string]JobSpec{def.Metadata.Alias: FromDefinition(&def)}

	diff := Compare(desired, actual)
	require.True(t, diff.Empty())
}

func TestCompareReportsOutputSchemaOnlyUpdates(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	persisted := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: "contract-producer"},
		Trigger: schema.Trigger{
			Type: schema.TriggerCron,
			Configuration: map[string]any{
				"cron":     "0 2 * * *",
				"timezone": "UTC",
			},
		},
		Steps: []schema.Step{{
			Name:    "export",
			Engine:  schema.EngineDocker,
			Image:   "alpine:3.23",
			Command: []string{"sh", "-c", "echo export"},
			OutputSchema: map[string]any{
				"type":       "object",
				"required":   []any{"row_count"},
				"properties": map[string]any{"row_count": map[string]any{"type": "integer"}},
			},
		}},
	}
	insertDiffDefinition(t, db, persisted)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	desiredDef := persisted
	desiredDef.Steps = append([]schema.Step(nil), persisted.Steps...)
	desiredDef.Steps[0].OutputSchema = map[string]any{
		"type":       "object",
		"required":   []any{},
		"properties": map[string]any{},
	}
	desired := map[string]JobSpec{desiredDef.Metadata.Alias: FromDefinition(&desiredDef)}

	diff := Compare(desired, actual)
	require.Len(t, diff.Updates, 1)
	require.Equal(t, "contract-producer", diff.Updates[0].Alias)
	require.Contains(t, diff.Updates[0].Diff, "OutputSchema")
	require.Contains(t, diff.Updates[0].Diff, "row_count")
}

func openDiffTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	require.NoError(t, db.AutoMigrate(
		&models.Trigger{},
		&models.Job{},
		&models.Atom{},
		&models.Task{},
		&models.Callback{},
	))
	return db
}

func closeDiffTestDB(db *gorm.DB) {
	if db == nil {
		return
	}
	if sqlDB, err := db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func insertDiffDefinition(t *testing.T, db *gorm.DB, def schema.Definition) {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Type:          models.TriggerType(def.Trigger.Type),
		Configuration: mustJSONString(t, def.Trigger.Configuration),
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)
	require.NoError(t, db.Create(&models.Job{
		ID:          jobID,
		Alias:       def.Metadata.Alias,
		TriggerID:   triggerID,
		Labels:      stringMapToJSONMap(def.Metadata.Labels),
		Annotations: stringMapToJSONMap(def.Metadata.Annotations),
		CreatedAt:   now,
		UpdatedAt:   now,
	}).Error)

	for idx, step := range def.Steps {
		atomID := uuid.New()
		require.NoError(t, db.Create(&models.Atom{
			ID:        atomID,
			Engine:    models.AtomEngine(step.Engine),
			Image:     step.Image,
			Command:   mustJSONString(t, step.Command),
			CreatedAt: now,
			UpdatedAt: now,
		}).Error)
		require.NoError(t, db.Create(&models.Task{
			ID:           uuid.New(),
			JobID:        jobID,
			AtomID:       atomID,
			Name:         step.Name,
			Position:     idx,
			Type:         schema.StepTypeTask,
			TriggerRule:  schema.TriggerRuleAllSuccess,
			OutputSchema: mustOptionalJSON(t, step.OutputSchema),
			CreatedAt:    now.Add(time.Duration(idx) * time.Millisecond),
			UpdatedAt:    now.Add(time.Duration(idx) * time.Millisecond),
		}).Error)
	}

	for idx, callback := range def.Callbacks {
		require.NoError(t, db.Create(&models.Callback{
			ID:            uuid.New(),
			JobID:         jobID,
			Type:          models.CallbackType(callback.Type),
			Configuration: mustJSONString(t, callback.Configuration),
			Position:      idx,
			CreatedAt:     now.Add(time.Duration(idx) * time.Millisecond),
			UpdatedAt:     now.Add(time.Duration(idx) * time.Millisecond),
		}).Error)
	}
}

func stringMapToJSONMap(in map[string]string) datatypes.JSONMap {
	out := make(datatypes.JSONMap, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func mustOptionalJSON(t *testing.T, value any) datatypes.JSON {
	t.Helper()
	if value == nil {
		return nil
	}
	return datatypes.JSON(mustJSONBytes(t, value))
}

func mustJSONString(t *testing.T, value any) string {
	t.Helper()
	return string(mustJSONBytes(t, value))
}

func mustJSONBytes(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
