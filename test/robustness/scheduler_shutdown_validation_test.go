//go:build integration

package robustness

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

type shutdownResultFixture struct {
	before              cluster.TaskRecipe
	final               cluster.Run
	recipes             []cluster.TaskRecipe
	starts, completions []recorder.Event
}

func newShutdownResultFixture(executions int) shutdownResultFixture {
	f := shutdownResultFixture{
		before: cluster.TaskRecipe{ID: "task-run", TaskID: "task", Status: "running", Image: "image", Command: "command", Attempt: 1, ClaimAttempt: 1, OwnerGeneration: 1},
		final:  cluster.Run{ID: "run", JobID: "job", Status: "succeeded", Tasks: []cluster.Task{{ID: "task", TaskID: "task", Status: "succeeded", Attempt: 1}}},
	}
	f.recipes = []cluster.TaskRecipe{f.before}
	f.recipes[0].Status = "succeeded"
	f.recipes[0].ClaimAttempt = executions
	f.recipes[0].OwnerGeneration = int64(executions)
	for i := range executions {
		start := recorder.Event{Kind: "start", RunID: "run", Step: cluster.BlockStep, Nonce: string(rune('a' + i)), At: time.Unix(int64(i*2+1), 0)}
		complete := start
		complete.Kind, complete.At = "complete", start.At.Add(time.Second)
		f.starts = append(f.starts, start)
		f.completions = append(f.completions, complete)
	}
	return f
}

func (f shutdownResultFixture) validate() error {
	return validateGracefulShutdownResult("run", "job", f.before, f.final, f.recipes, f.starts, f.completions)
}

func TestGracefulShutdownResultAcceptsSingleExecutionAndFencedRecovery(t *testing.T) {
	for _, test := range []struct {
		name       string
		executions int
		generation int64
	}{
		{"single", 1, 1},
		{"same owner reclaim", 2, 1},
		{"same owner repeated reclaims", 3, 1},
		{"takeover", 2, 2},
		{"repeated takeovers", 3, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newShutdownResultFixture(test.executions)
			f.recipes[0].OwnerGeneration = test.generation
			if err := f.validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("compact catalog identity differs from durable row", func(t *testing.T) {
		f := newShutdownResultFixture(2)
		f.recipes[0].OwnerGeneration = 1
		// This is the native compact API shape: both public identifiers name
		// the catalog task, while the recipe ID remains the durable task-run.
		const response = `{"id":"run","job_id":"job","status":"succeeded","tasks":[{"id":"task","task_id":"task","status":"succeeded","attempt":1}]}`
		if err := json.Unmarshal([]byte(response), &f.final); err != nil {
			t.Fatal(err)
		}
		if err := f.validate(); err != nil {
			t.Fatal(err)
		}
	})
	// Advancing ownership does not itself require a duplicate effect.
	f := newShutdownResultFixture(1)
	f.recipes[0].ClaimAttempt, f.recipes[0].OwnerGeneration = 2, 2
	if err := f.validate(); err != nil {
		t.Fatal(err)
	}
}

func TestGracefulShutdownResultRefusesIdentityStatusAndFenceDrift(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*shutdownResultFixture)
	}{
		{"run", func(f *shutdownResultFixture) { f.final.ID = "foreign" }},
		{"job", func(f *shutdownResultFixture) { f.final.JobID = "foreign" }},
		{"failed run", func(f *shutdownResultFixture) { f.final.Status = "failed" }},
		{"extra public task", func(f *shutdownResultFixture) { f.final.Tasks = append(f.final.Tasks, f.final.Tasks[0]) }},
		{"extra recipe", func(f *shutdownResultFixture) { f.recipes = append(f.recipes, f.recipes[0]) }},
		{"public catalog id", func(f *shutdownResultFixture) { f.final.Tasks[0].ID = "foreign" }},
		{"public id replaced with durable row", func(f *shutdownResultFixture) { f.final.Tasks[0].ID = f.before.ID }},
		{"public task", func(f *shutdownResultFixture) { f.final.Tasks[0].TaskID = "foreign" }},
		{"public retry", func(f *shutdownResultFixture) { f.final.Tasks[0].Attempt++ }},
		{"public task failed", func(f *shutdownResultFixture) { f.final.Tasks[0].Status = "failed" }},
		{"recipe task run", func(f *shutdownResultFixture) { f.recipes[0].ID = "foreign" }},
		{"recipe task", func(f *shutdownResultFixture) { f.recipes[0].TaskID = "foreign" }},
		{"recipe retry", func(f *shutdownResultFixture) { f.recipes[0].Attempt++ }},
		{"recipe cancelled", func(f *shutdownResultFixture) { f.recipes[0].Status = "cancelled" }},
		{"image", func(f *shutdownResultFixture) { f.recipes[0].Image = "foreign" }},
		{"command", func(f *shutdownResultFixture) { f.recipes[0].Command = "foreign" }},
		{"missing original claim", func(f *shutdownResultFixture) { f.before.ClaimAttempt = 0 }},
		{"missing original generation", func(f *shutdownResultFixture) { f.before.OwnerGeneration = 0 }},
		{"claim regressed", func(f *shutdownResultFixture) { f.recipes[0].ClaimAttempt = 0 }},
		{"generation regressed", func(f *shutdownResultFixture) { f.recipes[0].OwnerGeneration = 0 }},
		{"unfenced duplicate", func(f *shutdownResultFixture) { f.recipes[0].ClaimAttempt, f.recipes[0].OwnerGeneration = 1, 1 }},
		{"generation only", func(f *shutdownResultFixture) { f.recipes[0].ClaimAttempt = 1 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newShutdownResultFixture(2)
			test.mutate(&f)
			if err := f.validate(); err == nil {
				t.Fatal("invalid shutdown evidence accepted")
			}
		})
	}
	f := newShutdownResultFixture(3)
	f.recipes[0].ClaimAttempt, f.recipes[0].OwnerGeneration = 2, 2
	if err := f.validate(); err == nil {
		t.Fatal("more executions than observed claims accepted")
	}
}

