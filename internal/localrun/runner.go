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
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Config controls local runner behaviour.
type Config struct {
	MaxParallel int
	TaskTimeout time.Duration
	RunTimeout  time.Duration
	Env         map[string]string // extra env vars injected into every task
}

// Runner executes a job definition against an ephemeral in-memory database
// and the local container runtime.
type Runner struct {
	cfg Config
}

// New creates a Runner with the given configuration.
func New(cfg Config) *Runner {
	return &Runner{cfg: cfg}
}

// Run parses, imports, and executes the definition.
func (r *Runner) Run(ctx context.Context, def *schema.Definition) error {
	if err := def.Validate(); err != nil {
		return fmt.Errorf("validation: %w", err)
	}

	db, cleanup, err := OpenEphemeralDB()
	if err != nil {
		return fmt.Errorf("ephemeral database: %w", err)
	}
	defer cleanup()

	importer := jobdef.NewImporter(db)
	jobModel, err := importer.Apply(ctx, def)
	if err != nil {
		return fmt.Errorf("import: %w", err)
	}

	store := run.NewStore(db)

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
	return j.Run(ctx)
}

// dbTaskService returns a factory that produces task services backed by the given DB.
func dbTaskService(conn *gorm.DB) func(context.Context) task.Task {
	return func(ctx context.Context) task.Task {
		return task.Service(ctx).WithDatabase(conn)
	}
}

// dbAtomService returns a factory that produces atom services backed by the given DB.
func dbAtomService(conn *gorm.DB) func(context.Context) asvc.Atom {
	return func(ctx context.Context) asvc.Atom {
		return asvc.Service(ctx).WithDatabase(conn)
	}
}

// dbTaskEdgeService returns a factory that produces task-edge services backed by the given DB.
func dbTaskEdgeService(conn *gorm.DB) func(context.Context) taskedge.TaskEdge {
	return func(ctx context.Context) taskedge.TaskEdge {
		return taskedge.Service(ctx).WithDatabase(conn)
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
