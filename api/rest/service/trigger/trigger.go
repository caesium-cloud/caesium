package trigger

import (
	"context"
	"encoding/json"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Trigger interface {
	WithDatabase(*gorm.DB) Trigger
	List(*ListRequest) (models.Triggers, error)
	Get(uuid.UUID) (*models.Trigger, error)
	Create(*CreateRequest) (*models.Trigger, error)
	Delete(uuid.UUID) error
}

type triggerService struct {
	ctx context.Context
	db  *gorm.DB
}

func Service(ctx context.Context) Trigger {
	return &triggerService{
		ctx: ctx,
		db:  db.Connection(),
	}
}

func (t *triggerService) WithDatabase(conn *gorm.DB) Trigger {
	t.db = conn
	return t
}

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
	Type    string
}

func (t *triggerService) List(req *ListRequest) (models.Triggers, error) {
	var (
		triggers = make(models.Triggers, 0)
		q        = t.db.WithContext(t.ctx)
	)

	if req.Type != "" {
		q = q.Where("type = ?", req.Type)
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

	return triggers, q.Find(&triggers).Error
}

func (t *triggerService) Get(id uuid.UUID) (*models.Trigger, error) {
	var (
		trigger = new(models.Trigger)
		q       = t.db.WithContext(t.ctx)
	)

	return trigger, q.First(trigger, id).Error
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
		id = uuid.New()
		q  = t.db.WithContext(t.ctx)
	)

	cfg, err := req.ConfigurationString()
	if err != nil {
		return nil, err
	}

	trigger := &models.Trigger{
		ID:            id.String(),
		Type:          models.TriggerType(req.Type),
		Configuration: cfg,
	}

	return trigger, q.Create(trigger).Error
}

func (t *triggerService) Delete(id uuid.UUID) error {
	var (
		q = t.db.WithContext(t.ctx)
	)

	return q.Delete(&models.Trigger{}, id).Error
}
