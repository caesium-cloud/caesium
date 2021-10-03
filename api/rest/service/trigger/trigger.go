package trigger

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/doug-martin/goqu/v9"
	"github.com/google/uuid"
)

type Trigger interface {
	WithStore(*store.Store) Trigger
	List(*ListRequest) (models.Triggers, error)
	Get(uuid.UUID) (*models.Trigger, error)
	Create(*CreateRequest) (*models.Trigger, error)
	Delete(uuid.UUID) error
}

type triggerService struct {
	db db.Database
}

func Service() Trigger {
	return &triggerService{db: db.Service()}
}

func (t *triggerService) WithStore(s *store.Store) Trigger {
	t.db = db.Service().WithStore(s)
	return t
}

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
	Type    string
}

func (t *triggerService) List(req *ListRequest) (models.Triggers, error) {
	q := goqu.From(models.TriggerTable)

	if req.Type != "" {
		q = q.Where(goqu.Ex{"type": req.Type})
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

	resp, err := t.db.Query(&db.QueryRequest{
		Queries: []string{sql},
	})
	if err != nil {
		return nil, err
	}

	return models.NewTriggers(resp.Results[0])
}

func (t *triggerService) Get(id uuid.UUID) (*models.Trigger, error) {
	q := goqu.From(models.TriggerTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := t.db.Query(&db.QueryRequest{
		Queries: []string{sql},
	})
	if err != nil {
		return nil, err
	}

	if len(resp.Results[0].Values) == 0 {
		return nil, nil
	}

	return models.NewTrigger(
		resp.Results[0].Columns,
		resp.Results[0].Values[0],
	)
}

type CreateRequest struct {
	Type          string                 `json:"type"`
	Configuration map[string]interface{} `json:"configuration"`
}

func (r *CreateRequest) ConfigurationString() (string, error) {
	buf, err := json.Marshal(r.Configuration)
	return string(buf), err
}

func (t *triggerService) Create(req *CreateRequest) (*models.Trigger, error) {
	var (
		id        = uuid.New()
		createdAt = time.Now()
	)

	cfg, err := req.ConfigurationString()
	if err != nil {
		return nil, err
	}

	q := goqu.Insert(models.TriggerTable).Rows(
		models.Trigger{
			ID:            id.String(),
			Type:          models.TriggerType(req.Type),
			Configuration: cfg,
			CreatedAt:     createdAt,
			UpdatedAt:     createdAt,
		},
	)

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := t.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
	})

	switch {
	case err != nil:
		log.Error("create trigger failure", "error", err)

		return nil, err
	case resp.Results[0].Error != "":
		err = errors.New(resp.Results[0].Error)

		log.Error("create trigger failure", "error", err)

		return nil, err
	default:
		return t.Get(id)
	}
}

func (t *triggerService) Delete(id uuid.UUID) error {
	q := goqu.Delete(models.TriggerTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return err
	}

	_, err = t.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
	})

	return err
}
