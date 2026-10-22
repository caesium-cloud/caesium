package trigger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/internal/authz"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jsonutil"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrInvalidTriggerRequest = errors.New("invalid trigger request")
	ErrTriggerAliasConflict  = errors.New("trigger alias conflict")
)

type Trigger interface {
	WithDatabase(*gorm.DB) Trigger
	List(*ListRequest) (models.Triggers, error)
	ListByPath(string) (models.Triggers, error)
	ListByEventPattern(eventType, source string) (models.Triggers, error)
	Get(uuid.UUID) (*models.Trigger, error)
	Create(*CreateRequest) (*models.Trigger, error)
	Update(uuid.UUID, *UpdateRequest) (*models.Trigger, error)
	Delete(uuid.UUID) error
}

type triggerService struct {
	ctx context.Context
	db  *gorm.DB
}

type MutationHook func(context.Context) error

var (
	mutationHookMu sync.RWMutex
	mutationHook   MutationHook
)

func SetMutationHook(hook MutationHook) {
	mutationHookMu.Lock()
	defer mutationHookMu.Unlock()
	mutationHook = hook
}

func NotifyMutation(ctx context.Context) error {
	mutationHookMu.RLock()
	hook := mutationHook
	mutationHookMu.RUnlock()
	if hook == nil {
		return nil
	}
	return hook(ctx)
}

func Service(ctx context.Context) Trigger {
	return ServiceWithDatabase(ctx, db.Connection())
}

