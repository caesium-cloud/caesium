// Package agentprofile implements the AgentProfile server-side resource:
// the declarative image/engine/limits, secret:// model-credential
// references, session budgets, and default playbook a job's
// metadata.remediation.profile field references
// (docs/design-agent-in-the-loop.md "Declarative policy", Stream E2). It
// mirrors the api/rest/service/notification channel CRUD shape.
package agentprofile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	ErrInvalidProfile      = errors.New("invalid agent profile")
	ErrProfileNameConflict = errors.New("agent profile name conflict")
)

// DefaultTriageOnlyProfileName is the shipped zero-risk default profile name.
// It is defined canonically on the model so internal/jobdef's server-side
// lint can treat it as always resolvable; re-exported here for callers of
// this package.
const DefaultTriageOnlyProfileName = models.DefaultTriageOnlyProfileName

// allowedSecretProviders is the set of secret:// providers a SecretRefs entry
// may reference. It mirrors the providers internal/jobdef/secret resolves
// (env, k8s, vault) without importing its unexported provider constants.
var allowedSecretProviders = map[string]struct{}{
	"env":        {},
	"k8s":        {},
	"kubernetes": {},
	"vault":      {},
}

// Service manages AgentProfile resources.
type Service interface {
	List(req *ListRequest) ([]models.AgentProfile, error)
	Get(id uuid.UUID) (*models.AgentProfile, error)
	Create(req *CreateRequest) (*models.AgentProfile, error)
	Update(id uuid.UUID, req *UpdateRequest) (*models.AgentProfile, error)
	Delete(id uuid.UUID) error
}

type service struct {
	ctx context.Context
	db  *gorm.DB
}

// seededDefaults uses double-checked locking rather than sync.Once so a
// transient seed failure (e.g. the DB not yet reachable on the first call)
// is retried on the next New() instead of being permanently swallowed: the
// "done" flag is set only after a successful seed.
var (
	seedMu   sync.RWMutex
	seededOK bool
)

// New returns a new AgentProfile service. It lazily seeds the shipped
// triage-only default profile (idempotent — guarded by the unique name index
// as well as the double-checked flag) so a fresh deployment has a zero-risk
// profile to reference without an operator having to author one by hand. A
// failed seed is retried on a later call.
func New(ctx context.Context) Service {
	conn := db.Connection()
	ensureSeeded(conn)
	return &service{ctx: ctx, db: conn}
}

func ensureSeeded(conn *gorm.DB) {
	seedMu.RLock()
	done := seededOK
	seedMu.RUnlock()
	if done {
		return
	}

	seedMu.Lock()
	defer seedMu.Unlock()
	if seededOK {
		return
	}
	if err := SeedDefaults(context.Background(), conn); err == nil {
		seededOK = true
	}
}

// --- Request types ---

type ListRequest struct {
	Limit   uint64
	Offset  uint64
	OrderBy []string
}

type CreateRequest struct {
	Name       string                 `json:"name"`
	Image      string                 `json:"image"`
	Engine     models.AtomEngine      `json:"engine,omitempty"`
	Limits     map[string]interface{} `json:"limits,omitempty"`
	SecretRefs map[string]string      `json:"secret_refs,omitempty"`
	Budgets    map[string]interface{} `json:"budgets,omitempty"`
	Playbook   map[string]interface{} `json:"playbook,omitempty"`
}

type UpdateRequest struct {
	Name       *string                `json:"name,omitempty"`
	Image      *string                `json:"image,omitempty"`
	Engine     *models.AtomEngine     `json:"engine,omitempty"`
	Limits     map[string]interface{} `json:"limits,omitempty"`
	SecretRefs map[string]string      `json:"secret_refs,omitempty"`
	Budgets    map[string]interface{} `json:"budgets,omitempty"`
	Playbook   map[string]interface{} `json:"playbook,omitempty"`
}

// --- CRUD ---

func (s *service) List(req *ListRequest) ([]models.AgentProfile, error) {
	var profiles []models.AgentProfile
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

	return profiles, q.Find(&profiles).Error
}

