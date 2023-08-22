package job

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Job interface {
	WithDatabase(*gorm.DB) Job
	List(*ListRequest) (models.Jobs, error)
	Get(uuid.UUID) (*models.Job, error)
	Create(*CreateRequest) (*models.Job, error)
	Delete(uuid.UUID) error
}

type jobService struct {
	ctx context.Context
	db  *gorm.DB
}

func Service(ctx context.Context) Job {
	return &jobService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (j *jobService) WithDatabase(conn *gorm.DB) Job {
	j.db = conn
	return j
}

type ListRequest struct {
	Limit     uint64
	Offset    uint64
	OrderBy   []string
	TriggerID string
}

func (j *jobService) List(req *ListRequest) (models.Jobs, error) {
	var (
		jobs = make(models.Jobs, 0)
		q    = j.db.WithContext(j.ctx)
	)

	if req.TriggerID != "" {
		if _, err := uuid.Parse(req.TriggerID); err != nil {
			return nil, err
		}

		q = q.Where("trigger_id = ?", req.TriggerID)
	}

	for _, orderBy := range req.OrderBy {
		q = q.Order(orderBy)
	}

	if req.Limit > 0 {
		q = q.Limit(int(req.Limit))
	}

	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}

	return jobs, q.Find(&jobs).Error
}

func (j *jobService) Get(id uuid.UUID) (*models.Job, error) {
	var (
		job = &models.Job{ID: id}
		q   = j.db.WithContext(j.ctx)
	)

	return job, q.First(job).Error
}

type CreateRequest struct {
	TriggerID uuid.UUID `json:"trigger_id"`
}

func (j *jobService) Create(req *CreateRequest) (*models.Job, error) {
	var (
		id = uuid.New()
		q  = j.db.WithContext(j.ctx)
	)

	job := &models.Job{
		ID:        id,
		TriggerID: req.TriggerID,
	}

	return job, q.Create(job).Error
}

func (j *jobService) Delete(id uuid.UUID) error {
	var (
		q = j.db.WithContext(j.ctx)
	)

	return q.Delete(&models.Job{}, id).Error
}
