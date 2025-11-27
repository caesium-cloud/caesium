package dag

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
)

func TestRenderRendersLevels(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "task-a", AtomID: "atom-1", Successors: []string{"task-b"}},
			{ID: "task-b", AtomID: "atom-2"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	labeler := func(n *Node) string {
		return strings.ToUpper(n.ID())
	}

	output := Render(graph, RenderOptions{
		FocusedID: "task-b",
		Labeler:   labeler,
	})
	if output == "" {
		t.Fatal("expected non-empty layout output")
	}

	if !strings.Contains(output, "TASK-A") || !strings.Contains(output, "TASK-B") {
		t.Fatalf("layout missing formatted identifiers: %q", output)
	}

	if !strings.Contains(output, "▶ TASK-B") {
		t.Fatalf("expected focused marker arrow in layout: %q", output)
	}
}

func TestRenderHandlesNilGraph(t *testing.T) {
	if got := Render(nil, RenderOptions{FocusedID: "task-a"}); got != "" {
		t.Fatalf("expected empty layout for nil graph, got %q", got)
	}
}
