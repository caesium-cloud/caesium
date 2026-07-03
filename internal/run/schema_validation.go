package run

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

// ValidateTaskOutputSchema validates a task's captured output against its declared schema,
// persists any violations, and escalates them according to the configured validation mode.
func ValidateTaskOutputSchema(store *Store, runID, taskID uuid.UUID, output map[string]string, outputSchema []byte, schemaValidation string) error {
	if len(outputSchema) == 0 || schemaValidation == "" {
		return nil
	}

	violations, err := pkgtask.ValidateOutputSchemaBytes(output, outputSchema)
	if err != nil {
		log.Warn("schema validation error", "task_id", taskID, "error", err)
		return nil
	}
	if len(violations) == 0 {
		return nil
	}

	log.Warn("task output schema violations", "task_id", taskID, "violations", len(violations))
	if saveErr := store.SaveSchemaViolations(runID, taskID, violations); saveErr != nil {
		log.Warn("failed to persist schema violations", "task_id", taskID, "error", saveErr)
	}

	if schemaValidation == jobdef.SchemaValidationFail {
		// In fail mode the task fails and its task_failed event already carries
		// the violations, so no separate event is emitted.
		return fmt.Errorf("task %s output violates declared schema: %d violation(s)", taskID, len(violations))
	}

	// In warn mode the task does NOT fail, so the incident manager would never
	// observe the violation. Emit a dedicated schema_violation_recorded event so
	// the leader-gated incident subscriber can open a schema_violation incident.
	publishSchemaViolationEvent(store, runID, taskID, len(violations))

	return nil
}

// publishSchemaViolationEvent emits a schema_violation_recorded event for a
// warn-mode violation. Best-effort: a nil bus/store is a no-op. The event
// carries RunID/TaskID; the subscriber resolves job_id and task_name from the
// run when correlating.
func publishSchemaViolationEvent(store *Store, runID, taskID uuid.UUID, count int) {
	if store == nil {
		return
	}
	payload, _ := json.Marshal(struct {
		Violations int `json:"violations"`
	}{Violations: count})
	store.PublishEvents(event.Event{
		Type:      event.TypeSchemaViolationRecorded,
		RunID:     runID,
		TaskID:    taskID,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
	})
}
