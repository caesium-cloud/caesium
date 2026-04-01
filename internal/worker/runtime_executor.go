package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	taskFailurePolicyHalt     = "halt"
	taskFailurePolicyContinue = "continue"
	defaultLeaseRenewInterval = 1 * time.Second
	minLeaseRenewInterval     = 1 * time.Second
)

type runtimeExecutor struct {
	store             *run.Store
	taskTimeout       time.Duration
	workerLeaseTTL    time.Duration
	continueOnFailure bool
}

func NewRuntimeExecutor(store *run.Store, taskTimeout, workerLeaseTTL time.Duration, failurePolicy string) TaskExecutor {
	if store == nil {
		panic("runtime executor requires run store")
	}

	return (&runtimeExecutor{
		store:             store,
		taskTimeout:       taskTimeout,
		workerLeaseTTL:    workerLeaseTTL,
		continueOnFailure: normalizeTaskFailurePolicy(failurePolicy) == taskFailurePolicyContinue,
	}).Execute
}

func (e *runtimeExecutor) Execute(ctx context.Context, taskRun *models.TaskRun) {
	if taskRun == nil {
		return
	}

	jobAlias := ""
	resolveJobAlias := func() string {
		if jobAlias != "" {
			return jobAlias
		}

		var result struct {
			Alias string
		}
		if err := e.store.DB().
			Table("job_runs").
			Select("jobs.alias AS alias").
			Joins("join jobs on jobs.id = job_runs.job_id").
			Where("job_runs.id = ?", taskRun.JobRunID).
			Take(&result).Error; err == nil && strings.TrimSpace(result.Alias) != "" {
			jobAlias = result.Alias
			return jobAlias
		}

		jobAlias = "unknown"
		return jobAlias
	}

	// Load the task model to get retry configuration.
	var taskModel models.Task
	hasTaskModel := e.store.DB().First(&taskModel, "id = ?", taskRun.TaskID).Error == nil

	// Cache check: attempt to satisfy the task from cache before container execution.
	cacheConfig := cache.ConfigFromEnv()
	var cacheStore *cache.Store
	var cacheHash string
	if hasTaskModel {
		cacheStore = cache.NewStore(e.store.DB())

		// Look up job alias for hash computation.
		cacheJobAlias := resolveJobAlias()

		// Fetch predecessor outputs for hash input.
		predOutputs, predErr := e.store.PredecessorOutputs(taskRun.JobRunID, taskRun.TaskID)
		if predErr != nil {
			log.Warn("cache: failed to query predecessor outputs", "task_id", taskRun.TaskID, "error", predErr)
		}
		predHashes, predHashErr := e.store.PredecessorHashes(taskRun.JobRunID, taskRun.TaskID)
		if predHashErr != nil {
			log.Warn("cache: failed to query predecessor hashes", "task_id", taskRun.TaskID, "error", predHashErr)
		}

		// Fetch run params from the job run record.
		var runParams map[string]string
		var jobRun models.JobRun
		if err := e.store.DB().Select("params").First(&jobRun, "id = ?", taskRun.JobRunID).Error; err == nil && len(jobRun.Params) > 0 {
			_ = json.Unmarshal(jobRun.Params, &runParams)
		}

		// Load atom spec for env/workdir/mounts to match local execution hash.
		var atomSpec container.Spec
		var atomModel models.Atom
		if err := e.store.DB().First(&atomModel, "id = ?", taskRun.AtomID).Error; err == nil && len(atomModel.Spec) > 0 {
			_ = json.Unmarshal(atomModel.Spec, &atomSpec)
		}

		// Resolve step-level and job-level cache config.
		var stepCache, jobCache interface{}
		if len(taskModel.CacheConfig) > 0 {
			_ = json.Unmarshal(taskModel.CacheConfig, &stepCache)
		}
		var jobModel models.Job
		if err := e.store.DB().Select("cache_config").First(&jobModel, "id = ?", taskModel.JobID).Error; err == nil && len(jobModel.CacheConfig) > 0 {
			_ = json.Unmarshal(jobModel.CacheConfig, &jobCache)
		}
		cacheCfg := jobdefschema.ResolveCacheConfig(stepCache, jobCache, cacheConfig.Enabled, cacheConfig.TTL)
		if !cacheCfg.Enabled {
			cacheStore = nil
		} else {
			// Build merged env for hashing, excluding volatile per-run vars.
			mergedEnv := make(map[string]string, len(atomSpec.Env))
			for k, v := range atomSpec.Env {
				mergedEnv[k] = v
			}
			if outputEnv := pkgtask.BuildOutputEnv(predOutputs); len(outputEnv) > 0 {
				for k, v := range outputEnv {
					mergedEnv[k] = v
				}
			}

			hashInput := cache.HashInput{
				JobAlias:           cacheJobAlias,
				TaskName:           taskModel.Name,
				Image:              taskRun.Image,
				Command:            parseTaskCommand(taskRun.Command),
				Env:                mergedEnv,
				WorkDir:            atomSpec.WorkDir,
				Mounts:             atomSpec.Mounts,
				PredecessorHashes:  predHashes,
				PredecessorOutputs: predOutputs,
				RunParams:          runParams,
				CacheVersion:       cacheCfg.Version,
			}
			cacheHash = hashInput.Compute()
		}

		if cacheStore != nil {
			entry, found, getErr := cacheStore.Get(cacheHash)
			if getErr != nil {
				log.Warn("cache: lookup failed", "task_id", taskRun.TaskID, "hash", cacheHash, "error", getErr)
			} else if found {
				log.Info("cache hit for worker task", "task_id", taskRun.TaskID, "hash", cacheHash, "cached_run_id", entry.RunID)
				if err := e.store.CacheHitTaskClaimed(taskRun.JobRunID, taskRun.TaskID, run.CacheHitSource{
					RunID:     entry.RunID,
					CreatedAt: entry.CreatedAt,
					ExpiresAt: entry.ExpiresAt,
				}, entry.Result, taskRun.ClaimedBy, entry.Output, entry.BranchSelections); err != nil {
					if errors.Is(err, run.ErrTaskClaimMismatch) {
						log.Info("worker task claim changed during cache hit", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
						return
					}
					log.Error("cache: failed to persist cache hit", "task_id", taskRun.TaskID, "error", err)
					// Fall through to normal execution on persistence failure.
				} else {
					metrics.TaskCacheHitsTotal.WithLabelValues(cacheJobAlias, taskModel.Name).Inc()
					return
				}
			}
		}
	}

	maxAttempts := taskRun.MaxAttempts
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	currentAttempt := taskRun.Attempt
	if currentAttempt < 1 {
		currentAttempt = 1
	}

	var lastErr error
	for attempt := currentAttempt; attempt <= maxAttempts; attempt++ {
		execErr := e.executeTask(ctx, taskRun)
		if execErr == nil {
			// Store successful result in cache.
			if cacheStore != nil && cacheHash != "" {
				e.storeCacheEntry(cacheStore, cacheConfig, cacheHash, taskRun)
			}
			return
		}

		if errors.Is(execErr, run.ErrTaskClaimMismatch) {
			log.Info("worker task claim changed; skipping execution result", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}

		if errors.Is(execErr, context.Canceled) {
			log.Info("worker task canceled", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}

		lastErr = execErr

		// No more attempts — break to failure handling.
		if attempt >= maxAttempts {
			break
		}

		// Compute retry delay (retryDelay * 2^(attempt-1) if backoff, else retryDelay).
		var delay time.Duration
		if hasTaskModel && taskModel.RetryDelay > 0 {
			delay = taskModel.RetryDelay
			if taskModel.RetryBackoff {
				delay = taskModel.RetryDelay * (1 << uint(attempt-1))
			}
		}

		log.Info("retrying worker task", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "attempt", attempt, "next_attempt", attempt+1, "delay", delay, "error", lastErr)

		metrics.TaskRetriesTotal.WithLabelValues(resolveJobAlias(), taskRun.TaskID.String(), strconv.Itoa(attempt)).Inc()

		if retryErr := e.store.RetryTaskClaimed(taskRun.JobRunID, taskRun.TaskID, attempt+1, taskRun.ClaimedBy); retryErr != nil {
			if errors.Is(retryErr, run.ErrTaskClaimMismatch) {
				log.Info("worker task claim changed before retry persistence", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
				return
			}
			log.Error("failed to persist worker task retry state", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", retryErr)
		}

		// Update local attempt counter and sleep while renewing the lease.
		taskRun.Attempt = attempt + 1
		if delay > 0 {
			e.sleepRenewingLease(ctx, taskRun, delay)
		}

		if ctx.Err() != nil {
			return
		}
	}

	if persistErr := e.store.FailTaskClaimed(taskRun.JobRunID, taskRun.TaskID, lastErr, taskRun.ClaimedBy); persistErr != nil {
		if errors.Is(persistErr, run.ErrTaskClaimMismatch) {
			log.Info("worker task claim changed before failure persistence", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
			return
		}
		log.Error("failed to persist worker task failure", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", persistErr)
	}

	if !e.continueOnFailure {
		return
	}

	descendants, descErr := collectDescendantsFromEdges(e.store.DB(), taskRun.TaskID)
	if descErr != nil {
		log.Error("failed to collect descendant tasks", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", descErr)
		return
	}

	reason := fmt.Sprintf("skipped due to failed dependency task %s", taskRun.TaskID)
	for _, taskID := range descendants {
		if skipErr := e.store.SkipTask(taskRun.JobRunID, taskID, reason); skipErr != nil {
			log.Error("failed to persist skipped descendant task", "run_id", taskRun.JobRunID, "task_id", taskID, "error", skipErr)
		}
	}
}

// sleepRenewingLease sleeps for the given duration while periodically renewing the task lease.
func (e *runtimeExecutor) sleepRenewingLease(ctx context.Context, taskRun *models.TaskRun, delay time.Duration) {
	renewInterval := leaseRenewInterval(e.workerLeaseTTL)
	deadline := time.Now().Add(delay)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}

		next := remaining
		if renewInterval > 0 && renewInterval < next {
			next = renewInterval
		}

		timer := time.NewTimer(next)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}

		if time.Now().Before(deadline) {
			if err := e.renewLease(taskRun); err != nil {
				log.Error("failed to renew worker task lease during retry delay", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", err)
			}
		}
	}
}

func (e *runtimeExecutor) executeTask(ctx context.Context, taskRun *models.TaskRun) error {
	taskCtx := ctx
	cancel := func() {}
	if e.taskTimeout > 0 {
		taskCtx, cancel = context.WithTimeout(ctx, e.taskTimeout)
	}
	defer cancel()

	engine, err := newEngine(taskCtx, taskRun.Engine)
	if err != nil {
		return err
	}

	command := parseTaskCommand(taskRun.Command)
	atomName := fmt.Sprintf("%s-%s", taskRun.TaskID, taskRun.JobRunID)
	if taskRun.ClaimAttempt > 0 {
		atomName = fmt.Sprintf("%s-attempt%d", atomName, taskRun.ClaimAttempt)
	}

	// Query predecessor outputs from the DB and build env vars.
	spec := container.Spec{}
	predOutputs, predErr := e.store.PredecessorOutputs(taskRun.JobRunID, taskRun.TaskID)
	if predErr != nil {
		log.Warn("failed to query predecessor outputs", "task_id", taskRun.TaskID, "error", predErr)
	}
	if outputEnv := pkgtask.BuildOutputEnv(predOutputs); len(outputEnv) > 0 {
		spec.Env = outputEnv
	}

	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:    atomName,
		Image:   taskRun.Image,
		Command: command,
		Spec:    spec,
	})
	if err != nil {
		return err
	}

	if err := e.store.StartTaskClaimed(taskRun.JobRunID, taskRun.TaskID, a.ID(), taskRun.ClaimedBy); err != nil {
		return err
	}

	if err := e.monitorTask(taskCtx, taskRun, engine, a); err != nil {
		return err
	}

	// Parse structured task output and branch markers in a single pass
	// over the log stream (no full buffering).
	var taskOutput map[string]string
	var branchSelections []string
	var logSnapshot *run.TaskLogSnapshot
	logs, logErr := engine.Logs(&atom.EngineLogsRequest{ID: a.ID()})
	if logErr == nil {
		markers, parseErr := pkgtask.CaptureMarkers(logs, pkgtask.MaxLogSnapshotBytes)
		if closeErr := logs.Close(); closeErr != nil {
			log.Warn("failed to close log stream", "task_id", taskRun.TaskID, "error", closeErr)
		}
		if parseErr != nil {
			log.Warn("failed to parse task markers", "task_id", taskRun.TaskID, "error", parseErr)
		} else if markers != nil {
			taskOutput = markers.Output
			if len(markers.Branches) > 0 {
				branchSelections = markers.Branches
			}
			if markers.LogText != "" || markers.LogTruncated {
				logSnapshot = &run.TaskLogSnapshot{
					Text:      markers.LogText,
					Truncated: markers.LogTruncated,
				}
			}
		}
	}

	// Runtime schema validation: if the task declares an outputSchema and the job has
	// schemaValidation enabled, validate the actual output against the schema.
	if err := e.runSchemaValidation(taskRun, taskOutput); err != nil {
		return err
	}

	if err := e.store.CompleteTaskClaimed(taskRun.JobRunID, taskRun.TaskID, string(a.Result()), taskRun.ClaimedBy, taskOutput, branchSelections); err != nil {
		return err
	}
	if err := e.store.SaveTaskLogSnapshot(taskRun.JobRunID, taskRun.TaskID, logSnapshot); err != nil {
		log.Warn("failed to persist task log snapshot", "task_id", taskRun.TaskID, "error", err)
	}
	if !run.IsSuccessfulTaskResult(string(a.Result())) {
		return fmt.Errorf("task %s failed with result %q", taskRun.TaskID, a.Result())
	}

	return nil
}

func (e *runtimeExecutor) runSchemaValidation(taskRun *models.TaskRun, output map[string]string) error {
	if taskRun == nil {
		return nil
	}
	return run.ValidateTaskOutputSchema(e.store, taskRun.JobRunID, taskRun.TaskID, output, taskRun.OutputSchema, taskRun.SchemaValidation)
}

// storeCacheEntry reads back the completed task run and stores the result in the cache.
func (e *runtimeExecutor) storeCacheEntry(cacheStore *cache.Store, cacheConfig cache.Config, hash string, taskRun *models.TaskRun) {
	// Read back the completed task run to get output and result.
	var completed models.TaskRun
	if err := e.store.DB().Where("job_run_id = ? AND task_id = ?", taskRun.JobRunID, taskRun.TaskID).First(&completed).Error; err != nil {
		log.Warn("cache: failed to read completed task run for caching", "task_id", taskRun.TaskID, "error", err)
		return
	}

	// Only cache successful results.
	if !run.IsSuccessfulTaskResult(completed.Result) {
		return
	}

	// Resolve the job ID from the job run.
	var jobRun models.JobRun
	if err := e.store.DB().Select("job_id").First(&jobRun, "id = ?", taskRun.JobRunID).Error; err != nil {
		log.Warn("cache: failed to look up job ID for caching", "run_id", taskRun.JobRunID, "error", err)
		return
	}

	// Resolve task name.
	var taskModel models.Task
	if err := e.store.DB().Select("name").First(&taskModel, "id = ?", taskRun.TaskID).Error; err != nil {
		log.Warn("cache: failed to look up task name for caching", "task_id", taskRun.TaskID, "error", err)
		return
	}

	// Decode output and branch selections from JSON.
	var output map[string]string
	if len(completed.Output) > 0 {
		_ = json.Unmarshal(completed.Output, &output)
	}
	var branchSelections []string
	if len(completed.BranchSelections) > 0 {
		_ = json.Unmarshal(completed.BranchSelections, &branchSelections)
	}

	entry := &cache.Entry{
		Hash:             hash,
		JobID:            jobRun.JobID,
		TaskName:         taskModel.Name,
		Result:           completed.Result,
		Output:           output,
		BranchSelections: branchSelections,
		RunID:            taskRun.JobRunID,
		TaskRunID:        completed.ID,
		CreatedAt:        time.Now().UTC(),
	}

	if cacheConfig.TTL > 0 {
		expiresAt := entry.CreatedAt.Add(cacheConfig.TTL)
		entry.ExpiresAt = &expiresAt
	}

	if err := cacheStore.Put(entry); err != nil {
		log.Warn("cache: failed to store entry", "task_id", taskRun.TaskID, "hash", hash, "error", err)
	} else {
		log.Info("cache: stored entry for worker task", "task_id", taskRun.TaskID, "hash", hash)
	}
}

func (e *runtimeExecutor) monitorTask(ctx context.Context, taskRun *models.TaskRun, engine atom.Engine, a atom.Atom) error {
	ticker := time.NewTicker(leaseRenewInterval(e.workerLeaseTTL))
	defer ticker.Stop()

	waitResult := make(chan struct {
		atom atom.Atom
		err  error
	}, 1)
	go func() {
		next, err := engine.Wait(&atom.EngineWaitRequest{ID: a.ID(), Context: ctx})
		waitResult <- struct {
			atom atom.Atom
			err  error
		}{atom: next, err: err}
	}()

	for {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				if stopErr := engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true}); stopErr != nil {
					return fmt.Errorf("task %s timed out after %s and failed to stop atom %s: %w", taskRun.TaskID, e.taskTimeout, a.ID(), stopErr)
				}
				return fmt.Errorf("task %s timed out after %s", taskRun.TaskID, e.taskTimeout)
			}
			return ctx.Err()
		case result := <-waitResult:
			if result.err != nil {
				return result.err
			}
			a = result.atom
			return engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true})
		case <-ticker.C:
			if err := e.renewLease(taskRun); err != nil {
				log.Error("failed to renew worker task lease", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", err)
			}
		}
	}
}