func ServiceWithDatabase(ctx context.Context, conn *gorm.DB) Trigger {
	return &triggerService{
		ctx: ctx,
		db:  conn,
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
	// Namespace narrows the list to triggers serving at least one live job in
	// that namespace, within the caller's allowed set (the ?namespace= view
	// filter); empty applies no narrowing.
	Namespace string
}

func (t *triggerService) List(req *ListRequest) (models.Triggers, error) {
	var (
		triggers = make(models.Triggers, 0)
		q        = t.db.WithContext(t.ctx)
	)

	if req.Type != "" {
		q = q.Where("type = ?", req.Type)
	}
	q = t.namespaceFilter(q)
	if req.Namespace != "" {
		db := t.db.WithContext(t.ctx)
		inNamespace := authz.NarrowToNamespace(
			db.Model(&models.Job{}).Select("1").
				Where("jobs.trigger_id = triggers.id AND jobs.deleted_at IS NULL"),
			"jobs.namespace", req.Namespace)
		q = q.Where("EXISTS (?)", inNamespace)
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

// namespaceFilter narrows a trigger list for a namespace-confined caller
// (identity-and-access B4) with the trigger read rule of
// GET /v1/triggers/:id: a trigger is listed only when it serves at least one
// live job and the caller may read every namespace of the live jobs it
// serves. A trigger shared with another namespace's job, and one serving no
// job, stay cluster-only. Unrestricted callers get q unchanged.
func (t *triggerService) namespaceFilter(q *gorm.DB) *gorm.DB {
	if !authz.Restricted(t.ctx) {
		return q
	}
	db := t.db.WithContext(t.ctx)
	readable := authz.NamespaceFilter(db.Model(&models.Job{}).Select("jobs.id"), "jobs.namespace")
	served := db.Model(&models.Job{}).Select("1").
		Where("jobs.trigger_id = triggers.id AND jobs.deleted_at IS NULL")
	foreign := db.Model(&models.Job{}).Select("1").
		Where("jobs.trigger_id = triggers.id AND jobs.deleted_at IS NULL AND jobs.id NOT IN (?)", readable)
	return q.Where("EXISTS (?) AND NOT EXISTS (?)", served, foreign)
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

func (t *triggerService) ListByEventPattern(eventType, source string) (models.Triggers, error) {
	// Event trigger pattern matching is intentionally done by the router over
	// its in-memory snapshot. The service lookup is the load-all seam used by
	// router reloads and future callers that need the event-trigger set.
	_, _ = eventType, source

	var triggers models.Triggers
	err := t.db.WithContext(t.ctx).
		Where("type = ?", models.TriggerTypeEvent).
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
	Alias         string         `json:"alias"`
	Type          string         `json:"type"`
	Configuration map[string]any `json:"configuration"`
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
		return nil, fmt.Errorf("%w: %w", ErrInvalidTriggerRequest, err)
	}

	triggerType := models.TriggerType(req.Type)
	if err := validateTriggerRequest(triggerType, req.Configuration); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidTriggerRequest, err)
	}
	if err := ensureAliasAvailable(q, strings.TrimSpace(req.Alias), uuid.Nil); err != nil {
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

	if err := q.Create(trigger).Error; err != nil {
		return nil, err
	}
	return trigger, nil
}

type UpdateRequest struct {
	Alias         *string        `json:"alias,omitempty"`
	Configuration map[string]any `json:"configuration,omitempty"`
	// Scope is the writer's trigger-tenancy scope (identity-and-access B9),
	// set by the controller from the principal; nil is unrestricted. It is
	// never bound from the request body.
	Scope *internaljobdef.TenancyScope `json:"-"`
}

// Update edits a trigger. A confined writer, and every writer once a policy
// file is loaded, updates under trigger tenancy (B9): in one transaction (on
// PostgreSQL holding the new webhook path's advisory lock) the jobs the
// trigger serves are locked and re-authorised, the row must serve one
// namespace, and a new webhook path must not be subscribed from another
// namespace or by a job outside the writer's scope. Otherwise it updates
// exactly as before.
func (t *triggerService) Update(id uuid.UUID, req *UpdateRequest) (*models.Trigger, error) {
	if req.Scope == nil && !internaljobdef.TenancyInvariantEnforced() {
		return t.update(t.db.WithContext(t.ctx), id, req)
	}
	// The configuration is read as an HTTP trigger's here, with no read of
	// the row's type: the type is decided inside the transaction, from the
	// locked row (CheckTriggerPatchTx), so a concurrent apply that turns the
	// row into an HTTP trigger cannot slip a webhook path past the check. The
	// path's advisory lock is held either way (harmless for any other type).
	patch := internaljobdef.TriggerPatch{TriggerID: id, PathChanges: req.Configuration != nil}
	var keys []string
	if patch.PathChanges {
		patch.Path = internaljobdef.ProposedWebhookPath(string(models.TriggerTypeHTTP), req.Configuration)
		if patch.Path != "" {
			keys = append(keys, internaljobdef.PathLockKey(patch.Path))
		}
	}
	if beforePatchTx != nil {
		beforePatchTx(id)
	}
	var updated *models.Trigger
	err := internaljobdef.WriteTx(t.ctx, t.db, keys, func(tx *gorm.DB) error {
		if err := internaljobdef.CheckTriggerPatchTx(tx, patch, req.Scope); err != nil {
			return err
		}
		var err error
		updated, err = t.update(tx, id, req)
		return err
	})
	if err != nil {
		return nil, err
	}
	return updated, nil
}

// beforePatchTx, when set (tests only), runs in a tenancy-checked Update just
// before its transaction, where a concurrent apply would interleave.
var beforePatchTx func(id uuid.UUID)

func (t *triggerService) update(conn *gorm.DB, id uuid.UUID, req *UpdateRequest) (*models.Trigger, error) {
	var trigger models.Trigger
	if err := conn.First(&trigger, "id = ?", id).Error; err != nil {
		return nil, err
	}

	if req.Alias != nil {
		if err := ensureAliasAvailable(conn, strings.TrimSpace(*req.Alias), id); err != nil {
			return nil, err
		}
		trigger.Alias = *req.Alias
	}
	if req.Configuration != nil {
		if err := validateTriggerRequest(trigger.Type, req.Configuration); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidTriggerRequest, err)
		}
		cfg, err := jsonutil.MarshalMapString(req.Configuration)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidTriggerRequest, err)
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
	if err := conn.Model(&trigger).Updates(updates).Error; err != nil {
		return nil, err
	}

	return &trigger, nil
}

func (t *triggerService) Delete(id uuid.UUID) error {
	var (
		q = t.db.WithContext(t.ctx)
	)

	if err := q.Delete(&models.Trigger{}, id).Error; err != nil {
		return err
	}
	return nil
}

func validateTriggerRequest(triggerType models.TriggerType, configuration map[string]any) error {
	cfg := configuration
	if cfg == nil {
		cfg = map[string]any{}
	}
	return jobdefschema.ValidateTriggerSpec(&jobdefschema.Trigger{
		Type:          string(triggerType),
		Configuration: cfg,
	})
}

func ensureAliasAvailable(q *gorm.DB, alias string, excludeID uuid.UUID) error {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return nil
	}

	var existing models.Trigger
	query := q.Where("alias = ?", alias)
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.First(&existing).Error
	switch {
	case err == nil:
		return fmt.Errorf("%w: alias %q already exists", ErrTriggerAliasConflict, alias)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	default:
		return err
	}
}
