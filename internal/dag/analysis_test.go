package dag

import (
	"testing"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

func TestAnalyzeSequential(t *testing.T) {
	def := &jobdef.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   jobdef.Metadata{Alias: "seq"},
		Trigger:    jobdef.Trigger{Type: "cron", Configuration: map[string]any{"cron": "* * * * *"}},
		Steps: []jobdef.Step{
			{Name: "a", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine"},
			{Name: "b", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine"},
			{Name: "c", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine"},
		},
	}

	a, err := Analyze(def)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if a.MaxParallelism != 1 {
		t.Errorf("MaxParallelism = %d, want 1", a.MaxParallelism)
	}
	if len(a.ExecutionOrder) != 3 {
		t.Errorf("ExecutionOrder layers = %d, want 3", len(a.ExecutionOrder))
	}
	if len(a.RootSteps) != 1 || a.RootSteps[0] != "a" {
		t.Errorf("RootSteps = %v, want [a]", a.RootSteps)
	}
	if len(a.LeafSteps) != 1 || a.LeafSteps[0] != "c" {
		t.Errorf("LeafSteps = %v, want [c]", a.LeafSteps)
	}
}

func TestAnalyzeFanOut(t *testing.T) {
	def := &jobdef.Definition{
		APIVersion: "v1",
		Kind:       "Job",
		Metadata:   jobdef.Metadata{Alias: "fan"},
		Trigger:    jobdef.Trigger{Type: "cron", Configuration: map[string]any{"cron": "* * * * *"}},
		Steps: []jobdef.Step{
			{Name: "start", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine", Next: []string{"a", "b"}},
			{Name: "a", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine", DependsOn: []string{"start"}, Next: []string{"join"}},
			{Name: "b", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine", DependsOn: []string{"start"}, Next: []string{"join"}},
			{Name: "join", Type: jobdef.StepTypeTask, Engine: "docker", Image: "alpine", DependsOn: []string{"a", "b"}},
		},
	}

	a, err := Analyze(def)
	if err != nil {
		t.Fatalf("Analyze: %v", err)
	}

	if a.MaxParallelism != 2 {
		t.Errorf("MaxParallelism = %d, want 2", a.MaxParallelism)
	}
	if len(a.ExecutionOrder) != 3 {
		t.Errorf("ExecutionOrder layers = %d, want 3", len(a.ExecutionOrder))
	}
}

func TestUniqueImages(t *testing.T) {
	def := &jobdef.Definition{
		Steps: []jobdef.Step{
			{Image: "alpine:3.20"},
			{Image: "python:3.12"},
			{Image: "alpine:3.20"},
		},
	}

	images := UniqueImages(def)
	if len(images) != 2 {
		t.Errorf("UniqueImages = %v, want 2 items", images)
	}
	if images[0] != "alpine:3.20" || images[1] != "python:3.12" {
		t.Errorf("UniqueImages = %v, want [alpine:3.20 python:3.12]", images)
	}
}
