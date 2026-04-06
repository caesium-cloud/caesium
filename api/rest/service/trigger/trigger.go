package trigger

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Trigger interface {
	WithDatabase(*gorm.DB) Trigger
	List(*ListRequest) (models.Triggers, error)
	ListByPath(string) (models.Triggers, error)
	Get(uuid.UUID) (*models.Trigger, error)
	Create(*CreateRequest) (*models.Trigger, error)
	Update(uuid.UUID, *UpdateRequest) (*models.Trigger, error)
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

func (t *triggerService) ListByPath(path string) (models.Triggers, error) {
	normalizedPath := models.NormalizedTriggerPath(path)
	if normalizedPath == "" {
		return models.Triggers{}, nil
	}

	var triggers models.Triggers
	err := t.db.WithContext(t.ctx).
		Where("type = ? AND normalized_path = ?", models.TriggerTypeHTTP, normalizedPath).
		Find(&triggers).Error
	return triggers, err
}

func (t *triggerService) Get(id uuid.UUID) (*models.Trigger, error) {
	var (
		trigger = &models.Trigger{ID: id}
		q       = t.db.WithContext(t.ctx)
	)

	return trigger, q.First(trigger).Error
}

type CreateRequest struct {
	Alias         string                 `json:"alias"`
	Type          string                 `json:"type"`
	Configuration map[string]interface{} `json:"configuration"`
}

func (r *CreateRequest) ConfigurationString() (string, error) {
	return jsonutil.MarshalMapString(r.Configuration)
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

	triggerType := models.TriggerType(req.Type)
	if err := validateTriggerRequest(triggerType, req.Configuration); err != nil {
		return nil, err
	}

	trigger := &models.Trigger{
		ID:            id,
		Alias:         req.Alias,
		Type:          triggerType,
		Configuration: cfg,
	}
	if err := trigger.ApplyDerivedFields(); err != nil {
		return nil, err
	}

	return trigger, q.Create(trigger).Error
}

type UpdateRequest struct {
	Alias         *string                `json:"alias,omitempty"`
	Configuration map[string]interface{} `json:"configuration,omitempty"`
}

func (t *triggerService) Update(id uuid.UUID, req *UpdateRequest) (*models.Trigger, error) {
	var trigger models.Trigger
	if err := t.db.WithContext(t.ctx).First(&trigger, "id = ?", id).Error; err != nil {
		return nil, err
	}

	if req.Alias != nil {
		trigger.Alias = *req.Alias
	}
	if req.Configuration != nil {
		if err := validateTriggerRequest(trigger.Type, req.Configuration); err != nil {
			return nil, err
		}
		cfg, err := jsonutil.MarshalMapString(req.Configuration)
		if err != nil {
			return nil, err
		}
		trigger.Configuration = cfg
	}
	if err := trigger.ApplyDerivedFields(); err != nil {
		return nil, err
	}

	updates := map[string]any{
		"alias":           trigger.Alias,
		"configuration":   trigger.Configuration,
		"normalized_path": trigger.NormalizedPath,
	}
	if err := t.db.WithContext(t.ctx).Model(&trigger).Updates(updates).Error; err != nil {
		return nil, err
	}

	return &trigger, nil
}

func (t *triggerService) Delete(id uuid.UUID) error {
	var (
		q = t.db.WithContext(t.ctx)
	)

	return q.Delete(&models.Trigger{}, id).Error
}

func validateTriggerRequest(triggerType models.TriggerType, configuration map[string]interface{}) error {
	cfg := configuration
	if cfg == nil {
		cfg = map[string]interface{}{}
	}
	return jobdefschema.ValidateTriggerSpec(&jobdefschema.Trigger{
		Type:          string(triggerType),
		Configuration: cfg,
	})
}
