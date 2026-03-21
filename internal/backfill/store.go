package backfill

import (
	"errors"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Store provides CRUD operations for Backfill records.
type Store struct {
	db *gorm.DB
}

var (
	defaultStore     *Store
	defaultStoreOnce sync.Once
)

func NewStore(conn *gorm.DB) *Store {
	if conn == nil {
		panic("backfill store requires database connection")
	}
	return &Store{db: conn}
}

func Default() *Store {
	defaultStoreOnce.Do(func() {
		defaultStore = NewStore(db.Connection())
	})
	return defaultStore
}

func (s *Store) Create(b *models.Backfill) error {
	return s.db.Create(b).Error
}

func (s *Store) Get(id uuid.UUID) (*models.Backfill, error) {
	var b models.Backfill
	if err := s.db.First(&b, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) List(jobID uuid.UUID) ([]*models.Backfill, error) {
	var backfills []*models.Backfill
	if err := s.db.Where("job_id = ?", jobID).Order("created_at desc").Find(&backfills).Error; err != nil {
		return nil, err
	}
	return backfills, nil
}

func (s *Store) Cancel(id uuid.UUID) error {
	now := time.Now().UTC()
	return s.db.Model(&models.Backfill{}).
		Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
		Updates(map[string]interface{}{
			"status":       string(models.BackfillStatusCancelled),
			"completed_at": now,
		}).Error
}

func (s *Store) Complete(id uuid.UUID, failed bool) error {
	now := time.Now().UTC()
	status := models.BackfillStatusSucceeded
	if failed {
		status = models.BackfillStatusFailed
	}
	return s.db.Model(&models.Backfill{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":       string(status),
			"completed_at": now,
		}).Error
}

func (s *Store) IncrementCompleted(id uuid.UUID) error {
	return s.db.Model(&models.Backfill{}).
		Where("id = ?", id).
		UpdateColumn("completed_runs", gorm.Expr("completed_runs + 1")).Error
}

func (s *Store) IncrementFailed(id uuid.UUID) error {
	return s.db.Model(&models.Backfill{}).
		Where("id = ?", id).
		UpdateColumn("failed_runs", gorm.Expr("failed_runs + 1")).Error
}

// SetTotalRuns updates the total_runs counter on a backfill.
func (s *Store) SetTotalRuns(id uuid.UUID, total int) error {
	return s.db.Model(&models.Backfill{}).
		Where("id = ?", id).
		UpdateColumn("total_runs", total).Error
}

// IsRunning returns true if a backfill with the given ID is in the running state.
func (s *Store) IsRunning(id uuid.UUID) (bool, error) {
	var b models.Backfill
	err := s.db.Select("status").First(&b, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return b.Status == string(models.BackfillStatusRunning), nil
}

// LatestRunForLogicalDate returns the latest run for a job matching a logical_date param value, or nil if not found.
func (s *Store) LatestRunForLogicalDate(jobID uuid.UUID, logicalDate string) (string, error) {
	// Query job_runs where job_id matches and params JSON contains the logical_date.
	// Use JSON extraction compatible with SQLite and Postgres.
	type result struct {
		Status string
	}
	var r result
	err := s.db.Raw(
		`SELECT status FROM job_runs WHERE job_id = ? AND json_extract(params, '$.logical_date') = ? ORDER BY created_at DESC LIMIT 1`,
		jobID, logicalDate,
	).Scan(&r).Error
	if err != nil {
		return "", err
	}
	return r.Status, nil
}
