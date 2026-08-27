package task

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// PartitionGraph is the in-group dependsOn graph over a normalized partition
// list. Indegree is the number of sibling keys an instance waits on (seeded
// onto outstanding_predecessors). Dependents is the reverse adjacency used
// for sibling decrement and the skip cascade.
type PartitionGraph struct {
	Indegree   map[string]int
	Dependents map[string][]string
	Order      []string // Kahn order; emission order is PartitionIndex, not this
	MaxDepth   int
}

// ValidatePartitionGraph resolves dependsOn against the emitted key set,
// rejects dangling keys and self-references, and runs a Kahn pass to detect
// cycles. Errors name the offending key(s).
func ValidatePartitionGraph(parts []Partition) (*PartitionGraph, error) {
	keys := make(map[string]struct{}, len(parts))
	for _, p := range parts {
		if _, dup := keys[p.Key]; dup {
			return nil, fmt.Errorf("partition key %q appears more than once", p.Key)
		}
		keys[p.Key] = struct{}{}
	}

	indegree := make(map[string]int, len(parts))
	dependents := make(map[string][]string, len(parts))
	for _, p := range parts {
		indegree[p.Key] = 0
	}

	for _, p := range parts {
		seen := make(map[string]struct{}, len(p.DependsOn))
		for _, dep := range p.DependsOn {
			dep = strings.TrimSpace(dep)
			if dep == "" {
				return nil, fmt.Errorf("partition %q dependsOn contains an empty key", p.Key)
			}
			if dep == p.Key {
				return nil, fmt.Errorf("partition %q depends on itself", p.Key)
			}
			if _, ok := keys[dep]; !ok {
				return nil, fmt.Errorf("partition %q dependsOn %q which is not in the emitted set", p.Key, dep)
			}
			if _, dup := seen[dep]; dup {
				continue
			}
			seen[dep] = struct{}{}
			indegree[p.Key]++
			dependents[dep] = append(dependents[dep], p.Key)
		}
	}

	for k := range dependents {
		sort.Strings(dependents[k])
	}

	remaining := make(map[string]int, len(indegree))
	queue := make([]string, 0, len(parts))
	for k, d := range indegree {
		remaining[k] = d
		if d == 0 {
			queue = append(queue, k)
		}
	}
	sort.Strings(queue)

	order := make([]string, 0, len(parts))
	depth := make(map[string]int, len(parts))
	maxDepth := 0
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, dep := range dependents[n] {
			if depth[n]+1 > depth[dep] {
				depth[dep] = depth[n] + 1
				if depth[dep] > maxDepth {
					maxDepth = depth[dep]
				}
			}
			remaining[dep]--
			if remaining[dep] == 0 {
				queue = append(queue, dep)
			}
		}
	}

	if len(order) != len(parts) {
		cycleKeys := make([]string, 0)
		for k, d := range remaining {
			if d > 0 {
				cycleKeys = append(cycleKeys, k)
			}
		}
		sort.Strings(cycleKeys)
		quoted := make([]string, len(cycleKeys))
		for i, k := range cycleKeys {
			quoted[i] = strconv.Quote(k)
		}
		return nil, fmt.Errorf("partition dependsOn cycle involving %s", strings.Join(quoted, ", "))
	}

	return &PartitionGraph{
		Indegree:   indegree,
		Dependents: dependents,
		Order:      order,
		MaxDepth:   maxDepth,
	}, nil
}
