package incident

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func mkTrigger(t *testing.T, db *gorm.DB) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Trigger{
		ID:            id,
		Type:          models.TriggerTypeCron,
		Configuration: `{"cron":"0 * * * *"}`,
		CreatedAt:     now,
		UpdatedAt:     now,
	}).Error)
	return id
}

func mkJob(t *testing.T, db *gorm.DB, triggerID uuid.UUID, alias string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Job{
		ID:        id,
		Alias:     alias,
		TriggerID: triggerID,
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return id
}

func mkRun(t *testing.T, db *gorm.DB, jobID, triggerID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.JobRun{
		ID:        id,
		JobID:     jobID,
		TriggerID: triggerID,
		Status:    "succeeded",
		StartedAt: now,
		CreatedAt: now,
	}).Error)
	return id
}

func mkTaskRun(t *testing.T, db *gorm.DB, runID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.TaskRun{
		ID:        id,
		JobRunID:  runID,
		TaskID:    uuid.New(),
		AtomID:    uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "busybox",
		Command:   "echo",
		Status:    "succeeded",
		CreatedAt: now,
		UpdatedAt: now,
	}).Error)
	return id
}

func mkDataset(t *testing.T, db *gorm.DB, taskRunID uuid.UUID, dir, ns, name string) {
	t.Helper()
	require.NoError(t, db.Create(&models.LineageDataset{
		ID:        uuid.New(),
		TaskRunID: taskRunID,
		Namespace: ns,
		Name:      name,
		Direction: dir,
		CreatedAt: time.Now().UTC(),
	}).Error)
}

// TestFreezeAllowlistExcludesFailingRunOutputs is the security property: the
// frozen allowlist is seeded only from the failing job's TRUSTED historical
// outputs, never from the failing run's own outputs. A downstream job reachable
// only through an attacker-crafted output of the failing run must NOT appear.
func TestFreezeAllowlistExcludesFailingRunOutputs(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)

	// job-a: a trusted prior run produces D1; a FAILING run produces the poisoned
	// output "devil".
	jobA := mkJob(t, db, tr, "job-a")
	trustedRun := mkRun(t, db, jobA, tr)
	trustedTaskRun := mkTaskRun(t, db, trustedRun)
	mkDataset(t, db, trustedTaskRun, "output", "ns", "D1")

	failRun := mkRun(t, db, jobA, tr)
	failTaskRun := mkTaskRun(t, db, failRun)
	mkDataset(t, db, failTaskRun, "output", "ns", "devil")

	// job-b consumes the trusted D1 (legitimately downstream).
	jobB := mkJob(t, db, tr, "job-b")
	runB := mkRun(t, db, jobB, tr)
	taskB := mkTaskRun(t, db, runB)
	mkDataset(t, db, taskB, "input", "ns", "D1")
	mkDataset(t, db, taskB, "output", "ns", "D2")

	// job-c consumes ONLY the poisoned "devil" output — reachable only via the
	// failing run.
	jobC := mkJob(t, db, tr, "job-c")
	runC := mkRun(t, db, jobC, tr)
	taskC := mkTaskRun(t, db, runC)
	mkDataset(t, db, taskC, "input", "ns", "devil")
	mkDataset(t, db, taskC, "output", "ns", "D3")

	// Excluding the failing run: allowlist is {job-a, job-b}; job-c is NOT
	// reachable because the poisoned edge is excluded.
	frozen := FreezeAllowlist(ctx, db, jobA, &failRun)
	require.ElementsMatch(t, []string{"job-a", "job-b"}, frozen)
	require.NotContains(t, frozen, "job-c")

	// Contrast: if the failing run's outputs were NOT excluded, job-c leaks in —
	// this is exactly the widening the exclusion prevents.
	leaky := FreezeAllowlist(ctx, db, jobA, nil)
	require.Contains(t, leaky, "job-c")
}

// TestFreezeAllowlistDegradesToOwnJob proves the best-effort degradation: with no
// lineage graph the allowlist is just the incident's own job.
func TestFreezeAllowlistDegradesToOwnJob(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobA := mkJob(t, db, tr, "solo")

	frozen := FreezeAllowlist(ctx, db, jobA, nil)
	require.Equal(t, []string{"solo"}, frozen)
}

// TestOpenOrAppendFreezesAllowlist proves the freeze happens at incident open and
// is persisted on the incident row.
func TestOpenOrAppendFreezesAllowlist(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	tr := mkTrigger(t, db)
	jobA := mkJob(t, db, tr, "frozen-job")

	store := NewStore(db)
	inc, outcome, err := store.OpenOrAppend(ctx, OpenParams{
		JobID:    jobA,
		TaskName: "extract",
		Class:    ClassUnknown,
	})
	require.NoError(t, err)
	require.Equal(t, OutcomeOpened, outcome)

	got, err := store.AllowedJobsForIncident(ctx, inc.ID)
	require.NoError(t, err)
	require.Equal(t, []string{"frozen-job"}, got)
}
