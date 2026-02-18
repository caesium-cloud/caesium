package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/atom/docker"
	"github.com/caesium-cloud/caesium/internal/atom/kubernetes"
	"github.com/caesium-cloud/caesium/internal/atom/podman"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/caesium-cloud/caesium/pkg/log"
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

	err := e.executeTask(ctx, taskRun)
	if err == nil {
		return
	}
	if errors.Is(err, run.ErrTaskClaimMismatch) {
		log.Info("worker task claim changed; skipping execution result", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
		return
	}

	if errors.Is(err, context.Canceled) {
		log.Info("worker task canceled", "task_id", taskRun.TaskID, "run_id", taskRun.JobRunID)
		return
	}

	if persistErr := e.store.FailTaskClaimed(taskRun.JobRunID, taskRun.TaskID, err, taskRun.ClaimedBy); persistErr != nil {
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
	atomName := taskRun.TaskID.String()
	if taskRun.ClaimAttempt > 0 {
		atomName = fmt.Sprintf("%s-attempt%d", taskRun.TaskID, taskRun.ClaimAttempt)
	}

	a, err := engine.Create(&atom.EngineCreateRequest{
		Name:    atomName,
		Image:   taskRun.Image,
		Command: command,
		Spec:    container.Spec{},
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

	if err := e.store.CompleteTaskClaimed(taskRun.JobRunID, taskRun.TaskID, string(a.Result()), taskRun.ClaimedBy); err != nil {
		return err
	}

	return nil
}

func (e *runtimeExecutor) monitorTask(ctx context.Context, taskRun *models.TaskRun, engine atom.Engine, a atom.Atom) error {
	ticker := time.NewTicker(leaseRenewInterval(e.workerLeaseTTL))
	defer ticker.Stop()

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
		case <-ticker.C:
			if err := e.renewLease(taskRun); err != nil {
				log.Error("failed to renew worker task lease", "run_id", taskRun.JobRunID, "task_id", taskRun.TaskID, "error", err)
			}

			next, err := engine.Get(&atom.EngineGetRequest{ID: a.ID()})
			if err != nil {
				return err
			}
			a = next

			if !a.StoppedAt().IsZero() {
				return engine.Stop(&atom.EngineStopRequest{ID: a.ID(), Force: true})
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