func (s *service) Get(id uuid.UUID) (*models.AgentProfile, error) {
	var p models.AgentProfile
	return &p, s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error
}

func (s *service) Create(req *CreateRequest) (*models.AgentProfile, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidProfile)
	}
	image := strings.TrimSpace(req.Image)
	if image == "" {
		return nil, fmt.Errorf("%w: image is required", ErrInvalidProfile)
	}
	engine, err := validateEngine(req.Engine)
	if err != nil {
		return nil, err
	}
	if err := validateSecretRefs(req.SecretRefs); err != nil {
		return nil, err
	}

	if err := ensureNameAvailable(s.db.WithContext(s.ctx), name, uuid.Nil); err != nil {
		return nil, err
	}

	limitsJSON, err := marshalJSONMap(req.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid limits: %w", ErrInvalidProfile, err)
	}
	secretRefsJSON, err := marshalJSONMapString(req.SecretRefs)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid secret_refs: %w", ErrInvalidProfile, err)
	}
	budgetsJSON, err := marshalJSONMap(req.Budgets)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid budgets: %w", ErrInvalidProfile, err)
	}
	playbookJSON, err := marshalJSONMap(req.Playbook)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid playbook: %w", ErrInvalidProfile, err)
	}

	p := models.AgentProfile{
		ID:         uuid.New(),
		Name:       name,
		Image:      image,
		Engine:     engine,
		Limits:     limitsJSON,
		SecretRefs: secretRefsJSON,
		Budgets:    budgetsJSON,
		Playbook:   playbookJSON,
	}

	if err := s.db.WithContext(s.ctx).Create(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *service) Update(id uuid.UUID, req *UpdateRequest) (*models.AgentProfile, error) {
	var p models.AgentProfile
	if err := s.db.WithContext(s.ctx).First(&p, "id = ?", id).Error; err != nil {
		return nil, err
	}

	updates := map[string]interface{}{}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: name cannot be empty", ErrInvalidProfile)
		}
		if err := ensureNameAvailable(s.db.WithContext(s.ctx), name, id); err != nil {
			return nil, err
		}
		updates["name"] = name
	}

	if req.Image != nil {
		image := strings.TrimSpace(*req.Image)
		if image == "" {
			return nil, fmt.Errorf("%w: image cannot be empty", ErrInvalidProfile)
		}
		updates["image"] = image
	}

	if req.Engine != nil {
		engine, err := validateEngine(*req.Engine)
		if err != nil {
			return nil, err
		}
		updates["engine"] = engine
	}

	if req.Limits != nil {
		limitsJSON, err := marshalJSONMap(req.Limits)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid limits: %w", ErrInvalidProfile, err)
		}
		updates["limits"] = limitsJSON
	}

	if req.SecretRefs != nil {
		if err := validateSecretRefs(req.SecretRefs); err != nil {
			return nil, err
		}
		secretRefsJSON, err := marshalJSONMapString(req.SecretRefs)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid secret_refs: %w", ErrInvalidProfile, err)
		}
		updates["secret_refs"] = secretRefsJSON
	}

	if req.Budgets != nil {
		budgetsJSON, err := marshalJSONMap(req.Budgets)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid budgets: %w", ErrInvalidProfile, err)
		}
		updates["budgets"] = budgetsJSON
	}

	if req.Playbook != nil {
		playbookJSON, err := marshalJSONMap(req.Playbook)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid playbook: %w", ErrInvalidProfile, err)
		}
		updates["playbook"] = playbookJSON
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

func (s *service) Delete(id uuid.UUID) error {
	return s.db.WithContext(s.ctx).Delete(&models.AgentProfile{}, "id = ?", id).Error
}

// --- Helpers ---

func validateEngine(engine models.AtomEngine) (models.AtomEngine, error) {
	if strings.TrimSpace(string(engine)) == "" {
		return models.AtomEngineDocker, nil
	}
	switch engine {
	case models.AtomEngineDocker, models.AtomEngineKubernetes, models.AtomEnginePodman:
		return engine, nil
	default:
		return "", fmt.Errorf("%w: unsupported engine %q", ErrInvalidProfile, engine)
	}
}

