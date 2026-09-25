//go:build integration

package robustness

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

type raceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f raceRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestRaceGateRequiresBothRequestsBeforeForwarding(t *testing.T) {
	gate := newRequestRaceGate()
	defer gate.releaseBoth()
	var forwarded atomic.Int32
	next := raceRoundTripFunc(func(*http.Request) (*http.Response, error) {
		forwarded.Add(1)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	results := make(chan error, 2)
	for _, name := range []string{"replace", "complete"} {
		go func(name string) {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://race.test/"+name, nil)
			if err == nil {
				var resp *http.Response
				resp, err = gate.transport(name, next).RoundTrip(req)
				if resp != nil {
					_ = resp.Body.Close()
				}
			}
			results <- err
		}(name)
	}
	arrivals, err := gate.awaitBoth(ctx)
	if err != nil || len(arrivals) != 2 || forwarded.Load() != 0 {
		t.Fatalf("requests forwarded before both were ready: arrivals=%+v err=%v forwards=%d", arrivals, err, forwarded.Load())
	}
	released := gate.releaseBoth()
	for _, arrival := range arrivals {
		if !arrival.At.Before(released) {
			t.Fatalf("contender %s did not arrive before release", arrival.Name)
		}
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatalf("forwarded request: %v", err)
		}
	}
	if forwarded.Load() != 2 {
		t.Fatalf("forwarded %d, want 2", forwarded.Load())
	}
}

func TestRequestRaceGateCannotCertifyOneMissingContender(t *testing.T) {
	gate := newRequestRaceGate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if arrivals, err := gate.awaitBoth(ctx); err == nil || len(arrivals) != 0 {
		t.Fatalf("missing contenders were certified: arrivals=%+v err=%v", arrivals, err)
	}
}

func TestCancelledRaceTaskSetRejectsExtraAndMismatchedRows(t *testing.T) {
	fixture := func() (cluster.Run, []cluster.TaskRecipe, map[string]string, cluster.Task, cluster.TaskRecipe) {
		block := cluster.Task{ID: "block-catalog", TaskID: "block-catalog", Status: "cancelled", Attempt: 1, Image: "task:v1"}
		successor := cluster.Task{ID: "successor-catalog", TaskID: "successor-catalog", Status: "cancelled", Attempt: 0, Image: "task:v1"}
		public := cluster.Run{ID: "old-run", Tasks: []cluster.Task{block, successor}}
		durable := []cluster.TaskRecipe{
			{ID: "block-instance", TaskID: block.TaskID, Status: block.Status, Attempt: block.Attempt, Image: block.Image},
			{ID: "successor-instance", TaskID: successor.TaskID, Status: successor.Status, Attempt: successor.Attempt, Image: successor.Image},
		}
		names := map[string]string{block.TaskID: cluster.BlockStep, successor.TaskID: cluster.SuccessorStep}
		return public, durable, names, block, durable[0]
	}
	public, durable, names, block, preparedBlock := fixture()
	if err := checkCancelledRaceTaskSet(public, durable, names, public.ID, block, preparedBlock); err != nil {
		t.Fatalf("legal two-step cancellation rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*cluster.Run, *[]cluster.TaskRecipe, map[string]string)
	}{
		{"extra running block without start event", func(run *cluster.Run, recipes *[]cluster.TaskRecipe, _ map[string]string) {
			run.Tasks = append(run.Tasks, cluster.Task{ID: "extra-block", TaskID: "block-catalog", Status: "running", Image: "task:v1"})
			*recipes = append(*recipes, cluster.TaskRecipe{ID: "extra-block", TaskID: "block-catalog", Status: "running", Image: "task:v1"})
		}},
		{"extra unknown queued row without start event", func(run *cluster.Run, recipes *[]cluster.TaskRecipe, _ map[string]string) {
			run.Tasks = append(run.Tasks, cluster.Task{ID: "unknown-run", TaskID: "unknown-catalog", Status: "pending", Image: "task:v1"})
			*recipes = append(*recipes, cluster.TaskRecipe{ID: "unknown-run", TaskID: "unknown-catalog", Status: "pending", Image: "task:v1"})
		}},
		{"unknown durable row replaces block", func(_ *cluster.Run, recipes *[]cluster.TaskRecipe, _ map[string]string) {
			(*recipes)[0] = cluster.TaskRecipe{ID: "unknown-run", TaskID: "unknown-catalog", Status: "cancelled", Image: "task:v1"}
		}},
		{"public row mismatches durable identity", func(run *cluster.Run, _ *[]cluster.TaskRecipe, _ map[string]string) {
			run.Tasks[0].TaskID = "successor-catalog"
		}},
		{"public row mismatches durable attempt", func(run *cluster.Run, _ *[]cluster.TaskRecipe, _ map[string]string) {
			run.Tasks[0].Attempt++
		}},
		{"duplicate durable block instance", func(_ *cluster.Run, recipes *[]cluster.TaskRecipe, _ map[string]string) {
			*recipes = append(*recipes, cluster.TaskRecipe{ID: "second-block-instance", TaskID: "block-catalog", Status: "cancelled", Image: "task:v1"})
		}},
		{"public ID has unrelated identity", func(run *cluster.Run, _ *[]cluster.TaskRecipe, _ map[string]string) {
			run.Tasks[0].ID = "unknown-instance"
		}},
		{"durable successor remains claimed", func(_ *cluster.Run, recipes *[]cluster.TaskRecipe, _ map[string]string) {
			(*recipes)[1].ClaimedBy = "worker-a"
		}},
		{"catalog has extra step", func(_ *cluster.Run, _ *[]cluster.TaskRecipe, names map[string]string) {
			names["unknown-catalog"] = "unknown"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run, recipes, names, block, preparedBlock := fixture()
			tc.mutate(&run, &recipes, names)
			if err := checkCancelledRaceTaskSet(run, recipes, names, run.ID, block, preparedBlock); err == nil {
				t.Fatalf("invalid cancellation task set passed: public=%+v durable=%+v catalog=%v", run.Tasks, recipes, names)
			}
		})
	}
}
