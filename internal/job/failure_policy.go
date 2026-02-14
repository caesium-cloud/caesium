package job

import (
	"strings"

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
