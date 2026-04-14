package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrInvalidChannel       = errors.New("invalid notification channel")
	ErrChannelNameConflict  = errors.New("channel name conflict")
	ErrInvalidPolicy        = errors.New("invalid notification policy")
	ErrPolicyNameConflict   = errors.New("policy name conflict")
)

// Service manages notification channels and policies.
type Service interface {
	// Channels
	ListChannels(req *ListRequest) ([]models.NotificationChannel, error)
	GetChannel(id uuid.UUID) (*models.NotificationChannel, error)
	CreateChannel(req *CreateChannelRequest) (*models.NotificationChannel, error)
	UpdateChannel(id uuid.UUID, req *UpdateChannelRequest) (*models.NotificationChannel, error)
	DeleteChannel(id uuid.UUID) error

	// Policies
	ListPolicies(req *ListRequest) ([]models.NotificationPolicy, error)
	GetPolicy(id uuid.UUID) (*models.NotificationPolicy, error)
	CreatePolicy(req *CreatePolicyRequest) (*models.NotificationPolicy, error)
	UpdatePolicy(id uuid.UUID, req *UpdatePolicyRequest) (*models.NotificationPolicy, error)
	DeletePolicy(id uuid.UUID) error
}

type service struct {
	ctx context.Context
	db  *gorm.DB
}

// New returns a new notification service.
func New(ctx context.Context) Service {
	return &service{
		ctx: ctx,
		db:  db.Connection(),
	}
}

// --- Request types ---

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
}

type CreateChannelRequest struct {
	Name    string                 `json:"name"`
	Type    models.ChannelType     `json:"type"`
	Config  map[string]interface{} `json:"config"`
	Enabled *bool                  `json:"enabled,omitempty"`
}

type UpdateChannelRequest struct {
	Name    *string                `json:"name,omitempty"`
	Config  map[string]interface{} `json:"config,omitempty"`
	Enabled *bool                  `json:"enabled,omitempty"`
}