func (e *runtimeExecutor) renewLease(taskRun *models.TaskRun) error {
	if taskRun == nil || e.workerLeaseTTL <= 0 || strings.TrimSpace(taskRun.ClaimedBy) == "" {
		return nil
	}

	nextExpiry := time.Now().UTC().Add(e.workerLeaseTTL)
	return e.store.DB().Model(&models.TaskRun{}).
		Where("id = ? AND claimed_by = ?", taskRun.ID, taskRun.ClaimedBy).
		Update("claim_expires_at", nextExpiry).Error
}

func leaseRenewInterval(leaseTTL time.Duration) time.Duration {
	if leaseTTL <= 0 {
		return defaultLeaseRenewInterval
	}

	interval := leaseTTL / 2
	if interval < minLeaseRenewInterval {
		return minLeaseRenewInterval
	}
	return interval
}

func newEngine(ctx context.Context, engineType models.AtomEngine) (atom.Engine, error) {
	switch engineType {
	case models.AtomEngineDocker:
		return docker.NewEngine(ctx), nil
	case models.AtomEngineKubernetes:
		return kubernetes.NewEngine(ctx), nil
	case models.AtomEnginePodman:
		return podman.NewEngine(ctx), nil
	default:
		return nil, fmt.Errorf("unsupported engine type: %v", engineType)
	}
}

func parseTaskCommand(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	var parsed []string
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed
	}

	return []string{raw}
}

func normalizeTaskFailurePolicy(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case taskFailurePolicyContinue:
		return taskFailurePolicyContinue
	default:
		return taskFailurePolicyHalt
	}
}

func collectDescendantsFromEdges(db *gorm.DB, start uuid.UUID) ([]uuid.UUID, error) {
	queue := []uuid.UUID{start}
	seen := map[uuid.UUID]struct{}{}
	descendants := make([]uuid.UUID, 0)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		var edges []models.TaskEdge
		if err := db.Where("from_task_id = ?", current).Find(&edges).Error; err != nil {
			return nil, err
		}

		for _, edge := range edges {
			if _, ok := seen[edge.ToTaskID]; ok {
				continue
			}
			seen[edge.ToTaskID] = struct{}{}
			descendants = append(descendants, edge.ToTaskID)
			queue = append(queue, edge.ToTaskID)
		}
	}

	return descendants, nil
}
