package dagrender

import (
	"bytes"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/dag"
)

func TestRenderSequential(t *testing.T) {
	a := &dag.Analysis{
		Steps: []dag.StepInfo{
			{Name: "a", Successors: []string{"b"}},
			{Name: "b", Successors: []string{"c"}},
			{Name: "c"},
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
	if !strings.Contains(out, ">") {
		t.Errorf("expected arrowheads in output, got:\n%s", out)
	}
}

func TestRenderParallel(t *testing.T) {
	a := &dag.Analysis{
		Steps: []dag.StepInfo{
			{Name: "start", Successors: []string{"branch-a", "branch-b"}},
			{Name: "branch-a", Successors: []string{"join"}},
			{Name: "branch-b", Successors: []string{"join"}},
			{Name: "join"},
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
	// Should have vertical connectors for fan-out/fan-in.
	if !strings.Contains(out, "┬") && !strings.Contains(out, "├") {
		t.Errorf("expected fan-out junction characters, got:\n%s", out)
	}
	// Both branches should have arrowheads pointing to them.
	lines := strings.Split(out, "\n")
	arrowCount := 0
	for _, line := range lines {
		if strings.Contains(line, ">") {
			arrowCount++
		}
	}
	if arrowCount < 2 {
		t.Errorf("expected arrows to both branches, got %d arrow lines:\n%s", arrowCount, out)
	}
}

func TestRenderFanOutOnly(t *testing.T) {
	a := &dag.Analysis{
		Steps: []dag.StepInfo{
			{Name: "start", Successors: []string{"a", "b", "c"}},
			{Name: "a"},
			{Name: "b"},
			{Name: "c"},
		},
		ExecutionOrder: [][]string{{"start"}, {"a", "b", "c"}},
		MaxParallelism: 3,
	}

	var buf bytes.Buffer
	if err := Render(a, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "start") {
		t.Errorf("expected 'start' in output, got:\n%s", out)
	}
	// All three targets should have arrowheads.
	lines := strings.Split(out, "\n")
	arrowCount := 0
	for _, line := range lines {
		if strings.Contains(line, ">") {
			arrowCount++
		}
	}
	if arrowCount < 3 {
		t.Errorf("expected arrows to all 3 targets, got %d arrow lines:\n%s", arrowCount, out)
	}
}

func TestRenderEmpty(t *testing.T) {
	a := &dag.Analysis{}
	var buf bytes.Buffer
	if err := Render(a, &buf); err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.Contains(buf.String(), "empty") {
		t.Errorf("expected empty message, got:\n%s", buf.String())
	}
}
