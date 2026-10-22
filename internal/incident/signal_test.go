package incident

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestTaskRunSignal pins the shared row→Signal population the subscriber and
// `caesium why` both classify from: the caller's event type is kept, every
// row field is carried, and a run-level event without a task row carries only
// its event type (so run_timed_out still classifies as sla_risk).
func TestTaskRunSignal(t *testing.T) {
	code := 137
	row := &models.TaskRun{
		Result:           "failure",
		SchemaViolations: datatypes.JSON(`[{"field":"x"}]`),
		LogText:          "tail",
		Error:            "boom",
		ExitCode:         &code,
		OOMKnown:         true,
		OOMKilled:        false,
	}
	require.Equal(t, Signal{
		EventType:           string(event.TypeTaskFailed),
		Result:              "failure",
		HasSchemaViolations: true,
		LogTail:             "tail",
		Error:               "boom",
		ExitCode:            &code,
		OOMKnown:            true,
	}, TaskRunSignal(string(event.TypeTaskFailed), row))

	classifier := NewClassifier()
	timedOut := TaskRunSignal(string(event.TypeRunTimedOut), nil)
	require.Equal(t, Signal{EventType: string(event.TypeRunTimedOut)}, timedOut)
	require.Equal(t, ClassSLARisk, classifier.Classify(timedOut))
	// The event type still wins over a row that would classify otherwise.
	require.Equal(t, ClassSLARisk, classifier.Classify(TaskRunSignal(string(event.TypeRunTimedOut), &models.TaskRun{Error: "x " + secret.DeniedMarker + " y"})))
	require.Equal(t, ClassUnknown, classifier.Classify(TaskRunSignal(string(event.TypeRunFailed), nil)))
	require.Equal(t, ClassConfig, classifier.Classify(TaskRunSignal(string(event.TypeTaskFailed), &models.TaskRun{Error: "x " + secret.DeniedMarker + " y"})))
}
