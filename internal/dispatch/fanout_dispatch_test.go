package dispatch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// countingLimiter records every Acquire the dispatch loop performs.
type countingLimiter struct {
	mu       sync.Mutex
	acquired int
	resource []string
}

func (l *countingLimiter) Acquire(_ context.Context, resource string, _, _ int, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.acquired++
	l.resource = append(l.resource, resource)
	return true, nil
}

func (l *countingLimiter) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.acquired
}

// seedFannedRun creates a job whose single step is a rate-limited fan-out
// target, materialized as three instance TaskRun rows.  It returns the run ID,
// the catalog task ID, and the instance TaskRun IDs.
func seedFannedRun(t *testing.T, db *gorm.DB, store *run.Store) (uuid.UUID, uuid.UUID, []uuid.UUID) {
	t.Helper()
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "fo-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)

	limits, err := json.Marshal([]map[string]any{{"resource": "warehouse", "limit": 100, "window": "1m"}})
	require.NoError(t, err)
	job := &models.Job{
		ID: uuid.New(), Alias: "fo-job-" + uuid.NewString()[:8], TriggerID: trigger.ID,
		RateLimits: datatypes.JSON(limits), CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	runRecord, err := store.Start(job.ID, &trigger.ID)
	require.NoError(t, err)

	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)

	task := &models.Task{
		ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "shard", Position: 0,
		RateLimitResource: "warehouse", RateLimitUnits: 1,
		FanOutConfig: datatypes.JSON([]byte(`{"from":"producer"}`)),
		CreatedAt:    now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(task).Error)

	ids := make([]uuid.UUID, 0, 3)
	for i, key := range []string{"a", "b", "c"} {
		id := uuid.New()
		ids = append(ids, id)
		require.NoError(t, db.Create(&models.TaskRun{
			ID: id, JobRunID: runRecord.ID, TaskID: task.ID, AtomID: atomModel.ID,
			Engine: atomModel.Engine, Image: atomModel.Image, Command: atomModel.Command,
			Status: string(run.TaskStatusPending), Attempt: 1, MaxAttempts: 1,
			OutstandingPredecessors: 0,
			PartitionValue:          key, PartitionIndex: i, PartitionCount: 3,
			CreatedAt: now, UpdatedAt: now,
		}).Error)
	}
	return runRecord.ID, task.ID, ids
}

// TestDispatchRunInMemory_FannedInstancesCarryBothIdentities pins D6/G4: the
// owner-mode dispatch loop must send the *catalog* task ID (so rate limits and
// catalog lookups resolve) alongside the *instance* TaskRun ID (so the worker
// executes and completes the right sibling).  Before the fix the instance ID was
// sent as TaskID, so ratelimit.RuleForTask matched no row and the limit was
// silently skipped for every fan-out instance.
func TestDispatchRunInMemory_FannedInstancesCarryBothIdentities(t *testing.T) {
	var mu sync.Mutex
	var got []DispatchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID, taskID, instanceIDs := seedFannedRun(t, db, store)
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)

	mgr := run.NewOwnerManager(store, run.CheckpointConfig{Events: 1, Interval: time.Hour, KeepFulls: 3})
	limiter := &countingLimiter{}

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:       nodeID,
		APIPort:      apiPort,
		Token:        loopToken,
		Interval:     50 * time.Millisecond,
		BatchSize:    64,
		Deadline:     5 * time.Minute,
		LeaseTTL:     30 * time.Second,
		LeaseStore:   &testOwnerReader{ls},
		Store:        &testTaskReader{store},
		Peers:        &testPeerLister{},
		OwnerManager: mgr,
		RateLimitDB:  db,
		RateLimiter:  limiter,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 3, "every fan-out instance must be dispatched exactly once")

	seen := map[uuid.UUID]bool{}
	for _, req := range got {
		require.Equal(t, taskID, req.TaskID, "TaskID must stay the catalog task id")
		require.NotEqual(t, uuid.Nil, req.TaskRunID, "TaskRunID must identify the instance")
		require.NotEqual(t, req.TaskID, req.TaskRunID, "instance id must not be sent as the catalog id")
		seen[req.TaskRunID] = true
	}
	for _, id := range instanceIDs {
		require.True(t, seen[id], "instance %s was never dispatched", id)
	}
	require.Equal(t, 3, limiter.count(), "each fan-out instance must acquire the rate limit")
}

// TestDispatchRun_SQLPathCarriesTaskRunID asserts the SQL (non-owner-memory)
// dispatch route also names the instance row, so a worker resolving the
// dispatched row never has to disambiguate N siblings by task id.
func TestDispatchRun_SQLPathCarriesTaskRunID(t *testing.T) {
	var mu sync.Mutex
	var got []DispatchRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DispatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		got = append(got, req)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)

	nodeID, apiPort := serverNodeID(t, server)

	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	runID := uuid.New()
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, 30*time.Second)
	require.NoError(t, err)
	taskID := uuid.New()
	insertPendingTask(t, store, runID, taskID)

	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID:     nodeID,
		APIPort:    apiPort,
		Token:      loopToken,
		Interval:   50 * time.Millisecond,
		BatchSize:  64,
		Deadline:   5 * time.Minute,
		LeaseStore: &testOwnerReader{ls},
		Store:      &testTaskReader{store},
		Peers:      &testPeerLister{},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	t.Cleanup(cancel)
	loop.Run(ctx)

	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, got)

	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
	require.Equal(t, taskID, got[0].TaskID)
	require.Equal(t, row.ID, got[0].TaskRunID, "the SQL dispatch route must name the instance row")
}

// TestHandleDispatch_ClaimsTheNamedInstance pins the receiving half of the same
// identity contract the dispatch loop's envelope carries: HandleDispatch claimed,
// loaded, and rolled back by req.TaskID, so for a fanned group every one of
// ClaimTaskForDispatch / LoadDispatchedTaskRun / ReleaseTaskClaim resolved a
// catalog id that names N rows and failed ambiguously — no fan-out instance could
// be accepted at all.
func TestHandleDispatch_ClaimsTheNamedInstance(t *testing.T) {
	store, _, h := setupHandler(t)
	sub := &fakeSubmitter{}
	h = h.WithWorkerSubmitter(sub)

	runID, taskID, instanceIDs := seedFannedRun(t, store.DB(), store)
	target := instanceIDs[1]

	req := DispatchRequest{
		RunID:           runID,
		TaskID:          taskID,
		TaskRunID:       target,
		OwnerGeneration: 1,
		Attempt:         1,
		WorkerNode:      ownerNodeAddr,
		OwnerBaseURL:    "http://10.0.0.1:8080",
		Deadline:        time.Now().Add(5 * time.Minute),
	}
	w := postJSON(t, h.HandleDispatch, req)
	require.Equal(t, http.StatusAccepted, w.Code, "a fan-out instance dispatch must be accepted")

	require.Len(t, sub.accepted, 1)
	require.Equal(t, target, sub.accepted[0].Task.ID, "the worker must receive the named instance row")
	require.Equal(t, "b", sub.accepted[0].Task.PartitionValue)

	// Exactly the named instance is claimed; its siblings stay dispatchable.
	var rows []models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&rows).Error)
	require.Len(t, rows, 3)
	for i := range rows {
		if rows[i].ID == target {
			require.Equal(t, string(run.TaskStatusRunning), rows[i].Status)
			require.Equal(t, ownerNodeAddr, rows[i].ClaimedBy)
			continue
		}
		require.Equal(t, string(run.TaskStatusPending), rows[i].Status,
			"claiming one instance must not claim its siblings")
		require.Empty(t, rows[i].ClaimedBy)
	}
}
