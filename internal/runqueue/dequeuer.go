package runqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	jobexec "github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type LeaderCheck func(context.Context) (bool, error)
type LaunchFunc func(context.Context, *models.Job, *runstorage.JobRun)

type Config struct {
	DB                  *gorm.DB
	Store               *runstorage.Store
	NodeID              string
	Interval            time.Duration
	StaleClaimThreshold time.Duration
	LeaderCheck         LeaderCheck
	LaunchRun           LaunchFunc
}

type Dequeuer struct {
	db                  *gorm.DB
	store               *runstorage.Store
	nodeID              string
	interval            time.Duration
	staleClaimThreshold time.Duration
	leaderCheck         LeaderCheck
	launchRun           LaunchFunc
}

func NewDequeuer(cfg Config) *Dequeuer {
	if cfg.DB == nil {
		panic("run queue dequeuer requires database connection")
	}
	store := cfg.Store
	if store == nil {
		store = runstorage.NewStore(cfg.DB)
	}
	nodeID := strings.TrimSpace(cfg.NodeID)
	if nodeID == "" {
		nodeID = "runqueue"
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Second
	}
	staleClaimThreshold := cfg.StaleClaimThreshold
	if staleClaimThreshold <= 0 {
		staleClaimThreshold = 2 * time.Minute
	}
	return &Dequeuer{
		db:                  cfg.DB,
		store:               store,
		nodeID:              nodeID,
		interval:            interval,
		staleClaimThreshold: staleClaimThreshold,
		leaderCheck:         cfg.LeaderCheck,
		launchRun:           cfg.LaunchRun,
	}
}

func (d *Dequeuer) Run(ctx context.Context) {
	if err := d.DrainOnce(ctx); err != nil && ctx.Err() == nil {
		log.Error("run queue dequeuer drain failed", "error", err)
	}

	ticker := time.NewTicker(d.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := d.DrainOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("run queue dequeuer drain failed", "error", err)
			}
		}
	}
}

func (d *Dequeuer) DrainOnce(ctx context.Context) error {
	if d.leaderCheck != nil {
		leader, err := d.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			return nil
		}
	}

	if err := d.reclaimStaleClaims(ctx); err != nil {
		return err
	}

	var jobIDs []uuid.UUID
	if err := d.db.WithContext(ctx).
		Model(&models.RunQueue{}).
		Where("claimed_by = ''").
		Distinct("job_id").
		Pluck("job_id", &jobIDs).Error; err != nil {
		return err
	}
	for _, jobID := range jobIDs {
		if err := d.drainJob(ctx, jobID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Dequeuer) reclaimStaleClaims(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-d.staleClaimThreshold)
	result := d.db.WithContext(ctx).
		Model(&models.RunQueue{}).
		Where("claimed_by <> '' AND (claimed_at IS NULL OR claimed_at < ?)", cutoff).
		Updates(map[string]any{
			"claimed_by": "",
			"claimed_at": nil,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected > 0 {
		log.Warn("run queue dequeuer reclaimed stale claims", "count", result.RowsAffected, "cutoff", cutoff)
	}
	return nil
}

func (d *Dequeuer) drainJob(ctx context.Context, jobID uuid.UUID) error {
	cfg, ok, err := loadConcurrencyConfig(ctx, d.db, jobID)
	if err != nil {
		return err
	}
	if !ok || cfg.maxRuns <= 0 {
		return nil
	}
	active, err := d.store.CountActive(jobID)
	if err != nil {
		return err
	}
	for active < int64(cfg.maxRuns) {
		claim := fmt.Sprintf("%s/%s", d.nodeID, uuid.NewString())
		queued, err := d.store.DequeueNextRun(ctx, jobID, claim)
		if err != nil {
			return err
		}
		if queued == nil {
			return nil
		}
		started, err := d.store.StartQueuedRun(ctx, queued)
		if err != nil {
			if releaseErr := d.store.ReleaseQueuedRun(ctx, queued.ID, claim); releaseErr != nil {
				log.Warn("run queue dequeuer failed to release queued run", "queue_id", queued.ID, "error", releaseErr)
			}
			if errors.Is(err, runstorage.ErrMaxConcurrentRunsReached) || errors.Is(err, runstorage.ErrRunQueued) {
				return nil
			}
			return err
		}
		if started == nil {
			if err := d.store.ReleaseQueuedRun(ctx, queued.ID, claim); err != nil {
				return err
			}
			return nil
		}
		if err := d.store.DeleteQueuedRun(ctx, queued); err != nil {
			return err
		}
		if err := d.launch(ctx, started); err != nil {
			return err
		}
		active++
	}
	return nil
}

func (d *Dequeuer) launch(ctx context.Context, started *runstorage.JobRun) error {
	if started == nil {
		return nil
	}
	var jobModel models.Job
	if err := d.db.WithContext(ctx).First(&jobModel, "id = ?", started.JobID).Error; err != nil {
		return err
	}
	if d.launchRun != nil {
		d.launchRun(ctx, &jobModel, started)
		return nil
	}
	go func() {
		runCtx := runstorage.WithContext(context.WithoutCancel(ctx), started.ID)
		if err := jobexec.New(
			&jobModel,
			jobexec.WithRunStoreFactory(func() *runstorage.Store { return d.store }),
			jobexec.WithParams(started.Params),
		).Run(runCtx); err != nil {
			log.Error("run queue dequeuer job run failure", "job_id", started.JobID, "run_id", started.ID, "error", err)
		}
	}()
	return nil
}

type concurrencyConfig struct {
	maxRuns  int
	strategy string
}

func loadConcurrencyConfig(ctx context.Context, db *gorm.DB, jobID uuid.UUID) (concurrencyConfig, bool, error) {
	var row struct {
		Concurrency datatypes.JSON
	}
	err := db.WithContext(ctx).
		Model(&models.Job{}).
		Select("concurrency").
		Where("id = ?", jobID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return concurrencyConfig{}, false, nil
		}
		return concurrencyConfig{}, false, err
	}
	if len(row.Concurrency) == 0 {
		return concurrencyConfig{}, false, nil
	}
	var cfg *jobdefschema.Concurrency
	if err := json.Unmarshal(row.Concurrency, &cfg); err != nil {
		return concurrencyConfig{}, false, err
	}
	if cfg == nil {
		return concurrencyConfig{}, false, nil
	}
	strategy := strings.ToLower(strings.TrimSpace(cfg.Strategy))
	if strategy == "" {
		strategy = jobdefschema.ConcurrencyStrategyQueue
	}
	if strategy != jobdefschema.ConcurrencyStrategyQueue {
		return concurrencyConfig{}, false, nil
	}
	return concurrencyConfig{maxRuns: cfg.MaxRuns, strategy: strategy}, true, nil
}
