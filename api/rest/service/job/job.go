package job

import (
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/doug-martin/goqu/v9"
	"github.com/google/uuid"
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
	q := goqu.From(models.JobTable)

	if req.TriggerID != "" {
		if _, err := uuid.Parse(req.TriggerID); err != nil {
			return nil, err
		}

		q = q.Where(goqu.Ex{"trigger_id": req.TriggerID})
	}

	if len(req.OrderBy) > 0 {
		for _, col := range req.OrderBy {
			q = q.OrderAppend(goqu.C(col).Asc())
		}
	}

	if req.Limit > 0 {
		q = q.Limit(uint(req.Limit))
	}

	if req.Offset > 0 {
		q = q.Offset(uint(req.Offset))
	}

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := j.db.Query(&db.QueryRequest{
		Queries: []string{sql},
	})
	if err != nil {
		return nil, err
	}

	return models.NewJobs(resp.Results[0])
}

func (j *jobService) Get(id uuid.UUID) (*models.Job, error) {
	q := goqu.From(models.JobTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := j.db.Query(&db.QueryRequest{
		Queries: []string{sql},
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
	)

	q := goqu.Insert(models.JobTable).Rows(
		models.Job{
			ID:        id,
			TriggerID: uuid.MustParse(req.TriggerID),
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	)

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := j.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
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

func (j *jobService) Delete(id uuid.UUID) error {
	q := goqu.Delete(models.JobTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return err
	}

	_, err = j.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
	})

	return err
}
