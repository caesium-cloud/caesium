//go:build integration

package robustness

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
)

// requestRaceGate holds both client RoundTrips before either reaches a server.
// The two invocation intervals therefore overlap. It does not claim the two
// server transactions execute concurrently; durable event order decides that.
type requestRaceGate struct {
	ready   chan raceArrival
	release chan struct{}
	once    sync.Once
}

type raceArrival struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
}

func newRequestRaceGate() *requestRaceGate {
	return &requestRaceGate{ready: make(chan raceArrival, 2), release: make(chan struct{})}
}

type gatedRoundTripper struct {
	gate *requestRaceGate
	name string
	next http.RoundTripper
}

func (g *requestRaceGate) transport(name string, next http.RoundTripper) http.RoundTripper {
	if next == nil {
		next = http.DefaultTransport
	}
	return gatedRoundTripper{gate: g, name: name, next: next}
}

func (g gatedRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	select {
	case g.gate.ready <- raceArrival{Name: g.name, At: time.Now().UTC()}:
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
	select {
	case <-g.gate.release:
		return g.next.RoundTrip(req)
	case <-req.Context().Done():
		return nil, req.Context().Err()
	}
}

func (g *requestRaceGate) awaitBoth(ctx context.Context) ([]raceArrival, error) {
	arrivals := make([]raceArrival, 0, 2)
	seen := map[string]bool{}
	for len(arrivals) < 2 {
		select {
		case arrival := <-g.ready:
			if arrival.Name != "replace" && arrival.Name != "complete" {
				return nil, fmt.Errorf("unexpected race contender %q", arrival.Name)
			}
			if seen[arrival.Name] {
				return nil, fmt.Errorf("duplicate race contender %q", arrival.Name)
			}
			seen[arrival.Name] = true
			arrivals = append(arrivals, arrival)
		case <-ctx.Done():
			return nil, fmt.Errorf("both race contenders did not reach transport gate: %w", ctx.Err())
		}
	}
	return arrivals, nil
}

func (g *requestRaceGate) releaseBoth() time.Time {
	at := time.Now().UTC()
	g.once.Do(func() { close(g.release) })
	return at
}

// checkCancelledRaceTaskSet proves the two-step fixture has exactly one old
// block and one successor row, both cancelled and unclaimed, with every public
// row matched to its durable identity and attempt. A valid successor cannot
// mask an extra queued/running block or unknown task row.
func checkCancelledRaceTaskSet(public cluster.Run, recipes []cluster.TaskRecipe,
	names map[string]string, runID string, block cluster.Task, preparedBlock cluster.TaskRecipe) error {
	if public.ID != runID || len(names) != 2 || len(public.Tasks) != 2 || len(recipes) != 2 {
		return fmt.Errorf("unexpected cancellation fixture cardinality: run=%s want=%s catalog=%d public=%d durable=%d",
			public.ID, runID, len(names), len(public.Tasks), len(recipes))
	}
	seenCatalog := map[string]bool{}
	for taskID, step := range names {
		if taskID == "" || (step != cluster.BlockStep && step != cluster.SuccessorStep) || seenCatalog[step] {
			return fmt.Errorf("unexpected or duplicate catalog step %q for task %q", step, taskID)
		}
		seenCatalog[step] = true
	}
	if !seenCatalog[cluster.BlockStep] || !seenCatalog[cluster.SuccessorStep] ||
		block.ID == "" || block.TaskID == "" || preparedBlock.ID == "" ||
		block.TaskID != preparedBlock.TaskID || names[block.TaskID] != cluster.BlockStep {
		return fmt.Errorf("cancelled fixture lacks the prepared block and successor: block=%+v catalog=%v", block, names)
	}
	durableByID := make(map[string]cluster.TaskRecipe, 2)
	seenDurableStep := map[string]bool{}
	for _, recipe := range recipes {
		step, known := names[recipe.TaskID]
		if recipe.ID == "" || !known || seenDurableStep[step] || durableByID[recipe.ID].ID != "" {
			return fmt.Errorf("extra, unknown, or duplicate durable task: %+v", recipe)
		}
		if step == cluster.BlockStep && recipe.ID != preparedBlock.ID {
			return fmt.Errorf("durable block changed identity: prepared=%+v durable=%+v", preparedBlock, recipe)
		}
		if !strings.EqualFold(recipe.Status, "cancelled") || strings.TrimSpace(recipe.ClaimedBy) != "" {
			return fmt.Errorf("durable %s task is not cancelled and unclaimed: %+v", step, recipe)
		}
		seenDurableStep[step] = true
		durableByID[recipe.ID] = recipe
	}
	if !seenDurableStep[cluster.BlockStep] || !seenDurableStep[cluster.SuccessorStep] {
		return fmt.Errorf("durable cancellation task set is missing a fixture step: %v", seenDurableStep)
	}
	seenPublicID := map[string]bool{}
	for _, task := range public.Tasks {
		recipe, err := cluster.ResolveUnfannedTaskRecipe(task, recipes)
		if err != nil || durableByID[recipe.ID].ID == "" || seenPublicID[recipe.ID] {
			return fmt.Errorf("public task does not match one durable row: public=%+v durable=%+v err=%v", task, recipe, err)
		}
		seenPublicID[recipe.ID] = true
	}
	if len(seenPublicID) != len(durableByID) {
		return fmt.Errorf("public cancellation task set omitted durable rows: public=%d durable=%d", len(seenPublicID), len(durableByID))
	}
	return nil
}
