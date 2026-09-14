package job

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type localBlockingLogReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *localBlockingLogReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *localBlockingLogReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

type localStaticSecretResolver map[string]string

func (r localStaticSecretResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := r.ResolveWithIdentity(ctx, ref)
	return value, err
}

func (r localStaticSecretResolver) ResolveWithIdentity(_ context.Context, ref string) (string, secret.Identity, error) {
	return r[ref], secret.Identity{Provider: "env", Ref: ref, Verifiable: false, UnverifiableReason: "test"}, nil
}

func TestSecretLogDrainTimeoutFailsLocalTaskAfterStop(t *testing.T) {
	originalDrain, originalAbort := secretLogDrainTimeout, secretLogAbortTimeout
	secretLogDrainTimeout, secretLogAbortTimeout = time.Millisecond, time.Second
	t.Cleanup(func() { secretLogDrainTimeout, secretLogAbortTimeout = originalDrain, originalAbort })

	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	engine := newFakeEngine()
	jobID, taskID, atomID := uuid.New(), uuid.New(), uuid.New()
	tasks := models.Tasks{{ID: taskID, JobID: jobID, AtomID: atomID, Name: "secret"}}
	taskSvc := &fakeTaskService{tasks: tasks}
	atomSvc := &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtomWithEnv(t, atomID, map[string]string{"TOKEN": "secret://env/TOKEN"}),
	}}
	persistGraph(t, db, tasks, nil)
	engine.logReadersByName[taskID.String()] = &localBlockingLogReader{closed: make(chan struct{})}
	opts := append(withTestDeps(store, env.Environment{
		MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal,
	}, taskSvc, atomSvc, &fakeTaskEdgeService{}, engine),
		WithSecretResolver(localStaticSecretResolver{"secret://env/TOKEN": "resolved-token"}))

	runErr := New(&models.Job{ID: jobID}, opts...).Run(t.Context())
	require.ErrorContains(t, runErr, "timed out draining scrubbed task log")
	snapshot := latestRunSnapshot(t, store, jobID)
	taskRun := taskRunByID(snapshot, taskID)
	require.NotNil(t, taskRun)
	require.Equal(t, run.TaskStatusFailed, taskRun.Status)
	require.Contains(t, taskRun.Error, "timed out draining scrubbed task log")
	require.Empty(t, taskRun.Output, "partial marker output must not be accepted")
}
