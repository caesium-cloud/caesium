package job

import (
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
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
