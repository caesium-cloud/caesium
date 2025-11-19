package dag

import (
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

func TestFromJobDAGTranslatesGraph(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "task-a", AtomID: "atom-1", Successors: []string{"task-b", "task-c"}},
			{ID: "task-b", AtomID: "atom-2", Successors: []string{"task-d"}},
			{ID: "task-c", AtomID: "atom-3", Successors: []string{"task-d"}},
			{ID: "task-d", AtomID: "atom-4"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	if graph == nil {
		t.Fatal("expected graph to be non-nil")
	}

	if roots := graph.Roots(); len(roots) != 1 || roots[0].ID() != "task-a" {
		t.Fatalf("expected single root task-a, got %#v", roots)
	}

	nodeB, ok := graph.Node("task-b")
	if !ok {
		t.Fatal("expected to find node task-b")
	}

	if depth := nodeB.Depth(); depth != 1 {
		t.Fatalf("node task-b depth = %d, want 1", depth)
	}

	successors := nodeB.Successors()
	if len(successors) != 1 || successors[0].ID() != "task-d" {
		t.Fatalf("expected task-b successor task-d, got %#v", successors)
	}

	nodeD, ok := graph.Node("task-d")
	if !ok {
		t.Fatal("expected to find node task-d")
	}
	if preds := nodeD.Predecessors(); len(preds) != 2 {
		t.Fatalf("expected task-d to have two predecessors, got %d", len(preds))
	}
}

func TestFromJobDAGDetectsCycles(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "task-a", Successors: []string{"task-b"}},
			{ID: "task-b", Successors: []string{"task-a"}},
		},
	}

	if _, err := FromJobDAG(spec); err == nil {
		t.Fatal("expected cycle detection error")
	}
}
