package job

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
)

func (l *localRun) runTask(taskID uuid.UUID) ([]uuid.UUID, error) {
	ctx := l.ctx
	j := l.j
	store := l.store
	tasksByID := l.tasksByID
	runners := l.runners
	predecessors := l.predecessors
	runID := l.runID
	runQuarantined := l.runQuarantined
	taskOutputs := l.taskOutputs
	taskHashes := l.taskHashes
	taskQuarantine := l.taskQuarantine
	taskAttempts := l.taskAttempts
	failIdentity := func(failure error) ([]uuid.UUID, error) {
		if err := store.FailTask(runID, taskID, failure); err != nil {
			return nil, errors.Join(failure, errUnresolvedIdentityTerminalWrite, fmt.Errorf("persist task identity failure: %w", err))
		}
		return nil, failure
	}
	runner := runners[taskID]
	if runner == nil {
		return nil, fmt.Errorf("missing runner for task %s", taskID)
	}
	// Build predecessor output env vars for this task.
	predOutputs := make(map[string]map[string]string)
	predOutputsByID := make(map[uuid.UUID]map[string]string)
	for _, predID := range predecessors[taskID] {
		if outputs, ok := taskOutputs[predID]; ok && len(outputs) > 0 {
			predOutputsByID[predID] = outputs
			stepName := ""
			if t := tasksByID[predID]; t != nil {
				stepName = t.Name
			}
			if stepName == "" {
				stepName = predID.String()
			}
			predOutputs[stepName] = outputs
		}
	}
	outputEnv, err := pkgtask.BuildOutputEnv(predOutputs)
	if err != nil {
		return nil, err
	}

	taskModel := tasksByID[taskID]
	taskQuarantined := taskQuarantine[taskID] || runQuarantined

	if group, ok := l.lookupFanOutGroup(taskID); ok && len(group.Instances) > 0 {
		return l.runFannedGroup(taskID, runner, taskModel, group, outputEnv, predOutputs, predOutputsByID)
	}
	taskTimeout, timingErr := store.LocalTaskExecutionTimeout(ctx, runID, taskID)
	if timingErr != nil {
		return nil, timingErr
	}
	if taskTimeout == 0 {
		taskTimeout = runner.taskTimeout
	}

	// Cache check — attempt to bypass container execution.
	var inputHash string
	// hashInputBlob is the canonical secret-redacted decomposition of the
	// HashInput; declared here (like inputHash) so it survives into the
	// success path where it is also written onto the cache Entry, letting a
	// cache hit be explained as well as a re-run.
	var hashInputBlob []byte

	// The same resolver every fan-out instance uses, so the two paths cannot
	// drift on which fields are folded into the cache key.
	cacheCfg, hashArgs, predHashByID, err := l.resolveTaskCacheIdentity(taskID, taskModel, runner, outputEnv, predOutputs)
	if err != nil {
		return nil, err
	}
	// resolvedImageDigest is the content digest folded into inputHash when
	// pinning is on; empty otherwise. Reused when the result is cached so
	// the cache Entry records which image content the hash covers.
	resolvedImageDigest := hashArgs.ResolvedImageDigest

	if cacheCfg.Enabled || hashArgs.UnresolvedImageIdentity != "" {
		cacheStore := l.getCacheStore()
		taskName := ""
		if taskModel != nil {
			taskName = taskModel.Name
		}

		hashInput := hashArgs
		inputHash = hashInput.Compute()
		// Serialize the decomposed input to a canonical, secret-redacted
		// blob so `caesium why` can later diff this run field-by-field. A
		// serialization failure is non-fatal: persist the hash without the
		// blob (a missing blob degrades `why` to digest-only, never wrong).
		blob, blobErr := hashInput.CanonicalJSON(inputHash)
		if blobErr != nil {
			if hashArgs.UnresolvedImageIdentity != "" {
				return failIdentity(fmt.Errorf("serialize unresolved image identity: %w", blobErr))
			}
			log.Warn("failed to serialize hash-input blob", "task", taskName, "error", blobErr)
			blob = nil
		}
		hashInputBlob = blob
		if err := store.SetTaskHashWithBlob(runID, taskID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
			if hashArgs.UnresolvedImageIdentity != "" {
				return failIdentity(fmt.Errorf("persist unresolved image identity: %w", err))
			}
			log.Warn("failed to persist task hash", "task", taskName, "error", err)
		}
		if err := store.UpdateTaskExecutionDescriptorInputs(runID, taskID, predOutputsByID, predHashByID, inputHash, resolvedImageDigest, hashInputBlob); err != nil {
			if hashArgs.UnresolvedImageIdentity != "" {
				return failIdentity(fmt.Errorf("persist unresolved image execution descriptor: %w", err))
			}
			log.Warn("failed to persist task execution descriptor inputs", "task", taskName, "error", err)
		}

		var entry *cache.Entry
		var found bool
		var err error
		if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" {
			entry, found, err = cacheStore.Get(inputHash)
		}
		// A cache entry with no recorded partition list (nil, not merely
		// empty — see cache.Entry.Partitions) is ambiguous for a task with a
		// downstream fan-out consumer: it might be a pre-fan-out entry (or
		// one written before the parser learned to record an explicit `[]`)
		// that never had the chance to record one. Treat that combination as
		// a MISS, exactly like `found` were false, so the producer runs once
		// more and backfills a real (possibly still-empty) list. Ordinary
		// tasks are unaffected: HasAnyFanOutConsumerForRun/HasFanOutSuccessor
		// are false for them, so a legitimately partition-less entry keeps
		// hitting as before.
		//
		// BOTH lookup errors below fail CLOSED — treat the hit as a MISS, exactly
		// as a cacheStore.Get error already does in this same block. An
		// inconclusive answer from either query would otherwise let an
		// unrecorded-partitions entry resolve "cached" on a run that DOES use
		// fan-out: the group expands to nothing and the consumer is silently
		// skipped via onEmpty — the exact collapse this gate exists to prevent.
		// The cost of failing closed is one extra execution of this task on a
		// rare transient read error; the cost of failing open is a silently
		// empty group. (The per-run pre-filter is still what keeps ordinary
		// hits cheap — it only changes what happens when that query ERRORS.)
		if found && entry.Partitions == nil {
			hasAnyFanOut, hafErr := store.HasAnyFanOutConsumerForRun(runID)
			switch {
			case hafErr != nil:
				log.Warn("cache: failed to check whether this run uses fan-out; treating the hit as unusable (fail closed)",
					"task", taskName, "hash", inputHash[:12], "error", hafErr)
				found = false
			case hasAnyFanOut:
				hasConsumer, hcErr := store.HasFanOutSuccessor(runID, taskID)
				switch {
				case hcErr != nil:
					log.Warn("cache: failed to check for a fan-out consumer; treating the hit as unusable (fail closed)",
						"task", taskName, "hash", inputHash[:12], "error", hcErr)
					found = false
				case hasConsumer:
					log.Info("cache: no partition list recorded on the cache entry; re-running producer to record one",
						"task", taskName, "hash", inputHash[:12])
					found = false
				}
			}
		}
		switch {
		case err != nil:
			log.Warn("cache lookup failed", "task", taskName, "error", err)
		case found:
			if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
				return nil, cause
			}
			if !taskQuarantined {
				metrics.TaskCacheHitsTotal.WithLabelValues(j.alias, taskName).Inc()
			}
			log.Info("cache hit", "task", taskName, "hash", inputHash[:12])

			// A fan-out producer's partition list is part of what its
			// execution produced, so it rides the cache entry and must be
			// replayed into the cache-hit transaction: without it the
			// producer resolves, the consumer's group never expands, and the
			// fanned step silently collapses to its unexpanded template row.
			cacheResult, cacheErr := applyCacheHit(store, runID, taskID, run.CacheHitSource{
				RunID:     entry.RunID,
				CreatedAt: entry.CreatedAt,
				ExpiresAt: entry.ExpiresAt,
			}, entry.Result, entry.Output, entry.BranchSelections, entry.Partitions)
			if cacheErr != nil {
				log.Error("failed to apply cache hit", "task", taskName, "error", cacheErr)
				// Fall through to normal execution.
			} else {
				l.registerExpansion(cacheResult)
				if len(entry.Output) > 0 {
					taskOutputs[taskID] = entry.Output
				}
				taskHashes[taskID] = inputHash
				var skipped []uuid.UUID
				if cacheResult != nil && len(cacheResult.SkippedTaskIDs) > 0 {
					skipped = cacheResult.SkippedTaskIDs
				}
				if !run.IsSuccessfulTaskResult(entry.Result) {
					return skipped, fmt.Errorf("task %s failed with cached result %q", taskID, entry.Result)
				}
				return skipped, nil
			}
		default:
			if cacheCfg.Enabled && !taskQuarantined {
				metrics.TaskCacheMissesTotal.WithLabelValues(j.alias, taskName).Inc()
			}
		}
	}

	// Frozen on the row, exactly as the distributed worker reads it — see
	// the identical note in runFannedGroup.
	maxAttempts := max(runner.maxAttempts, 1)

	var lastErr error
	for attempt := max(taskAttempts[taskID], 1); attempt <= maxAttempts; attempt++ {
		// A cancelled run must not start another attempt. The retry budget
		// is spent on transient failures, and a cancellation is not one:
		// without this the cancel that ended attempt N was itself the
		// trigger for attempt N+1 launching a fresh container on a run the
		// operator had already stopped. The delay-based select below only
		// covers retryDelay > 0, which is the default, so this is the check
		// that holds for a step with no delay configured.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		taskCtx := ctx
		cancel := func() {}
		if taskTimeout > 0 {
			taskCtx, cancel = context.WithTimeout(ctx, taskTimeout)
		}

		result, output, branchNames, partitions, metricsCapture, logSnapshot, execErr := l.executeAtom(taskCtx, taskID, uuid.Nil, attempt, taskTimeout, runner, outputEnv)
		cancel()
		if execErr == nil {
			if cause := context.Cause(ctx); run.IsRunDeadlineError(cause) {
				execErr = cause
			}
		}

		if execErr == nil {
			// Frozen row, not the live catalog - see the fanned twin above.
			// This also removes the nil dereference the row-built runner
			// exposed: taskModel is tasksByID[taskID], previously guaranteed
			// non-nil only because the runner map was itself built from the
			// live catalog. Keying on taskID keeps validation running for a
			// row whose catalog task has vanished, which a nil-taskModel
			// guard would instead silently skip.
			if err := run.ValidateTaskOutputSchema(store, runID, taskID, output, runner.outputSchema, runner.schemaValidation); err != nil {
				if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
					log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
				}
				execErr = err
			}
		}

		if execErr == nil {
			// Data-quality seam, beside schema validation. The unfanned
			// path has one row per (run, task), so the catalog task id
			// resolves it unambiguously.
			if err := run.EvaluateDataAssertions(ctx, store, runID, taskID, uuid.Nil, metricsCapture); err != nil {
				if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
					log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
				}
				execErr = err
			}
		}

		if execErr == nil {
			completeResult, completeErr := store.CompleteTaskWithPartitions(runID, taskID, result, output, branchNames, partitions)
			if completeErr != nil {
				return nil, completeErr
			}
			l.registerExpansion(completeResult)
			if snapshotErr := store.SaveCapturedTaskLogSnapshot(runID, taskID, logSnapshot); snapshotErr != nil {
				log.Warn("failed to persist task log snapshot", "job_id", j.id, "task_id", taskID, "error", snapshotErr)
			}
			if len(output) > 0 {
				taskOutputs[taskID] = output
			}

			// Keep uncertain identity available to downstream tasks even though
			// this execution cannot publish a cache entry or short-circuit.
			if inputHash != "" {
				taskHashes[taskID] = inputHash
			}

			// Store successful result in cache, reusing the hash computed earlier.
			if cacheCfg.Enabled && hashArgs.UnresolvedImageIdentity == "" && inputHash != "" && run.IsSuccessfulTaskResult(result) {
				cacheStore := l.getCacheStore()
				taskName := ""
				if taskModel != nil {
					taskName = taskModel.Name
				}

				if taskQuarantined {
					taskHashes[taskID] = inputHash
					log.Info("quarantined task skipped cache publication", "task", taskName, "hash", inputHash[:12])
				} else {
					// Value-verified short-circuit (D2): this task re-executed
					// because its OWN identity hash (inputHash) changed. If it
					// produced output byte-identical to a prior successful run,
					// present that prior run's identity to downstream consumers so
					// a downstream whose only changed input was this step stays a
					// cache hit instead of re-running. The substitution only
					// happens when content equality is PROVEN (see
					// cache.EquivalentPriorHash); on any uncertainty it returns
					// inputHash unchanged (re-run downstream — always safe). The
					// proof reads priors filtered to exclude inputHash, so the
					// order relative to the Put below does not matter.
					effectiveHash := inputHash
					if priors, priorErr := cacheStore.PriorEntriesByTask(j.id, taskName, inputHash); priorErr != nil {
						log.Warn("short-circuit: failed to load prior entries", "task", taskName, "error", priorErr)
					} else {
						effectiveHash = cache.EquivalentPriorHash(inputHash, output, priors)
					}
					// taskHashes drives the in-memory predHashes a downstream task
					// folds into its own key; storing the effective (possibly
					// prior) identity is what stops the cascade locally.
					taskHashes[taskID] = effectiveHash
					if effectiveHash != inputHash {
						metrics.TaskCacheShortCircuitsTotal.WithLabelValues(j.alias, taskName).Inc()
						log.Info("value-verified short-circuit", "task", taskName, "new_hash", inputHash[:12], "effective_hash", effectiveHash[:12])
						if scErr := store.SetTaskEffectiveHash(runID, taskID, effectiveHash); scErr != nil {
							log.Warn("short-circuit: failed to persist effective hash", "task", taskName, "error", scErr)
						}
					}

					expiresAt := cache.EntryExpiry(time.Now(), cacheCfg.TTL, cacheCfg.TTLNever)
					if putErr := cacheStore.Put(&cache.Entry{
						Hash:             inputHash,
						JobID:            j.id,
						TaskName:         taskName,
						Result:           result,
						Output:           output,
						BranchSelections: branchNames,
						// A fan-out producer's emitted partition list is part
						// of what its execution produced: without it a cache
						// hit on the producer would resolve the step without
						// ever expanding its consumer's group.
						Partitions:          partitions,
						RunID:               runID,
						TaskRunID:           taskID,
						ResolvedImageDigest: resolvedImageDigest,
						HashInputBlob:       hashInputBlob,
						CreatedAt:           time.Now(),
						ExpiresAt:           expiresAt,
					}); putErr != nil {
						log.Warn("failed to store cache entry", "task", taskName, "error", putErr)
					}
				}
			}

			var skipped []uuid.UUID
			if completeResult != nil && len(completeResult.SkippedTaskIDs) > 0 {
				skipped = completeResult.SkippedTaskIDs
			}
			if !run.IsSuccessfulTaskResult(result) {
				return skipped, fmt.Errorf("task %s failed with result %q", taskID, result)
			}
			return skipped, nil
		}
		if run.IsRunDeadlineError(execErr) {
			// Preserve the typed cause for the run loop. CompleteIfActive in
			// the run completion defer owns the atomic all-task transition.
			return nil, execErr
		}
		lastErr = execErr

		// No more attempts — mark as permanently failed.
		if attempt >= maxAttempts {
			break
		}

		// Compute retry delay.
		delay := computeRetryDelay(taskModel, attempt)

		log.Info("retrying task", "job_id", j.id, "task_id", taskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

		if !taskQuarantined {
			metrics.TaskRetriesTotal.WithLabelValues(j.alias, taskID.String(), strconv.Itoa(attempt)).Inc()
		}

		if err := store.RetryTask(runID, taskID, attempt+1); err != nil {
			log.Error("failed to persist task retry state", "run_id", runID, "task_id", taskID, "error", err)
		}

		if delay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}
	}

	if persistErr := store.FailTask(runID, taskID, lastErr); persistErr != nil {
		log.Error("failed to persist task failure", "run_id", runID, "task_id", taskID, "error", persistErr)
	}
	return nil, lastErr
}

func (l *localRun) dispatchTask(taskID uuid.UUID) error {
	ctx := l.ctx
	deferred := l.deferred
	dispatched := l.dispatched
	// An EXPANDED fan-out step acquires its tokens per instance inside
	// runFannedGroup, not once here for the whole group. Acquiring here would
	// be wrong twice over: one token would admit all N partitions, and the
	// rejection path would park the row by catalog task id — which names N
	// rows, so RateLimitTask returns ErrAmbiguousTaskRun and this dispatch
	// error halts the entire run.
	if group, ok := l.lookupFanOutGroup(taskID); !ok || len(group.Instances) == 0 {
		acquired, retryAfter, err := l.acquireRateLimitFor(taskID, taskID, "")
		if err != nil {
			return err
		}
		if !acquired {
			deferred[taskID] = retryAfter
			return nil
		}
	}

	l.active++
	if err := l.taskPool.Submit(ctx, func() {
		skipped, err := l.runTask(taskID)
		l.results <- taskResult{id: taskID, err: err, skippedByBranch: skipped}
	}); err != nil {
		l.active--
		return err
	}
	dispatched[taskID] = true
	return nil
}
