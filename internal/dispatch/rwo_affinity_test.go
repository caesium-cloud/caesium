package dispatch

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

const (
	rwoPeerA = "10.0.0.1:9001"
	rwoPeerB = "10.0.0.2:9001"
	// rwoGonePeer is a worker that placed a volume and then left the cluster.
	rwoGonePeer = "10.9.9.9:9001"
)

// affinityStep is one task in a run the dispatcher will place. createdAt
// orders PendingTasksForDispatch, which is what makes the round-robin
// position of an unpinned step deterministic.
type affinityStep struct {
	name      string
	mounts    []container.VolumeMount
	status    string
	claimedBy string
	createdAt time.Time
}

func volumeMount(name, accessMode string) container.VolumeMount {
	return container.VolumeMount{
		Name:       name,
		AccessMode: accessMode,
		Type:       container.VolumeMountTypeVolume,
		Source:     name,
		Target:     "/" + name,
	}
}

// seedAffinityRun inserts a running job whose steps carry the given resolved
// mounts. Each step is its own catalog task, so two mounts of one volume name
// are two steps sharing it.
func seedAffinityRun(t *testing.T, store *run.Store, steps []affinityStep) (uuid.UUID, []uuid.UUID) {
	t.Helper()
	db := store.DB()
	now := time.Now().UTC()

	trigger := &models.Trigger{ID: uuid.New(), Alias: "rwo-trigger-" + uuid.NewString(), Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)

	job := &models.Job{ID: uuid.New(), Alias: "rwo-job-" + uuid.NewString(), TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)

	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)

	taskIDs := make([]uuid.UUID, 0, len(steps))
	for i, step := range steps {
		raw, err := json.Marshal(container.Spec{ResolvedVolumeMounts: step.mounts})
		require.NoError(t, err)
		atom := &models.Atom{
			ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1",
			Command: `["sh","-c","echo hi"]`, Spec: datatypes.JSON(raw),
			CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, db.Create(atom).Error)

		taskID := uuid.New()
		task := &models.Task{
			ID: taskID, JobID: job.ID, AtomID: atom.ID, Name: step.name,
			CreatedAt: now, UpdatedAt: now,
		}
		require.NoError(t, db.Create(task).Error)

		status := step.status
		if status == "" {
			status = string(run.TaskStatusPending)
		}
		created := step.createdAt
		if created.IsZero() {
			created = now.Add(time.Duration(i) * time.Second)
		}
		tr := &models.TaskRun{
			ID: uuid.New(), JobRunID: jobRun.ID, TaskID: taskID, AtomID: atom.ID,
			Engine: models.AtomEngineDocker, Image: "busybox:1.36.1",
			Command: `["sh","-c","echo hi"]`, Status: status, ClaimedBy: step.claimedBy,
			Attempt: 1, MaxAttempts: 1, OutstandingPredecessors: 0,
			CreatedAt: created, UpdatedAt: created,
		}
		require.NoError(t, db.Create(tr).Error)
		taskIDs = append(taskIDs, taskID)
	}
	return jobRun.ID, taskIDs
}

// rwoLoop is a SQL-lane dispatcher with two live peers. buildPeers lists the
// remote peer first and this node last, so round-robin position 0 is peer B
// and position 1 is peer A.
func rwoLoop(t *testing.T, store *run.Store, ls *run.LeaseStore, peerA, peerB *peerStub) *DispatchLoop {
	t.Helper()
	return NewDispatchLoop(DispatchLoopConfig{
		NodeID:     rwoPeerA,
		Token:      loopToken,
		Interval:   time.Hour,
		BatchSize:  64,
		Deadline:   time.Minute,
		LeaseTTL:   30 * time.Second,
		LeaseStore: &testOwnerReader{ls},
		Store:      store,
		Peers:      &testPeerLister{peers: []string{rwoPeerB}},
		PeerBaseURL: func(addr string) string {
			switch addr {
			case rwoPeerA:
				return peerA.server.URL
			case rwoPeerB:
				return peerB.server.URL
			default:
				return ""
			}
		},
	})
}

