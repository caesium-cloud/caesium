package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func localRetryCompensationFixture(t *testing.T) (*runstorage.Store, *models.Job, *runstorage.JobRun, uuid.UUID) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	j := &models.Job{ID: uuid.New(), Alias: "local-retry-read-" + uuid.NewString()}
	require.NoError(t, db.Create(j).Error)
	a := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "frozen:1", Command: `["false"]`}
	require.NoError(t, db.Create(a).Error)
	task := &models.Task{ID: uuid.New(), JobID: j.ID, AtomID: a.ID, Name: "work"}
	require.NoError(t, db.Create(task).Error)
	store := runstorage.NewStore(db)
	r, err := store.Start(j.ID, nil, runstorage.WithStartParams(map[string]string{"input": "durable", "zero": "0"}))
	require.NoError(t, err)
	require.NoError(t, store.RegisterTask(r.ID, task, a, 0))
	require.NoError(t, store.FailTask(r.ID, task.ID, errors.New("first task failed")))
	require.NoError(t, store.Complete(r.ID, errors.New("first attempt failed")))
	entry, err := store.Get(r.ID)
	require.NoError(t, err)
	return store, j, entry, task.ID
}

// Target the joined exact-ID refresh outside the retry transaction, allowing
// the joined load which records run_retried inside that transaction to succeed.
func failLocalRetryReadback(t *testing.T, store *runstorage.Store, id uuid.UUID, fault error) (*runstorage.JobRun, error) {
	t.Helper()
	const callback = "test:local_retry_postcommit_refresh"
	reads := 0
	require.NoError(t, store.DB().Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		_, transaction := tx.Statement.ConnPool.(gorm.TxCommitter)
		if tx.Statement.Table != "job_runs" || len(tx.Statement.Joins) == 0 || transaction {
			return
		}
		where, ok := tx.Statement.Clauses["WHERE"].Expression.(clause.Where)
		if !ok {
			return
		}
		for _, condition := range where.Exprs {
			expr, ok := condition.(clause.Expr)
			if !ok || expr.SQL != "job_runs.id = ?" || len(expr.Vars) != 1 {
				continue
			}
			readID, ok := expr.Vars[0].(uuid.UUID)
			if !ok || readID != id {
				continue
			}
			reads++
			_ = tx.AddError(fault)
		}
	}))
	defer func() { require.NoError(t, store.DB().Callback().Query().Remove(callback)) }()
	r, err := store.RetryFromFailure(id)
	require.Equal(t, 1, reads)
	require.ErrorIs(t, err, fault)
	committedID, committed := runstorage.CommittedRunID(err)
	require.True(t, committed)
	require.Equal(t, id, committedID)
	return r, err
}

func TestLocalWholeRetryPostCommitFailureFinalizesSynchronouslyBeforeReturning(t *testing.T) {
	store, j, entry, taskID := localRetryCompensationFixture(t)
	other, err := store.Start(j.ID, nil)
	require.NoError(t, err)
	fault := errors.New("local retry refresh failed")
	finalizing := make(chan struct{}, 1)
	finish := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(finish) }) }
	faulted := false
	const callback = "test:local_retry_block_compensation"
	require.NoError(t, store.DB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "job_runs" && faulted {
			var row models.JobRun
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).First(&row, "id = ?", entry.ID).Error)
			require.Equal(t, string(runstorage.StatusRunning), row.Status)
			var task models.TaskRun
			require.NoError(t, tx.Session(&gorm.Session{NewDB: true}).Where("job_run_id = ? AND task_id = ?", entry.ID, taskID).First(&task).Error)
			require.Equal(t, string(runstorage.TaskStatusPending), task.Status)
			finalizing <- struct{}{}
			<-finish
		}
	}))
	done := make(chan error, 1)
	exited := make(chan struct{})
	t.Cleanup(func() { unblock(); <-exited; _ = store.DB().Callback().Update().Remove(callback) })
	go func() {
		defer close(exited)
		_, err := startLocalWholeRunRetry(t.Context(), j, entry.ID, entry,
			func(id uuid.UUID) (*runstorage.JobRun, error) {
				r, err := failLocalRetryReadback(t, store, id, fault)
				faulted = true
				return r, err
			},
			store.CompleteIfActive,
			func(_ context.Context, _ *models.Job, _ *runstorage.JobRun, release func()) {
				t.Error("committed error must never launch an engine")
				release()
			})
		done <- err
	}()
	select {
	case <-finalizing:
	case <-time.After(5 * time.Second):
		t.Fatal("committed reopen did not reach compensation")
	}
	select {
	case <-done:
		t.Fatal("helper returned before terminal persistence")
	default:
	}
	require.Equal(t, 1, job.CancelRunContexts(entry.ID), "registration held through synchronous compensation")
	unblock()
	err = <-done
	require.ErrorIs(t, err, fault, "original CLI diagnostic remains authoritative")
	require.Zero(t, job.CancelRunContexts(entry.ID))
	row, err := store.Get(entry.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusFailed, row.Status)
	require.Contains(t, row.Error, fault.Error())
	require.Equal(t, entry.Params, row.Params)
	untouched, err := store.Get(other.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusRunning, untouched.Status)
	require.Nil(t, untouched.CompletedAt)
}

