package run

import (
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

// CollectPredecessorStatuses returns the known statuses for the given set of
// predecessor task IDs using the in-memory outcomes map.  Predecessors with no
// recorded outcome yet are omitted.
//
// This and SatisfiesTriggerRule are the single source of truth for trigger-rule
// evaluation, shared by the local executor (internal/job) and the run-owner
// in-memory state machine (RunState) so DAG advancement semantics cannot drift
// between the two paths.
func CollectPredecessorStatuses(predIDs []uuid.UUID, taskOutcomes map[uuid.UUID]TaskStatus) []TaskStatus {
	statuses := make([]TaskStatus, 0, len(predIDs))
	for _, id := range predIDs {
		if status, ok := taskOutcomes[id]; ok {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

// SatisfiesTriggerRule evaluates the trigger rule against the provided
// predecessor statuses.  It returns true when the task should run, false when
// it should be skipped.  An empty rule defaults to all_success; a task with no
// predecessors always runs.
func SatisfiesTriggerRule(rule string, predStatuses []TaskStatus) bool {
	if rule == "" {
		rule = jobdefschema.TriggerRuleAllSuccess
	}

	// A task with no predecessors always runs regardless of rule.
	if len(predStatuses) == 0 {
		return true
	}

	isTerminal := func(s TaskStatus) bool {
		return s == TaskStatusSucceeded || s == TaskStatusCached || s == TaskStatusFailed || s == TaskStatusSkipped
	}

	switch rule {
	case jobdefschema.TriggerRuleAllSuccess:
		for _, s := range predStatuses {
			if !IsTerminalSuccess(s) {
				return false
			}
		}
		return true

	case jobdefschema.TriggerRuleAllDone, jobdefschema.TriggerRuleAlways:
		for _, s := range predStatuses {
			if !isTerminal(s) {
				return false
			}
		}
		return true

	case jobdefschema.TriggerRuleAllFailed:
		for _, s := range predStatuses {
			if s != TaskStatusFailed {
				return false
			}
		}
		return true

	case jobdefschema.TriggerRuleOneSuccess:
		for _, s := range predStatuses {
			if IsTerminalSuccess(s) {
				return true
			}
		}
		return false

	default:
		// Unknown rule: default to all_success behaviour.
		for _, s := range predStatuses {
			if !IsTerminalSuccess(s) {
				return false
			}
		}
		return true
	}
}
