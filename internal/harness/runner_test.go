package harness

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// seedImpactGraph builds a minimal persisted lineage graph in an in-memory DB:
// a producer task_run with an `output` dataset and a consumer task_run that
// declares the same dataset as `input` and emits its own `output` — exactly the
// shape QueryImpact traverses.
func seedImpactGraph(t *testing.T, ns string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(models.All...))

	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Type: models.TriggerTypeCron}).Error)
	job := &models.Job{ID: uuid.New(), Alias: "harness-job", TriggerID: triggerID}
	require.NoError(t, db.Create(job).Error)
	jobRun := &models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: triggerID, Status: "succeeded", StartedAt: time.Now().UTC()}
	require.NoError(t, db.Create(jobRun).Error)

	mkTaskRun := func() *models.TaskRun {
		atomID := uuid.New()
		require.NoError(t, db.Create(&models.Atom{ID: atomID, Engine: models.AtomEngineDocker, Image: "alpine:3.23"}).Error)
		tr := &models.TaskRun{
			ID: uuid.New(), JobRunID: jobRun.ID, TaskID: uuid.New(), AtomID: atomID,
			Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "[]", Status: "succeeded", Attempt: 1,
		}
		require.NoError(t, db.Create(tr).Error)
		return tr
	}
	mkDataset := func(tr *models.TaskRun, name, direction string) {
		require.NoError(t, db.Create(&models.LineageDataset{
			ID: uuid.New(), TaskRunID: tr.ID, Namespace: ns, Name: name,
			Direction: direction, FacetSummary: datatypes.JSON([]byte("{}")), CreatedAt: time.Now().UTC(),
		}).Error)
	}

	producer := mkTaskRun()
	mkDataset(producer, "harness-job.extract.output", "output")
	consumer := mkTaskRun()
	mkDataset(consumer, "harness-job.extract.output", "input")
	mkDataset(consumer, "harness-job.transform.output", "output")
	return db
}

func TestEvaluateImpactPassesWhenDownstreamPresent(t *testing.T) {
	db := seedImpactGraph(t, harnessNamespace)
	failures := evaluateImpact(context.Background(), db, ImpactExpectation{
		Dataset:    "harness-job.extract.output",
		Downstream: []string{"harness-job.transform.output"},
	})
	require.Empty(t, failures, "expected the downstream consumer to be found")
}

func TestEvaluateImpactFailsWhenDownstreamMissing(t *testing.T) {
	db := seedImpactGraph(t, harnessNamespace)
	failures := evaluateImpact(context.Background(), db, ImpactExpectation{
		Dataset:    "harness-job.extract.output",
		Downstream: []string{"harness-job.nonexistent.output"},
	})
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], "expected downstream")
	require.Contains(t, failures[0], "harness-job.transform.output", "failure should list what WAS found")
}

func TestEvaluateImpactFailsWithoutDatabase(t *testing.T) {
	failures := evaluateImpact(context.Background(), nil, ImpactExpectation{
		Dataset:    "x",
		Downstream: []string{"y"},
	})
	require.Len(t, failures, 1)
	require.Contains(t, failures[0], "no database available")
}
