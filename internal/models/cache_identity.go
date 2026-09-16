package models

import "github.com/google/uuid"

// FrozenImageIdentityChecks derives one run-wide gate from a complete frozen DAG.
// Missing predecessors prevent proving that identity checks are unnecessary.
func FrozenImageIdentityChecks(descriptors []TaskExecutionDescriptor) *bool {
	required := false
	ids := make(map[uuid.UUID]bool, len(descriptors))
	for _, desc := range descriptors {
		ids[desc.Baseline.TaskID] = true
		required = required || desc.Cache.Enabled && desc.Cache.PinDigests
	}
	if required {
		return &required
	}
	for _, desc := range descriptors {
		if desc.DAG.OutstandingPredecessors > len(desc.DAG.Predecessors) {
			return nil
		}
		for _, pred := range desc.DAG.Predecessors {
			if !ids[pred.TaskID] {
				return nil
			}
		}
	}
	return &required
}
