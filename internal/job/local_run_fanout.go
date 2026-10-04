package job

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

func (l *localRun) rehydrateFanOutGroups() {
	store := l.store
	tasksByID := l.tasksByID
	runID := l.runID
	fanOutGroups := l.fanOutGroups
	var rows []models.TaskRun
	if err := store.DB().
		Where("job_run_id = ? AND partition_count > 0", runID).
		Order("task_id ASC, partition_index ASC").
		Find(&rows).Error; err != nil {
		log.Warn("failed to rehydrate fan-out groups", "run_id", runID, "error", err)
		return
	}
	byTask := make(map[uuid.UUID][]models.TaskRun, len(rows))
	order := make([]uuid.UUID, 0, len(rows))
	for _, row := range rows {
		if _, seen := byTask[row.TaskID]; !seen {
			order = append(order, row.TaskID)
		}
		byTask[row.TaskID] = append(byTask[row.TaskID], row)
	}
	for _, tid := range order {
		if existing, ok := fanOutGroups[tid]; ok && len(existing.Instances) > 0 {
			continue
		}
		instances := byTask[tid]
		g := run.ExpandedGroup{TaskID: tid, Dependents: map[string][]string{}}
		if t := tasksByID[tid]; t != nil {
			g.TaskName = t.Name
		}
		for _, row := range instances {
			var deps []string
			if len(row.PartitionDependsOn) > 0 {
				if err := json.Unmarshal(row.PartitionDependsOn, &deps); err != nil {
					log.Warn("failed to decode partition dependsOn", "task_id", tid, "partition", row.PartitionValue, "error", err)
				}
			}
			var attrs map[string]string
			if len(row.PartitionAttributes) > 0 {
				if err := json.Unmarshal(row.PartitionAttributes, &attrs); err != nil {
					log.Warn("failed to decode partition attributes", "task_id", tid, "partition", row.PartitionValue, "error", err)
				}
			}
			g.Instances = append(g.Instances, run.ExpandedInstance{
				TaskRunID:               row.ID,
				TaskID:                  tid,
				PartitionIndex:          row.PartitionIndex,
				Partition:               pkgtask.Partition{Key: row.PartitionValue, Fingerprint: row.PartitionFingerprint, DependsOn: deps, Attributes: attrs},
				OutstandingPredecessors: row.OutstandingPredecessors,
			})
			for _, d := range deps {
				g.Dependents[d] = append(g.Dependents[d], row.PartitionValue)
			}
		}
		if len(g.Instances) > 0 {
			fanOutGroups[tid] = g
		}
	}
}

func (l *localRun) registerExpansion(res *run.CompleteTaskResult) {
	fanOutGroups := l.fanOutGroups
	fanOutGroupsMu := &l.fanOutGroupsMu
	if res == nil || res.Expansion == nil {
		return
	}
	fanOutGroupsMu.Lock()
	defer fanOutGroupsMu.Unlock()
	for _, g := range res.Expansion.Groups {
		if len(g.Instances) > 0 {
			fanOutGroups[g.TaskID] = g
		}
	}
}

func (l *localRun) lookupFanOutGroup(taskID uuid.UUID) (run.ExpandedGroup, bool) {
	fanOutGroups := l.fanOutGroups
	fanOutGroupsMu := &l.fanOutGroupsMu
	fanOutGroupsMu.Lock()
	defer fanOutGroupsMu.Unlock()
	g, ok := fanOutGroups[taskID]
	return g, ok
}

