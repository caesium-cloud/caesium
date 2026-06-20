package jobdef

import (
	"context"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// SnapshotQuery provides read-only access to dag_snapshot rows.
// It does not touch the write path (writeSnapshotTx in importer.go).
// ctx is propagated to every query via db.WithContext so that
// request cancellation and deadlines are honoured — matching the
// house pattern used by other services (trigger, notification, etc.).
type SnapshotQuery struct {
	ctx context.Context
	db  *gorm.DB
}

// NewSnapshotQuery returns a SnapshotQuery backed by conn.
func NewSnapshotQuery(ctx context.Context, conn *gorm.DB) *SnapshotQuery {
	return &SnapshotQuery{ctx: ctx, db: conn}
}

// Latest returns the most-recently written snapshot for jobID, or
// gorm.ErrRecordNotFound when no snapshot exists.
func (q *SnapshotQuery) Latest(jobID uuid.UUID) (*models.DagSnapshot, error) {
	var snap models.DagSnapshot
	err := q.db.WithContext(q.ctx).
		Where("job_id = ?", jobID).
		Order("created_at desc").
		Limit(1).
		First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// ByContentHash returns the snapshot matching jobID + contentHash, or
// gorm.ErrRecordNotFound when none matches.
func (q *SnapshotQuery) ByContentHash(jobID uuid.UUID, contentHash string) (*models.DagSnapshot, error) {
	var snap models.DagSnapshot
	err := q.db.WithContext(q.ctx).
		Where("job_id = ? AND content_hash = ?", jobID, contentHash).
		Order("created_at desc").
		Limit(1).
		First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// ByGitCommit returns the most-recent snapshot for jobID whose git_commit
// matches commit, or gorm.ErrRecordNotFound when none matches.
func (q *SnapshotQuery) ByGitCommit(jobID uuid.UUID, commit string) (*models.DagSnapshot, error) {
	if commit == "" {
		return nil, fmt.Errorf("commit must not be empty")
	}
	var snap models.DagSnapshot
	err := q.db.WithContext(q.ctx).
		Where("job_id = ? AND git_commit = ?", jobID, commit).
		Order("created_at desc").
		Limit(1).
		First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// List returns all snapshots for jobID ordered by created_at descending
// (newest first).
func (q *SnapshotQuery) List(jobID uuid.UUID) ([]models.DagSnapshot, error) {
	var snaps []models.DagSnapshot
	err := q.db.WithContext(q.ctx).
		Where("job_id = ?", jobID).
		Order("created_at desc").
		Find(&snaps).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return snaps, nil
}
