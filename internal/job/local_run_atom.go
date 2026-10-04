package job

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/imagecheck"
	"github.com/caesium-cloud/caesium/internal/incident"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

// executeAtom creates, monitors, and stops a container for one execution attempt.
// It returns the atom result string, any parsed task outputs, any branch
// selections (for branch-type tasks), a persisted log snapshot, and any error.
//
// instanceID identifies the TaskRun row this attempt belongs to. It is
// uuid.Nil for an unfanned step (whose single row is addressable by taskID)
// and the instance's TaskRun primary key for a fan-out partition, where N
// sibling rows share (runID, taskID) and every store write and container name
// must therefore be keyed on the instance, not the catalog task.
func (l *localRun) executeAtom(taskCtx context.Context, taskID, instanceID uuid.UUID, attempt int, attemptTimeout time.Duration, runner *atomRunner, extraEnv map[string]string) (string, map[string]string, []string, []pkgtask.Partition, run.MetricsCapture, *run.TaskLogSnapshot, error) {
	j := l.j
	store := l.store
	snapshot := l.snapshot
	vars := l.vars
	secretResolver := l.secretResolver
	runID := l.runID
	runQuarantined := l.runQuarantined
	paramEnv := l.paramEnv
	taskQuarantine := l.taskQuarantine
	attemptContextError := func() error {
		cause := context.Cause(taskCtx)
		switch {
		case cause == nil:
			return nil
		case run.IsRunDeadlineError(cause):
			return cause
		case errors.Is(cause, context.DeadlineExceeded):
			return fmt.Errorf("task %s timed out after %s", taskID, attemptTimeout)
		case errors.Is(cause, context.Canceled):
			return fmt.Errorf("task %s cancelled: %w", taskID, cause)
		default:
			return cause
		}
	}
	// taskRef is what the run store resolves this execution to; see
	// loadTaskRunByIDOrUnique for the primary-key-or-task-ID contract.
	taskRef := taskID
	atomName := fmt.Sprintf("%s-%s", taskID, runID)
	if instanceID != uuid.Nil {
		taskRef = instanceID
		// Sibling partitions run against the same catalog task in the same
		// run, so the container name must carry the instance identity or
		// Docker rejects every sibling after the first with a name conflict.
		atomName = fmt.Sprintf("%s-%s", atomName, instanceID)
	}
	if attempt > 1 {
		atomName = fmt.Sprintf("%s-attempt%d", atomName, attempt)
	}

	image := imagecheck.PinReference(runner.image, runner.resolvedImageDigest)
	log.Info("running atom", "job_id", j.id, "task_id", taskID, "instance_id", instanceID, "image", image, "cmd", runner.command, "attempt", attempt)

	spec := runner.spec
	taskQuarantined := taskQuarantine[taskID] || runQuarantined
	if taskQuarantined {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, ErrLocalQuarantinedReplayUnsupported
	}
	interpolated, err := jobdefruntime.InterpolateParamRefs(spec.Env, snapshot.Params)
	if err != nil {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, err
	}
	spec.Env = interpolated
	rawSecretEnv := spec.Env
	spec, secretIdentities, err := jobdefruntime.ResolveContainerSpecSecretsWithIdentities(taskCtx, secretResolver, spec)
	if err != nil {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, err
	}
	secretValues := incident.SecretValuesFromEnv(rawSecretEnv, spec.Env)
	secretBearing := len(secretIdentities) > 0
	secretLogFence := run.SecretLogFence{Attempt: attempt, Generation: uuid.NewString()}
	if len(secretIdentities) > 0 {
		refs := make([]models.TaskExecutionSecretRef, 0, len(secretIdentities))
		for _, resolved := range secretIdentities {
			refs = append(refs, run.SecretIdentityDescriptorRef(resolved.EnvKey, resolved.Ref, resolved.Identity))
		}
		if err := store.UpdateTaskExecutionDescriptorSecretRefs(runID, taskRef, refs); err != nil {
			log.Warn("failed to persist task execution descriptor secret identity", "task_id", taskID, "error", err)
		}
	}
	if secretBearing {
		if err := store.PrepareSecretTaskLog(runID, taskRef, secretLogFence); err != nil {
			return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("prepare scrubbed task log: %w", err)
		}
	}
	if len(paramEnv) > 0 || len(extraEnv) > 0 {
		merged := make(map[string]string, len(spec.Env)+len(paramEnv)+len(extraEnv))
		maps.Copy(merged, spec.Env)
		maps.Copy(merged, paramEnv)
		maps.Copy(merged, extraEnv)
		spec.Env = merged
	}

	if ctxErr := attemptContextError(); ctxErr != nil {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, ctxErr
	}
	engine, err := runner.newEngine(taskCtx)
	if err != nil {
		if ctxErr := attemptContextError(); ctxErr != nil {
			return "", nil, nil, nil, run.MetricsCapture{}, nil, ctxErr
		}
		return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("task %s: %w", taskID, err)
	}
	if ctxErr := attemptContextError(); ctxErr != nil {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, ctxErr
	}
	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:    atomName,
		Image:   image,
		Command: runner.command,
		Spec:    spec,
	})
	if err != nil {
		if ctxErr := attemptContextError(); ctxErr != nil {
			return "", nil, nil, nil, run.MetricsCapture{}, nil, ctxErr
		}
		return "", nil, nil, nil, run.MetricsCapture{}, nil, err
	}
	if ctxErr := attemptContextError(); ctxErr != nil {
		if stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true}); stopErr != nil {
			return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("%w; failed to stop atom %s: %v", ctxErr, a.ID(), stopErr)
		}
		return "", nil, nil, nil, run.MetricsCapture{}, nil, ctxErr
	}

	if err := store.StartTask(runID, taskRef, a.ID()); err != nil {
		return "", nil, nil, nil, run.MetricsCapture{}, nil, err
	}

	var resourceSampler *atom.ResourceSampler
	if vars.ResourceStatsEnabled {
		resourceSampler = atom.StartResourceSampler(taskCtx, engine, a.ID(), vars.ResourceStatsSampleInterval)
		defer resourceSampler.Stop(nil)
	}

	persistResourceOutcome := func(final atom.Atom) {
		if resourceSampler == nil {
			return
		}
		outcome := run.TaskResourceOutcome{ResourceSummary: resourceSampler.Stop(final), RuntimeID: a.ID(), Attempt: attempt}
		if final != nil {
			outcome.ExitCode = final.ExitCode()
		}
		if resourceErr := store.SetTaskResourceOutcome(runID, taskRef, outcome); resourceErr != nil {
			log.Warn("failed to persist task resource outcome", "task_id", taskID, "error", resourceErr)
		}
	}

	var (
		secretLogStream    io.ReadCloser
		secretLogCollector *run.SecretLogCollector
		secretLogResult    <-chan run.SecretLogCaptureResult
	)
	if secretBearing {
		if stream, streamErr := engine.Logs(&atom.EngineLogsRequest{ID: a.ID()}); streamErr != nil {
			log.Warn("failed to open scrubbed live task log; will retry after completion",
				"task_id", taskID, "atom_id", a.ID(), "error", streamErr)
		} else {
			secretLogStream = stream
			collector := run.NewSecretLogCollector(store, runID, taskRef, secretLogFence,
				secretValues, pkgtask.MaxLogSnapshotBytes)
			secretLogCollector = collector
			secretLogResult = run.StartSecretTaskLogCapture(stream, collector,
				vars.OutputRefMaxBytes.Int64(), env.Variables().FanOutMaxPartitions)
		}
	}

	waitResult := make(chan struct {
		atom atom.Atom
		err  error
	}, 1)
	go func() {
		next, waitErr := engine.Wait(&atom.EngineWaitRequest{ID: a.ID(), Context: taskCtx})
		waitResult <- struct {
			atom atom.Atom
			err  error
		}{atom: next, err: waitErr}
	}()

	// abandonAtom force-stops the container and classifies why we are walking
	// away from it. It is shared by BOTH doors of the select below, and that
	// sharing is the point.
	//
	// When taskCtx ends, engine.Wait ALSO returns — with ctx.Err() — so
	// `taskCtx.Done()` and `waitResult` become ready at the same instant and
	// Go picks between them uniformly at random. Stopping the atom on only
	// the taskCtx.Done() door therefore abandoned roughly half of all
	// cancelled containers, which is the very orphan this cancel path exists
	// to kill. It reproduced as an arm64-only unit failure
	// (TestRunLocalCancelStopsAtom) purely because the slower runner made
	// the cancel land before Wait started polling more often; the race is
	// arch-independent and real against Docker, whose Wait returns
	// waitCtx.Err() the same way (internal/atom/docker/engine.go).
	//
	// Any OTHER wait error is stopped too, matching what the distributed
	// worker already does (internal/worker/runtime_executor.go monitorTask):
	// a failed Wait means we stopped watching, never that the container
	// stopped.
	//
	// Force, like the timeout branch always did: a container the run has
	// given up on must not outlive it by its own graceful stop timeout. A
	// failed Stop is reported rather than swallowed — "cancelled" and
	// "cancelled but the container is still out there" are different
	// operational facts.
	abandonAtom := func(waitErr error) (string, map[string]string, []string, []pkgtask.Partition, run.MetricsCapture, *run.TaskLogSnapshot, error) {
		// Join sampling before Stop removes the runtime. A failed Wait has
		// no terminal inspect evidence, but completed samples remain valid.
		persistResourceOutcome(nil)
		if secretLogCollector != nil {
			secretLogCollector.Abort()
		}
		if secretLogStream != nil {
			_ = secretLogStream.Close()
		}
		stopErr := engine.Stop(&atom.EngineStopRequest{
			ID:    a.ID(),
			Force: true,
		})
		cause := context.Cause(taskCtx)
		switch {
		case run.IsRunDeadlineError(cause):
			if stopErr != nil {
				return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("%w; failed to stop atom %s: %v", cause, a.ID(), stopErr)
			}
			return "", nil, nil, nil, run.MetricsCapture{}, nil, cause
		case errors.Is(cause, context.DeadlineExceeded):
			if stopErr != nil {
				return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskID, attemptTimeout, a.ID(), stopErr)
			}
			return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("task %s timed out after %s", taskID, attemptTimeout)
		case errors.Is(cause, context.Canceled):
			if stopErr != nil {
				return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("task %s cancelled and failed to stop atom %s: %w", taskID, a.ID(), stopErr)
			}
			return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("task %s cancelled: %w", taskID, cause)
		}
		// taskCtx is still live, so this is a genuine wait failure rather
		// than a cancellation arriving by the other door. The stop is
		// best-effort here: the wait error is the cause worth surfacing.
		if stopErr != nil {
			log.Warn("failed to stop atom after engine wait error", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "error", stopErr)
		}
		if waitErr != nil {
			return "", nil, nil, nil, run.MetricsCapture{}, nil, waitErr
		}
		return "", nil, nil, nil, run.MetricsCapture{}, nil, taskCtx.Err()
	}

	select {
	case <-taskCtx.Done():
		return abandonAtom(nil)
	case result := <-waitResult:
		if result.err != nil {
			return abandonAtom(result.err)
		}
		a = result.atom
		log.Info("atom finished", "job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "result", a.Result())

		persistResourceOutcome(a)

		// Capture the raw exit code before Result() folds it into a coarse
		// status and the incident classifier loses it. Best-effort.
		if resourceSampler == nil {
			if exitErr := store.SetTaskExitCode(runID, taskRef, a.ExitCode()); exitErr != nil {
				log.Warn("failed to persist task exit code", "task_id", taskID, "error", exitErr)
			}
		}

		// Parse both structured outputs and branch markers in a single
		// pass over the log stream (no full buffering).
		var taskOutput map[string]string
		var branchNames []string
		var logSnapshot *run.TaskLogSnapshot
		var partitions []pkgtask.Partition
		// metricsCapture carries the samples AND how completely they were
		// read: a log this executor could not fetch or parse, and a metrics
		// scan that overflowed its cap, both mean a declared metric's
		// absence proves nothing (issue #437). The evaluator downgrades
		// those verdicts to `unavailable` instead of failing the task for
		// an infrastructure fault.
		var metricsCapture run.MetricsCapture
		var markers *pkgtask.Markers
		var parseErr error
		var logErr error
		var secretLogDrainTimedOut bool
		if secretLogResult != nil {
			capture, timedOut := run.DrainSecretTaskLogCapture(secretLogResult, secretLogCollector, secretLogStream,
				secretLogDrainTimeout, secretLogAbortTimeout)
			markers, parseErr = capture.Markers, capture.Err
			secretLogDrainTimedOut = timedOut
			if capture.PersistErr != nil {
				log.Warn("failed to persist scrubbed live task log", "task_id", taskID, "error", capture.PersistErr)
			}
		} else {
			logStream, openErr := engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
			logErr = openErr
			if openErr == nil {
				if secretBearing {
					collector := run.NewSecretLogCollector(store, runID, taskRef, secretLogFence,
						secretValues, pkgtask.MaxLogSnapshotBytes)
					markers, parseErr = run.CaptureSecretTaskLogs(logStream, collector,
						vars.OutputRefMaxBytes.Int64(), env.Variables().FanOutMaxPartitions)
					if persistErr := collector.Err(); persistErr != nil {
						log.Warn("failed to persist scrubbed task log", "task_id", taskID, "error", persistErr)
					}
				} else {
					markers, parseErr = pkgtask.CaptureMarkersWithLimits(logStream, pkgtask.MaxLogSnapshotBytes, vars.OutputRefMaxBytes.Int64(), env.Variables().FanOutMaxPartitions)
					if closeErr := logStream.Close(); closeErr != nil {
						log.Warn("failed to close log stream", "task_id", taskID, "error", closeErr)
					}
				}
			}
		}
		if logErr != nil {
			metricsCapture.Unreadable = true
			log.Warn("failed to read task logs; markers and dataset metrics are unavailable for this task",
				"job_id", j.id, "task_id", taskID, "atom_id", a.ID(), "error", logErr)
		} else {
			switch {
			case parseErr != nil:
				if _, ok := errors.AsType[*pkgtask.PartitionError](parseErr); ok && !secretLogDrainTimedOut {
					stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true})
					if stopErr != nil {
						return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("%w (also failed to stop atom: %w)", parseErr, stopErr)
					}
					return "", nil, nil, nil, run.MetricsCapture{}, nil, parseErr
				}
				metricsCapture.Unreadable = true
				log.Warn("failed to parse task markers", "task_id", taskID, "error", parseErr)
			case markers == nil:
				metricsCapture.Unreadable = true
			default:
				taskOutput = markers.Output
				branchNames = markers.Branches
				partitions = markers.Partitions
				metricsCapture.Samples = markers.Metrics
				if markers.MetricsTruncated {
					metricsCapture.Truncated = true
					log.Warn("dataset metrics exceeded the marker cap; some samples were dropped",
						"task_id", taskID, "cap_bytes", pkgtask.MaxMetricsBytes)
				}
				if markers.LogText != "" || markers.LogTruncated {
					logSnapshot = &run.TaskLogSnapshot{
						Text:      markers.LogText,
						Truncated: markers.LogTruncated,
					}
				}
			}
		}

		stopErr := engine.Stop(&atom.EngineStopRequest{
			ID:    a.ID(),
			Force: true,
		})
		if secretLogDrainTimedOut {
			if stopErr != nil {
				return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("timed out draining scrubbed task log and failed to stop atom: %w", stopErr)
			}
			return "", nil, nil, nil, run.MetricsCapture{}, nil, fmt.Errorf("timed out draining scrubbed task log")
		}
		return string(a.Result()), taskOutput, branchNames, partitions, metricsCapture, logSnapshot, stopErr
	}
}
