//go:build integration

package test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestResumeWithoutEdges(t *testing.T) {
	db := openIntegrationDB(t)
	now := time.Now().UTC()

	trig := &models.Trigger{
		ID:            uuid.New(),
		Alias:         "resume-no-edges-trigger",
		Type:          models.TriggerTypeHTTP,
		Configuration: "{}",
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	require.NoError(t, db.Create(trig).Error)

	job := &models.Job{
		ID:        uuid.New(),
		Alias:     "resume-no-edges-job",
		TriggerID: trig.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(job).Error)

	atomA := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.20",
		Command:   "[\"true\"]",
		Spec:      datatypes.JSON([]byte(`{"env":{}}`)),
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atomA).Error)

	atomB := &models.Atom{
		ID:        uuid.New(),
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.20",
		Command:   "[\"true\"]",
		Spec:      datatypes.JSON([]byte(`{"env":{}}`)),
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(atomB).Error)

	taskA := &models.Task{
		ID:        uuid.New(),
		JobID:     job.ID,
		AtomID:    atomA.ID,
		CreatedAt: now,
		UpdatedAt: now,
	}
	require.NoError(t, db.Create(taskA).Error)

	taskB := &models.Task{
		ID:        uuid.New(),
		JobID:     job.ID,
		AtomID:    atomB.ID,
		CreatedAt: now.Add(2 * time.Second),
		UpdatedAt: now.Add(2 * time.Second),
	}
	require.NoError(t, db.Create(taskB).Error)

	store := run.NewStore(db)
	runEntry, err := store.Start(job.ID, nil)
	require.NoError(t, err)

	require.NoError(t, store.RegisterTask(runEntry.ID, taskA, atomA, 0))
	require.NoError(t, store.RegisterTask(runEntry.ID, taskB, atomB, 1))
	require.NoError(t, store.StartTask(runEntry.ID, taskA.ID, "runtime-a"))
	require.NoError(t, store.CompleteTask(runEntry.ID, taskA.ID, "ok"))

	var taskRun models.TaskRun
	require.NoError(t, db.Where("job_run_id = ? AND task_id = ?", runEntry.ID, taskB.ID).First(&taskRun).Error)
	require.Equal(t, 0, taskRun.OutstandingPredecessors)
}

func openIntegrationDB(t *testing.T) *gorm.DB {
	conn, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	gormDB, err := gorm.Open(dqlite.Dialector{Conn: conn}, &gorm.Config{})
	require.NoError(t, err)

	for _, model := range models.All {
		require.NoError(t, gormDB.AutoMigrate(model))
	}

	return gormDB
}
