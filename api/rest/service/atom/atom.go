package atom

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/query"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm/clause"
)

type Atom interface {
	WithStore(*store.Store) Atom
	List(*ListRequest) (models.Atoms, error)
	Get(uuid.UUID) (*models.Atom, error)
	Create(*CreateRequest) (*models.Atom, error)
	Delete(uuid.UUID) error
}

type atomService struct {
	db db.Database
}

func Service() Atom {
	return &atomService{db: db.Service()}
}

func (a *atomService) WithStore(s *store.Store) Atom {
	a.db = db.Service().WithStore(s)
	return a
}

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
	Engine  string
}

func (a *atomService) List(req *ListRequest) (models.Atoms, error) {
	q := query.Session()

	if req.Engine != "" {
		q = q.Where("engine = ?", req.Engine)
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

	stmt := q.Find(&models.Atoms{}).Statement

	resp, err := a.db.Query(&db.QueryRequest{
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

	return models.NewAtoms(resp.Results[0])
}

func (a *atomService) Get(id uuid.UUID) (*models.Atom, error) {
	stmt := query.Session().
		First(&models.Atom{}, "id = ?", id.String()).
		Statement

	resp, err := a.db.Query(&db.QueryRequest{
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

	return models.NewAtom(
		resp.Results[0].Columns,
		resp.Results[0].Values[0],
	)
}

type CreateRequest struct {
	Engine  string   `json:"engine"`
	Image   string   `json:"image"`
	Command []string `json:"command"`
}

func (r *CreateRequest) CommandString() (string, error) {
	buf, err := json.Marshal(r.Command)
	return string(buf), err
}

func (a *atomService) Create(req *CreateRequest) (*models.Atom, error) {
	var (
		id        = uuid.New()
		createdAt = time.Now()
		q         = query.Session()
	)

	cmd, err := req.CommandString()
	if err != nil {
		return nil, err
	}

	atom := &models.Atom{
		ID:        id,
		Engine:    models.AtomEngine(req.Engine),
		Image:     req.Image,
		Command:   cmd,
		CreatedAt: createdAt,
		UpdatedAt: createdAt,
	}

	stmt := q.Create(atom).Statement

	resp, err := a.db.Execute(&db.ExecuteRequest{
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
		atom, err = a.Get(id)
		return atom, err
	}
}

func (a *atomService) Delete(id uuid.UUID) (err error) {
	stmt := query.Session().Delete(&models.Atom{}, id.String()).Statement

	_, err = a.db.Execute(&db.ExecuteRequest{
		Statements: []*db.Statement{
			{
				Sql:        stmt.SQL.String(),
				Parameters: stmt.Vars,
			},
		},
	})

	return
}
