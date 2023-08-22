package atom

import (
	"context"
	"encoding/json"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Atom interface {
	WithDatabase(*gorm.DB) Atom
	List(*ListRequest) (models.Atoms, error)
	Get(uuid.UUID) (*models.Atom, error)
	Create(*CreateRequest) (*models.Atom, error)
	Delete(uuid.UUID) error
}

type atomService struct {
	ctx context.Context
	db  *gorm.DB
}

func Service(ctx context.Context) Atom {
	return &atomService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (a *atomService) WithDatabase(conn *gorm.DB) Atom {
	a.db = conn
	return a
}

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
	Engine  string
}

func (a *atomService) List(req *ListRequest) (models.Atoms, error) {
	var (
		atoms = make(models.Atoms, 0)
		q     = a.db.WithContext(a.ctx)
	)

	if req.Engine != "" {
		q = q.Where("enging = ?", req.Engine)
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

	return atoms, q.Find(&atoms).Error
}

func (a *atomService) Get(id uuid.UUID) (*models.Atom, error) {
	var (
		atom = &models.Atom{ID: id}
		q    = a.db.WithContext(a.ctx)
	)

	return atom, q.First(atom).Error
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
		id = uuid.New()
		q  = a.db.WithContext(a.ctx)
	)

	cmd, err := req.CommandString()
	if err != nil {
		return nil, err
	}

	atom := &models.Atom{
		ID:      id,
		Engine:  models.AtomEngine(req.Engine),
		Image:   req.Image,
		Command: cmd,
	}

	return atom, q.Create(atom).Error
}

func (a *atomService) Delete(id uuid.UUID) error {
	var (
		q = a.db.WithContext(a.ctx)
	)

	return q.Delete(&models.Job{}, id).Error
}
