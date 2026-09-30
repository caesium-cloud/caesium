package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/dispatch"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestRuntimeExecutorRejectedFanOutTerminalizesWithoutReplay(t *testing.T) {
	for _, test := range []struct{ name, logs, detail string }{
		{"cycle", `##caesium::partitions [{"key":"a","dependsOn":["b"]},{"key":"b","dependsOn":["a"]}]` + "\n", "cycle"},
		{"empty", "no partitions\n", "no partitions"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := seedProducerWithConsumer(t, "completion-rejected-"+test.name)
			f.taskRun.MaxAttempts = 3
			require.NoError(t, f.db.Model(f.taskRun).Update("max_attempts", 3).Error)
			require.NoError(t, f.db.Model(f.consumer).Update("fan_out_config", datatypes.JSON([]byte(`{"from":"list","onEmpty":"fail"}`))).Error)
			ls := run.NewLeaseStore(f.db)
			lease, err := ls.AcquireLease(t.Context(), f.jobRun.ID, "owner", time.Hour)
			require.NoError(t, err)
			handler := dispatch.NewHandler(f.store, ls, "owner", "token")
			var statuses []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req dispatch.CompleteRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
				statuses = append(statuses, req.Status)
				// PostComplete sends a fresh body; record it without replacing real handler behavior.
				r.Body = http.NoBody
				body, err := json.Marshal(req)
				require.NoError(t, err)
				r.Body = io.NopCloser(bytes.NewReader(body))
				handler.HandleComplete(w, r)
			}))
			defer server.Close()
			engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: test.logs, waitResult: atom.Success}}
			executor := &runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(ctx context.Context, _ string, token string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
				return dispatch.PostComplete(ctx, server.URL, token, req)
			}}
			meta := ownerMeta()
			meta.OwnerGeneration = lease
			meta.Token = "token"
			meta.WorkerNode = f.taskRun.ClaimedBy
			meta.Attempt = 1
			executor.Execute(withDispatchMeta(t.Context(), meta), f.taskRun)
			require.Equal(t, []string{"succeeded", "failed"}, statuses)
			require.Equal(t, 1, engine.creates, "deterministic rejection cannot replay a finished atom")
			var row models.TaskRun
			require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
			require.Equal(t, "failed", row.Status)
			require.Contains(t, row.Error, test.detail)
			require.Equal(t, 1, row.Attempt)
			require.Positive(t, row.TerminalSequence)
			var events int64
			require.NoError(t, f.db.Model(&models.ExecutionEvent{}).Where("run_id = ? AND task_id = ? AND type = ?", row.JobRunID, row.TaskID, "task_failed").Count(&events).Error)
			require.EqualValues(t, 1, events)
			var unfinished int64
			require.NoError(t, f.db.Model(&models.TaskRun{}).Where("job_run_id = ? AND status IN ?", row.JobRunID, []string{"pending", "running"}).Count(&unfinished).Error)
			require.Zero(t, unfinished, "the live owner waiter can now finalize this failed run")
		})
	}
}

func TestRuntimeExecutorRejectedCachedFanOutDoesNotLaunchAtom(t *testing.T) {
	f := seedProducerWithConsumer(t, "cached-application-rejected")
	cold := &partitionEmittingEngine{logs: producerMarkerLog}
	(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return cold, nil }}).Execute(t.Context(), f.taskRun)
	entries, err := cache.NewStore(f.db).ListByJob(f.jobRun.JobID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	// A restored cache entry can predate validation of partition dependencies.
	require.NoError(t, f.db.Model(&models.TaskCache{}).Where("hash = ?", entries[0].Hash).Update("partitions", datatypes.JSON([]byte(`[{"key":"a","dependsOn":["b"]},{"key":"b","dependsOn":["a"]}]`))).Error)
	fresh := f.newProducerRunAttempt(t)
	fresh.MaxAttempts = 3
	require.NoError(t, f.db.Model(fresh).Update("max_attempts", 3).Error)
	ls := run.NewLeaseStore(f.db)
	lease, err := ls.AcquireLease(t.Context(), fresh.JobRunID, "owner", time.Hour)
	require.NoError(t, err)
	handler := dispatch.NewHandler(f.store, ls, "owner", "token")
	var statuses []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var req dispatch.CompleteRequest
		require.NoError(t, json.Unmarshal(data, &req))
		statuses = append(statuses, req.Status)
		r.Body = io.NopCloser(bytes.NewReader(data))
		handler.HandleComplete(w, r)
	}))
	defer server.Close()
	engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{}}
	meta := ownerMeta()
	meta.OwnerGeneration = lease
	meta.WorkerNode = fresh.ClaimedBy
	meta.Token = "token"
	meta.Attempt = 1
	(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(ctx context.Context, _ string, token string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		return dispatch.PostComplete(ctx, server.URL, token, req)
	}}).Execute(withDispatchMeta(t.Context(), meta), fresh)
	require.Equal(t, []string{"cached", "failed"}, statuses)
	require.Zero(t, engine.creates)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", fresh.ID).Error)
	require.Equal(t, "failed", row.Status)
	require.Contains(t, row.Error, "cycle")
	require.Positive(t, row.TerminalSequence)
}

func TestRuntimeExecutorLegacyApplicationRejectionFailsFinishedAtomOnce(t *testing.T) {
	f := seedProducerTaskRun(t, "legacy-application-rejected")
	f.taskRun.MaxAttempts = 3
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req dispatch.CompleteRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		if req.Status == "succeeded" {
			w.WriteHeader(http.StatusConflict)
			require.NoError(t, json.NewEncoder(w).Encode(dispatch.ErrorResponse{Code: dispatch.ReasonTaskNotRunning, Message: "legacy partition cycle"}))
			return
		}
		require.Equal(t, "failed", req.Status)
		require.Contains(t, req.Error, "legacy partition cycle")
		require.NoError(t, f.store.FailTaskClaimed(req.RunID, req.TaskRunID, fmt.Errorf("%s", req.Error), req.WorkerNode))
		require.NoError(t, json.NewEncoder(w).Encode(dispatch.CompleteResponse{Accepted: true}))
	}))
	defer server.Close()
	engine := &reviewCountingEngine{captureCreateEngine: &captureCreateEngine{logs: "finished\n", waitResult: atom.Success}}
	meta := ownerMeta()
	meta.WorkerNode = f.taskRun.ClaimedBy
	(&runtimeExecutor{store: f.store, localSink: NewLocalSink(f.store), engineFactory: func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil }, completePost: func(ctx context.Context, _ string, token string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
		return dispatch.PostComplete(ctx, server.URL, token, req)
	}}).Execute(withDispatchMeta(t.Context(), meta), f.taskRun)
	require.Equal(t, 1, engine.creates)
	var row models.TaskRun
	require.NoError(t, f.db.First(&row, "id = ?", f.taskRun.ID).Error)
	require.Equal(t, "failed", row.Status)
	require.Contains(t, row.Error, "legacy partition cycle")
}
