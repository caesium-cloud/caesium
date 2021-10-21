package task

import (
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/query"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

type Task interface {
	WithStore(*store.Store) Task
	List(*ListRequest) (models.Tasks, error)
	Get(uuid.UUID) (*models.Task, error)
	Create(*CreateRequest) (*models.Task, error)
	Delete(uuid.UUID) error
}

type taskService struct {
	db db.Database
}

func Service() Task {
	return &taskService{db: db.Service()}
}

func (t *taskService) WithStore(s *store.Store) Task {
	t.db = db.Service().WithStore(s)
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
	q := query.Session()

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

	if len(req.OrderBy) > 0 {
		for _, col := range req.OrderBy {
			q = q.Order(
				clause.OrderByColumn{
					Column: clause.Column{Name: col},
				},
			)
		}
	}

	if req.Limit > 0 {
		q = q.Limit(int(req.Limit))
	}

	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}

	stmt := q.Find(&models.Task{}).Statement

	resp, err := t.db.Query(&db.QueryRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	return models.NewTasks(resp.Results[0])
}

func (t *taskService) Get(id uuid.UUID) (*models.Task, error) {
	stmt := query.Session().
		First(&models.Task{}, "id = ?", id.String()).
		Statement

	resp, err := t.db.Query(&db.QueryRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Results[0].Values) == 0 {
		return nil, nil
	}

	return models.NewTask(
		resp.Results[0].Columns,
		resp.Results[0].Values[0],
	)
}

type CreateRequest struct {
	JobID  string  `json:"job_id"`
	AtomID string  `json:"atom_id"`
	NextID *string `json:"next_id"`
}

func (t *taskService) Create(req *CreateRequest) (*models.Task, error) {
	var (
		id        = uuid.New()
		createdAt = time.Now()
		q         = query.Session()
	)

	task := models.Task{
		ID:        id,
		JobID:     uuid.MustParse(req.JobID),
		AtomID:    uuid.MustParse(req.AtomID),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}

	if req.NextID != nil {
		id, err := uuid.Parse(*req.NextID)
		if err != nil {
			return nil, err
		}

		task.NextID = &id
	}

	stmt := q.Create(task).Statement

	resp, err := t.db.Execute(&db.ExecuteRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})

	switch {
	case err != nil:
		return nil, err
	case resp.Results[0].Error != "":
		return nil, errors.New(resp.Results[0].Error)
	default:
		return t.Get(id)
	}
}

func (t *taskService) Delete(id uuid.UUID) (err error) {
	stmt := query.Session().Delete(&models.Task{}, id.String()).Statement

	_, err = t.db.Execute(&db.ExecuteRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})

	return
}