func (l *localRun) runFannedGroup(
	taskID uuid.UUID,
	runner *atomRunner,
	taskModel *models.Task,
	group run.ExpandedGroup,
	outputEnv map[string]string,
	predOutputs map[string]map[string]string,
	predOutputsByID map[uuid.UUID]map[string]string,
) ([]uuid.UUID, error) {
	ctx := l.ctx
	j := l.j
	store := l.store
	runID := l.runID
	runQuarantined := l.runQuarantined
	maxParallel := l.maxParallel
	taskOutputs := l.taskOutputs
	taskHashes := l.taskHashes
	taskQuarantine := l.taskQuarantine
	if len(group.Instances) == 0 {
		return nil, nil
	}

	fo, decodeErr := jobdefruntime.DecodeFanOutConfig(nil)
	if taskModel != nil {
		if decoded, err := jobdefruntime.DecodeFanOutConfig(taskModel.FanOutConfig); err == nil {
			fo = decoded
		} else {
			decodeErr = err
		}
	}
	if decodeErr != nil {
		log.Warn("failed to decode fanOut config", "job_id", j.id, "task_id", taskID, "error", decodeErr)
	}

	envName := jobdefschema.DefaultFanOutEnv
	// An omitted failurePolicy is fail_fast, NOT continue. The schema
	// validator stamps that default onto the stored config
	// (pkg/jobdef/definition.go validateSteps) and the run owner normalizes
	// identically (run.normalizeFanOutFailurePolicy): only an explicit
	// "continue" continues, and anything else — "" or a value this build
	// does not recognize — fails the group fast. The three lanes must agree
	// here or a job that omits the key runs every sibling locally and
	// cancels them under the owner, which is the mode-dependent divergence
	// the plan's route-completeness contract exists to prevent.
	failurePolicy := jobdefschema.FanOutFailureFailFast
	groupParallel := maxParallel
	if fo != nil {
		if fo.Env != "" {
			envName = fo.Env
		}
		if fo.FailurePolicy == jobdefschema.FanOutFailureContinue {
			failurePolicy = jobdefschema.FanOutFailureContinue
		}
		// maxParallel caps the group; the job-level pool still bounds the
		// total, so the effective cap is the smaller of the two. Unset (0)
		// means "bounded only by the job".
		if fo.MaxParallel > 0 && fo.MaxParallel < groupParallel {
			groupParallel = fo.MaxParallel
		}
	}
	if groupParallel < 1 {
		groupParallel = 1
	}
	failFast := failurePolicy == jobdefschema.FanOutFailureFailFast

	// Static per-instance facts, keyed by TaskRun id. Statuses and readiness
	// come from the store, never from this map.
	type instanceMeta struct {
		partition  pkgtask.Partition
		maxAttempt int
	}
	// The attempt budget the ROW froze (task.Retries+1 at RegisterTasks),
	// which is what the distributed worker runs on (taskRun.MaxAttempts).
	// Reading taskModel.Retries here would give a retried run a different
	// budget per lane after a `job apply` changed `retries:`.
	maxAttempts := max(runner.maxAttempts, 1)
	meta := make(map[uuid.UUID]instanceMeta, len(group.Instances))
	for _, inst := range group.Instances {
		meta[inst.TaskRunID] = instanceMeta{partition: inst.Partition, maxAttempt: maxAttempts}
	}

	cacheCfg, hashArgs, predHashByID, err := l.resolveTaskCacheIdentity(taskID, taskModel, runner, outputEnv, predOutputs)
	if err != nil {
		return nil, err
	}
	taskQuarantined := taskQuarantine[taskID] || runQuarantined
	taskName := ""
	if taskModel != nil {
		taskName = taskModel.Name
	}

	type instanceResult struct {
		taskRunID uuid.UUID
		partition string
		output    map[string]string
		// skippedTasks are CATALOG task ids the store skipped downstream when
		// this instance resolved the group. They are what the run loop wants
		// back — never instance primary keys, which it would miscount as DAG
		// nodes in terminalTasks.
		skippedTasks []uuid.UUID
		// identityHash is this instance's own cache identity, collected so the
		// group can fold one aggregate hash for downstream steps.
		identityHash string
		// retry is set when the attempt failed but the instance has attempts
		// left; the row has already been reset to pending.
		retry bool
		// abort stops dispatch if even the pre-execution failure cannot be
		// persisted. Re-reading that pending row must never redrive it.
		abort bool
		err   error
	}

	var (
		byPartition = make(map[string]map[string]string)
		// skippedTaskIDs are catalog task ids to hand back to the run loop.
		skippedTaskIDs []uuid.UUID
		seenSkipped    = make(map[uuid.UUID]bool)
		hashByInstance = make(map[uuid.UUID]string, len(group.Instances))
		inFlight       int
		sawFailure     bool
		// rateLimitFailed records that acquiring a rate-limit token itself
		// errored (a store/limiter fault, not a rejection). It is separate
		// from sawFailure because a rejected acquisition is normal and a
		// broken one must not leave the group parked for a whole window
		// waiting on a decision nothing is going to make.
		rateLimitFailed bool
		firstErr        error
		results         = make(chan instanceResult, len(group.Instances))
		running         = make(map[uuid.UUID]bool, len(group.Instances))
	)

	// dispatch runs one attempt of one instance. It owns every terminal write
	// for that instance.
	dispatchInstance := func(taskRunID uuid.UUID, m instanceMeta, attempt int) {
		// A cancelled run must not start another partition attempt, for the
		// same reason the unfanned loop refuses one: the retry budget exists
		// for transient faults, and cancellation is not one. Checked here
		// because this closure is re-entered for attempt N+1 after
		// RetryTaskInstance, so a cancel that ended attempt N would
		// otherwise be what launches the next container.
		if err := ctx.Err(); err != nil {
			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: err}
			return
		}
		taskTimeout, timingErr := store.LocalTaskExecutionTimeout(ctx, runID, taskRunID)
		if timingErr != nil {
			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: timingErr}
			return
		}
		if taskTimeout == 0 {
			taskTimeout = runner.taskTimeout
		}
		failIdentity := func(cause error) {
			persistErr := store.FailTaskInstance(runID, taskRunID, cause)
			if persistErr != nil {
				cause = errors.Join(cause, errUnresolvedIdentityTerminalWrite, fmt.Errorf("persist partition failure: %w", persistErr))
			}
			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: cause, abort: persistErr != nil}
		}

		partEnv := map[string]string{
			envName: m.partition.Key,
		}
		if raw, err := m.partition.CanonicalJSON(); err == nil {
			partEnv[jobdefschema.FanOutPartitionJSONEnv] = string(raw)
		}
		extra := make(map[string]string, len(outputEnv)+len(partEnv))
		maps.Copy(extra, outputEnv)
		maps.Copy(extra, partEnv)

		// Per-partition identity: the shared args plus this instance's
		// partition fields. The partition env above is deliberately NOT part
		// of the hash — dependsOn rides inside CAESIUM_PARTITION_JSON and is
		// a scheduling instruction, not a data input.
		//
		// The identity is computed and persisted whether or not caching is
		// enabled: it is what makes a partition addressable to `caesium
		// receipt get`, `caesium why --partition` and `run retry
		// --partition`. Caching is only one consumer of it, and gates just
		// the lookup and the publish below.
		args := hashArgs
		args.Partition = m.partition.Key
		args.PartitionFingerprint = m.partition.Fingerprint
		args.PartitionAttributes = m.partition.Attributes
		hashInput := args
		inputHash := hashInput.Compute()
		hashInputBlob, blobErr := hashInput.CanonicalJSON(inputHash)
		if blobErr != nil {
			if args.UnresolvedImageIdentity != "" {
				failIdentity(fmt.Errorf("serialize unresolved image identity: %w", blobErr))
				return
			}
			log.Warn("failed to serialize hash-input blob", "task", taskName, "partition", m.partition.Key, "error", blobErr)
			hashInputBlob = nil
		}
		// SetTaskHashWithBlob resolves its second argument through
		// loadTaskRunByIDOrUnique, so the instance's TaskRun id addresses
		// exactly this row — the same primary-key-or-task-id contract
		// StartTask/SetTaskExitCode already take.
		if err := store.SetTaskHashWithBlob(runID, taskRunID, inputHash, args.ResolvedImageDigest, hashInputBlob); err != nil {
			if args.UnresolvedImageIdentity != "" {
				failIdentity(fmt.Errorf("persist unresolved image identity: %w", err))
				return
			}
			log.Warn("failed to persist partition hash", "task", taskName, "partition", m.partition.Key, "error", err)
		}
		if err := store.UpdateTaskExecutionDescriptorInputs(runID, taskRunID, predOutputsByID, predHashByID, inputHash, args.ResolvedImageDigest, hashInputBlob); err != nil {
			if args.UnresolvedImageIdentity != "" {
				failIdentity(fmt.Errorf("persist unresolved image execution descriptor: %w", err))
				return
			}
			log.Warn("failed to persist partition descriptor inputs", "task", taskName, "partition", m.partition.Key, "error", err)
		}

		// This cache-hit site — a FANNED INSTANCE resolving its OWN
		// per-partition result — is deliberately NOT gated like the two
		// producer-facing cache-hit sites in this file and in
		// internal/worker/runtime_executor.go (see F7,
		// run.Store.HasFanOutSuccessor): a fanned instance can never itself
		// be a fan-out PRODUCER, because pkg/jobdef/definition.go's step
		// validation rejects chained fan-out ("fanOut.from %q is itself a
		// fanOut step") for every job accepted through
		// internal/jobdef/importer.go, which is the only writer of
		// Task.FanOutConfig. So entry.Partitions is never consulted here,
		// and there is no downstream group this hit could silently collapse.
		// If chained fan-out is ever allowed, this invariant breaks and this
		// site needs the same gate.
		if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" {
			if attempt == 1 {
				if entry, found, err := l.getCacheStore().Get(inputHash); err != nil {
					log.Warn("cache lookup failed", "task", taskName, "partition", m.partition.Key, "error", err)
				} else if found {
					if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
						results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: cause}
						return
					}
					if !taskQuarantined {
						metrics.TaskCacheHitsTotal.WithLabelValues(j.alias, taskName).Inc()
					}
					cacheRes, cacheErr := store.CacheHitTask(runID, taskRunID, run.CacheHitSource{
						RunID:     entry.RunID,
						CreatedAt: entry.CreatedAt,
						ExpiresAt: entry.ExpiresAt,
					}, entry.Result, entry.Output, entry.BranchSelections)
					if cacheErr != nil {
						log.Error("failed to apply partition cache hit", "task", taskName, "partition", m.partition.Key, "error", cacheErr)
						// Fall through to normal execution.
					} else {
						var hitErr error
						if !run.IsSuccessfulTaskResult(entry.Result) {
							hitErr = fmt.Errorf("partition %q failed with cached result %q", m.partition.Key, entry.Result)
						}
						var hitSkipped []uuid.UUID
						if cacheRes != nil {
							hitSkipped = cacheRes.SkippedTaskIDs
						}
						results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, output: entry.Output, skippedTasks: hitSkipped, identityHash: inputHash, err: hitErr}
						return
					}
				} else if !taskQuarantined {
					metrics.TaskCacheMissesTotal.WithLabelValues(j.alias, taskName).Inc()
				}
			}
		}

		taskCtx := ctx
		cancel := func() {}
		if taskTimeout > 0 {
			taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
		}
		result, output, branches, _, metricsCapture, logSnapshot, execErr := l.executeAtom(taskCtx, taskID, taskRunID, attempt, taskTimeout, runner, extra)
		cancel()
		if execErr == nil {
			if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
				execErr = cause
			}
		}

		if execErr == nil {
			// Record violations on THIS INSTANCE's row. SaveSchemaViolations
			// refuses a catalog task id that resolves to N siblings and only
			// logs the refusal, so keying on the catalog task meant a fanned
			// step recorded nothing: fail mode lost the evidence for the
			// failure it was reporting, and warn mode opened an incident with
			// no row.
			//
			// The schema and its enforcement mode come from the FROZEN row
			// (runner), not from the live catalog task, matching
			// runtimeExecutor.runSchemaValidation. taskID is the catalog id
			// the row itself names, so no live-task lookup is needed and a
			// vanished catalog task can no longer skip validation the run was
			// registered to perform.
			if err := run.ValidateTaskOutputSchemaInstance(store, runID, taskID, taskRunID, output, runner.outputSchema, runner.schemaValidation); err != nil {
				execErr = err
			}
		}

		if execErr == nil {
			// Data-quality seam, beside schema validation and keyed on THIS
			// instance's row: a fanned step records its samples per
			// partition (see run.EvaluateDataAssertions).
			if err := run.EvaluateDataAssertions(ctx, store, runID, taskID, taskRunID, metricsCapture); err != nil {
				execErr = err
			}
		}

		if execErr != nil {
			_ = store.SaveCapturedTaskLogSnapshot(runID, taskRunID, logSnapshot)
			if run.IsRunDeadlineError(execErr) {
				// The run completion defer fails every unfinished row in one
				// transaction. Retrying or failing this instance here would apply
				// ordinary task policy first and turn siblings into skips.
				results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: execErr}
				return
			}
			// Retries cover execution errors only, matching the unfanned
			// local path: a container that ran and exited non-zero is a
			// terminal result, not a transient fault.
			if attempt < m.maxAttempt {
				if !taskQuarantined {
					metrics.TaskRetriesTotal.WithLabelValues(j.alias, taskID.String(), strconv.Itoa(attempt)).Inc()
				}
				if retryErr := store.RetryTaskInstance(runID, taskRunID, attempt+1); retryErr != nil {
					log.Error("failed to persist partition retry state", "run_id", runID, "partition", m.partition.Key, "error", retryErr)
				} else {
					log.Info("retrying partition", "job_id", j.id, "task_id", taskID, "partition", m.partition.Key, "attempt", attempt, "next_attempt", attempt+1, "error", execErr)
					results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, retry: true, err: execErr}
					return
				}
			}
			// FailTask persists the real cause on this instance's row and
			// runs the transitive in-group skip cascade. CompleteTaskInstance
			// would stamp the canned "command exited with non-zero status".
			if persistErr := store.FailTaskInstance(runID, taskRunID, execErr); persistErr != nil {
				log.Error("failed to persist partition failure", "run_id", runID, "partition", m.partition.Key, "error", persistErr)
			}
			log.Error("partition execution failed", "job_id", j.id, "task_id", taskID, "partition", m.partition.Key, "error", execErr)
			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: execErr}
			return
		}

		completeRes, completeErr := store.CompleteTaskInstance(taskRunID, result, output, branches, nil)
		if completeErr != nil {
			results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, err: completeErr}
			return
		}
		_ = store.SaveCapturedTaskLogSnapshot(runID, taskRunID, logSnapshot)
		var completeSkipped []uuid.UUID
		if completeRes != nil {
			completeSkipped = completeRes.SkippedTaskIDs
		}

		if !run.IsSuccessfulTaskResult(result) {
			results <- instanceResult{
				taskRunID:    taskRunID,
				partition:    m.partition.Key,
				skippedTasks: completeSkipped,
				err:          fmt.Errorf("partition %q failed with result %q", m.partition.Key, result),
			}
			return
		}

		// Publish the successful result so a later run of the same partition
		// set is a hit. Quarantined replays never publish.
		if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" && inputHash != "" {
			if taskQuarantined {
				log.Info("quarantined partition skipped cache publication", "task", taskName, "partition", m.partition.Key)
			} else {
				expiresAt := cache.EntryExpiry(time.Now(), cacheCfg.TTL, cacheCfg.TTLNever)
				if putErr := l.getCacheStore().Put(&cache.Entry{
					Hash:                inputHash,
					JobID:               j.id,
					TaskName:            taskName,
					Result:              result,
					Output:              output,
					BranchSelections:    branches,
					RunID:               runID,
					TaskRunID:           taskRunID,
					ResolvedImageDigest: hashArgs.ResolvedImageDigest,
					HashInputBlob:       hashInputBlob,
					CreatedAt:           time.Now(),
					ExpiresAt:           expiresAt,
				}); putErr != nil {
					log.Warn("failed to store partition cache entry", "task", taskName, "partition", m.partition.Key, "error", putErr)
				}
			}
		}

		results <- instanceResult{taskRunID: taskRunID, partition: m.partition.Key, output: output, skippedTasks: completeSkipped, identityHash: inputHash}
	}

	// absorb folds one reported instance result into the group's bookkeeping.
	// Shared by the loop and by the cancellation drain below so a cancelled
	// group still collects the identities and outputs of instances that DID
	// finish — the fan-in aggregate is rebuilt from them.
	absorb := func(res instanceResult) {
		if res.abort {
			firstErr = errors.Join(firstErr, res.err)
		}
		inFlight--
		delete(running, res.taskRunID)
		for _, id := range res.skippedTasks {
			if !seenSkipped[id] {
				seenSkipped[id] = true
				skippedTaskIDs = append(skippedTaskIDs, id)
			}
		}
		if res.identityHash != "" {
			hashByInstance[res.taskRunID] = res.identityHash
		}
	}

	for {
		rows, err := store.TaskRunInstances(ctx, runID, taskID)
		if err != nil {
			if ctx.Err() == nil {
				return skippedTaskIDs, err
			}
			// The run was cancelled out from under the loop, and this read
			// carries the run's context, so it fails before a single row has
			// been examined. Returning here is what left a cancelled run's
			// instances stranded even after the SWEEP was detached: the sweep
			// was never reached. Drain what is still in flight — those
			// containers' contexts are cancelled too, so they resolve
			// promptly — and fall through to the sweep, which runs detached
			// and is the only thing that will resolve what was never
			// dispatched.
			if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) || firstErr == nil {
				firstErr = cause
			}
			sawFailure = true
			for inFlight > 0 {
				absorb(<-results)
			}
			break
		}

		terminal := 0
		var ready []*run.TaskRun
		// rateLimitedUntil is the earliest moment a parked instance becomes
		// dispatchable again. RateLimitTask persists the deadline on the row,
		// so a parked instance is recognizable across loop passes without any
		// in-memory bookkeeping — the same way readiness is read, not tracked.
		var rateLimitedUntil time.Time
		noteRateLimited := func(at time.Time) {
			if at.IsZero() {
				return
			}
			if rateLimitedUntil.IsZero() || at.Before(rateLimitedUntil) {
				rateLimitedUntil = at
			}
		}
		now := time.Now().UTC()
		for _, row := range rows {
			if row == nil {
				continue
			}
			if run.IsTerminal(row.Status) {
				terminal++
				continue
			}
			if running[row.ID] {
				continue
			}
			// Readiness is the store's scalar, seeded at expansion and
			// decremented by each terminal sibling.
			if row.Status == run.TaskStatusPending && row.OutstandingPredecessors == 0 {
				if row.RateLimitRetryAfter != nil && row.RateLimitRetryAfter.After(now) {
					noteRateLimited(*row.RateLimitRetryAfter)
					continue
				}
				ready = append(ready, row)
			}
		}

		if terminal == len(rows) && inFlight == 0 {
			break
		}

		if !failFast || !sawFailure {
			for _, row := range ready {
				if inFlight >= groupParallel {
					break
				}
				m, ok := meta[row.ID]
				if !ok {
					// An instance row the expansion payload did not describe
					// cannot be executed safely; leave it to the store.
					continue
				}
				// One token per INSTANCE, acquired before the container is
				// launched and against the step's own rule. The token is not
				// taken for the group at dispatch time, so a `2 per minute`
				// rule admits two partitions a minute here exactly as it does
				// under the claimer and the owner dispatcher.
				acquired, retryAfter, rlErr := l.acquireRateLimitFor(taskID, row.ID, m.partition.Key)
				if rlErr != nil {
					if firstErr == nil {
						firstErr = rlErr
					}
					sawFailure = true
					rateLimitFailed = true
					break
				}
				if !acquired {
					noteRateLimited(retryAfter)
					continue
				}
				j.noteInstanceDispatched(row.ID)
				running[row.ID] = true
				inFlight++
				// Attempt is durable execution identity. On local re-entry the
				// row may already be pending at attempt N after a retry reset;
				// restarting at one would violate the secret-log fence and also
				// incorrectly repeat first-attempt cache behavior.
				go dispatchInstance(row.ID, m, max(row.Attempt, 1))
			}
		}

		if inFlight == 0 {
			if !rateLimitedUntil.IsZero() && !rateLimitFailed && (!failFast || !sawFailure) {
				// Every dispatchable instance is parked behind the rate-limit
				// window. Waiting here is what keeps the group alive: breaking
				// out would hand still-runnable partitions to the straggler
				// sweep, which resolves them as "never dispatched" and fails a
				// run whose only problem was that it was going too fast.
				wait := time.Until(rateLimitedUntil)
				if wait <= 0 {
					continue
				}
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) || firstErr == nil {
						firstErr = cause
					}
				case <-timer.C:
					timer.Stop()
					continue
				}
			}
			// Nothing running and nothing dispatchable. Either fail_fast has
			// tripped, or the remaining instances are blocked behind siblings
			// the store has already resolved. Either way the straggler sweep
			// after the loop resolves whatever is left.
			break
		}

		res := <-results
		absorb(res)

		if res.retry {
			// The row is pending again; the next loop pass re-reads it.
			continue
		}
		if res.err != nil {
			sawFailure = true
			if run.IsRunDeadlineError(res.err) || firstErr == nil {
				firstErr = res.err
			}
			if res.abort {
				for inFlight > 0 {
					absorb(<-results)
				}
				break
			}
			continue
		}
		if len(res.output) > 0 {
			byPartition[res.partition] = res.output
		}
	}

	// Run-timeout finalization owns every unfinished row in one transaction.
	// The generic straggler sweep below records skips and unrecorded outcomes;
	// doing that after the absolute deadline would hide which work the timeout
	// actually interrupted and leave those rows outside the atomic failure.
	if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
		return skippedTaskIDs, cause
	}
	if run.IsRunDeadlineError(firstErr) {
		return skippedTaskIDs, firstErr
	}

	// The group node is about to be reported terminal to the run loop, so no
	// instance row may be left non-terminal: an orphan pending row keeps the
	// run's own accounting (and any downstream group-status read) waiting on
	// something nothing will ever dispatch. Resolve stragglers explicitly and
	// say so, rather than leaving the run to time out. In local mode there is
	// no recovery owner to revisit the row later, so "leave it and let
	// recovery sort it out" is not an available option here.
	//
	// DO NOT "harmonize" the store primitive this calls with the one the
	// fail-fast cascade calls. SkipTaskInstance resolves any non-terminal row
	// (internal/run/store_instance.go); markInstanceSkippedTx, used by
	// failFastSkipSiblingsTx, is deliberately pending-only. They look like an
	// inconsistency and are not one: the guard belongs to the caller's
	// knowledge, not to the primitive. This sweep has drained every in-flight
	// instance and therefore KNOWS the containers are over; the SQL cascade
	// does not, because a distributed worker may still POST a completion, and
	// resolving a live row there would invite that worker to contradict it.
	// Same operation, opposite epistemic position, correctly different guards.
	//
	// The sweep runs on a DETACHED context, and that is the whole reason it
	// still works when it is needed most. It used to read through the run's
	// own ctx, so a cancelled run made the very first query return
	// context.Canceled and the sweep returned before resolving anything —
	// exactly the "stranded for good" outcome the paragraph above says is not
	// an available option in local mode. Cancellation is not a reason to skip
	// the cleanup; it is the most common reason to need it. The timeout keeps
	// a detached context from becoming an unbounded one if the DB is wedged.
	sweepCtx, cancelSweep := context.WithTimeout(context.WithoutCancel(ctx), fanOutSweepTimeout)
	defer cancelSweep()

	// Captured once, before the writes below, so every row in this sweep gets
	// the same explanation. ctx here is the RUN's context, not sweepCtx.
	runCancelled := ctx.Err() != nil

	rows, err := store.TaskRunInstances(sweepCtx, runID, taskID)
	if err != nil {
		return skippedTaskIDs, errors.Join(firstErr, err)
	}
	var stranded []string
	var unrecorded []string
	cancelledUnresolved := 0
	for _, row := range rows {
		if row == nil || run.IsTerminal(row.Status) {
			continue
		}
		// A row still RUNNING here is a different animal from a pending one.
		// Both loop exits above are gated on inFlight == 0 and the only path
		// past the bottom of the loop is `res := <-results`, so every
		// instance this group dispatched has already reported: the container
		// is provably over. Still-running therefore means the completion
		// WRITE failed, not that the work was cancelled — and the container
		// may well have SUCCEEDED. Blaming that row on the group's failure
		// policy puts a confident, wrong explanation on the one instance
		// whose outcome is genuinely unknown, and that string is what
		// `caesium run partitions` and `caesium why --partition` display.
		//
		// It is still RESOLVED rather than left alone: local mode has no
		// recovery owner to revisit it, so an unresolved row is stranded for
		// good and hangs the accounting this sweep exists to protect. Say the
		// true thing about it instead of leaving it.
		wasRunning := row.Status == run.TaskStatusRunning
		// A row still parked behind its rate-limit window is a third animal
		// again: it was dispatchable, it was deliberately held back, and the
		// run ended (cancelled, timed out) before the window rolled. Calling
		// that "never dispatched (unresolved in-group dependency)" sends the
		// reader hunting a dependsOn bug that does not exist.
		rateLimited := !wasRunning && row.RateLimitRetryAfter != nil && row.RateLimitRetryAfter.After(time.Now().UTC())
		// The status stays SKIPPED even for a cancelled run: these instances
		// never ran, so `failed` — what the unfanned local path stamps on the
		// task it was actually executing — would be a lie about work that
		// never started, and would drag the failure accounting and the
		// in-group cascade along with it. Only the REASON mirrors that path,
		// so both lanes read the same way in `caesium run partitions`.
		reason := "fan-out instance was never dispatched (unresolved in-group dependency)"
		switch {
		case wasRunning:
			reason = "fan-out instance outcome unrecorded: completion write failed"
		case failFast && sawFailure:
			reason = "fan-out group failed fast"
		case runCancelled && rateLimited:
			reason = fmt.Sprintf("fan-out instance was parked by the step's rate limit when the run was cancelled: %v", ctx.Err())
		case runCancelled:
			reason = fmt.Sprintf("fan-out instance cancelled before dispatch: %v", ctx.Err())
		case rateLimited:
			reason = "fan-out instance still parked by the step's rate limit when the run ended"
		}
		// Note this resolves an INSTANCE row; its id is deliberately not
		// added to skippedTaskIDs, which the run loop reads as catalog task
		// ids and counts against the DAG's node total.
		if skipErr := store.SkipTaskInstance(runID, row.ID, reason); skipErr != nil {
			return skippedTaskIDs, errors.Join(firstErr, skipErr)
		}
		switch {
		case wasRunning:
			unrecorded = append(unrecorded, row.PartitionValue)
		case runCancelled:
			// Counted, not "stranded": the run was cancelled, and reporting
			// these as unresolved partitions would bury the one fact that
			// explains all of them under a list of symptoms.
			cancelledUnresolved++
		case !failFast || !sawFailure:
			stranded = append(stranded, row.PartitionValue)
		}
	}
	// Logged unconditionally, unlike the stranded case below: a lost outcome
	// is worth surfacing whatever the failure policy, and gating it on
	// !failFast is how it stayed invisible.
	if len(unrecorded) > 0 {
		log.Error("fan-out instances completed without a recorded outcome", "job_id", j.id, "task_id", taskID, "partitions", unrecorded)
		if firstErr == nil {
			firstErr = fmt.Errorf("fan-out step %s left %d partition(s) with an unrecorded outcome: %v", taskID, len(unrecorded), unrecorded)
		}
	}
	if len(stranded) > 0 {
		log.Error("fan-out instances were never dispatched", "job_id", j.id, "task_id", taskID, "partitions", stranded)
		if firstErr == nil {
			firstErr = fmt.Errorf("fan-out step %s left %d partition(s) unresolved: %v", taskID, len(stranded), stranded)
		}
	}
	// Only when the cancellation actually left work unresolved. A group whose
	// every instance had already finished when the run was cancelled did not
	// fail, and manufacturing an error for it would turn a clean group into a
	// failed one on the way out.
	if cancelledUnresolved > 0 && firstErr == nil {
		firstErr = fmt.Errorf("fan-out step %s left %d partition(s) unresolved when the run was cancelled: %w",
			taskID, cancelledUnresolved, ctx.Err())
	}

	// Aggregate from the store so cache hits, skips and failures are all
	// reflected, not just what this loop executed.
	//
	// The instance ROWS are the aggregate's source of truth, not the
	// byPartition/hashByInstance maps above: those only ever describe the
	// instances THIS invocation dispatched. After a manual partition retry
	// (`caesium run retry --partition`) or a RetryFromFailure that preserved
	// the succeeded siblings, that is a single instance — so the rebuilt
	// fan-in aggregate reported PARTITION_COUNT=1 for an N-partition group
	// and the group identity hash folded one instance instead of N, re-keying
	// every downstream step purely because someone retried a partition.
	// Hydrating from the rows makes a retried run's aggregate and group hash
	// identical to a fresh run's.
	// sweepCtx, not ctx: this rebuild is the other half of the cleanup above
	// and is just as necessary on a cancelled run — a downstream step's
	// predecessor hashes and fan-in aggregate must not silently vanish
	// because the run's context died between the last instance and here.
	identities, err := store.FanOutInstanceIdentities(sweepCtx, runID, taskID)
	if err != nil {
		return skippedTaskIDs, errors.Join(firstErr, err)
	}
	succeeded, failed := 0, 0
	// groupHashes are the terminal-success instances' identities in
	// partition-index order (FanOutInstanceIdentities orders by
	// partition_index), the order run.GroupIdentityHash requires.
	var groupHashes []string
	for _, inst := range identities {
		switch inst.Status {
		case run.TaskStatusSucceeded, run.TaskStatusCached:
			succeeded++
			// The persisted (effective) identity is preferred over this
			// invocation's computed one so the value folded here is
			// byte-identical to what the SQL read path
			// (store.PredecessorHashes) folds for the same group. The
			// in-memory value is the fallback for the narrow case where
			// persisting the hash failed.
			h := inst.IdentityHash
			if h == "" {
				h = hashByInstance[inst.TaskRunID]
			}
			if h != "" {
				groupHashes = append(groupHashes, h)
			}
			if _, seen := byPartition[inst.PartitionValue]; !seen && len(inst.Output) > 0 {
				byPartition[inst.PartitionValue] = inst.Output
			}
		case run.TaskStatusFailed:
			failed++
		}
	}
	// An aggregate that does not fit MaxOutputBytes fails the GROUP rather
	// than publishing a partial contract: silently collapsing to the three
	// counters would drop every user key a downstream step reads.
	aggregate, aggErr := pkgtask.AggregateFanInOutputs(taskName, byPartition, succeeded, failed)
	if aggErr != nil {
		log.Error("failed to aggregate fan-in outputs", "job_id", j.id, "task_id", taskID, "error", aggErr)
		if firstErr == nil {
			firstErr = aggErr
		}
	} else {
		taskOutputs[taskID] = aggregate
	}

	// Publish ONE aggregate identity for the group so a downstream step folds
	// the fanned predecessor into its own cache key as a single
	// PredecessorHashes entry — the same value the SQL read path
	// (store.PredecessorHashes) computes for the distributed lane. Without
	// this the local lane contributed nothing for a fanned predecessor, so a
	// downstream step's identity was blind to its input changing.
	if h := run.GroupIdentityHash(groupHashes); h != "" {
		taskHashes[taskID] = h
	}

	return skippedTaskIDs, firstErr
}
