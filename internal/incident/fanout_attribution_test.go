package incident

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fanout_attribution_test.go covers the G6 "re-key" defect on the incident path:
// resolving (job_run_id, task_id) with .First() returns an ARBITRARY sibling of a
// fanned group, so an incident could be classified from a SUCCEEDED instance —
// wrong class, empty log tail, no error text, and a remediation dispatched
// against a row that never failed.

// seedFanOutFailure writes a 4-partition group where partition "a" succeeded,
// "b" and "d" failed, and "c" succeeded. The FAILED instances carry the
// diagnostic signal; the succeeded ones are deliberately first in partition
// order so a naive .First() picks the wrong row.
func seedFanOutFailure(t *testing.T, db *gorm.DB) (jobID, runID, taskID uuid.UUID, failedIDs []uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()
	jobID, runID, taskID = uuid.New(), uuid.New(), uuid.New()

	require.NoError(t, db.Create(&models.JobRun{
		ID: runID, JobID: jobID, Status: "failed", StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}).Error)
	require.NoError(t, db.Create(&models.Task{
		ID: taskID, JobID: jobID, AtomID: uuid.New(), Name: "extract", CreatedAt: now, UpdatedAt: now,
	}).Error)

	exit := 137
	rows := []struct {
		value  string
		status string
		log    string
		err    string
	}{
		{"a", "succeeded", "all good", ""},
		{"b", "failed", "Error: permission denied reading /secure", "auth failed on b"},
		{"c", "succeeded", "all good", ""},
		{"d", "failed", "Error: permission denied reading /secure", "auth failed on d"},
	}
	for i, r := range rows {
		id := uuid.New()
		tr := &models.TaskRun{
			ID: id, JobRunID: runID, TaskID: taskID, AtomID: uuid.New(),
			Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: "sh",
			Status: r.status, LogText: r.log, Error: r.err, Attempt: 1,
			PartitionValue: r.value, PartitionIndex: i, PartitionCount: len(rows),
			CreatedAt: now, UpdatedAt: now,
		}
		if r.status == "failed" {
			tr.Result = "failure"
			tr.ExitCode = &exit
			failedIDs = append(failedIDs, id)
		}
		require.NoError(t, db.Create(tr).Error)
	}
	return jobID, runID, taskID, failedIDs
}

// TestSubscriber_FannedGroupClassifiesFromTheFailedInstance is the core
// regression: partition "a" succeeded and sorts first, so the pre-fix `.First()`
// classified the incident from a green row (class "unknown", no error).
func TestSubscriber_FannedGroupClassifiesFromTheFailedInstance(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID, _ := seedFanOutFailure(t, db)

	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	require.Equal(t, string(ClassAuthFailure), inc.Class,
		"the class must come from a FAILED instance's log, not a succeeded sibling's")
	require.Equal(t, "auth failed on b", inc.LastError,
		"the first failed partition in order is the attributed one")
	require.Equal(t, "extract", inc.TaskName)

	var evidence map[string]any
	require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
	require.Equal(t, "b", evidence["partition"], "evidence names the attributed instance")
	require.EqualValues(t, 2, evidence["failed_partition_count"])
	require.Equal(t, []any{"b", "d"}, evidence["failed_partitions"],
		"every failed sibling is reported, not just the attributed one")
}

// TestResolveContext_UnfannedTaskCarriesNoPartitionEvidence keeps unfanned
// incidents byte-identical: no partition keys in the evidence blob.
func TestSubscriber_UnfannedIncidentEvidenceUnchanged(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	bus := event.New()
	startSubscriber(t, bus, db, 0)

	jobID, runID, taskID := seedFailedTask(t, db, "Error: permission denied reading /secure")
	bus.Publish(event.Event{Type: event.TypeTaskFailed, JobID: jobID, RunID: runID, TaskID: taskID, Timestamp: time.Now()})
	waitForIncidents(t, db, 1)

	var inc models.Incident
	require.NoError(t, db.First(&inc).Error)
	var evidence map[string]any
	require.NoError(t, json.Unmarshal(inc.Evidence, &evidence))
	require.NotContains(t, evidence, "partition")
	require.NotContains(t, evidence, "failed_partitions")
	require.NotContains(t, evidence, "failed_partition_count")
}

