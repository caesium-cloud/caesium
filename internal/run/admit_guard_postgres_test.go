package run

import (
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm/clause"
)

// TestPostgresGuardedAdmissionWaitsForAJobMove (identity-and-access A6): on
// PostgreSQL READ COMMITTED a guarded admission holds the job row FOR SHARE,
// so a concurrent move holding it FOR UPDATE (as the importer does) makes the
// admission wait; once the move commits, the guard sees the new namespace
// and refuses, and no run is written under the old one. Opt-in via
// CAESIUM_TEST_POSTGRES_DSN.
func TestPostgresGuardedAdmissionWaitsForAJobMove(t *testing.T) {
	db := openDeadlinePostgres(t)
	require.NoError(t, db.AutoMigrate(
		&models.RunQueue{},
		&models.RunStartIdempotency{},
		&models.DatasetDeclaration{},
		&models.DatasetHold{},
	))
	store := NewStore(db)
	now := time.Now().UTC()
	triggerID := uuid.New()
	require.NoError(t, db.Create(&models.Trigger{ID: triggerID, Alias: "pg-guard-trigger", Type: models.TriggerTypeHTTP, CreatedAt: now, UpdatedAt: now}).Error)
	job := &models.Job{ID: uuid.New(), TriggerID: triggerID, Alias: "pg-guard", Namespace: "marketing"}
	require.NoError(t, db.Create(job).Error)

	mover := db.Begin()
	require.NoError(t, mover.Error)
	var locked models.Job
	require.NoError(t, mover.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, "id = ?", job.ID).Error)
	require.NoError(t, mover.Model(&models.Job{}).Where("id = ?", job.ID).Update("namespace", "finance").Error)

	refused := errors.New("outside the caller's namespaces")
	seen := make(chan string, 2)
	done := make(chan error, 1)
	go func() {
		_, err := store.StartWithContext(t.Context(), job.ID, &triggerID, WithStartNamespaceGuard(func(namespace string) error {
			seen <- namespace
			if namespace != "marketing" {
				return refused
			}
			return nil
		}))
		done <- err
	}()
	select {
	case err := <-done:
		_ = mover.Rollback()
		t.Fatalf("the guarded admission did not wait for the uncommitted move: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	require.NoError(t, mover.Commit().Error)

	select {
	case err := <-done:
		require.ErrorIs(t, err, refused)
	case <-time.After(10 * time.Second):
		t.Fatal("the guarded admission never resumed after the move committed")
	}
	require.Equal(t, "finance", <-seen, "the guard judged the committed namespace")
	var runs int64
	require.NoError(t, db.Model(&models.JobRun{}).Where("job_id = ?", job.ID).Count(&runs).Error)
	require.Zero(t, runs)
}
