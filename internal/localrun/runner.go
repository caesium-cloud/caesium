// Package localrun executes job DAGs locally without a running server.
package localrun

import (
	"context"
	"fmt"
	"time"

	asvc "github.com/caesium-cloud/caesium/api/rest/service/atom"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/caesium-cloud/caesium/internal/job"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Config controls local runner behaviour.
type Config struct {
	MaxParallel int
	TaskTimeout time.Duration
	RunTimeout  time.Duration
	Env         map[string]string // extra env vars injected into every task
	OnPrepared  func(store *run.Store, db *gorm.DB, jobModel *models.Job) error
}

// Runner executes a job definition against an ephemeral in-memory database
// and the local container runtime.
type Runner struct {
	cfg Config
}

// RunResult captures the persisted outcome of a local run for harness assertions.
type RunResult struct {
	RunID  uuid.UUID
	JobID  uuid.UUID
	Alias  string
	Status string
	Error  string
	Tasks  []TaskResult
}

// TaskResult captures the persisted outcome of one task execution.
type TaskResult struct {
	TaskID           uuid.UUID
	Name             string
	Status           string
	Output           map[string]string
	SchemaViolations []pkgtask.SchemaViolation
	LogText          string
	LogTruncated     bool
	CacheHit         bool
	Error            string
}

// New creates a Runner with the given configuration.
func New(cfg Config) *Runner {
	return &Runner{cfg: cfg}
}

// Run parses, imports, and executes the definition.
func (r *Runner) Run(ctx context.Context, def *schema.Definition) error {
	_, err := r.RunWithResult(ctx, def)
	return err
}

// RunWithResult parses, imports, executes, and returns the persisted run result.
func (r *Runner) RunWithResult(ctx context.Context, def *schema.Definition) (*RunResult, error) {
	if err := def.Validate(); err != nil {
		return nil, fmt.Errorf("validation: %w", err)
	}

	db, cleanup, err := OpenEphemeralDB()
	if err != nil {
		return nil, fmt.Errorf("ephemeral database: %w", err)
	}
	defer cleanup()

	importer := jobdef.NewImporter(db)
	jobModel, err := importer.Apply(ctx, def)
	if err != nil {
		return nil, fmt.Errorf("import: %w", err)
	}

	store := run.NewStore(db)
	if r.cfg.OnPrepared != nil {
		if err := r.cfg.OnPrepared(store, db, jobModel); err != nil {
			return nil, fmt.Errorf("prepare run: %w", err)
		}
	}

	envCopy := env.Variables()
	envCopy.ExecutionMode = "local"
	if r.cfg.MaxParallel > 0 {
		envCopy.MaxParallelTasks = r.cfg.MaxParallel
	}
	if r.cfg.TaskTimeout > 0 {
		envCopy.TaskTimeout = r.cfg.TaskTimeout
	}

	opts := []job.JobOption{
		job.WithRunStoreFactory(func() *run.Store { return store }),
		job.WithEnvVariables(func() env.Environment { return envCopy }),
		job.WithTaskServiceFactory(dbTaskService(db)),
		job.WithAtomServiceFactory(dbAtomService(db)),
		job.WithTaskEdgeServiceFactory(dbTaskEdgeService(db)),
		job.WithDispatchRunCallbacks(func(context.Context, uuid.UUID, uuid.UUID, error) error { return nil }),
	}

	if r.cfg.TaskTimeout > 0 {
		jobModel.TaskTimeout = r.cfg.TaskTimeout
	}
	if r.cfg.MaxParallel > 0 {
		jobModel.MaxParallelTasks = r.cfg.MaxParallel
	}

	// Apply run-level timeout via context.
	if r.cfg.RunTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.cfg.RunTimeout)
		defer cancel()
	}

	j := job.New(jobModel, opts...)
	runErr := j.Run(ctx)

	result, resultErr := collectRunResult(store, db, jobModel)
	if resultErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("%w (result collection failed: %v)", runErr, resultErr)
		}
		return nil, resultErr
	}

	return result, runErr
}

