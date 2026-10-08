package run

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runlife"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func supervisedRequestContext(t *testing.T) context.Context {
	t.Helper()
	s := runlife.New(context.Background())
	t.Cleanup(func() {
		s.CloseAndCancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, s.Wait(ctx))
	})
	return runlife.WithSupervisor(t.Context(), s)
}

func TestSupervisedKickoffsKeepRequestValuesAndRunCancellation(t *testing.T) {
	oldExecute, oldGet := runExecution, postGetRun
	postGetRun = func(id uuid.UUID) (*runstorage.JobRun, error) {
		return &runstorage.JobRun{ID: id, Status: runstorage.StatusRunning}, nil
	}
	t.Cleanup(func() { runExecution, postGetRun = oldExecute, oldGet })
	for _, kind := range []string{"manual", "whole retry", "partition retry"} {
		t.Run(kind, func(t *testing.T) {
			type key struct{}
			request, cancelRequest := context.WithCancel(context.WithValue(context.Background(), key{}, "value"))
			owner := runlife.New(context.Background())
			child, release, err := owner.Reserve(request)
			require.NoError(t, err)
			started := make(chan context.Context, 1)
			finish := make(chan struct{})
			runExecution = func(ctx context.Context, _ *models.Job, _ map[string]string) error {
				started <- ctx
				<-finish
				return nil
			}
			j := &models.Job{ID: uuid.New()}
			runID := uuid.New()
			r := &runstorage.JobRun{ID: runID}
			switch kind {
			case "manual":
				launchRun(child, j, r, release)
			case "whole retry":
				// Retry registers cancellation before its durable retry mutation.
				registered, unregister := job.RegisterRunCancel(child, runID)
				launchWholeRunRetry(registered, j, r, func() { unregister(); release() })
			default:
				kickoffPartitionRetryRun(child, j, runID, nil, release)
			}
			running := <-started
			cancelRequest()
			require.NoError(t, running.Err())
			require.Equal(t, "value", running.Value(key{}))
			id, ok := runstorage.FromContext(running)
			require.True(t, ok)
			require.Equal(t, runID, id)
			require.Equal(t, 1, job.CancelRunContexts(runID))
			require.ErrorIs(t, running.Err(), context.Canceled)
			owner.CloseAndCancel()
			expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			defer cancel()
			require.ErrorIs(t, owner.Wait(expired), context.DeadlineExceeded)
			close(finish)
			require.NoError(t, owner.Wait(context.Background()))
			require.Zero(t, job.CancelRunContexts(runID))
		})
	}
}
