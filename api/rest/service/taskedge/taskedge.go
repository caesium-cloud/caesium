package taskedge

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type TaskEdge interface {
	WithDatabase(*gorm.DB) TaskEdge
	List(*ListRequest) (models.TaskEdges, error)
}

type taskEdgeService struct {
	ctx context.Context
	db  *gorm.DB
}

func Service(ctx context.Context) TaskEdge {
	return &taskEdgeService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (t *taskEdgeService) WithDatabase(conn *gorm.DB) TaskEdge {
	t.db = conn
	return t
}

type ListRequest struct {
	JobID      string
	FromTaskID string
	ToTaskID   string
	OrderBy    []string
	Limit      uint64
	Offset     uint64
}

func (t *taskEdgeService) List(req *ListRequest) (models.TaskEdges, error) {
	edges := make(models.TaskEdges, 0)
	q := t.db.WithContext(t.ctx)

	if req.JobID != "" {
		if _, err := uuid.Parse(req.JobID); err != nil {
			return nil, err
		}
		q = q.Where("job_id = ?", req.JobID)
	}

	if req.FromTaskID != "" {
		if _, err := uuid.Parse(req.FromTaskID); err != nil {
			return nil, err
		}
		q = q.Where("from_task_id = ?", req.FromTaskID)
	}

	if req.ToTaskID != "" {
		if _, err := uuid.Parse(req.ToTaskID); err != nil {
			return nil, err
		}
		q = q.Where("to_task_id = ?", req.ToTaskID)
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

	return edges, q.Find(&edges).Error
}