// validateSecretRefs checks that every value is a syntactically valid
// secret:// URI from a known provider and writes the trimmed value back into
// the map so the stored reference is clean. It never resolves the value —
// only the reference is stored, exactly like other secret-bearing
// job-definition config (internal/jobdef/secret).
func validateSecretRefs(refs map[string]string) error {
	for key, ref := range refs {
		ref = strings.TrimSpace(ref)
		if ref == "" {
			return fmt.Errorf("%w: secret_refs[%q] must not be empty", ErrInvalidProfile, key)
		}
		if !strings.HasPrefix(ref, "secret://") {
			return fmt.Errorf("%w: secret_refs[%q] must be a secret:// reference", ErrInvalidProfile, key)
		}
		parsed, err := secret.Parse(ref)
		if err != nil {
			return fmt.Errorf("%w: secret_refs[%q]: %w", ErrInvalidProfile, key, err)
		}
		if _, ok := allowedSecretProviders[parsed.Provider]; !ok {
			return fmt.Errorf("%w: secret_refs[%q] has unsupported provider %q", ErrInvalidProfile, key, parsed.Provider)
		}
		refs[key] = ref
	}
	return nil
}

func ensureNameAvailable(q *gorm.DB, name string, excludeID uuid.UUID) error {
	var existing models.AgentProfile
	query := q.Where("name = ?", name)
	if excludeID != uuid.Nil {
		query = query.Where("id <> ?", excludeID)
	}
	err := query.First(&existing).Error
	switch {
	case err == nil:
		return fmt.Errorf("%w: name %q already exists", ErrProfileNameConflict, name)
	case errors.Is(err, gorm.ErrRecordNotFound):
		return nil
	default:
		return err
	}
}

func marshalJSONMap(m map[string]interface{}) (datatypes.JSON, error) {
	if len(m) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(data), nil
}

func marshalJSONMapString(m map[string]string) (datatypes.JSON, error) {
	if len(m) == 0 {
		return nil, nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(data), nil
}

// SeedDefaults idempotently creates the shipped triage-only default profile
// (tier 0 + escalate; zero-risk) if it does not already exist, so a fresh
// deployment always has one AgentProfile a job can reference to adopt
// remediation incrementally. Safe to call concurrently / repeatedly: the
// unique name index makes the create a no-op race rather than a duplicate. A
// nil connection is a retryable failure (not a silent no-op) so ensureSeeded
// tries again once the DB is reachable rather than marking seeding done.
func SeedDefaults(ctx context.Context, conn *gorm.DB) error {
	if conn == nil {
		return errors.New("agentprofile: cannot seed defaults with a nil database connection")
	}
	var existing models.AgentProfile
	err := conn.WithContext(ctx).Where("name = ?", DefaultTriageOnlyProfileName).First(&existing).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	playbook, err := json.Marshal(map[string]interface{}{
		"autonomy": map[string]interface{}{
			"allow": []string{},
		},
		"escalation": map[string]interface{}{
			"after": "15m",
		},
	})
	if err != nil {
		return err
	}

	p := models.AgentProfile{
		ID:       uuid.New(),
		Name:     DefaultTriageOnlyProfileName,
		Image:    "caesiumcloud/triage-agent:latest",
		Engine:   models.AtomEngineDocker,
		Playbook: datatypes.JSON(playbook),
	}
	if err := conn.WithContext(ctx).Create(&p).Error; err != nil {
		// A concurrent seeder or an operator-created row with the same name
		// racing us is not an error — the profile exists either way.
		if errors.Is(err, gorm.ErrDuplicatedKey) {
			return nil
		}
		var check models.AgentProfile
		if lookupErr := conn.WithContext(ctx).Where("name = ?", DefaultTriageOnlyProfileName).First(&check).Error; lookupErr == nil {
			return nil
		}
		return err
	}
	return nil
}
