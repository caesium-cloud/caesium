package incident

import (
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// DedupeKey is the stable correlation key for an incident:
// (job_id, task_name, failure_class). Failures sharing a key fold into one
// incident rather than opening twins.
func DedupeKey(jobID uuid.UUID, taskName string, class FailureClass) string {
	return fmt.Sprintf("%s|%s|%s", jobID.String(), taskName, class)
}

// allowedTransitions encodes the incident status machine:
//
//	open → triaging → (awaiting_approval ↔ triaging) → remediated | escalated → closed
//
// plus suppressed / abandoned as terminal dispositions reachable from any
// non-terminal state. remediated and escalated are pre-terminal (they still
// transition to closed); closed/suppressed/abandoned are terminal.
var allowedTransitions = map[models.IncidentStatus]map[models.IncidentStatus]struct{}{
	models.IncidentStatusOpen: {
		models.IncidentStatusTriaging:   {},
		models.IncidentStatusEscalated:  {},
		models.IncidentStatusRemediated: {},
		models.IncidentStatusSuppressed: {},
		models.IncidentStatusAbandoned:  {},
	},
	models.IncidentStatusTriaging: {
		models.IncidentStatusAwaitingApproval: {},
		models.IncidentStatusRemediated:       {},
		models.IncidentStatusEscalated:        {},
		models.IncidentStatusSuppressed:       {},
		models.IncidentStatusAbandoned:        {},
	},
	models.IncidentStatusAwaitingApproval: {
		models.IncidentStatusTriaging:   {},
		models.IncidentStatusRemediated: {},
		models.IncidentStatusEscalated:  {},
		models.IncidentStatusSuppressed: {},
		models.IncidentStatusAbandoned:  {},
	},
	models.IncidentStatusRemediated: {
		models.IncidentStatusClosed: {},
	},
	models.IncidentStatusEscalated: {
		models.IncidentStatusClosed:    {},
		models.IncidentStatusAbandoned: {},
	},
}

// CanTransition reports whether the status machine permits from → to.
func CanTransition(from, to models.IncidentStatus) bool {
	if from == to {
		return false
	}
	targets, ok := allowedTransitions[from]
	if !ok {
		return false
	}
	_, ok = targets[to]
	return ok
}
