package diff

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/testutil"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonmap"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
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
		&models.TaskEdge{},
		// The declared registry carries the per-dataset assertion policy the
		// spec now diffs (data-circuit-breaker A3).
		&models.DatasetDeclaration{},
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

// insertDiffDefinition writes the rows job apply would persist for def.
// Step type, engine, and triggerRule defaults are spelled out here rather than
// calling prepareSteps, so a bug in that helper still fails the round trip.
func insertDiffDefinition(t *testing.T, db *gorm.DB, def schema.Definition) {
	t.Helper()
	now := time.Now().UTC()
	triggerID := uuid.New()
	jobID := uuid.New()

	cfg := map[string]any{}
	for key, value := range def.Trigger.Configuration {
		cfg[key] = value
	}
	if len(def.Trigger.DefaultParams) > 0 {
		params := make(map[string]string, len(def.Trigger.DefaultParams))
		for key, value := range def.Trigger.DefaultParams {
			params[key] = value
		}
		cfg["defaultParams"] = params
	}
	require.NoError(t, db.Create(&models.Trigger{
		ID:            triggerID,
		Alias:         def.Metadata.Alias,
		Type:          models.TriggerType(def.Trigger.Type),
		Configuration: mustJSONString(t, cfg),
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)

	job := models.Job{
		ID:               jobID,
		Alias:            def.Metadata.Alias,
		Namespace:        models.NamespaceOrDefault(def.Metadata.Namespace),
		TriggerID:        triggerID,
		Labels:           stringMapToJSONMap(def.Metadata.Labels),
		Annotations:      stringMapToJSONMap(def.Metadata.Annotations),
		MaxParallelTasks: def.Metadata.MaxParallelTasks,
		TaskTimeout:      def.Metadata.TaskTimeout,
		RunTimeout:       def.Metadata.RunTimeout,
		Priority:         def.Metadata.Priority,
		SchemaValidation: def.Metadata.SchemaValidation,
		OnUpstreamHold:   def.Metadata.OnUpstreamHold,
		ReplaySafe:       def.Metadata.ReplaySafe,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if def.Metadata.Concurrency != nil {
		job.Concurrency = mustOptionalJSON(t, def.Metadata.Concurrency)
	}
	if len(def.Metadata.RateLimits) > 0 {
		job.RateLimits = mustOptionalJSON(t, def.Metadata.RateLimits)
	}
	if def.Metadata.SLA != nil && def.Metadata.SLA.HasSLA() {
		job.SLA = mustOptionalJSON(t, def.Metadata.SLA)
	}
	if def.Metadata.Cache != nil {
		job.CacheConfig = mustOptionalJSON(t, def.Metadata.Cache)
	}
	if def.Metadata.Remediation != nil {
		job.Remediation = mustOptionalJSON(t, def.Metadata.Remediation)
	}
	require.NoError(t, db.Create(&job).Error)

	steps := slices.Clone(def.Steps)
	for i := range steps {
		stepType := strings.TrimSpace(steps[i].Type)
		if stepType == "" {
			stepType = schema.StepTypeTask
		}
		steps[i].Type = stepType
		if steps[i].Engine == "" {
			steps[i].Engine = schema.EngineDocker
		}
		rule := strings.TrimSpace(steps[i].TriggerRule)
		if rule == "" {
			rule = schema.TriggerRuleAllSuccess
		}
		steps[i].TriggerRule = rule
	}
	successors, err := schema.DeriveStepSuccessors(steps)
	require.NoError(t, err)

	taskIDByName := make(map[string]uuid.UUID, len(steps))
	for idx := range steps {
		step := &steps[idx]
		runtime, err := def.RuntimeSpecForStep(step)
		require.NoError(t, err)
		specJSON, err := json.Marshal(runtime)
		require.NoError(t, err)
		command, err := jsonutil.MarshalSliceString(step.Command)
		require.NoError(t, err)
		replaySafe := def.EffectiveReplaySafeForStep(step)

		atomID := uuid.New()
		require.NoError(t, db.Create(&models.Atom{
			ID:         atomID,
			Engine:     models.AtomEngine(step.Engine),
			Image:      step.Image,
			Command:    command,
			Spec:       datatypes.JSON(specJSON),
			ReplaySafe: replaySafe,
			CreatedAt:  now,
			UpdatedAt:  now,
		}).Error)

		taskID := uuid.New()
		taskIDByName[step.Name] = taskID
		task := models.Task{
			ID:           taskID,
			JobID:        jobID,
			AtomID:       atomID,
			Name:         step.Name,
			Position:     idx,
			Type:         step.Type,
			NodeSelector: jsonmap.FromStringMap(step.NodeSelector),
			Retries:      step.Retries,
			RetryDelay:   step.RetryDelay,
			RetryBackoff: step.RetryBackoff,
			TriggerRule:  step.TriggerRule,
			ReplaySafe:   replaySafe,
			CacheConfig:  mustOptionalJSON(t, step.Cache),
			OutputSchema: mustOptionalJSON(t, step.OutputSchema),
			InputSchema:  mustOptionalJSON(t, step.InputSchema),
			CreatedAt:    now.Add(time.Duration(idx) * time.Millisecond),
			UpdatedAt:    now.Add(time.Duration(idx) * time.Millisecond),
		}
		if step.RateLimit != nil {
			task.RateLimitResource = strings.TrimSpace(step.RateLimit.Resource)
			task.RateLimitUnits = step.RateLimit.Units
		}
		if step.FanOut != nil {
			task.FanOutConfig = mustOptionalJSON(t, step.FanOut)
		}
		require.NoError(t, db.Create(&task).Error)
	}

	for from, tos := range successors {
		for _, to := range tos {
			require.NoError(t, db.Create(&models.TaskEdge{
				ID:         uuid.New(),
				JobID:      jobID,
				FromTaskID: taskIDByName[from],
				ToTaskID:   taskIDByName[to],
				CreatedAt:  now,
				UpdatedAt:  now,
			}).Error)
		}
	}

	decls, err := freshness.BuildDeclarations(&def, jobID, def.Metadata.Alias)
	require.NoError(t, err)
	if len(decls) > 0 {
		require.NoError(t, db.Create(&decls).Error)
	}

	for idx, callback := range def.Callbacks {
		configuration, err := jsonutil.MarshalMapString(callback.Configuration)
		require.NoError(t, err)
		require.NoError(t, db.Create(&models.Callback{
			ID:            uuid.New(),
			JobID:         jobID,
			Type:          models.CallbackType(callback.Type),
			Configuration: configuration,
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
	return testutil.MustJSONBytes(t, value)
}
