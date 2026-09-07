package run

import (
	"slices"

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

// IsTolerantTriggerRule reports whether a rule explicitly handles upstream
// FAILURE, and therefore must never be pre-emptively skipped when a
// predecessor fails under the `continue` failure policy — its own rule
// evaluation, once every predecessor is terminal, is what decides.
//
// It lives here with CollectPredecessorStatuses and SatisfiesTriggerRule
// because all three answer the same question and all three have more than one
// caller: the local executor's skipDescendantsFiltered (internal/job) and the
// distributed worker's descendant sweep (internal/worker) both need it, and
// they cannot share a helper anywhere else — internal/job imports
// internal/worker, so the dependency can only point this way. They had drifted:
// the local sweep filtered by rule and the worker's did not, so under
// `continue` a distributed run skipped the very all_done consumer a failed
// predecessor had just released.
func IsTolerantTriggerRule(rule string) bool {
	switch rule {
	case jobdefschema.TriggerRuleAllDone,
		jobdefschema.TriggerRuleAllFailed,
		jobdefschema.TriggerRuleAlways,
		jobdefschema.TriggerRuleOneSuccess:
		return true
	default:
		return false
	}
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
			if !IsTerminal(s) {
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
		return slices.ContainsFunc(predStatuses, IsTerminalSuccess)

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
