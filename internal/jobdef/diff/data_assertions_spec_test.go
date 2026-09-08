package diff

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/freshness"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/ptr"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// data_assertions_spec_test.go pins that the data circuit breaker's ENFORCED
// policy is diffable. `onViolation: warn → hold` is the difference between a
// logged note and a broken circuit that skips every downstream consumer, and
// `metadata.onUpstreamHold: skip → run` is the difference between a consumer
// that stops on poison and one that eats it. A change that rendered as "no
// changes" would let an approver wave it through — the failure the JobSpec
// comment on Remediation forbids, and the same diff an ApprovalRequest shows.

func assertionsDefinition(onViolation, release, onUpstreamHold string) schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata: schema.Metadata{
			Alias:          "orders-daily",
			OnUpstreamHold: onUpstreamHold,
		},
		Trigger: schema.Trigger{
			Type:          schema.TriggerCron,
			Configuration: map[string]any{"cron": "0 * * * *"},
		},
		Steps: []schema.Step{{
			Name:    "load",
			Engine:  schema.EngineDocker,
			Image:   "alpine:3.23",
			Command: []string{"sh", "-c", "echo load"},
			Datasets: &schema.StepDatasets{
				Produces: []schema.ProducedDataset{{
					Name: "warehouse/orders",
					Assertions: &schema.DatasetAssertions{
						RowCount: &schema.AssertionSpec{Min: ptr.Of(1000.0)},
					},
					OnViolation: onViolation,
					Release:     release,
				}},
			},
		}},
	}
}

// insertAssertionsDefinition seeds the job the way the importer does, including
// the persisted onUpstreamHold column and the declared registry rows that
// freshness.BuildDeclarations produces — so the DB side of the diff is built
// from the real writer's output, not a hand-shaped row.
func insertAssertionsDefinition(t *testing.T, db *gorm.DB, def schema.Definition) {
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
		ID:             jobID,
		Alias:          def.Metadata.Alias,
		TriggerID:      triggerID,
		OnUpstreamHold: def.Metadata.OnUpstreamHold,
		CreatedAt:      now,
		UpdatedAt:      now,
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
			ID:          uuid.New(),
			JobID:       jobID,
			AtomID:      atomID,
			Name:        step.Name,
			Position:    idx,
			Type:        schema.StepTypeTask,
			TriggerRule: schema.TriggerRuleAllSuccess,
			CreatedAt:   now.Add(time.Duration(idx) * time.Millisecond),
			UpdatedAt:   now.Add(time.Duration(idx) * time.Millisecond),
		}).Error)
	}

	decls, err := freshness.BuildDeclarations(&def, jobID, def.Metadata.Alias)
	require.NoError(t, err)
	if len(decls) > 0 {
		require.NoError(t, db.Create(&decls).Error)
	}
}

func TestDiffMatchesPersistedAssertionPolicy(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	def := assertionsDefinition(schema.DatasetOnViolationWarn, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	insertAssertionsDefinition(t, db, def)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	desired := map[string]JobSpec{def.Metadata.Alias: FromDefinition(&def)}
	require.True(t, Compare(desired, actual).Empty(),
		"an unchanged assertion policy must not report a diff")
}

func TestDiffReportsOnViolationFlip(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	persisted := assertionsDefinition(schema.DatasetOnViolationWarn, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	insertAssertionsDefinition(t, db, persisted)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	// warn → hold: the same YAML in every other respect.
	updated := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	desired := map[string]JobSpec{updated.Metadata.Alias: FromDefinition(&updated)}

	diff := Compare(desired, actual)
	require.False(t, diff.Empty(), "onViolation: warn → hold must not render as no changes")
	require.Len(t, diff.Updates, 1)
	require.Equal(t, "orders-daily", diff.Updates[0].Alias)
	require.Contains(t, diff.Updates[0].Diff, schema.DatasetOnViolationHold,
		"the rendered diff must name the new disposition")
}

func TestDiffReportsReleaseFlip(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	persisted := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	insertAssertionsDefinition(t, db, persisted)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	updated := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseManual, schema.OnUpstreamHoldSkip)
	desired := map[string]JobSpec{updated.Metadata.Alias: FromDefinition(&updated)}

	require.False(t, Compare(desired, actual).Empty(), "release: auto → manual must show in the diff")
}

func TestDiffReportsAssertionBoundChange(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	persisted := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	insertAssertionsDefinition(t, db, persisted)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	updated := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	updated.Steps[0].Datasets.Produces[0].Assertions.RowCount.Min = ptr.Of(50.0)
	desired := map[string]JobSpec{updated.Metadata.Alias: FromDefinition(&updated)}

	require.False(t, Compare(desired, actual).Empty(), "a loosened rowCount floor must show in the diff")
}

func TestDiffReportsOnUpstreamHoldFlip(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	persisted := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldSkip)
	insertAssertionsDefinition(t, db, persisted)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)

	updated := assertionsDefinition(schema.DatasetOnViolationHold, schema.DatasetReleaseAuto, schema.OnUpstreamHoldRun)
	desired := map[string]JobSpec{updated.Metadata.Alias: FromDefinition(&updated)}

	diff := Compare(desired, actual)
	require.False(t, diff.Empty(), "metadata.onUpstreamHold: skip → run must not render as no changes")
	require.Len(t, diff.Updates, 1)
	require.Contains(t, diff.Updates[0].Diff, "OnUpstreamHold",
		"the rendered diff must name the changed field")
}

// TestDiffIgnoresPolicyFreeProducedDatasets guards against noise: a job that
// declares datasets with no assertion policy must diff exactly as it did before
// this surface existed.
func TestDiffIgnoresPolicyFreeProducedDatasets(t *testing.T) {
	db := openDiffTestDB(t)
	defer closeDiffTestDB(db)

	def := assertionsDefinition("", "", "")
	def.Steps[0].Datasets.Produces[0].Assertions = nil
	def.Steps[0].Datasets.Produces[0].Freshness = "6h"
	insertAssertionsDefinition(t, db, def)

	actual, err := LoadDatabaseSpecs(context.Background(), db)
	require.NoError(t, err)
	require.Empty(t, actual[def.Metadata.Alias].Steps[0].Produces)

	desired := map[string]JobSpec{def.Metadata.Alias: FromDefinition(&def)}
	require.True(t, Compare(desired, actual).Empty())
}
