package dag

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/cmd/console/api"
	"github.com/charmbracelet/lipgloss"
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

	// Verify nodes are rendered as boxes with engine icons
	if !strings.Contains(output, "⚙") {
		t.Fatalf("layout missing engine icon: %q", output)
	}
}

func TestRenderHandlesNilGraph(t *testing.T) {
	if got := Render(nil, RenderOptions{FocusedID: "task-a"}); got != "" {
		t.Fatalf("expected empty layout for nil graph, got %q", got)
	}
}

func TestRenderBoxesContainStatusInfo(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "node-1", AtomID: "atom-1", Successors: []string{"node-2"}},
			{ID: "node-2", AtomID: "atom-2"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	output := Render(graph, RenderOptions{
		TaskStatus: map[string]TaskInfo{
			"node-1": {Status: "succeeded", Duration: "2.3s"},
			"node-2": {Status: "running", Duration: "1.2s", SpinnerFrame: "⠋", ClaimedBy: "node-b"},
		},
	})

	if !strings.Contains(output, "✓") {
		t.Fatalf("expected succeeded icon in output: %q", output)
	}
	if !strings.Contains(output, "2.3s") {
		t.Fatalf("expected duration in output: %q", output)
	}
	if !strings.Contains(output, "⠋") {
		t.Fatalf("expected spinner frame in output: %q", output)
	}
	if !strings.Contains(output, "1.2s") {
		t.Fatalf("expected running duration in output: %q", output)
	}
	if !strings.Contains(output, "node: node-b") {
		t.Fatalf("expected claimed-by node in output: %q", output)
	}
}

func TestRenderFanOut(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "root", AtomID: "a1", Successors: []string{"left", "right"}},
			{ID: "left", AtomID: "a2"},
			{ID: "right", AtomID: "a3"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	output := Render(graph, RenderOptions{MaxWidth: 80})

	if !strings.Contains(output, "root") {
		t.Fatalf("missing root node: %q", output)
	}
	if !strings.Contains(output, "left") {
		t.Fatalf("missing left node: %q", output)
	}
	if !strings.Contains(output, "right") {
		t.Fatalf("missing right node: %q", output)
	}

	if strings.Contains(output, "├├") {
		t.Fatalf("unexpected duplicated tee junction in output: %q", output)
	}
	if !strings.Contains(output, "┴") || !strings.Contains(output, "┬") {
		t.Fatalf("expected top and bottom tee junctions in output: %q", output)
	}
}

func TestRenderBoxesShowEngineAndCommand(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "node-1", AtomID: "atom-1"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	output := Render(graph, RenderOptions{
		TaskStatus: map[string]TaskInfo{
			"node-1": {
				Status:  "running",
				Engine:  "docker",
				Command: []string{"sh", "-c", "echo hello world"},
			},
		},
	})

	if !strings.Contains(output, "🐳") {
		t.Fatalf("expected docker whale icon: %q", output)
	}
	if !strings.Contains(output, "echo hello world") {
		t.Fatalf("expected command summary in output: %q", output)
	}
}

func TestShortCommandTruncates(t *testing.T) {
	cmd := []string{"sh", "-c", "echo this is a very long command that should be truncated"}
	result := shortCommand(cmd, 24)
	if len([]rune(result)) > 24 {
		t.Fatalf("expected truncated command, got %q (len %d)", result, len([]rune(result)))
	}
	if !strings.HasSuffix(result, "…") {
		t.Fatalf("expected ellipsis suffix, got %q", result)
	}
}

func TestEngineIcons(t *testing.T) {
	cases := map[string]string{
		"docker":     "🐳",
		"kubernetes": "☸",
		"k8s":        "☸",
		"podman":     "🦭",
		"":           "⚙",
		"unknown":    "⚙",
	}
	for engine, want := range cases {
		got := engineIcon(engine)
		if got != want {
			t.Errorf("engineIcon(%q) = %q, want %q", engine, got, want)
		}
	}
}

func TestNodeCountAndAllNodes(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "a", Successors: []string{"b", "c"}},
			{ID: "b"},
			{ID: "c"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	if got := graph.NodeCount(); got != 3 {
		t.Fatalf("expected NodeCount 3, got %d", got)
	}

	all := graph.AllNodes()
	if len(all) != 3 {
		t.Fatalf("expected AllNodes to return 3 nodes, got %d", len(all))
	}
}

func TestRenderCentersGraphWithinMaxWidth(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "root", AtomID: "a1", Successors: []string{"left", "right"}},
			{ID: "left", AtomID: "a2"},
			{ID: "right", AtomID: "a3"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	base := Render(graph, RenderOptions{})
	baseLines := strings.Split(base, "\n")
	if len(baseLines) == 0 {
		t.Fatal("expected base layout lines")
	}

	graphWidth := 0
	for _, line := range baseLines {
		if w := lipgloss.Width(line); w > graphWidth {
			graphWidth = w
		}
	}
	maxWidth := graphWidth + 24
	padding := (maxWidth - graphWidth) / 2
	if padding <= 0 {
		t.Fatalf("expected positive centering padding, got %d", padding)
	}

	centered := Render(graph, RenderOptions{MaxWidth: maxWidth})
	centeredLines := strings.Split(centered, "\n")
	if len(centeredLines) != len(baseLines) {
		t.Fatalf("line count mismatch: base=%d centered=%d", len(baseLines), len(centeredLines))
	}

	prefix := strings.Repeat(" ", padding)
	for i := range baseLines {
		want := prefix + baseLines[i]
		if centeredLines[i] != want {
			t.Fatalf("line %d not centered as expected\nwant: %q\ngot:  %q", i, want, centeredLines[i])
		}
	}
}

func TestRenderDoesNotPadWhenGraphWiderThanMaxWidth(t *testing.T) {
	spec := &api.JobDAG{
		Nodes: []api.JobDAGNode{
			{ID: "root", AtomID: "a1", Successors: []string{"left", "right"}},
			{ID: "left", AtomID: "a2"},
			{ID: "right", AtomID: "a3"},
		},
	}

	graph, err := FromJobDAG(spec)
	if err != nil {
		t.Fatalf("FromJobDAG returned error: %v", err)
	}

	base := Render(graph, RenderOptions{})
	baseWidth := 0
	for _, line := range strings.Split(base, "\n") {
		if w := lipgloss.Width(line); w > baseWidth {
			baseWidth = w
		}
	}

	got := Render(graph, RenderOptions{MaxWidth: baseWidth - 1})
	if got != base {
		t.Fatalf("expected layout to remain unchanged when max width is smaller than graph width")
	}
}

func TestPrefixMultilineAppliesPaddingToAllLines(t *testing.T) {
	got := prefixMultiline("a\nb\nc", 3)
	want := "   a\n   b\n   c"
	if got != want {
		t.Fatalf("unexpected padded output\nwant: %q\ngot:  %q", want, got)
	}
}