// dbTaskService returns a factory that produces task services backed by the given DB.
// It uses ServiceWithDB to avoid triggering the global db.Connection() singleton.
func dbTaskService(conn *gorm.DB) func(context.Context) task.Task {
	return func(ctx context.Context) task.Task {
		return task.ServiceWithDB(ctx, conn)
	}
}

// dbAtomService returns a factory that produces atom services backed by the given DB.
// It uses ServiceWithDB to avoid triggering the global db.Connection() singleton.
func dbAtomService(conn *gorm.DB) func(context.Context) asvc.Atom {
	return func(ctx context.Context) asvc.Atom {
		return asvc.ServiceWithDB(ctx, conn)
	}
}

// dbTaskEdgeService returns a factory that produces task-edge services backed by the given DB.
// It uses ServiceWithDB to avoid triggering the global db.Connection() singleton.
func dbTaskEdgeService(conn *gorm.DB) func(context.Context) taskedge.TaskEdge {
	return func(ctx context.Context) taskedge.TaskEdge {
		return taskedge.ServiceWithDB(ctx, conn)
	}
}

// OpenEphemeralDB creates an in-memory SQLite database with all models migrated.
// The returned cleanup function closes the database.
func OpenEphemeralDB() (*gorm.DB, func(), error) {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared&_busy_timeout=5000"

	// Import sqlite driver — same as testutil but without testing.TB dependency.
	db, err := openSQLite(dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("open sqlite: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, nil, fmt.Errorf("sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	if err := db.AutoMigrate(models.All...); err != nil {
		_ = sqlDB.Close()
		return nil, nil, fmt.Errorf("migrate: %w", err)
	}

	// Enable foreign keys for SQLite.
	db.Exec("PRAGMA foreign_keys = ON")

	cleanup := func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	}
	return db, cleanup, nil
}

func collectRunResult(store *run.Store, db *gorm.DB, jobModel *models.Job) (*RunResult, error) {
	runRecord, err := store.Latest(jobModel.ID)
	if err != nil {
		return nil, fmt.Errorf("load latest run: %w", err)
	}

	var taskModels []models.Task
	if err := db.Where("job_id = ?", jobModel.ID).Order("position asc").Find(&taskModels).Error; err != nil {
		return nil, fmt.Errorf("load task models: %w", err)
	}

	taskByID := make(map[uuid.UUID]*run.TaskRun, len(runRecord.Tasks))
	for _, task := range runRecord.Tasks {
		taskByID[task.TaskID] = task
	}

	result := &RunResult{
		RunID:  runRecord.ID,
		JobID:  jobModel.ID,
		Alias:  jobModel.Alias,
		Status: string(runRecord.Status),
		Error:  runRecord.Error,
		Tasks:  make([]TaskResult, 0, len(taskModels)),
	}

	for _, taskModel := range taskModels {
		taskRun, ok := taskByID[taskModel.ID]
		if !ok {
			result.Tasks = append(result.Tasks, TaskResult{
				Name:   taskModel.Name,
				Status: string(run.TaskStatusPending),
			})
			continue
		}

		taskResult := TaskResult{
			TaskID:           taskModel.ID,
			Name:             taskModel.Name,
			Status:           string(taskRun.Status),
			Output:           taskRun.Output,
			SchemaViolations: taskRun.SchemaViolations,
			CacheHit:         taskRun.CacheHit,
			Error:            taskRun.Error,
		}

		snapshot, err := store.GetTaskLogSnapshot(runRecord.ID, taskModel.ID)
		if err != nil {
			return nil, fmt.Errorf("load task log snapshot for %s: %w", taskModel.Name, err)
		}
		if snapshot != nil {
			taskResult.LogText = snapshot.Text
			taskResult.LogTruncated = snapshot.Truncated
		}

		result.Tasks = append(result.Tasks, taskResult)
	}

	return result, nil
}
