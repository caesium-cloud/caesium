package dagrender

import (
	"bytes"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/daganalysis"
)

func TestRenderSequential(t *testing.T) {
	a := &daganalysis.DAGAnalysis{
		Steps: []daganalysis.StepInfo{
			{Name: "a", Depth: 0},
			{Name: "b", Depth: 1},
			{Name: "c", Depth: 2},
		},
		ExecutionOrder: [][]string{{"a"}, {"b"}, {"c"}},
		MaxParallelism: 1,
	}

	var buf bytes.Buffer
	if err := Render(a, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "a") || !strings.Contains(out, "b") || !strings.Contains(out, "c") {
		t.Errorf("expected all step names in output, got:\n%s", out)
	}
	if !strings.Contains(out, "-->") {
		t.Errorf("expected arrows in output, got:\n%s", out)
	}
}

func TestRenderParallel(t *testing.T) {
	a := &daganalysis.DAGAnalysis{
		Steps: []daganalysis.StepInfo{
			{Name: "start", Depth: 0},
			{Name: "branch-a", Depth: 1},
			{Name: "branch-b", Depth: 1},
			{Name: "join", Depth: 2},
		},
		ExecutionOrder: [][]string{{"start"}, {"branch-a", "branch-b"}, {"join"}},
		MaxParallelism: 2,
	}

	var buf bytes.Buffer
	if err := Render(a, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "branch-a") || !strings.Contains(out, "branch-b") {
		t.Errorf("expected parallel branches in output, got:\n%s", out)
	}
}

func TestRenderEmpty(t *testing.T) {
	a := &daganalysis.DAGAnalysis{}
	var buf bytes.Buffer
	if err := Render(a, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "empty") {
		t.Errorf("expected empty message, got:\n%s", buf.String())
	}
}
