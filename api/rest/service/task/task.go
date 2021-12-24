package task

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Task interface {
	WithDatabase(*gorm.DB) Task
	List(*ListRequest) (models.Tasks, error)
	Get(uuid.UUID) (*models.Task, error)
	Create(*CreateRequest) (*models.Task, error)
	Delete(uuid.UUID) error
}

type taskService struct {
	ctx context.Context
	db  *gorm.DB
}

func Service(ctx context.Context) Task {
	return &taskService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (t *taskService) WithDatabase(conn *gorm.DB) Task {
	t.db = conn
	return t
}

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
	JobID   string
	AtomID  string
	NextID  string
}

func (t *taskService) List(req *ListRequest) (models.Tasks, error) {
	var (
		tasks = make(models.Tasks, 0)
		q     = t.db.WithContext(t.ctx)
	)

	if req.JobID != "" {
		if _, err := uuid.Parse(req.JobID); err != nil {
			return nil, err
		}

		q = q.Where("job_id = ?", req.JobID)
	}

	if req.AtomID != "" {
		if _, err := uuid.Parse(req.AtomID); err != nil {
			return nil, err
		}

		q = q.Where("atom_id = ?", req.AtomID)
	}

	if req.NextID != "" {
		if _, err := uuid.Parse(req.NextID); err != nil {
			return nil, err
		}

		q = q.Where("next_id = ?", req.NextID)
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

	return tasks, q.Find(&tasks).Error
}

func (t *taskService) Get(id uuid.UUID) (*models.Task, error) {
	var (
		task = new(models.Task)
		q    = t.db.WithContext(t.ctx)
	)

	return task, q.First(task, id).Error
}

type CreateRequest struct {
	JobID  string  `json:"job_id"`
	AtomID string  `json:"atom_id"`
	NextID *string `json:"next_id"`
}

func (t *taskService) Create(req *CreateRequest) (*models.Task, error) {
	var (
		id = uuid.New()
		q  = t.db.WithContext(t.ctx)
	)

	task := &models.Task{
		ID:     id,
		JobID:  uuid.MustParse(req.JobID),
		AtomID: uuid.MustParse(req.AtomID),
	}

	if req.NextID != nil {
		id, err := uuid.Parse(*req.NextID)
		if err != nil {
			return nil, err
		}

		task.NextID = &id
	}

	return task, q.Create(task).Error
}

func (t *taskService) Delete(id uuid.UUID) error {
	var (
		q = t.db.WithContext(t.ctx)
	)

	return q.Delete(&models.Task{}, id).Error
}
