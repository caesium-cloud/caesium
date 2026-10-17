package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestConfinedRunReadsWithholdLegacyBaselineEvidence: a run read under a
// namespace restriction (the history scope the run handlers set) carries no
// baseline evidence of a verdict recorded before baselines were
// namespace-owned, through Get, List and Latest alike; an unrestricted read
// is unchanged (identity-and-access B4).
func TestConfinedRunReadsWithholdLegacyBaselineEvidence(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeCron}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "legacy-baseline", Namespace: "finance", TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1"}
	require.NoError(t, db.Create(atom).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "load"}
	require.NoError(t, db.Create(task).Error)
	runID := uuid.New()
	require.NoError(t, db.Create(&models.JobRun{ID: runID, JobID: job.ID, TriggerID: trigger.ID, Namespace: "finance",
		Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	message := "observed 10 deviates 9990 from the baseline median 31337 (5 sample(s)), more than the declared 50%"
	violations, err := json.Marshal([]runstorage.DataViolation{{Dataset: "d", Metric: "rowCount",
		Assertion: runstorage.AssertionDeltaFromBaseline, DeltaFromBaseline: "50%", BaselineMedian: ptr(31337.0), BaselineSamples: 5,
		Bound: ptr(15668.5), Message: message}})
	require.NoError(t, err)
	require.NoError(t, db.Create(&models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: task.ID, AtomID: atom.ID,
		Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "[]", Status: "failed",
		Error: "violates its declared data assertions: " + message, DataViolations: datatypes.JSON(violations)}).Error)

	// The run list pages only task counters; Get and Latest carry the verdicts.
	read := func(ctx context.Context) map[string]string {
		svc := New(ctx).WithDatabase(db)
		got, err := svc.Get(runID)
		require.NoError(t, err)
		listed, _, _, err := svc.List(job.ID, 10, 0)
		require.NoError(t, err)
		require.Len(t, listed, 1)
		latest, err := svc.Latest(job.ID)
		require.NoError(t, err)
		out := make(map[string]string, 3)
		for name, run := range map[string]*runstorage.JobRun{"get": got, "list": listed[0], "latest": latest} {
			body, err := json.Marshal(run)
			require.NoError(t, err)
			out[name] = string(body)
		}
		return out
	}
	confined := read(authz.WithAllowedNamespaces(context.Background(), []string{"finance"}))
	for name, body := range confined {
		require.NotContains(t, body, "31337", "%s: a confined reader never receives a legacy baseline median", name)
		require.NotContains(t, body, "15668", name)
	}
	unrestricted := read(context.Background())
	for _, name := range []string{"get", "latest"} {
		require.Contains(t, confined[name], runstorage.BaselineEvidenceWithheld, name)
		require.Contains(t, unrestricted[name], "31337", "%s: an unrestricted reader is unchanged", name)
	}
}

func ptr(v float64) *float64 { return &v }