func workerByTask(stubs ...*peerStub) map[uuid.UUID]string {
	out := map[uuid.UUID]string{}
	for _, stub := range stubs {
		for _, req := range stub.requests() {
			out[req.TaskID] = req.WorkerNode
		}
	}
	return out
}

func TestDispatchRun_SharedReadWriteOnceStepsShareOnePeer(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	base := time.Now().UTC().Add(-time.Minute)
	runID, taskIDs := seedAffinityRun(t, store, []affinityStep{
		{name: "other", createdAt: base, mounts: []container.VolumeMount{volumeMount("scratch", accessModeReadWriteOnce)}},
		{name: "writer", createdAt: base.Add(time.Second), mounts: []container.VolumeMount{volumeMount("work", accessModeReadWriteOnce)}},
		{name: "reader", createdAt: base.Add(2 * time.Second), mounts: []container.VolumeMount{volumeMount("work", accessModeReadWriteOnce)}},
	})
	_, err := ls.AcquireLease(context.Background(), runID, rwoPeerA, 30*time.Second)
	require.NoError(t, err)

	peerA, peerB := newPeerStub(t), newPeerStub(t)
	rwoLoop(t, store, ls, peerA, peerB).tick(context.Background())

	placed := workerByTask(peerA, peerB)
	other, writer, reader := taskIDs[0], taskIDs[1], taskIDs[2]
	require.Equal(t, rwoPeerB, placed[other], "a volume only one step mounts stays on round-robin")
	require.Equal(t, rwoPeerA, placed[writer])
	require.Equal(t, placed[writer], placed[reader], "steps that share a ReadWriteOnce volume land on one worker")
	require.NotEqual(t, placed[other], placed[writer])
}

func TestDispatchRun_MissingReadWriteOncePeerFailsTask(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	base := time.Now().UTC().Add(-time.Minute)
	runID, taskIDs := seedAffinityRun(t, store, []affinityStep{
		{
			name: "writer", createdAt: base, status: string(run.TaskStatusSucceeded), claimedBy: rwoGonePeer,
			mounts: []container.VolumeMount{volumeMount("work", accessModeReadWriteOnce)},
		},
		{
			name: "reader", createdAt: base.Add(time.Second),
			mounts: []container.VolumeMount{volumeMount("work", accessModeReadWriteOnce)},
		},
	})
	_, err := ls.AcquireLease(context.Background(), runID, rwoPeerA, 30*time.Second)
	require.NoError(t, err)

	peerA, peerB := newPeerStub(t), newPeerStub(t)
	rwoLoop(t, store, ls, peerA, peerB).tick(context.Background())

	require.Empty(t, peerA.requests())
	require.Empty(t, peerB.requests())

	var row models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskIDs[1]).First(&row).Error)
	require.Equal(t, string(run.TaskStatusFailed), row.Status)
	require.Contains(t, row.Error, rwoGonePeer)
	require.Contains(t, row.Error, "ReadWriteOnce")
	require.Contains(t, row.Error, "work")

	var writer models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runID, taskIDs[0]).First(&writer).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), writer.Status)
	require.Equal(t, rwoGonePeer, writer.ClaimedBy)
}

func TestDispatchRun_ReadWriteManyStaysRoundRobin(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)

	base := time.Now().UTC().Add(-time.Minute)
	shared := []container.VolumeMount{volumeMount("work", "ReadWriteMany")}
	runID, taskIDs := seedAffinityRun(t, store, []affinityStep{
		{name: "writer", createdAt: base, mounts: shared},
		{name: "reader", createdAt: base.Add(time.Second), mounts: shared},
	})
	_, err := ls.AcquireLease(context.Background(), runID, rwoPeerA, 30*time.Second)
	require.NoError(t, err)

	peerA, peerB := newPeerStub(t), newPeerStub(t)
	rwoLoop(t, store, ls, peerA, peerB).tick(context.Background())

	placed := workerByTask(peerA, peerB)
	require.Equal(t, rwoPeerB, placed[taskIDs[0]])
	require.Equal(t, rwoPeerA, placed[taskIDs[1]])
	require.NotEqual(t, placed[taskIDs[0]], placed[taskIDs[1]])
}
