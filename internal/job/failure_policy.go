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

// collectPredecessorStatuses and satisfiesTriggerRule are thin wrappers over the
// canonical implementations in internal/run, kept so the local executor's call
// sites stay unchanged while the run-owner in-memory state machine shares the
// exact same trigger-rule semantics (single source of truth in internal/run).
func collectPredecessorStatuses(predIDs []uuid.UUID, taskOutcomes map[uuid.UUID]run.TaskStatus) []run.TaskStatus {
	return run.CollectPredecessorStatuses(predIDs, taskOutcomes)
}

func satisfiesTriggerRule(rule string, predStatuses []run.TaskStatus) bool {
	return run.SatisfiesTriggerRule(rule, predStatuses)
}