type CreatePolicyRequest struct {
	Name       string   `json:"name"`
	ChannelID  uuid.UUID `json:"channel_id"`
	EventTypes []string `json:"event_types"`
	Filters    map[string]interface{} `json:"filters,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
}

type UpdatePolicyRequest struct {
	Name       *string  `json:"name,omitempty"`
	ChannelID  *uuid.UUID `json:"channel_id,omitempty"`
	EventTypes []string `json:"event_types,omitempty"`
	Filters    map[string]interface{} `json:"filters,omitempty"`
	Enabled    *bool    `json:"enabled,omitempty"`
}

// --- Channel CRUD ---

func (s *service) ListChannels(req *ListRequest) ([]models.NotificationChannel, error) {
	var channels []models.NotificationChannel
	q := s.db.WithContext(s.ctx)

	if req != nil {
		if req.Limit > 0 {
			q = q.Limit(int(req.Limit))
		}
		if req.Offset > 0 {
			q = q.Offset(int(req.Offset))
		}
		for _, ob := range req.OrderBy {
			q = q.Order(ob)
		}
	}

	return channels, q.Find(&channels).Error
}

func (s *service) GetChannel(id uuid.UUID) (*models.NotificationChannel, error) {
	var ch models.NotificationChannel
	return &ch, s.db.WithContext(s.ctx).First(&ch, "id = ?", id).Error
}

func (s *service) CreateChannel(req *CreateChannelRequest) (*models.NotificationChannel, error) {
	if err := validateChannelType(req.Type); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidChannel)
	}

	if err := ensureChannelNameAvailable(s.db.WithContext(s.ctx), name, uuid.Nil); err != nil {
		return nil, err
	}

	configJSON, err := json.Marshal(req.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid config: %w", ErrInvalidChannel, err)
	}

	ch := models.NotificationChannel{
		ID:      uuid.New(),
		Name:    name,
		Type:    req.Type,
		Config:  configJSON,
		Enabled: true,
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}

	if err := s.db.WithContext(s.ctx).Create(&ch).Error; err != nil {
		return nil, err
	}
	return &ch, nil
}

func (s *service) UpdateChannel(id uuid.UUID, req *UpdateChannelRequest) (*models.NotificationChannel, error) {
	var ch models.NotificationChannel
	if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", id).Error; err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidChannel)
		}
		if err := ensureChannelNameAvailable(s.db.WithContext(s.ctx), name, id); err != nil {
			return nil, err
		}
		updates["name"] = name
	}

	if req.Config != nil {
		configJSON, err := json.Marshal(req.Config)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid config: %w", ErrInvalidChannel, err)
		}
		updates["config"] = configJSON
	}

	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) > 0 {
		if err := s.db.WithContext(s.ctx).Model(&ch).Updates(updates).Error; err != nil {
			return nil, err
		}
	}

	// Reload
	if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &ch, nil
}

func (s *service) DeleteChannel(id uuid.UUID) error {
	return s.db.WithContext(s.ctx).Delete(&models.NotificationChannel{}, "id = ?", id).Error
}

// --- Policy CRUD ---

func (s *service) ListPolicies(req *ListRequest) ([]models.NotificationPolicy, error) {
	var policies []models.NotificationPolicy
	q := s.db.WithContext(s.ctx)

	if req != nil {
		if req.Limit > 0 {
			q = q.Limit(int(req.Limit))
		}
		if req.Offset > 0 {
			q = q.Offset(int(req.Offset))
		}
		for _, ob := range req.OrderBy {
			q = q.Order(ob)
		}
	}

	return policies, q.Find(&policies).Error
}

func (s *service) GetPolicy(id uuid.UUID) (*models.NotificationPolicy, error) {
	var p models.NotificationPolicy
	return &p, s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error
}

func (s *service) CreatePolicy(req *CreatePolicyRequest) (*models.NotificationPolicy, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidPolicy)
	}

	if err := ensurePolicyNameAvailable(s.db.WithContext(s.ctx), name, uuid.Nil); err != nil {
		return nil, err
	}

	// Validate channel exists
	var ch models.NotificationChannel
	if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", req.ChannelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: channel %s not found", ErrInvalidPolicy, req.ChannelID)
		}
		return nil, err
	}

	if err := validateEventTypes(req.EventTypes); err != nil {
		return nil, err
	}

	eventTypesJSON, err := json.Marshal(req.EventTypes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	var filtersJSON []byte
	if req.Filters != nil {
		filtersJSON, err = json.Marshal(req.Filters)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid filters: %w", ErrInvalidPolicy, err)
		}
		if err := validateFilters(filtersJSON); err != nil {
			return nil, err
		}
	}

	p := models.NotificationPolicy{
		ID:         uuid.New(),
		Name:       name,
		ChannelID:  req.ChannelID,
		EventTypes: eventTypesJSON,
		Filters:    filtersJSON,
		Enabled:    true,
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	if err := s.db.WithContext(s.ctx).Create(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *service) UpdatePolicy(id uuid.UUID, req *UpdatePolicyRequest) (*models.NotificationPolicy, error) {
	var p models.NotificationPolicy
	if err := s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidPolicy)
		}
		if err := ensurePolicyNameAvailable(s.db.WithContext(s.ctx), name, id); err != nil {
			return nil, err
		}
		updates["name"] = name
	}

	if req.ChannelID != nil {
		var ch models.NotificationChannel
		if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", *req.ChannelID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, fmt.Errorf("%w: channel %s not found", ErrInvalidPolicy, req.ChannelID)
			}
			return nil, err
		}
		updates["channel_id"] = *req.ChannelID
	}

	if len(req.EventTypes) > 0 {
		if err := validateEventTypes(req.EventTypes); err != nil {
			return nil, err
		}
		eventTypesJSON, err := json.Marshal(req.EventTypes)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
		}
		updates["event_types"] = eventTypesJSON
	}

	if req.Filters != nil {
		filtersJSON, err := json.Marshal(req.Filters)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid filters: %w", ErrInvalidPolicy, err)
		}
		if err := validateFilters(filtersJSON); err != nil {
			return nil, err
		}
		updates["filters"] = filtersJSON
	}

	if req.Enabled != nil {
		updates["enabled"] = *req.Enabled
	}

	if len(updates) > 0 {
		if err := s.db.WithContext(s.ctx).Model(&p).Updates(updates).Error; err != nil {
			return nil, err
		}
	}

	if err := s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *service) DeletePolicy(id uuid.UUID) error {
	return s.db.WithContext(s.ctx).Delete(&models.NotificationPolicy{}, "id = ?", id).Error
}

// --- Helpers ---

func validateChannelType(ct models.ChannelType) error {
	valid := notification.ValidChannelTypes()
	if _, ok := valid[ct]; !ok {
		return fmt.Errorf("%w: unsupported channel type %q", ErrInvalidChannel, ct)
	}
	return nil
}

func validateEventTypes(types []string) error {
	if len(types) == 0 {
		return fmt.Errorf("%w: at least one event type is required", ErrInvalidPolicy)
	}
	valid := notification.ValidEventTypes()
	for _, t := range types {
		if _, ok := valid[event.Type(t)]; !ok {
			return fmt.Errorf("%w: unsupported event type %q", ErrInvalidPolicy, t)
		}
	}
	return nil
}

func ensureChannelNameAvailable(q *gorm.DB, name string, excludeID uuid.UUID) error {
	var existing models.NotificationChannel
	query := q.Where("name = ?", name)
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.First(&existing).Error
	switch {
	case err == nil:
		return fmt.Errorf("%w: name %q already exists", ErrChannelNameConflict, name)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	default:
		return err
	}
}

func ensurePolicyNameAvailable(q *gorm.DB, name string, excludeID uuid.UUID) error {
	var existing models.NotificationPolicy
	query := q.Where("name = ?", name)
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.First(&existing).Error
	switch {
	case err == nil:
		return fmt.Errorf("%w: name %q already exists", ErrPolicyNameConflict, name)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	default:
		return err
	}
}

// validateFilters checks that the filters JSON is a valid PolicyFilter.
// This prevents malformed filters from being persisted, which would cause
// the subscriber to fail closed and silently drop notifications.
func validateFilters(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var f notification.PolicyFilter
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("%w: filters must be a valid JSON object with optional fields: job_ids, job_alias, labels", ErrInvalidPolicy)
	}
	return nil
}
