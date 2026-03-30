package cache

import (
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Entry represents a cached task result.
type Entry struct {
	Hash             string
	JobID            uuid.UUID
	TaskName         string
	Result           string
	Output           map[string]string
	BranchSelections []string
	RunID            uuid.UUID
	TaskRunID        uuid.UUID
	CreatedAt        time.Time
	ExpiresAt        *time.Time
}

// Store provides cache operations backed by GORM.
type Store struct {
	db *gorm.DB
}

// NewStore creates a new cache store.
func NewStore(db *gorm.DB) *Store {
	return &Store{db: db}
}

// Get retrieves a cache entry by hash. Returns nil, false, nil if not found or expired.
func (s *Store) Get(hash string) (*Entry, bool, error) {
	var model models.TaskCache
	result := s.db.Where("hash = ?", hash).First(&model)
	if result.Error != nil {
		if result.Error == gorm.ErrRecordNotFound {
			return nil, false, nil
		}
		return nil, false, result.Error
	}

	// Check expiry
	if model.ExpiresAt != nil && model.ExpiresAt.Before(time.Now().UTC()) {
		return nil, false, nil
	}

	entry, err := modelToEntry(&model)
	if err != nil {
		return nil, false, err
	}
	return entry, true, nil
}

// Put stores a cache entry.
func (s *Store) Put(entry *Entry) error {
	model, err := entryToModel(entry)
	if err != nil {
		return err
	}

	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "hash"}},
		DoUpdates: clause.AssignmentColumns([]string{"job_id", "task_name", "result", "output", "branch_selections", "run_id", "task_run_id", "created_at", "expires_at"}),
	}).Create(model).Error
}

// Invalidate removes cache entries for a specific task.
func (s *Store) Invalidate(jobID uuid.UUID, taskName string) error {
	return s.db.Where("job_id = ? AND task_name = ?", jobID, taskName).
		Delete(&models.TaskCache{}).Error
}

// InvalidateJob removes all cache entries for a job.
func (s *Store) InvalidateJob(jobID uuid.UUID) error {
	return s.db.Where("job_id = ?", jobID).Delete(&models.TaskCache{}).Error
}

// ListByJob returns all cache entries for a job.
func (s *Store) ListByJob(jobID uuid.UUID) ([]Entry, error) {
	var cacheModels []models.TaskCache
	now := time.Now().UTC()
	if err := s.db.Where("job_id = ? AND (expires_at IS NULL OR expires_at > ?)", jobID, now).
		Find(&cacheModels).Error; err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(cacheModels))
	for i := range cacheModels {
		entry, err := modelToEntry(&cacheModels[i])
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, nil
}

// Prune removes expired entries. Returns count of deleted entries.
func (s *Store) Prune() (int, error) {
	now := time.Now().UTC()
	result := s.db.Where("expires_at IS NOT NULL AND expires_at <= ?", now).
		Delete(&models.TaskCache{})
	if result.Error != nil {
		return 0, result.Error
	}
	return int(result.RowsAffected), nil
}

func modelToEntry(model *models.TaskCache) (*Entry, error) {
	entry := &Entry{
		Hash:      model.Hash,
		JobID:     model.JobID,
		TaskName:  model.TaskName,
		Result:    model.Result,
		RunID:     model.RunID,
		TaskRunID: model.TaskRunID,
		CreatedAt: model.CreatedAt,
		ExpiresAt: model.ExpiresAt,
	}

	if len(model.Output) > 0 {
		if err := json.Unmarshal(model.Output, &entry.Output); err != nil {
			return nil, err
		}
	}

	if len(model.BranchSelections) > 0 {
		if err := json.Unmarshal(model.BranchSelections, &entry.BranchSelections); err != nil {
			return nil, err
		}
	}

	return entry, nil
}

func entryToModel(entry *Entry) (*models.TaskCache, error) {
	model := &models.TaskCache{
		Hash:      entry.Hash,
		JobID:     entry.JobID,
		TaskName:  entry.TaskName,
		Result:    entry.Result,
		RunID:     entry.RunID,
		TaskRunID: entry.TaskRunID,
		CreatedAt: entry.CreatedAt,
		ExpiresAt: entry.ExpiresAt,
	}

	if len(entry.Output) > 0 {
		encoded, err := json.Marshal(entry.Output)
		if err != nil {
			return nil, err
		}
		model.Output = datatypes.JSON(encoded)
	}

	if len(entry.BranchSelections) > 0 {
		encoded, err := json.Marshal(entry.BranchSelections)
		if err != nil {
			return nil, err
		}
		model.BranchSelections = datatypes.JSON(encoded)
	}

	return model, nil
}
