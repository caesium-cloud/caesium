package run

import (
	"fmt"

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
		return fmt.Errorf("task %s output violates declared schema: %d violation(s)", taskID, len(violations))
	}

	return nil
}
