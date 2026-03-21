package backfill

import (
	"encoding/json"
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

// RequestCancel persists a cancellation request for a running backfill.
func (s *Store) RequestCancel(id uuid.UUID) error {
	now := time.Now().UTC()
	return s.db.Model(&models.Backfill{}).
		Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
		Updates(map[string]interface{}{
			"cancel_requested_at": now,
		}).Error
}

// MarkCancelled marks a running backfill as fully cancelled after in-flight
// work has drained.
func (s *Store) MarkCancelled(id uuid.UUID) error {
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
		Where("id = ? AND status = ?", id, string(models.BackfillStatusRunning)).
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

// IsCancelRequested returns true if a running backfill has a persisted cancel
// request.
func (s *Store) IsCancelRequested(id uuid.UUID) (bool, error) {
	var b models.Backfill
	err := s.db.Select("status", "cancel_requested_at").First(&b, "id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return b.Status == string(models.BackfillStatusRunning) && b.CancelRequestedAt != nil, nil
}

// LatestRunForLogicalDate returns the status of the most recent run for a job
// whose params contain the given logical_date value, or "" if none exists.
// It uses Go-side filtering to remain portable across SQLite and Postgres.
func (s *Store) LatestRunForLogicalDate(jobID uuid.UUID, logicalDate string) (string, error) {
	type runRow struct {
		Status string
		Params []byte
	}
	var rows []runRow
	err := s.db.Raw(
		`SELECT status, params FROM job_runs WHERE job_id = ? AND params IS NOT NULL ORDER BY created_at DESC`,
		jobID,
	).Scan(&rows).Error
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		var p map[string]string
		if err := json.Unmarshal(row.Params, &p); err != nil {
			continue
		}
		if p["logical_date"] == logicalDate {
			return row.Status, nil
		}
	}
	return "", nil
}
