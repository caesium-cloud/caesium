package run

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
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
	// The override is consulted only once violations exist, so the common
	// (clean) path pays no extra read on every validating task.
	if SchemaGateOverridden(store, runID) {
		logSchemaGateBypass(runID, taskID, len(violations))
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

// SchemaGateOverridden reports whether an APPROVED tier-3 `override_schema_gate`
// action bypassed output-schema enforcement for this one run
// (models.JobRun.SchemaGateOverride, written only by the incident action
// executor after a human approval).
//
// It is the single read point both validation entry points consult, so the
// bypass cannot apply to the unfanned path and silently not to the fanned one.
// A missing store/run or a read error is "not overridden" — the gate stays ON,
// which is the safe direction for a security-adjacent bypass.
func SchemaGateOverridden(store *Store, runID uuid.UUID) bool {
	if store == nil || store.db == nil || runID == uuid.Nil {
		return false
	}
	var overridden bool
	if err := store.db.
		Model(&models.JobRun{}).
		Select("schema_gate_override").
		Where("id = ?", runID).
		Scan(&overridden).Error; err != nil {
		log.Warn("failed to read schema gate override; enforcing schema", "run_id", runID, "error", err)
		return false
	}
	return overridden
}

// logSchemaGateBypass records that an approved override suppressed real
// violations. The bypass must never be silent: it is the audit breadcrumb that
// pairs with the AgentAction row and the `caesium why` remediation provenance,
// and it names how many violations were let through.
func logSchemaGateBypass(runID, taskID uuid.UUID, violations int) {
	log.Warn("output schema violations suppressed by an approved override_schema_gate action",
		"run_id", runID,
		"task_id", taskID,
		"violations", violations,
	)
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