func TestAttributionTaskRun_PrefersFirstFailedThenFirstRow(t *testing.T) {
	rows := []models.TaskRun{
		{PartitionValue: "a", Status: "succeeded"},
		{PartitionValue: "b", Status: "failed"},
		{PartitionValue: "c", Status: "failed"},
	}
	require.Equal(t, "b", attributionTaskRun(rows).PartitionValue)

	allGreen := []models.TaskRun{
		{PartitionValue: "a", Status: "succeeded"},
		{PartitionValue: "b", Status: "succeeded"},
	}
	require.Equal(t, "a", attributionTaskRun(allGreen).PartitionValue,
		"with nothing failed the first row is a deterministic fallback")
}

func TestFailedPartitionValues_NilForUnfanned(t *testing.T) {
	require.Nil(t, failedPartitionValues([]models.TaskRun{{Status: "failed"}}))
	require.Equal(t, []string{"b"}, failedPartitionValues([]models.TaskRun{
		{PartitionValue: "a", Status: "succeeded"},
		{PartitionValue: "b", Status: "failed"},
	}))
}

func TestCappedPartitionList_TruncationIsVisible(t *testing.T) {
	values := make([]string, evidencePartitionListCap+5)
	for i := range values {
		values[i] = "p"
	}
	got := cappedPartitionList(values)
	require.Len(t, got, evidencePartitionListCap+1)
	require.Equal(t, "(+5 more)", got[len(got)-1])
}

// TestBuildBundle_FannedFailureDescribesTheFailedInstances is the bundle half of
// the same defect: an agent triaging "extract failed" must get the failed
// instance's scrubbed log and the full list of failed siblings, not a green
// row's empty log.
func TestBuildBundle_FannedFailureDescribesTheFailedInstances(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	jobID, runID, taskID, failedIDs := seedFanOutFailure(t, db)
	// BuildBundle loads the job topology, so the job row must exist.
	tr := mkTrigger(t, db)
	require.NoError(t, db.Model(&models.Job{}).Create(&models.Job{
		ID: jobID, Alias: "vendor-x", TriggerID: tr,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}).Error)

	store := NewStore(db)
	incRunID, incTaskID := runID, taskID
	inc, _, err := store.OpenOrAppend(ctx, OpenParams{
		JobID: jobID, RunID: &incRunID, TaskID: &incTaskID,
		TaskName: "extract", Class: ClassAuthFailure, LastError: "auth failed on b",
	})
	require.NoError(t, err)

	b, err := BuildBundle(ctx, db, inc.ID, nil)
	require.NoError(t, err)

	require.Equal(t, "b", b.Failure.Partition, "the described instance is the first FAILED one")
	require.Contains(t, b.Failure.LogTail, "permission denied",
		"a succeeded sibling's log would carry no failure signal at all")
	require.NotNil(t, b.Failure.ExitCode)
	require.Equal(t, 137, *b.Failure.ExitCode)

	require.Equal(t, 4, b.Failure.PartitionCount)
	require.Len(t, b.Failure.Partitions, 2, "both failed siblings are listed")
	require.Equal(t, "b", b.Failure.Partitions[0].Partition)
	require.Equal(t, "d", b.Failure.Partitions[1].Partition)
	require.Equal(t, failedIDs[0], b.Failure.Partitions[0].TaskRunID,
		"the task_run_id is the handle for fetching that instance's log")
	require.False(t, b.Failure.PartitionsTruncated)
}

func TestFailedPartitionEntries_CapsAndFlagsTruncation(t *testing.T) {
	rows := make([]models.TaskRun, bundleFailedPartitionCap+3)
	for i := range rows {
		rows[i] = models.TaskRun{ID: uuid.New(), PartitionValue: "p", PartitionIndex: i, Status: "failed"}
	}
	entries, truncated := failedPartitionEntries(rows)
	require.Len(t, entries, bundleFailedPartitionCap)
	require.True(t, truncated)

	none, truncated := failedPartitionEntries([]models.TaskRun{{Status: "succeeded"}})
	require.Nil(t, none)
	require.False(t, truncated)
}