func TestLocalWholeRetryPostCommitCompensationPreservesDurableCancellation(t *testing.T) {
	store, j, entry, _ := localRetryCompensationFixture(t)
	fault := errors.New("local retry refresh failed")
	var originalCause string
	var diagnostic error
	_, err := startLocalWholeRunRetry(t.Context(), j, entry.ID, entry,
		func(id uuid.UUID) (*runstorage.JobRun, error) {
			r, admissionErr := failLocalRetryReadback(t, store, id, fault)
			diagnostic = admissionErr
			require.NoError(t, store.CancelRun(t.Context(), id))
			require.Equal(t, 1, job.CancelRunContexts(id))
			row, getErr := store.Get(id)
			require.NoError(t, getErr)
			originalCause = row.Error
			return r, admissionErr
		}, store.CompleteIfActive,
		func(context.Context, *models.Job, *runstorage.JobRun, func()) {
			t.Fatal("cancelled committed error launched")
		})
	require.Same(t, diagnostic, err)
	require.Zero(t, job.CancelRunContexts(entry.ID))
	row, err := store.Get(entry.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusCancelled, row.Status)
	require.Equal(t, originalCause, row.Error)
}

func TestLocalWholeRetryRejectsUnrelatedCommittedOrPreloadedIdentity(t *testing.T) {
	for _, kind := range []string{"unrelated marker", "unrelated preloaded run", "unrelated preloaded job", "ordinary failure"} {
		t.Run(kind, func(t *testing.T) {
			j := &models.Job{ID: uuid.New()}
			runID := uuid.New()
			entry := &runstorage.JobRun{ID: runID, JobID: j.ID}
			fault := errors.New("retry failed")
			admissionErr := fmt.Errorf("wrapped: %w", &runstorage.RunCommittedError{RunID: runID, JobID: j.ID, Err: fault})
			switch kind {
			case "unrelated marker":
				admissionErr = &runstorage.RunCommittedError{RunID: uuid.New(), JobID: j.ID, Err: fault}
			case "unrelated preloaded run":
				entry.ID = uuid.New()
			case "unrelated preloaded job":
				entry.JobID = uuid.New()
			default:
				admissionErr = fault
			}
			_, err := startLocalWholeRunRetry(t.Context(), j, runID, entry,
				func(uuid.UUID) (*runstorage.JobRun, error) { return nil, admissionErr },
				func(uuid.UUID, error) (bool, error) {
					t.Fatal("unvalidated or uncommitted retry compensated")
					return false, nil
				},
				func(context.Context, *models.Job, *runstorage.JobRun, func()) { t.Fatal("unvalidated retry launched") })
			require.ErrorIs(t, err, fault)
			require.Zero(t, job.CancelRunContexts(runID))
		})
	}
}

