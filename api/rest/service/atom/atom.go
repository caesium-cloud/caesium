package atom

import (
	"errors"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/db"
	"github.com/caesium-cloud/caesium/db/store"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/doug-martin/goqu/v9"
	"github.com/google/uuid"
	"gopkg.in/yaml.v2"
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
	q := goqu.From(models.AtomTable)

	if req.Engine != "" {
		q = q.Where(goqu.Ex{"engine": req.Engine})
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

	resp, err := a.db.Query(&db.QueryRequest{
		Queries: []string{sql},
	})
	if err != nil {
		return nil, err
	}

	return models.NewAtoms(resp.Results[0])
}

func (a *atomService) Get(id uuid.UUID) (*models.Atom, error) {
	q := goqu.From(models.AtomTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := a.db.Query(&db.QueryRequest{
		Queries: []string{sql},
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
	buf, err := yaml.Marshal(r.Command)
	return string(buf), err
}

func (a *atomService) Create(req *CreateRequest) (*models.Atom, error) {
	var (
		id        = uuid.New()
		createdAt = time.Now()
	)

	cmd, err := req.CommandString()
	if err != nil {
		return nil, err
	}

	q := goqu.Insert(models.AtomTable).Rows(
		models.Atom{
			ID:        id,
			Engine:    req.Engine,
			Image:     req.Image,
			Command:   cmd,
			CreatedAt: createdAt,
			UpdatedAt: createdAt,
		},
	)

	sql, _, err := q.ToSQL()
	if err != nil {
		return nil, err
	}

	resp, err := a.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
	})

	switch {
	case err != nil:
		return nil, err
	case resp.Results[0].Error != "":
		return nil, errors.New(resp.Results[0].Error)
	default:
		return a.Get(id)
	}
}

func (a *atomService) Delete(id uuid.UUID) error {
	q := goqu.Delete(models.AtomTable).
		Where(goqu.Ex{"id": id.String()})

	sql, _, err := q.ToSQL()
	if err != nil {
		return err
	}

	_, err = a.db.Execute(&db.ExecuteRequest{
		Statements: []string{sql},
	})

	return err
}
