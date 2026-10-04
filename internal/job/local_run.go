package job

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/ratelimit"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/worker"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// localRun owns local execution state after durable registration. Its map
// references are shared with admitted workers; fan-out group access retains
// the mutex used by the original execution closures.
type localRun struct {
	ctx                         context.Context
	j                           *job
	store                       *run.Store
	snapshot, currentRun        *run.JobRun
	tasks                       models.Tasks
	atomsByTask                 map[uuid.UUID]*models.Atom
	tasksByID                   map[uuid.UUID]*models.Task
	runners                     map[uuid.UUID]*atomRunner
	taskOrder                   map[uuid.UUID]int
	triggerRuleByTask           map[uuid.UUID]string
	adjacency, predecessors     map[uuid.UUID][]uuid.UUID
	indegree                    map[uuid.UUID]int
	vars                        env.Environment
	secretResolver              secret.Resolver
	runID                       uuid.UUID
	runQuarantined              bool
	maxParallel                 int
	continueOnFailure           bool
	imageIdentityChecksRequired bool
	paramEnv                    map[string]string
	queue                       []uuid.UUID
	inQueue, processed          map[uuid.UUID]bool
	taskOutcomes                map[uuid.UUID]run.TaskStatus
	taskOutputs                 map[uuid.UUID]map[string]string
	taskHashes                  map[uuid.UUID]string
	taskQuarantine              map[uuid.UUID]bool
	taskAttempts                map[uuid.UUID]int
	terminalTasks               int
	deferred                    map[uuid.UUID]time.Time
	dispatched                  map[uuid.UUID]bool
	fanOutGroups                map[uuid.UUID]run.ExpandedGroup
	fanOutGroupsMu              sync.Mutex
	cacheStore                  *cache.Store
	cacheStoreOnce              sync.Once
	rateLimiter                 *ratelimit.Limiter
	taskPool                    *worker.Pool
	results                     chan taskResult
	active                      int
}

func (l *localRun) push(id uuid.UUID) {
	taskOrder := l.taskOrder
	inQueue := l.inQueue
	processed := l.processed
	if processed[id] || inQueue[id] {
		return
	}
	l.queue = append(l.queue, id)
	inQueue[id] = true
	slices.SortFunc(l.queue, func(a, b uuid.UUID) int {
		return cmp.Compare(taskOrder[a], taskOrder[b])
	})
}

func (l *localRun) propagateSkipped(start uuid.UUID) error {
	store := l.store
	triggerRuleByTask := l.triggerRuleByTask
	adjacency := l.adjacency
	predecessors := l.predecessors
	indegree := l.indegree
	runID := l.runID
	inQueue := l.inQueue
	processed := l.processed
	taskOutcomes := l.taskOutcomes
	queue := []uuid.UUID{start}
	seen := map[uuid.UUID]struct{}{start: {}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		for _, successor := range adjacency[current] {
			if processed[successor] {
				continue
			}
			if _, ok := indegree[successor]; !ok {
				continue
			}
			if indegree[successor] > 0 {
				indegree[successor]--
			}
			if indegree[successor] != 0 {
				continue
			}

			predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
			if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
				l.push(successor)
				continue
			}

			skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
			if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
				return err
			}

			taskOutcomes[successor] = run.TaskStatusSkipped
			processed[successor] = true
			l.terminalTasks++
			delete(inQueue, successor)

			if _, ok := seen[successor]; ok {
				continue
			}
			seen[successor] = struct{}{}
			queue = append(queue, successor)
		}
	}

	return nil
}

func (l *localRun) moveDueDeferred() bool {
	deferred := l.deferred
	now := time.Now().UTC()
	moved := false
	for taskID, retryAfter := range deferred {
		if retryAfter.After(now) {
			continue
		}
		delete(deferred, taskID)
		l.push(taskID)
		moved = true
	}
	return moved
}

func (l *localRun) nextDeferredAt() (time.Time, bool) {
	deferred := l.deferred
	var next time.Time
	for _, retryAfter := range deferred {
		if next.IsZero() || retryAfter.Before(next) {
			next = retryAfter
		}
	}
	return next, !next.IsZero()
}

