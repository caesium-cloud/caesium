package worker

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/stretchr/testify/require"
)

type workerBlockingLogReader struct {
	closed chan struct{}
	once   sync.Once
}

func (r *workerBlockingLogReader) Read([]byte) (int, error) {
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *workerBlockingLogReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	return nil
}

func TestSecretLogDrainTimeoutFailsWorkerTaskAfterStop(t *testing.T) {
	originalDrain, originalAbort := secretLogDrainTimeout, secretLogAbortTimeout
	secretLogDrainTimeout, secretLogAbortTimeout = time.Millisecond, time.Second
	t.Cleanup(func() { secretLogDrainTimeout, secretLogAbortTimeout = originalDrain, originalAbort })

	taskRun, db := seedSchemaValidationTaskRun(t, "")
	spec, err := json.Marshal(container.Spec{Env: map[string]string{"TOKEN": "secret://env/TOKEN"}})
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.Atom{}).Where("id = ?", taskRun.AtomID).Update("spec", spec).Error)
	engine := &captureCreateEngine{logsReader: &workerBlockingLogReader{closed: make(chan struct{})}}
	store := run.NewStore(db)
	executor := &runtimeExecutor{
		store: store, localSink: NewLocalSink(store),
		secretResolver: staticSecretResolver{"secret://env/TOKEN": "resolved-token"},
		engineFactory:  func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
	}
	executor.Execute(context.Background(), taskRun)

	row := reloadTaskRun(t, executor, taskRun.ID)
	require.Equal(t, string(run.TaskStatusFailed), row.Status)
	require.Contains(t, row.Error, "timed out draining scrubbed task log")
	require.Positive(t, engine.stopCalls, "the runtime must be stopped before the timeout is reported")
	require.Empty(t, row.Output, "partial marker output must not be accepted")
}
