// Package daganalysis computes topology metrics for job DAGs.
package daganalysis

import (
	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

// StepInfo describes a single step in the DAG.
type StepInfo struct {
	Name       string
	Engine     string
	Image      string
	DependsOn  []string
	Successors []string
	Depth      int // topological depth (layer index)
}

// DAGAnalysis holds the computed topology of a job definition.
type DAGAnalysis struct {
	Steps          []StepInfo
	ExecutionOrder [][]string // layers of parallelizable steps
	MaxParallelism int
	RootSteps      []string // steps with no predecessors
	LeafSteps      []string // steps with no successors
}

// Analyze computes topology metrics for the given definition.
// The definition must already be validated.
func Analyze(def *jobdef.Definition) (*DAGAnalysis, error) {
	successors, err := jobdef.DeriveStepSuccessors(def.Steps)
	if err != nil {
		return nil, err
	}

	// Build predecessor map from successors.
	predecessors := make(map[string][]string, len(def.Steps))
	for _, s := range def.Steps {
		predecessors[s.Name] = nil
	}
	for from, succs := range successors {
		for _, to := range succs {
			predecessors[to] = append(predecessors[to], from)
		}
	}

	// BFS layer decomposition (Kahn's algorithm variant).
	inDegree := make(map[string]int, len(def.Steps))
	for _, s := range def.Steps {
		inDegree[s.Name] = len(predecessors[s.Name])
	}

	var roots []string
	for _, s := range def.Steps {
		if inDegree[s.Name] == 0 {
			roots = append(roots, s.Name)
		}
	}

	depth := make(map[string]int, len(def.Steps))
	var layers [][]string
	queue := make([]string, len(roots))
	copy(queue, roots)

	for len(queue) > 0 {
		layer := queue
		queue = nil
		layers = append(layers, layer)
		for _, name := range layer {
			depth[name] = len(layers) - 1
			for _, succ := range successors[name] {
				inDegree[succ]--
				if inDegree[succ] == 0 {
					queue = append(queue, succ)
				}
			}
		}
	}

	maxParallel := 0
	for _, layer := range layers {
		if len(layer) > maxParallel {
			maxParallel = len(layer)
		}
	}

	// Leaf steps: no successors.
	var leaves []string
	for _, s := range def.Steps {
		if len(successors[s.Name]) == 0 {
			leaves = append(leaves, s.Name)
		}
	}

	// Build StepInfo slice preserving definition order.
	steps := make([]StepInfo, len(def.Steps))
	for i, s := range def.Steps {
		steps[i] = StepInfo{
			Name:       s.Name,
			Engine:     s.Engine,
			Image:      s.Image,
			DependsOn:  predecessors[s.Name],
			Successors: successors[s.Name],
			Depth:      depth[s.Name],
		}
	}

	return &DAGAnalysis{
		Steps:          steps,
		ExecutionOrder: layers,
		MaxParallelism: maxParallel,
		RootSteps:      roots,
		LeafSteps:      leaves,
	}, nil
}

// UniqueImages returns the deduplicated set of container images in definition order.
func UniqueImages(def *jobdef.Definition) []string {
	seen := make(map[string]struct{}, len(def.Steps))
	var images []string
	for _, s := range def.Steps {
		if _, ok := seen[s.Image]; ok {
			continue
		}
		seen[s.Image] = struct{}{}
		images = append(images, s.Image)
	}
	return images
}