func TestGracefulShutdownResultRequiresUniquePairedBoundEffects(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*shutdownResultFixture)
	}{
		{"no starts", func(f *shutdownResultFixture) { f.starts, f.completions = nil, nil }},
		{"missing completion", func(f *shutdownResultFixture) { f.completions = f.completions[:1] }},
		{"blank nonce", func(f *shutdownResultFixture) { f.starts[0].Nonce = " " }},
		{"duplicate start", func(f *shutdownResultFixture) { f.starts[1] = f.starts[0] }},
		{"duplicate completion", func(f *shutdownResultFixture) { f.completions[1] = f.completions[0] }},
		{"unmatched nonce", func(f *shutdownResultFixture) { f.completions[0].Nonce = "foreign" }},
		{"foreign start run", func(f *shutdownResultFixture) { f.starts[0].RunID = "foreign" }},
		{"foreign completion run", func(f *shutdownResultFixture) { f.completions[0].RunID = "foreign" }},
		{"foreign start step", func(f *shutdownResultFixture) { f.starts[0].Step = "foreign" }},
		{"foreign completion step", func(f *shutdownResultFixture) { f.completions[0].Step = "foreign" }},
		{"wrong start kind", func(f *shutdownResultFixture) { f.starts[0].Kind = "complete" }},
		{"wrong completion kind", func(f *shutdownResultFixture) { f.completions[0].Kind = "start" }},
		{"missing start clock", func(f *shutdownResultFixture) { f.starts[0].At = time.Time{} }},
		{"premature completion", func(f *shutdownResultFixture) { f.completions[0].At = f.starts[0].At.Add(-time.Second) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newShutdownResultFixture(2)
			test.mutate(&f)
			if err := f.validate(); err == nil {
				t.Fatal("invalid recorder evidence accepted")
			}
		})
	}
}
