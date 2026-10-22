package agent

import (
	"context"
	"testing"
	"time"

	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func seedRunIn(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	return id
}

func runIDs(runs []RunSummary) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// TestHistoryConfinedToIncidentNamespace (identity-and-access A8): history
// lists only runs recorded in the incident's namespace, for the incident's own
// job and an allow-listed one alike, and an allow-listed job that moved out of
// the namespace is refused.
func TestHistoryConfinedToIncidentNamespace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	own := seedJob(t, db, "own-job")
	other := seedJob(t, db, "other-job")
	for _, id := range []uuid.UUID{own, other} {
		require.NoError(t, db.Model(&models.Job{}).Where("id = ?", id).Update("namespace", "marketing").Error)
	}
	inc := seedIncident(t, db, own)
	require.NoError(t, db.Model(inc).Update("namespace", "marketing").Error)
	inc.Namespace = "marketing"

	ownMarketing := seedRunIn(t, db, own, "marketing")
	seedRunIn(t, db, own, "finance")
	otherMarketing := seedRunIn(t, db, other, "marketing")
	seedRunIn(t, db, other, "finance")

	svc := &Service{ctx: context.Background(), db: db}
	runs, err := svc.History(inc, "", nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{ownMarketing}, runIDs(runs), "the own job's finance-era run stays with finance")

	runs, err = svc.History(inc, "other-job", []string{"other-job"})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{otherMarketing}, runIDs(runs))

	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", other).Update("namespace", "finance").Error)
	_, err = svc.History(inc, "other-job", []string{"other-job"})
	require.ErrorIs(t, err, ErrForbiddenJob, "an allow-listed job that moved out is no longer readable")

	// The own job stays addressable after it moved out, but only its runs
	// recorded in the incident's namespace.
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", own).Update("namespace", "finance").Error)
	runs, err = svc.History(inc, "own-job", nil)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{ownMarketing}, runIDs(runs))
}

// TestBundleForConfinedReaderWithholdsPreUpgradeBaseline: a failure message
// that quotes a baseline recorded before baselines were namespace-owned is
// withheld from a confined reader (the incident list's last_error rule) and
// served as stored to an unconfined one.
func TestBundleForConfinedReaderWithholdsPreUpgradeBaseline(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	now := time.Now().UTC()
	jobID := seedJob(t, db, "baseline-job")
	runID := seedRunIn(t, db, jobID, "default")
	taskID := uuid.New()
	require.NoError(t, db.Create(&models.Task{ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "process"}).Error)
	legacy := "observed 10 deviates from the baseline median 31337 (5 sample(s))"
	require.NoError(t, db.Create(&models.TaskRun{
		ID: uuid.New(), JobRunID: runID, TaskID: taskID, AtomID: uuid.New(), Engine: models.AtomEngineDocker,
		Image: "busybox:1.36.1", Command: `["echo"]`, Status: "failed", Attempt: 1, Error: "assertion failed: " + legacy,
		DataViolations: datatypes.JSON(`[{"dataset":"d","metric":"rowCount","assertion":"deltaFromBaseline","observed":10,"bound":15668.5,"baseline_median":31337,"baseline_samples":5,"message":"` + legacy + `"}]`),
		CreatedAt:      now, UpdatedAt: now,
	}).Error)
	inc, _, err := iincident.NewStore(db).OpenOrAppend(context.Background(), iincident.OpenParams{
		JobID: jobID, RunID: &runID, TaskID: &taskID, TaskName: "process", Class: iincident.ClassUnknown,
	})
	require.NoError(t, err)

	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runID).Update("error", "run failed: "+legacy).Error)

	svc := &Service{ctx: context.Background(), db: db}
	b, err := svc.Bundle(inc.ID)
	require.NoError(t, err)
	require.Contains(t, b.Failure.Error, "31337")
	require.Contains(t, b.RunHistory[0].Error, "31337")
	runs, err := svc.History(inc, "", nil)
	require.NoError(t, err)
	require.Contains(t, runs[0].Error, "31337")

	confined := svc.ForReader(iincident.ReaderConfined)
	b, err = confined.Bundle(inc.ID)
	require.NoError(t, err)
	require.NotContains(t, b.Failure.Error, "31337")
	require.NotEmpty(t, b.RunHistory)
	require.NotContains(t, b.RunHistory[0].Error, "31337")
	runs, err = confined.History(inc, "", nil)
	require.NoError(t, err)
	require.NotEmpty(t, runs)
	require.NotContains(t, runs[0].Error, "31337")
}

// TestLegacyAuthorWritesUnmarkedNotes: a note written with a credential from
// before namespace confinement is stored unmarked (A8).
func TestLegacyAuthorWritesUnmarkedNotes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	inc := seedIncident(t, db, seedJob(t, db, "author-job"))
	SetActionExecutor(nil)
	svc := &Service{ctx: context.Background(), db: db}
	legacy, err := svc.ForLegacyAuthor(true).Note(inc, "legacy")
	require.NoError(t, err)
	require.False(t, legacy.NamespaceConfined)
	current, err := svc.Note(inc, "current")
	require.NoError(t, err)
	require.True(t, current.NamespaceConfined)
	proposed, err := svc.ForLegacyAuthor(true).ProposeAction(inc, ActionRequest{Type: "retry_from_failure"})
	require.NoError(t, err)
	require.False(t, proposed.Action.NamespaceConfined)
}

// TestBundleForConfinedReaderWithholdsLegacyNotes: the service passes its
// reader to the bundle, so a confined reader gets no pre-A8 agent note.
func TestBundleForConfinedReaderWithholdsLegacyNotes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	inc := seedIncident(t, db, seedJob(t, db, "noted-job"))
	legacy, err := iincident.RecordNote(context.Background(), db, inc.ID, nil, &inc.Namespace, "legacy")
	require.NoError(t, err)
	require.NoError(t, db.Model(legacy).Update("namespace_confined", false).Error)

	svc := &Service{ctx: context.Background(), db: db}
	b, err := svc.Bundle(inc.ID)
	require.NoError(t, err)
	require.Len(t, b.Notes, 1)
	b, err = svc.ForReader(iincident.ReaderConfined).Bundle(inc.ID)
	require.NoError(t, err)
	require.Empty(t, b.Notes)
}
