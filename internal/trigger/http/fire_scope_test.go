package http

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestFireWithScopeRunsOnlyAllowedJobsThroughTheGuard (A6): a confined fire
// skips jobs outside its scope and hands every other job's admission the
// caller's start guard; the unguarded path is never used for it.
func TestFireWithScopeRunsOnlyAllowedJobsThroughTheGuard(t *testing.T) {
	t.Parallel()
	inScope := &models.Job{ID: uuid.New(), Alias: "mkt", Namespace: "marketing"}
	outOfScope := &models.Job{ID: uuid.New(), Alias: "fin", Namespace: "finance"}
	refused := errors.New("refused")

	var (
		mu      sync.Mutex
		guarded []uuid.UUID
		results []error
	)
	done := make(chan struct{}, 2)
	h := &HTTP{
		id: uuid.New(),
		listJobs: func(context.Context, string) (models.Jobs, error) {
			return models.Jobs{inScope, outOfScope}, nil
		},
		runJob: func(context.Context, *models.Job, map[string]string, string) error {
			t.Error("a confined fire must not take the unguarded path")
			return nil
		},
		runGuardedJob: func(_ context.Context, j *models.Job, _ map[string]string, priority string, guard func(string) error) error {
			mu.Lock()
			guarded = append(guarded, j.ID)
			// Admission calls the guard with the namespace it freezes.
			results = append(results, guard("marketing"), guard("finance"))
			mu.Unlock()
			require.Equal(t, "high", priority)
			done <- struct{}{}
			return nil
		},
	}
	err := h.FireWithParams(context.Background(), nil,
		WithPriority("high"),
		WithFireScope(func(_, namespace string) bool { return namespace == "marketing" }),
		WithFireStartGuard(func(j *models.Job, namespace string) error {
			require.Equal(t, inScope.ID, j.ID)
			if namespace != "marketing" {
				return refused
			}
			return nil
		}))
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the guarded run")
	}
	select {
	case <-done:
		t.Fatal("the out-of-scope job ran")
	case <-time.After(200 * time.Millisecond):
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []uuid.UUID{inScope.ID}, guarded)
	require.Equal(t, []error{nil, refused}, results)
}
