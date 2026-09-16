package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Drive the inbound worker queue, pool, renewal ticker, and real completion
// HTTP client together. A finished result remains claim-tracked during recovery.
func TestWorkerCompletionRecoveryRetainsClaimUntilBoundOrLoss(t *testing.T) {
	for _, loseClaim := range []bool{false, true} {
		t.Run(fmt.Sprint(loseClaim), func(t *testing.T) {
			requests := make(chan dispatch.CompleteRequest, 8)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req dispatch.CompleteRequest
				if r.URL.Path != "/internal/complete" || json.NewDecoder(r.Body).Decode(&req) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- req
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"code":"owner_not_ready"}`))
			}))
			defer server.Close()
			leaseTTL := time.Second
			renewer := &retentionClaimRenewer{}
			result := make(chan error, 1)
			pool := NewPool(1)
			w := NewWorker(&sequenceClaimer{}, pool, time.Millisecond, func(ctx context.Context, task *models.TaskRun) {
				meta, ok := dispatchMetaFrom(ctx)
				if !ok {
					result <- errors.New("missing dispatch metadata")
					return
				}
				result <- newOwnerSink(meta, nil).Succeeded(ctx, task, "success", map[string]string{"result": "kept"}, nil)
			}).WithLeaseRenewal(renewer, leaseTTL, 5*time.Millisecond).WithInboundDispatch("test-token")
			task := makeTask("worker-a", time.Now().Add(20*time.Millisecond))
			require.NoError(t, w.SubmitDispatched(dispatch.InboundDispatch{Task: task, OwnerBaseURL: server.URL, WorkerNode: task.ClaimedBy, OwnerGeneration: 7, Attempt: 3}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			workerDone := make(chan error, 1)
			go func() { workerDone <- w.Run(ctx) }()
			select {
			case req := <-requests:
				require.Equal(t, task.ID, req.TaskRunID)
				require.Equal(t, int64(7), req.OwnerGeneration)
				require.Equal(t, 3, req.Attempt)
				require.Equal(t, "success", req.Result)
				require.Equal(t, "kept", req.Outputs["result"])
			case <-time.After(3 * time.Second):
				t.Fatal("worker never reported completion")
			}
			require.Eventually(t, func() bool { return renewer.callCount() > 0 }, 3*time.Second, 5*time.Millisecond, "completion retention must keep renewing the claim")
			if loseClaim {
				renewer.lost.Store(true)
			}
			select {
			case err := <-result:
				if loseClaim {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.ErrorIs(t, err, errOwnerCompletionRetentionExpired)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("recovery did not release the worker")
			}
			require.NoError(t, ctx.Err(), "worker remains usable after this completion stops")
			pool.Wait()
			require.True(t, pool.TryAcquire(), "completion must release its slot")
			pool.Release()
			w.inFlightMu.Lock()
			remaining := len(w.inFlight)
			w.inFlightMu.Unlock()
			require.Zero(t, remaining)
			cancel()
			require.NoError(t, <-workerDone)
		})
	}
}

type retentionClaimRenewer struct {
	fakeLeaseRenewer
	lost atomic.Bool
}

func (r *retentionClaimRenewer) ClaimedTaskRunIDs(_ context.Context, _ string, ids []uuid.UUID) ([]uuid.UUID, error) {
	if r.lost.Load() {
		return nil, nil
	}
	return ids, nil
}

func TestRuntimeExecutorCompletionRetentionDoesNotChangeFinishedOutcome(t *testing.T) {
	for _, mixedBusy := range []bool{false, true} {
		for _, route := range []string{"succeeded", "cached", "failed"} {
			t.Run(fmt.Sprintf("%s/mixed_busy=%t", route, mixedBusy), func(t *testing.T) {
				f := seedProducerTaskRun(t, "completion-retention-"+route)
				task := f.taskRun
				if route == "cached" {
					cold := &partitionEmittingEngine{logs: producerMarkerLog}
					(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return cold, nil }}).Execute(t.Context(), task)
					task = f.newProducerRunAttempt(t)
				}
				task.MaxAttempts = 3
				logs := "done\n"
				if route == "failed" {
					task.MaxAttempts = 1
					logs = "##caesium::partitions not-json\n"
				}
				engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: logs, waitResult: atom.Success}}
				var statuses []string
				executor := &runtimeExecutor{store: f.store, localSink: &fakeSink{}, engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(_ context.Context, _, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
					statuses = append(statuses, req.Status)
					if mixedBusy && len(statuses) != 2 {
						return nil, dispatch.ErrOwnerBusy
					}
					return nil, dispatch.ErrOwnerNotReady
				}}
				meta := ownerMeta()
				meta.CompletionTimeout = 20 * time.Millisecond
				if mixedBusy {
					meta.CompletionTimeout = 15 * time.Second
				}
				executor.Execute(withDispatchMeta(t.Context(), meta), task)
				expectedPosts := 1
				if mixedBusy {
					expectedPosts = len(ownerBusyBackoffs) + 2
				}
				require.Len(t, statuses, expectedPosts)
				for _, status := range statuses {
					require.Equal(t, route, status, "delivery exhaustion cannot change a retained outcome")
				}
				expectedCreates := 1
				if route == "cached" {
					expectedCreates = 0
				}
				require.Equal(t, expectedCreates, engine.creates)
				var row models.TaskRun
				require.NoError(t, f.db.First(&row, "id = ?", task.ID).Error)
				require.Equal(t, "running", row.Status)
				var runRow models.JobRun
				require.NoError(t, f.db.First(&runRow, "id = ?", task.JobRunID).Error)
				require.Equal(t, "running", runRow.Status, "retention expiry cannot trigger ordinary failure cascades")
			})
		}
	}

}

func TestClaimerLeaseGuardBindsItsOwnCutoff(t *testing.T) {
	// Existing real-SQL tests exercise both coordination modes. This matrix
	// additionally checks every dialect's generated guard and argument contract.
	f := seedProducerTaskRun(t, "lease-guard-bindings")
	c := NewClaimer("worker", f.store, time.Minute)
	now := time.Now().UTC()
	for _, dialect := range []string{"sqlite", "dqlite", "postgres"} {
		guard, args, err := c.liveLeaseGuardSQL(dialect, "tr", now)
		require.NoError(t, err)
		require.Equal(t, strings.Count(guard, "?"), len(args))
		if !f.store.OwnerMemoryAdvancementEnabled() {
			require.Equal(t, []any{now}, args)
		} else {
			require.Empty(t, args)
		}
	}
	_, _, err := c.liveLeaseGuardSQL("unsupported", "tr", now)
	require.Error(t, err)
}
