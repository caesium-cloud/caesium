package job

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

type Job interface {
	WithStore(*store.Store) Job
	List(*ListRequest) (models.Jobs, error)
	Get(uuid.UUID) (*models.Job, error)
	Create(*CreateRequest) (*models.Job, error)
	Delete(uuid.UUID) error
}

type jobService struct {
	db db.Database
}

func Service() Job {
	return &jobService{db: db.Service()}
}

func (j *jobService) WithStore(s *store.Store) Job {
	j.db = db.Service().WithStore(s)
	return j
}

type ListRequest struct {
	Limit     uint64
	Offset    uint64
	OrderBy   []string
	TriggerID string
}

func (j *jobService) List(req *ListRequest) (models.Jobs, error) {
	q := query.Session()

	if req.TriggerID != "" {
		if _, err := uuid.Parse(req.TriggerID); err != nil {
			return nil, err
		}

		q = q.Where("trigger_id = ?", req.TriggerID)
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

	stmt := q.Find(&models.Job{}).Statement

	resp, err := j.db.Query(&db.QueryRequest{
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

	return models.NewJobs(resp.Results[0])
}

func (j *jobService) Get(id uuid.UUID) (*models.Job, error) {
	stmt := query.Session().
		First(&models.Atom{}, "id = ?", id.String()).
		Statement

	resp, err := j.db.Query(&db.QueryRequest{
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

	return models.NewJob(
		resp.Results[0].Columns,
		resp.Results[0].Values[0],
	)
}

type CreateRequest struct {
	TriggerID string `json:"trigger_id"`
}

func (j *jobService) Create(req *CreateRequest) (*models.Job, error) {
	var (
		id        = uuid.New()
		createdAt = time.Now()
		q         = query.Session()
	)

	job := &models.Job{
		ID:        id,
		TriggerID: uuid.MustParse(req.TriggerID),
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}

	stmt := q.Create(job).Statement

	resp, err := j.db.Execute(&db.ExecuteRequest{
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
		return j.Get(id)
	}
}

func (j *jobService) Delete(id uuid.UUID) (err error) {
	stmt := query.Session().Delete(&models.Job{}, id.String()).Statement

	_, err = j.db.Execute(&db.ExecuteRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})

	return
}