// haltUnstarted is the local executor's half of the `halt` failure policy.
// The store resolves every not-yet-dispatched step whose trigger rule is not
// failure-tolerant as skipped (run.Store.HaltUnstartedTasks — the same
// primitive the distributed worker calls, so both lanes stamp the same rows
// with the same reason), and this brings the in-memory DAG into line: each
// skipped node is retired and its successors advanced through
// propagateSkipped, which is what releases an `all_done` cleanup that sits
// downstream of a halted branch. Nothing here touches `halt`: that flag is
// the hard stop for dispatch errors and cancellation, whereas a policy halt
// keeps the loop running for the tolerant successors it deliberately leaves
// dispatchable.
func (l *localRun) haltUnstarted(failedID uuid.UUID) error {
	store := l.store
	tasks := l.tasks
	runID := l.runID
	inQueue := l.inQueue
	processed := l.processed
	taskOutcomes := l.taskOutcomes
	deferred := l.deferred
	dispatched := l.dispatched
	candidates := make([]uuid.UUID, 0, len(tasks))
	for _, t := range tasks {
		if !processed[t.ID] && !dispatched[t.ID] {
			candidates = append(candidates, t.ID)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	skipped, err := store.HaltUnstartedTasks(runID, failedID, candidates)
	if err != nil {
		return err
	}
	for _, id := range skipped {
		if processed[id] {
			continue
		}
		taskOutcomes[id] = run.TaskStatusSkipped
		processed[id] = true
		l.terminalTasks++
		delete(inQueue, id)
		delete(deferred, id)
		if err := l.propagateSkipped(id); err != nil {
			return err
		}
	}
	l.queue = slices.DeleteFunc(l.queue, func(id uuid.UUID) bool { return processed[id] })
	return nil
}

// execute retains the scheduler's separate return and completion errors.
// A final unresolved-dependencies return must not become the completion cause.
func (l *localRun) execute(initialCompletionErr error) (returnErr error, completionErr error) {
	runErr := initialCompletionErr
	ctx := l.ctx
	j := l.j
	store := l.store
	snapshot := l.snapshot
	currentRun := l.currentRun
	tasks := l.tasks
	triggerRuleByTask := l.triggerRuleByTask
	adjacency := l.adjacency
	predecessors := l.predecessors
	indegree := l.indegree
	runID := l.runID
	maxParallel := l.maxParallel
	continueOnFailure := l.continueOnFailure
	inQueue := l.inQueue
	processed := l.processed
	taskOutcomes := l.taskOutcomes

	// A resumed per-partition retry executes the reset instance and whatever
	// its success releases — nothing else. Under the halt failure policy the
	// original engine's first failure resolved every not-yet-dispatched
	// intolerant step as skipped (haltUnstarted), so those cannot come back;
	// but a tolerant root the halt left dispatchable, or a row registered
	// before that sweep existed, can still sit pending with indegree 0, and
	// seeding it here would resurrect work on the back of an unrelated retry.
	// So when retry-reset instances exist, only the nodes that own one enter
	// the initial queue; their successors are released through the ordinary
	// in-loop path.
	retryOwners := make(map[uuid.UUID]struct{})
	if pendingRetries, err := store.PendingPartitionRetries(runID); err != nil {
		log.Warn("failed to read pending partition retries for re-entry", "run_id", runID, "error", err)
	} else {
		for i := range pendingRetries {
			retryOwners[pendingRetries[i].TaskID] = struct{}{}
		}
	}
	for _, taskState := range currentRun.Tasks {
		if processed[taskState.ID] {
			continue
		}
		if len(retryOwners) > 0 {
			if _, owns := retryOwners[taskState.ID]; !owns {
				continue
			}
		}
		if indegree[taskState.ID] == 0 {
			l.push(taskState.ID)
		}
	}

	if len(l.queue) == 0 && l.terminalTasks < len(tasks) {
		runErr = fmt.Errorf("job %s has no runnable tasks (verify DAG configuration)", j.id)
		return runErr, runErr
	}

	l.paramEnv = buildParamEnv(snapshot.ID, j.alias, snapshot.Params)

	l.fanOutGroups = make(map[uuid.UUID]run.ExpandedGroup)
	// fanOutGroups is written from whichever worker goroutine completes a
	// producer and read by whichever goroutine next runs a fanned step; the
	// two are only DAG-ordered relative to EACH OTHER, so an unrelated task
	// running concurrently makes the map a shared mutable. Every access after
	// the run loop starts goes through registerExpansion/lookupFanOutGroup.

	l.rehydrateFanOutGroups()

	// liveTaskCount is the number of DAG *nodes* the run must resolve, which is
	// the static task count: fan-out changes the TaskRun row count, never the
	// node count. A fanned step stays one node in adjacency/indegree here and is
	// collapsed back to one entry by convertRunModelWithDB, so both this guard
	// and waitForRunCompletion count in the same unit. The instance rows behind a
	// group are accounted for inside runFannedGroup, which does not return until
	// every one of them is terminal.
	liveTaskCount := len(tasks)

	var limiterOpts []ratelimit.Option
	if j.rateLimitClock != nil {
		limiterOpts = append(limiterOpts, ratelimit.WithClock(j.rateLimitClock))
	}
	l.rateLimiter = ratelimit.NewLimiter(store.DB(), limiterOpts...)

	l.taskPool = worker.NewPool(maxParallel)

	l.results = make(chan taskResult)
	l.active = 0
	halt := false
	l.deferred = make(map[uuid.UUID]time.Time)
	deferred := l.deferred
	// dispatched records every node handed to the pool. The halt sweep below
	// must never resolve one of these: its row is still `pending` in SQL until
	// executeAtom's StartTask, so the store cannot tell it from a node this loop
	// has not reached, and skipping it would record a container that ran as
	// skipped.
	l.dispatched = make(map[uuid.UUID]bool)

	for (!halt && (len(l.queue) > 0 || len(deferred) > 0)) || l.active > 0 {
		if !halt && l.moveDueDeferred() {
			continue
		}

		for !halt && l.active < maxParallel && len(l.queue) > 0 {
			taskID := l.queue[0]
			l.queue = l.queue[1:]
			delete(inQueue, taskID)

			if processed[taskID] {
				continue
			}

			if err := l.dispatchTask(taskID); err != nil {
				if runErr == nil {
					runErr = err
				}
				halt = true
				l.queue = l.queue[:0]
				break
			}
		}

		var result taskResult
		gotResult := false
		if halt && len(l.queue) == 0 && len(deferred) > 0 {
			if l.active == 0 {
				break
			}
			var ok bool
			result, ok = waitForHaltedDispatchResult(l.results, haltedDispatchWaitInterval)
			if !ok {
				continue
			}
			l.active--
			gotResult = true
		} else if len(l.queue) == 0 && len(deferred) > 0 {
			next, ok := l.nextDeferredAt()
			if !ok {
				continue
			}
			wait := time.Until(next)
			if wait <= 0 {
				continue
			}
			timer := time.NewTimer(wait)
			if l.active == 0 {
				select {
				case <-ctx.Done():
					timer.Stop()
					runErr = context.Cause(ctx)
					halt = true
					continue
				case <-timer.C:
					continue
				}
			}
			select {
			case result = <-l.results:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				l.active--
				gotResult = true
			case <-ctx.Done():
				timer.Stop()
				runErr = context.Cause(ctx)
				halt = true
				continue
			case <-timer.C:
				continue
			}
		}

		if !gotResult {
			if l.active == 0 {
				break
			}
			result = <-l.results
			l.active--
		}

		if processed[result.id] {
			continue
		}

		processed[result.id] = true
		l.terminalTasks++

		if result.err != nil {
			if errors.Is(result.err, errUnresolvedIdentityTerminalWrite) {
				// A storage failure left the predecessor's identity uncertain.
				// Drain admitted work but never dispatch downstream cache checks.
				halt = true
				l.queue = l.queue[:0]
			}
			taskOutcomes[result.id] = run.TaskStatusFailed
			if run.IsRunDeadlineError(result.err) {
				// A genuine run deadline is the final run classification even
				// when another independent task failed earlier under continue.
				runErr = result.err
				// CompleteIfActive below owns the atomic run-timeout transition.
				// Do not apply ordinary task-failure policy here: it would turn
				// unfinished siblings into skips just before that transition.
				halt = true
				l.queue = l.queue[:0]
				continue
			}
			if runErr == nil {
				runErr = result.err
			}
			// Under BOTH failure policies the failed node's descendants resolve
			// the same way: skip only the downstream tasks whose trigger rules
			// require all predecessors to succeed. Tasks with all_done,
			// all_failed, always or one_success rules are left to the normal
			// indegree path so they can still run / evaluate their own rule.
			// The policies differ only in what happens to everything ELSE, and
			// that is haltUnstarted's job at the end of this branch.
			skipReason := fmt.Sprintf("skipped due to failed dependency task %s", result.id)
			skipDescendantsFiltered(
				adjacency, predecessors, triggerRuleByTask,
				result.id, processed, inQueue,
				func(id uuid.UUID) {
					if err := store.SkipTask(runID, id, skipReason); err != nil {
						log.Error("failed to persist task skip", "run_id", runID, "task_id", id, "error", err)
						if runErr == nil {
							runErr = err
						}
						halt = true
						l.queue = l.queue[:0]
					}
					taskOutcomes[id] = run.TaskStatusSkipped
					processed[id] = true
					l.terminalTasks++
					delete(inQueue, id)
					if !halt {
						if propErr := l.propagateSkipped(id); propErr != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", id, "error", propErr)
							if runErr == nil {
								runErr = propErr
							}
							halt = true
							l.queue = l.queue[:0]
						}
					}
				},
			)

			// Decrement indegree for successors that were NOT skipped (they
			// have a failure-tolerant trigger rule). When their indegree
			// reaches 0, evaluate the rule and push or skip accordingly.
			if !halt {
				for _, successor := range adjacency[result.id] {
					if processed[successor] {
						continue
					}
					if _, ok := indegree[successor]; !ok {
						continue
					}
					if indegree[successor] > 0 {
						indegree[successor]--
					}
					if indegree[successor] == 0 {
						predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
						if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
							l.push(successor)
						} else {
							skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
							if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
								log.Error("failed to persist trigger rule skip", "run_id", runID, "task_id", successor, "error", err)
								if runErr == nil {
									runErr = err
								}
								halt = true
								l.queue = l.queue[:0]
								break
							}
							taskOutcomes[successor] = run.TaskStatusSkipped
							processed[successor] = true
							l.terminalTasks++
							delete(inQueue, successor)
							if err := l.propagateSkipped(successor); err != nil {
								log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
								if runErr == nil {
									runErr = err
								}
								halt = true
								l.queue = l.queue[:0]
								break
							}
						}
					}
				}
			}

			// `halt`: stop admitting new work. Every step not yet dispatched
			// whose rule is not failure-tolerant is resolved skipped, in the
			// store and here; the tolerant ones stay dispatchable and the loop
			// keeps running them (and whatever they release) to completion.
			// Work already dispatched finishes on its own. The run still ends
			// failed on runErr, whatever the cleanup steps then do.
			if !continueOnFailure && !halt {
				if err := l.haltUnstarted(result.id); err != nil {
					log.Error("failed to halt unstarted tasks", "run_id", runID, "task_id", result.id, "error", err)
					if runErr == nil {
						runErr = err
					}
					halt = true
					l.queue = l.queue[:0]
				}
			}
			continue
		}

		taskOutcomes[result.id] = run.TaskStatusSucceeded

		// Update local state for any tasks the run store skipped while
		// resolving branch filtering or trigger-rule evaluation.
		skippedSet := make(map[uuid.UUID]bool, len(result.skippedByBranch))
		for _, skippedID := range result.skippedByBranch {
			if processed[skippedID] {
				skippedSet[skippedID] = true
				continue
			}

			skippedSet[skippedID] = true
			taskOutcomes[skippedID] = run.TaskStatusSkipped
			processed[skippedID] = true
			l.terminalTasks++
			delete(inQueue, skippedID)

			if err := l.propagateSkipped(skippedID); err != nil {
				log.Error("failed to propagate skipped task", "run_id", runID, "task_id", skippedID, "error", err)
				if runErr == nil {
					runErr = err
				}
				halt = true
				l.queue = l.queue[:0]
				break
			}
		}

		if !halt {
			for _, successor := range adjacency[result.id] {
				if _, ok := indegree[successor]; !ok {
					continue
				}

				// Skip successors already handled by branch filtering in the store.
				if skippedSet[successor] {
					continue
				}
				// …and successors this loop already retired: a join the halt
				// sweep (or a failed sibling predecessor) skipped while this
				// predecessor was still running. Re-evaluating its rule here
				// would count it terminal a second time.
				if processed[successor] {
					continue
				}

				if indegree[successor] > 0 {
					indegree[successor]--
				}
				if indegree[successor] == 0 {
					predStatuses := collectPredecessorStatuses(predecessors[successor], taskOutcomes)
					if satisfiesTriggerRule(triggerRuleByTask[successor], predStatuses) {
						l.push(successor)
					} else {
						skipRuleReason := fmt.Sprintf("trigger rule %q not satisfied", triggerRuleByTask[successor])
						if err := store.SkipTask(runID, successor, skipRuleReason); err != nil {
							log.Error("failed to persist trigger rule skip", "run_id", runID, "task_id", successor, "error", err)
							if runErr == nil {
								runErr = err
							}
							halt = true
							l.queue = l.queue[:0]
							break
						}
						taskOutcomes[successor] = run.TaskStatusSkipped
						processed[successor] = true
						l.terminalTasks++
						delete(inQueue, successor)
						if err := l.propagateSkipped(successor); err != nil {
							log.Error("failed to propagate skipped task", "run_id", runID, "task_id", successor, "error", err)
							if runErr == nil {
								runErr = err
							}
							halt = true
							l.queue = l.queue[:0]
							break
						}
					}
				}
			}
		}
	}

	if l.terminalTasks != liveTaskCount {
		if runErr != nil {
			return runErr, runErr
		}
		return fmt.Errorf("job %s reached terminal state for %d of %d tasks; remaining tasks may be waiting on unresolved dependencies", j.id, l.terminalTasks, liveTaskCount), runErr
	}

	if runErr != nil {
		return runErr, runErr
	}

	return nil, runErr
}
