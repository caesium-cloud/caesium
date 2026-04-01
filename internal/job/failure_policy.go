package job

import (
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
)

const (
	taskFailurePolicyHalt     = "halt"
	taskFailurePolicyContinue = "continue"
)

func normalizeTaskFailurePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case taskFailurePolicyContinue:
		return taskFailurePolicyContinue
	default:
		return taskFailurePolicyHalt
	}
}

// computeRetryDelay returns the delay before the next retry attempt.
// If RetryBackoff is true, the delay doubles with each attempt: retryDelay * 2^(attempt-1).
// If RetryBackoff is false, the delay is constant.
// Returns zero if task is nil or RetryDelay is zero.
func computeRetryDelay(task *models.Task, attempt int) time.Duration {
	if task == nil || task.RetryDelay <= 0 {
		return 0
	}
	if task.RetryBackoff {
		return task.RetryDelay * (1 << uint(attempt-1))
	}
	return task.RetryDelay
}

func collectDescendants(adjacency map[uuid.UUID][]uuid.UUID, start uuid.UUID) []uuid.UUID {
	queue := append([]uuid.UUID(nil), adjacency[start]...)
	seen := make(map[uuid.UUID]struct{}, len(queue))
	out := make([]uuid.UUID, 0, len(queue))

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
		queue = append(queue, adjacency[id]...)
	}

	return out
}

// isTolerantRule returns true for trigger rules that explicitly handle
// failures and should therefore NOT be pre-emptively skipped when an
// upstream task fails under the "continue" failure policy.
func isTolerantRule(rule string) bool {
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

// skipDescendantsFiltered walks the adjacency graph from start, calling
// skipFn for every descendant whose trigger rule is NOT tolerant of
// failures (for example all_success). Tolerant descendants are
// left for the normal indegree path and are NOT passed to skipFn.
func skipDescendantsFiltered(
	adjacency map[uuid.UUID][]uuid.UUID,
	predecessors map[uuid.UUID][]uuid.UUID,
	triggerRuleByTask map[uuid.UUID]string,
	start uuid.UUID,
	processed map[uuid.UUID]bool,
	inQueue map[uuid.UUID]bool,
	skipFn func(uuid.UUID),
) {
	queue := append([]uuid.UUID(nil), adjacency[start]...)
	seen := make(map[uuid.UUID]struct{}, len(queue))

	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]

		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}

		if processed[id] || inQueue[id] {
			continue
		}

		rule := triggerRuleByTask[id]
		if isTolerantRule(rule) {
			// Do not skip; leave for the indegree path.
			continue
		}

		skipFn(id)

		// Continue walking descendants of the skipped task.
		queue = append(queue, adjacency[id]...)
	}
}

// collectPredecessorStatuses returns the known statuses for the given set of
// predecessor task IDs using the in-memory outcomes map.
func collectPredecessorStatuses(predIDs []uuid.UUID, taskOutcomes map[uuid.UUID]run.TaskStatus) []run.TaskStatus {
	statuses := make([]run.TaskStatus, 0, len(predIDs))
	for _, id := range predIDs {
		if status, ok := taskOutcomes[id]; ok {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

// satisfiesTriggerRule evaluates the trigger rule against the provided
// predecessor statuses. It returns true when the task should run, false
// when it should be skipped. An empty rule defaults to all_success.
func satisfiesTriggerRule(rule string, predStatuses []run.TaskStatus) bool {
	if rule == "" {
		rule = jobdefschema.TriggerRuleAllSuccess
	}

	// A task with no predecessors always runs regardless of rule.
	if len(predStatuses) == 0 {
		return true
	}

	isTerminal := func(s run.TaskStatus) bool {
		return s == run.TaskStatusSucceeded || s == run.TaskStatusCached || s == run.TaskStatusFailed || s == run.TaskStatusSkipped
	}

	switch rule {
	case jobdefschema.TriggerRuleAllSuccess:
		for _, s := range predStatuses {
			if !run.IsTerminalSuccess(s) {
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
			if s != run.TaskStatusFailed {
				return false
			}
		}
		return true

	case jobdefschema.TriggerRuleOneSuccess:
		for _, s := range predStatuses {
			if run.IsTerminalSuccess(s) {
				return true
			}
		}
		return false

	default:
		// Unknown rule: default to all_success behaviour.
		for _, s := range predStatuses {
			if !run.IsTerminalSuccess(s) {
				return false
			}
		}
		return true
	}
}
