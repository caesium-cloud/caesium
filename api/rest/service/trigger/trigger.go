package trigger

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/query"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"
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
	q := query.Session()

	if req.Type != "" {
		q = q.Where("type = ?", req.Type)
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

	stmt := q.Find(&models.Trigger{}).Statement

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

	return models.NewTriggers(resp.Results[0])
}

func (t *triggerService) Get(id uuid.UUID) (*models.Trigger, error) {
	stmt := query.Session().
		First(&models.Trigger{}, "id = ?", id.String()).
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
		q         = query.Session()
	)

	cfg, err := req.ConfigurationString()
	if err != nil {
		return nil, err
	}

	trigger := &models.Trigger{
		ID:            id.String(),
		Type:          models.TriggerType(req.Type),
		Configuration: cfg,
		CreatedAt:     createdAt,
		UpdatedAt:     createdAt,
	}

	stmt := q.Create(trigger).Statement

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

func (t *triggerService) Delete(id uuid.UUID) (err error) {
	stmt := query.Session().Delete(&models.Trigger{}, id.String()).Statement

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
