package job

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// fanout_schema_test.go pins where a fanned step's output-schema violations are
// recorded: on the INSTANCE that emitted the bad output.
//
// SaveSchemaViolations refuses a catalog task id that resolves to N sibling rows
// (ErrAmbiguousTaskRun) and the validation helper only LOGS that refusal, so
// keying the write on the catalog id recorded nothing at all for a fanned step —
// fail mode reported a violation whose evidence was never written, warn mode
// opened a schema_violation incident with no row behind it.

// declareFanOutOutputSchema puts a required-integer outputSchema on the fanned
// step, in both the fake task service and the DB row.
func declareFanOutOutputSchema(t *testing.T, f *fanOutFixture) {
	t.Helper()

	schemaBytes, err := json.Marshal(map[string]any{
		"type":       "object",
		"properties": map[string]any{"rows": map[string]any{"type": "integer"}},
		"required":   []string{"rows"},
	})
	require.NoError(t, err)

	f.taskSvc.tasks[1].OutputSchema = datatypes.JSON(schemaBytes)
	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.fanned).
		Update("output_schema", datatypes.JSON(schemaBytes)).Error)
}

// persistJobSchemaValidation writes the job row RegisterTasks reads
// metadata.schemaValidation from when it FREEZES a task's enforcement mode onto
// its task_runs row (internal/run/store.go: the freeze is gated on the job row
// being found). Both executors now read that frozen value rather than the live
// job, so an in-memory models.Job alone no longer configures validation - the
// same requirement the distributed lane has always had.
func persistJobSchemaValidation(t *testing.T, f *fanOutFixture, mode string) {
	t.Helper()
	require.NoError(t, f.db.Create(&models.Job{
		ID:               f.jobID,
		Alias:            "fanout-schema-" + f.jobID.String(),
		TriggerID:        uuid.New(),
		SchemaValidation: mode,
	}).Error)
}

func schemaViolationsByPartition(t *testing.T, rows []models.TaskRun) map[string][]pkgtask.SchemaViolation {
	t.Helper()
	out := make(map[string][]pkgtask.SchemaViolation, len(rows))
	for _, r := range rows {
		if len(r.SchemaViolations) == 0 {
			continue
		}
		var violations []pkgtask.SchemaViolation
		require.NoError(t, json.Unmarshal(r.SchemaViolations, &violations))
		out[r.PartitionValue] = violations
	}
	return out
}

// TestFanOutLocalSchemaViolationsLandOnTheOffendingInstance: one bad partition
// out of three must record its violations, and only its own.
func TestFanOutLocalSchemaViolationsLandOnTheOffendingInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	declareFanOutOutputSchema(t, f)

	f.engine.logsByPartition["a"] = "##caesium::output {\"rows\":\"1\"}\n"
	f.engine.logsByPartition["b"] = "##caesium::output {\"rows\":\"not-a-number\"}\n"
	f.engine.logsByPartition["c"] = "##caesium::output {\"rows\":\"3\"}\n"

	persistJobSchemaValidation(t, f, schema.SchemaValidationWarn)

	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	// warn mode: the offending partition still succeeds, so the ONLY durable
	// record of the violation is the row this write lands on.
	require.NoError(t, New(&models.Job{ID: f.jobID, SchemaValidation: schema.SchemaValidationWarn}, opts...).
		Run(context.Background()))

	rows := f.instanceRows(t)
	require.Len(t, rows, 3)
	for _, r := range rows {
		require.Equal(t, string(run.TaskStatusSucceeded), r.Status, "partition %s", r.PartitionValue)
	}

	violations := schemaViolationsByPartition(t, rows)
	require.Contains(t, violations, "b", "the offending partition must record its own violations")
	require.NotEmpty(t, violations["b"])
	require.Contains(t, violations["b"][0].Message, "integer")
	require.NotContains(t, violations, "a", "a clean sibling must not inherit the violation")
	require.NotContains(t, violations, "c", "a clean sibling must not inherit the violation")
}

// TestFanOutLocalSchemaFailModeRecordsEvidenceOnTheFailedInstance: in fail mode
// the offending partition fails, and the row that failed must carry the reason
// it failed. Without the instance-keyed write the failure was reported with no
// evidence anywhere.
func TestFanOutLocalSchemaFailModeRecordsEvidenceOnTheFailedInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	declareFanOutOutputSchema(t, f)

	f.engine.logsByPartition["a"] = "##caesium::output {\"rows\":\"1\"}\n"
	f.engine.logsByPartition["b"] = "##caesium::output {\"rows\":\"not-a-number\"}\n"
	f.engine.logsByPartition["c"] = "##caesium::output {\"rows\":\"3\"}\n"

	persistJobSchemaValidation(t, f, schema.SchemaValidationFail)

	opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
	require.Error(t, New(&models.Job{ID: f.jobID, SchemaValidation: schema.SchemaValidationFail}, opts...).
		Run(context.Background()))

	rows := f.instanceRows(t)
	statuses := statusByPartition(rows)
	require.Equal(t, string(run.TaskStatusFailed), statuses["b"])
	require.Equal(t, string(run.TaskStatusSucceeded), statuses["a"])
	require.Equal(t, string(run.TaskStatusSucceeded), statuses["c"])

	violations := schemaViolationsByPartition(t, rows)
	require.Contains(t, violations, "b",
		"the failed partition must carry the violations that explain its failure")
	require.NotContains(t, violations, "a")
	require.NotContains(t, violations, "c")
}
