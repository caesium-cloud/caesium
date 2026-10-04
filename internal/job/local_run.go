package job

import (
	"context"
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
