package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/notification"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/sqlerr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrInvalidChannel      = errors.New("invalid notification channel")
	ErrChannelNameConflict = errors.New("channel name conflict")
	ErrInvalidPolicy       = errors.New("invalid notification policy")
	ErrPolicyNameConflict  = errors.New("policy name conflict")
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
	return NewWithDatabase(ctx, db.Connection())
}

// NewWithDatabase returns a notification service over conn.
func NewWithDatabase(ctx context.Context, conn *gorm.DB) Service {
	return &service{
		ctx: ctx,
		db:  conn,
	}
}

// --- Request types ---

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
}

// CreateChannelRequest creates a channel. Namespace is optional: blank means
// "default", "*" makes the channel cluster-shared.
type CreateChannelRequest struct {
	Namespace string             `json:"namespace,omitempty"`
	Name      string             `json:"name"`
	Type      models.ChannelType `json:"type"`
	Config    map[string]any     `json:"config"`
	Enabled   *bool              `json:"enabled,omitempty"`
}

// UpdateChannelRequest patches a channel. Namespace is create-only: an omitted
// or blank value, or the stored namespace echoed back, is accepted; any other
// value is rejected with models.ErrNamespaceImmutable.
type UpdateChannelRequest struct {
	Namespace *string        `json:"namespace,omitempty"`
	Name      *string        `json:"name,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Enabled   *bool          `json:"enabled,omitempty"`
}

// CreatePolicyRequest creates a policy. Namespace is optional (blank means
// "default"; it is never inherited from the channel), and the channel must be
// in the policy's namespace or "*".
type CreatePolicyRequest struct {
	Namespace  string         `json:"namespace,omitempty"`
	Name       string         `json:"name"`
	ChannelID  uuid.UUID      `json:"channel_id"`
	EventTypes []string       `json:"event_types"`
	Filters    map[string]any `json:"filters,omitempty"`
	Enabled    *bool          `json:"enabled,omitempty"`
}

// UpdatePolicyRequest patches a policy. Namespace is create-only, as for
// UpdateChannelRequest.
type UpdatePolicyRequest struct {
	Namespace  *string        `json:"namespace,omitempty"`
	Name       *string        `json:"name,omitempty"`
	ChannelID  *uuid.UUID     `json:"channel_id,omitempty"`
	EventTypes []string       `json:"event_types,omitempty"`
	Filters    map[string]any `json:"filters,omitempty"`
	Enabled    *bool          `json:"enabled,omitempty"`
}

// --- Channel CRUD ---

// ListChannels lists channels. A namespace-confined caller (a collection
// route, identity-and-access D4) sees only its namespaces' channels and the
// cluster-shared ones.
func (s *service) ListChannels(req *ListRequest) ([]models.NotificationChannel, error) {
	var channels []models.NotificationChannel
	q := authz.SharedObjectFilter(s.db.WithContext(s.ctx), "namespace")

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
	if err := models.CheckSharedObjectName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}

	namespace, err := models.NormalizeObjectNamespace(req.Namespace)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}
	if err := ensureNamespaceDeclared(namespace); err != nil {
		return nil, err
	}

	if err := ensureChannelNameAvailable(s.db.WithContext(s.ctx), name, uuid.Nil); err != nil {
		return nil, err
	}

	configJSON, err := json.Marshal(req.Config)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid config: %w", ErrInvalidChannel, err)
	}

	ch := models.NotificationChannel{
		ID:        uuid.New(),
		Namespace: namespace,
		Name:      name,
		Type:      req.Type,
		Config:    configJSON,
		Enabled:   true,
	}
	if req.Enabled != nil {
		ch.Enabled = *req.Enabled
	}

	err = db.TransactionOn(s.ctx, s.db, func(tx *gorm.DB) error {
		if err := claimName(tx, &models.NotificationChannel{}, name, uuid.Nil, ErrChannelNameConflict); err != nil {
			return err
		}
		return tx.Create(&ch).Error
	})
	if err != nil {
		return nil, nameConflictError(err, ErrChannelNameConflict, name)
	}
	return &ch, nil
}

func (s *service) UpdateChannel(id uuid.UUID, req *UpdateChannelRequest) (*models.NotificationChannel, error) {
	var ch models.NotificationChannel
	if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", id).Error; err != nil {
		return nil, err
	}

	// Namespace is create-only; check before any other validation or write.
	if err := models.CheckNamespaceUnchanged(ch.Namespace, req.Namespace); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
	}

	updates := map[string]any{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidChannel)
		}
		if err := models.CheckSharedObjectName(name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidChannel, err)
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
		if err := updateClaimingName(s.ctx, s.db, &models.NotificationChannel{}, &ch, id, updates, ErrChannelNameConflict); err != nil {
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
	return s.db.WithContext(s.ctx).Unscoped().Delete(&models.NotificationChannel{}, "id = ?", id).Error
}

// --- Policy CRUD ---

// ListPolicies lists policies, confined like ListChannels.
func (s *service) ListPolicies(req *ListRequest) ([]models.NotificationPolicy, error) {
	var policies []models.NotificationPolicy
	q := authz.SharedObjectFilter(s.db.WithContext(s.ctx), "namespace")

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
	if err := models.CheckSharedObjectName(name); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	namespace, err := models.NormalizeObjectNamespace(req.Namespace)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}
	if err := ensureNamespaceDeclared(namespace); err != nil {
		return nil, err
	}

	if err := ensurePolicyNameAvailable(s.db.WithContext(s.ctx), name, uuid.Nil); err != nil {
		return nil, err
	}

	// Validate channel exists and is visible from the policy's namespace.
	var ch models.NotificationChannel
	if err := s.db.WithContext(s.ctx).First(&ch, "id = ?", req.ChannelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, fmt.Errorf("%w: channel %s not found", ErrInvalidPolicy, req.ChannelID)
		}
		return nil, err
	}
	if err := ensureChannelVisible(namespace, &ch); err != nil {
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
		Namespace:  namespace,
		Name:       name,
		ChannelID:  req.ChannelID,
		EventTypes: eventTypesJSON,
		Filters:    filtersJSON,
		Enabled:    true,
	}
	if req.Enabled != nil {
		p.Enabled = *req.Enabled
	}

	err = db.TransactionOn(s.ctx, s.db, func(tx *gorm.DB) error {
		if err := claimName(tx, &models.NotificationPolicy{}, name, uuid.Nil, ErrPolicyNameConflict); err != nil {
			return err
		}
		return tx.Create(&p).Error
	})
	if err != nil {
		return nil, nameConflictError(err, ErrPolicyNameConflict, name)
	}
	return &p, nil
}

func (s *service) UpdatePolicy(id uuid.UUID, req *UpdatePolicyRequest) (*models.NotificationPolicy, error) {
	var p models.NotificationPolicy
	if err := s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}

	// Namespace is create-only; check before any other validation or write.
	if err := models.CheckNamespaceUnchanged(p.Namespace, req.Namespace); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
	}

	updates := map[string]any{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidPolicy)
		}
		if err := models.CheckSharedObjectName(name); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidPolicy, err)
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
		if err := ensureChannelVisible(p.Namespace, &ch); err != nil {
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
		if err := updateClaimingName(s.ctx, s.db, &models.NotificationPolicy{}, &p, id, updates, ErrPolicyNameConflict); err != nil {
			return nil, err
		}
	}

	if err := s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// DeletePolicy soft-deletes a policy and releases its name, so the name can
// be created again (identity-and-access D4).
func (s *service) DeletePolicy(id uuid.UUID) error {
	return db.TransactionOn(s.ctx, s.db, func(tx *gorm.DB) error {
		return models.SoftDeleteSharedObject(tx, &models.NotificationPolicy{}, id)
	})
}

// --- Helpers ---

// ensureChannelVisible enforces that a policy references only a channel in its
// own namespace or the cluster-shared namespace (design §10.4).
func ensureChannelVisible(policyNamespace string, ch *models.NotificationChannel) error {
	if models.ObjectVisibleFrom(policyNamespace, ch.Namespace) {
		return nil
	}
	return fmt.Errorf("%w: channel %s is in namespace %q; a policy in namespace %q may reference only channels in its own namespace or %q",
		ErrInvalidPolicy, ch.ID, models.NamespaceOrDefault(ch.Namespace), models.NamespaceOrDefault(policyNamespace), models.ClusterNamespace)
}

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

// ensureNamespaceDeclared rejects a create in a namespace the served policy
// file does not declare (identity-and-access D4; the rule B2 applies to jobs
// and A5 to keys). Without a policy file every namespace is accepted, and "*"
// always is.
func ensureNamespaceDeclared(namespace string) error {
	if undeclared := auth.UndeclaredKeyNamespaces([]string{namespace}); len(undeclared) > 0 {
		return &auth.UndeclaredNamespacesError{Namespaces: undeclared}
	}
	return nil
}

// claimName claims a globally unique shared-object name inside tx: a live
// holder is conflict, a tombstone is renamed out of the way
// (models.ClaimSharedObjectName).
func claimName(tx *gorm.DB, model any, name string, excludeID uuid.UUID, conflict error) error {
	taken, err := models.ClaimSharedObjectName(tx, model, name, excludeID)
	if err != nil {
		return err
	}
	if taken {
		return fmt.Errorf("%w: name %q already exists", conflict, name)
	}
	return nil
}

// updateClaimingName applies updates to row (a loaded row of model's table)
// in one transaction, claiming the new name first when the update renames it.
func updateClaimingName(ctx context.Context, conn *gorm.DB, model, row any, id uuid.UUID, updates map[string]any, conflict error) error {
	name, renamed := updates["name"].(string)
	err := db.TransactionOn(ctx, conn, func(tx *gorm.DB) error {
		if renamed {
			if err := claimName(tx, model, name, id, conflict); err != nil {
				return err
			}
		}
		return tx.Model(row).Updates(updates).Error
	})
	return nameConflictError(err, conflict, name)
}

// nameConflictError maps a write that lost a race for a name (the unique
// index) to the same conflict a claim reports, never a 500.
func nameConflictError(err, conflict error, name string) error {
	if err != nil && !errors.Is(err, conflict) && sqlerr.IsUniqueConstraint(err) {
		return fmt.Errorf("%w: name %q already exists", conflict, name)
	}
	return err
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