func TestLocalWholeRetryCompensationFailureReturnsOriginalDiagnostic(t *testing.T) {
	store, j, entry, _ := localRetryCompensationFixture(t)
	fault := errors.New("refresh failed")
	writeErr := errors.New("compensation unavailable")
	var diagnostic error
	calls := 0
	_, err := startLocalWholeRunRetry(t.Context(), j, entry.ID, entry,
		func(id uuid.UUID) (*runstorage.JobRun, error) {
			r, err := failLocalRetryReadback(t, store, id, fault)
			diagnostic = err
			return r, err
		},
		func(id uuid.UUID, cause error) (bool, error) {
			calls++
			require.Equal(t, entry.ID, id)
			require.Same(t, diagnostic, cause)
			const callback = "test:local_retry_compensation_failure"
			require.NoError(t, store.DB().Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "job_runs" {
					require.Equal(t, 1, job.CancelRunContexts(id), "failed write still holds registration")
					_ = tx.AddError(writeErr)
				}
			}))
			defer func() { require.NoError(t, store.DB().Callback().Update().Remove(callback)) }()
			completed, completeErr := store.CompleteIfActive(id, cause)
			require.False(t, completed)
			require.ErrorIs(t, completeErr, writeErr)
			return completed, completeErr
		},
		func(context.Context, *models.Job, *runstorage.JobRun, func()) {
			t.Fatal("unresolved compensation launched")
		})
	require.Equal(t, 1, calls)
	require.Same(t, diagnostic, err)
	require.Zero(t, job.CancelRunContexts(entry.ID))
	row, err := store.Get(entry.ID)
	require.NoError(t, err)
	require.Equal(t, runstorage.StatusRunning, row.Status, "write failure must remain unresolved, not report terminal success")
	require.Nil(t, row.CompletedAt)
}

// Drive the helper-to-Fatal process boundary in a test subprocess with actual
// store readback compensation. Production main calls this same Fatal routine.
func TestLocalWholeRetryProcessExitWaitsForCompensation(t *testing.T) {
	const childEnv = "CAESIUM_TEST_RETRY_FATAL_CHILD"
	if os.Getenv(childEnv) == "1" {
		store, j, entry, _ := localRetryCompensationFixture(t)
		fault := errors.New("subprocess retry readback failed")
		_, err := startLocalWholeRunRetry(t.Context(), j, entry.ID, entry,
			func(id uuid.UUID) (*runstorage.JobRun, error) { return failLocalRetryReadback(t, store, id, fault) },
			func(id uuid.UUID, cause error) (bool, error) {
				_, writeErr := fmt.Fprintln(os.Stdout, "retry-compensation-entered")
				require.NoError(t, writeErr)
				var gate [1]byte
				_, readErr := io.ReadFull(os.Stdin, gate[:])
				require.NoError(t, readErr)
				complete, completeErr := store.CompleteIfActive(id, cause)
				require.NoError(t, completeErr)
				row, getErr := store.Get(id)
				require.NoError(t, getErr)
				require.Equal(t, runstorage.StatusFailed, row.Status)
				_, writeErr = fmt.Fprintln(os.Stdout, "retry-compensation-persisted")
				require.NoError(t, writeErr)
				return complete, completeErr
			}, func(context.Context, *models.Job, *runstorage.JobRun, func()) {
				t.Fatal("error compensation launched an engine")
			})
		require.ErrorIs(t, err, fault)
		require.Zero(t, job.CancelRunContexts(entry.ID))
		log.Fatal("test command fatal after compensated diagnostic", "error", err)
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, executable, "-test.run=^TestLocalWholeRetryProcessExitWaitsForCompensation$")
	child.Env = append(os.Environ(), childEnv+"=1")
	stdout, output := io.Pipe()
	child.Stdout = output
	stdin, err := child.StdinPipe()
	require.NoError(t, err)
	child.Stderr = os.Stderr
	require.NoError(t, child.Start())
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		err := child.Wait()
		_ = output.Close()
		done <- err
	}()
	t.Cleanup(func() { _ = stdin.Close(); _ = stdout.Close(); cancel(); <-exited })
	scanner := bufio.NewScanner(stdout)
	entered := false
	for scanner.Scan() {
		if scanner.Text() == "retry-compensation-entered" {
			entered = true
			break
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, entered, "child did not reach compensation")
	select {
	case err := <-done:
		t.Fatalf("process exited before compensation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	_, err = stdin.Write([]byte{1})
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	persisted := false
	for scanner.Scan() {
		if scanner.Text() == "retry-compensation-persisted" {
			persisted = true
		}
	}
	require.NoError(t, scanner.Err())
	require.True(t, persisted, "process fatal exit preceded terminal persistence")
	err = <-done
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 1, exitErr.ExitCode(), "original retry error still reaches Fatal")
}
