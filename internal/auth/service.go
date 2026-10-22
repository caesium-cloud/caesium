package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrKeyNotFound = errors.New("api key not found")
	ErrKeyRevoked  = errors.New("api key revoked")
	ErrKeyExpired  = errors.New("api key expired")
	ErrForbidden   = errors.New("insufficient permissions")

	// ErrUserBoundKeyRotation refuses rotating a user-bound key: a login
	// mints a new one instead (E4).
	ErrUserBoundKeyRotation = errors.New("user-bound api keys cannot be rotated")
)

const (
	// defaultValidationFailureMinLatency pads all auth-failure responses to a
	// uniform floor, preventing timing-based key enumeration. 100ms is the
	// minimum recommended by common practice; operators can tune via
	// WithValidationFailureMinLatency.
	defaultValidationFailureMinLatency = 100 * time.Millisecond
	bootstrapAdminSlot                 = "bootstrap-admin"
	bootstrapRetryAttempts             = 5
	bootstrapRetryDelay                = 10 * time.Millisecond
)

// Service provides API key management and validation.
type Service struct {
	db *gorm.DB

	keyHashSecret               string
	validationFailureMinLatency time.Duration
	sleep                       func(time.Duration)
	now                         func() time.Time
	policySource                policy.Source
	policyHolder                *policy.Holder
	userKeyTTL                  time.Duration
	// groupFreshness, when set, admits a user-bound key's user (group
	// refresh and the maximum staleness, identity-and-access E2).
	groupFreshness GroupFreshness

	// lastUsedMu protects the async last_used_at update buffer.
	lastUsedMu sync.Mutex
	lastUsed   map[uuid.UUID]time.Time
}

// ServiceOption customizes auth service behavior.
type ServiceOption func(*Service)

// WithKeyHashSecret configures the server-side secret used for HMAC-SHA256 key hashes.
func WithKeyHashSecret(secret string) ServiceOption {
	return func(s *Service) {
		s.keyHashSecret = secret
	}
}

// WithPolicySource configures the grant source for session principals.
func WithPolicySource(src policy.Source) ServiceOption {
	return func(s *Service) {
		s.policySource = src
	}
}

// PolicySource returns the configured grant source, or nil when there is none.
func (s *Service) PolicySource() policy.Source {
	if s == nil {
		return nil
	}
	return s.policySource
}

// WithPolicyHolder configures the access policy holder behind
// GET /v1/auth/policy, POST /v1/auth/policy/reload and namespace settings.
// It is independent of WithPolicySource: with no policy file the holder only
// renders the env translation and the grant source stays the RoleMapper (or
// nil).
func WithPolicyHolder(h *policy.Holder) ServiceOption {
	return func(s *Service) {
		s.policyHolder = h
	}
}

// PolicyHolder returns the configured access policy holder, or nil.
func (s *Service) PolicyHolder() *policy.Holder {
	if s == nil {
		return nil
	}
	return s.policyHolder
}

// WithValidationFailureMinLatency configures the minimum latency for auth failures.
func WithValidationFailureMinLatency(d time.Duration) ServiceOption {
	return func(s *Service) {
		s.validationFailureMinLatency = d
	}
}

// WithGroupFreshness makes user-bound key validation admit the key's user
// through f (identity-and-access E2).
func WithGroupFreshness(f GroupFreshness) ServiceOption {
	return func(s *Service) {
		s.groupFreshness = f
	}
}

// WithNow overrides the service clock. Intended for tests.
func WithNow(now func() time.Time) ServiceOption {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

// WithSleep overrides the service sleep function. Intended for tests.
func WithSleep(sleep func(time.Duration)) ServiceOption {
	return func(s *Service) {
		if sleep != nil {
			s.sleep = sleep
		}
	}
}

// NewService creates a new auth service backed by the given database.
func NewService(db *gorm.DB, opts ...ServiceOption) *Service {
	s := &Service{
		db:                          db,
		lastUsed:                    make(map[uuid.UUID]time.Time),
		validationFailureMinLatency: defaultValidationFailureMinLatency,
		sleep:                       time.Sleep,
		now:                         time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RunLastUsedFlusher periodically flushes buffered last_used_at timestamps to
// the database until ctx is cancelled. This keeps the hot auth path free of
// writes while allowing the caller to own goroutine lifecycle.
func (s *Service) RunLastUsedFlusher(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushLastUsed()
			return
		case <-ticker.C:
			s.flushLastUsed()
		}
	}
}

func (s *Service) flushLastUsed() {
	s.lastUsedMu.Lock()
	pending := s.lastUsed
	s.lastUsed = make(map[uuid.UUID]time.Time, len(pending))
	s.lastUsedMu.Unlock()

	if len(pending) == 0 {
		return
	}

	ids := make([]uuid.UUID, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}

	if err := s.db.Model(&models.APIKey{}).Where("id IN ?", ids).Update("last_used_at", s.nowUTC()).Error; err != nil {
		log.Warn("failed to batch update api key last_used_at", "error", err)
	}
}

func (s *Service) recordLastUsed(id uuid.UUID) {
	s.lastUsedMu.Lock()
	s.lastUsed[id] = s.nowUTC()
	s.lastUsedMu.Unlock()
}

// ValidateKey looks up a plaintext API key, verifies it is active (for a
// user-bound key, that its user is too; see AuthenticateKey), and returns the
// key record. On success it asynchronously updates last_used_at.
func (s *Service) ValidateKey(plaintext string) (*models.APIKey, error) {
	key, _, err := s.AuthenticateKey(context.Background(), plaintext)
	return key, err
}

// AuthenticateKey validates a bearer token and returns its stored key and the
// principal it authenticates. The stored row decides the kind, never the
// token's prefix: a row without user_id is a plain key (PrincipalFromKey); a
// row with one is user-bound and authenticates its user, resolved afresh on
// every call (validateUserKey). Every refusal is one of the validation
// sentinels and is padded to the same minimum latency, so a caller cannot
// tell a disabled user from a revoked key. On success it asynchronously
// updates last_used_at.
func (s *Service) AuthenticateKey(ctx context.Context, plaintext string) (_ *models.APIKey, _ *Principal, retErr error) {
	startedAt := s.now()
	defer func() {
		s.applyFailureLatency(startedAt, retErr)
	}()

	hash, err := HashKey(plaintext, s.keyHashSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("hash api key: %w", err)
	}

	var key models.APIKey
	if err := s.db.Where("key_hash = ?", hash).First(&key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, ErrKeyNotFound
		}
		return nil, nil, fmt.Errorf("lookup api key: %w", err)
	}

	if key.IsRevoked() {
		return nil, nil, ErrKeyRevoked
	}
	if s.isExpired(&key) {
		return nil, nil, ErrKeyExpired
	}

	var principal *Principal
	if key.IsUserBound() {
		principal, err = s.validateUserKey(ctx, &key)
		if err != nil {
			return nil, nil, err
		}
	} else {
		principal = PrincipalFromKey(&key)
	}

	s.recordLastUsed(key.ID)
	return &key, principal, nil
}

// CreateKeyRequest holds the parameters for creating a new API key.
type CreateKeyRequest struct {
	Description string
	Role        models.Role
	Scope       *models.KeyScope
	// Namespaces is persisted verbatim. nil (or empty) stores SQL NULL, which
	// grants Role cluster-wide; see PrincipalFromKey.
	Namespaces datatypes.JSON
	CreatedBy  string
	ExpiresAt  *time.Time
}

// CreateKeyResponse is returned on key creation — the only time the plaintext is available.
type CreateKeyResponse struct {
	Plaintext string         `json:"key"`
	Key       *models.APIKey `json:"api_key"`
}

// CreateKey generates a new API key and persists its hash.
func (s *Service) CreateKey(req *CreateKeyRequest) (*CreateKeyResponse, error) {
	resp, err := s.newKey(req)
	if err != nil {
		return nil, err
	}
	if err := s.db.Create(resp.Key).Error; err != nil {
		return nil, fmt.Errorf("persist api key: %w", err)
	}
	return resp, nil
}

// newKey generates a key and its record without persisting it.
func (s *Service) newKey(req *CreateKeyRequest) (*CreateKeyResponse, error) {
	plaintext, prefix, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	hash, err := HashKey(plaintext, s.keyHashSecret)
	if err != nil {
		return nil, err
	}

	var scopeJSON []byte
	if req.Scope != nil {
		scopeJSON, err = json.Marshal(req.Scope)
		if err != nil {
			return nil, fmt.Errorf("marshal scope: %w", err)
		}
	}

	key := &models.APIKey{
		ID:          uuid.New(),
		KeyPrefix:   prefix,
		KeyHash:     hash,
		Description: req.Description,
		Role:        req.Role,
		Scope:       scopeJSON,
		Namespaces:  req.Namespaces,
		CreatedBy:   req.CreatedBy,
		CreatedAt:   s.nowUTC(),
		ExpiresAt:   req.ExpiresAt,
	}
	return &CreateKeyResponse{Plaintext: plaintext, Key: key}, nil
}

// ListKeys returns all API keys (without hashes, which are excluded by the model JSON tag).
func (s *Service) ListKeys() ([]models.APIKey, error) {
	var keys []models.APIKey
	if err := s.db.Order("created_at DESC").Find(&keys).Error; err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	return keys, nil
}

// RevokeKey sets revoked_at on the specified key. It is the trusted internal
// revocation (the incident supervisor revokes the agent-session keys it
// minted); request handlers use RevokeKeyAuthorized.
func (s *Service) RevokeKey(id uuid.UUID) error {
	result := s.db.Model(&models.APIKey{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", s.nowUTC())
	if result.Error != nil {
		return fmt.Errorf("revoke api key: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// errNoKeyAuthorizer guards the authorized key operations against a missing
// authorization callback: they never act without one.
var errNoKeyAuthorizer = errors.New("api key operation requires an authorizer")

// keyWriteHook, when set (tests only), runs inside the authorized key
// transactions between reading the stored key and writing, so a test can
// interleave a competing change deterministically. An error it returns fails
// that attempt as a statement error would.
var keyWriteHook func(tx *gorm.DB, id uuid.UUID) error

// keyTransaction runs fn in one transaction and retries the whole
// transaction on catalog contention (catalogRetryPolicy: a dqlite/SQLite lock
// or busy error, such as a read snapshot another connection's commit kept
// from upgrading, or a PostgreSQL serialization failure). A rolled-back
// attempt leaves nothing behind, so fn must reset anything it reports.
func (s *Service) keyTransaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return dbretry.Retry(ctx, catalogRetryPolicy(), func() error {
		return s.db.WithContext(ctx).Transaction(fn)
	})
}

// lockKeyRow reads the stored key for update: SELECT ... FOR UPDATE where the
// backend has row locks (PostgreSQL), so a concurrent revoke or rotation of
// the same key waits for this transaction. The SQLite and dqlite dialects
// drop the clause; their write transactions are already serialised.
func lockKeyRow(tx *gorm.DB, id uuid.UUID) (*models.APIKey, error) {
	var key models.APIKey
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&key, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}
	return &key, nil
}

// RevokeKeyAuthorized revokes a key on behalf of a request: it reads the
// stored key, lets authorize decide against that row, and revokes it, all in
// one transaction. The revocation applies only to a key still active when it
// is written. It returns the revoked key.
func (s *Service) RevokeKeyAuthorized(ctx context.Context, id uuid.UUID, authorize KeyAuthorizer) (*models.APIKey, error) {
	if authorize == nil {
		return nil, errNoKeyAuthorizer
	}
	var revoked *models.APIKey
	err := s.keyTransaction(ctx, func(tx *gorm.DB) error {
		revoked = nil
		key, err := lockKeyRow(tx, id)
		if err != nil {
			return err
		}
		if err := authorize(key); err != nil {
			return err
		}
		if key.IsRevoked() {
			return ErrKeyNotFound
		}
		if keyWriteHook != nil {
			if err := keyWriteHook(tx, id); err != nil {
				return err
			}
		}
		now := s.nowUTC()
		result := tx.Model(&models.APIKey{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now)
		if result.Error != nil {
			return fmt.Errorf("revoke api key: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return ErrKeyNotFound
		}
		key.RevokedAt = &now
		revoked = key
		return nil
	})
	if err != nil {
		return nil, err
	}
	return revoked, nil
}

// RotateKeyAuthorized replaces a key on behalf of a request. In one
// transaction it reads the stored key, lets authorize decide against that
// row, mints a replacement with the same role, scope, namespaces and expiry
// (rotation replaces the secret, never the grant or its lifetime), and lets
// the old key live on for gracePeriod at most: its expiry is never moved
// later. A key that is revoked, expired, user-bound (a login mints a new one),
// has unusable namespaces, or is confined to a namespace the policy file does
// not declare is refused, and nothing is written. Every refusal comes after
// authorize, so a caller that may not administer the key learns nothing about
// it.
func (s *Service) RotateKeyAuthorized(ctx context.Context, id uuid.UUID, gracePeriod time.Duration, actor string, authorize KeyAuthorizer) (*CreateKeyResponse, error) {
	if authorize == nil {
		return nil, errNoKeyAuthorizer
	}
	var resp *CreateKeyResponse
	err := s.keyTransaction(ctx, func(tx *gorm.DB) error {
		resp = nil
		oldKey, err := lockKeyRow(tx, id)
		if err != nil {
			return err
		}
		if err := authorize(oldKey); err != nil {
			return err
		}
		if oldKey.IsUserBound() {
			return ErrUserBoundKeyRotation
		}
		if oldKey.IsRevoked() {
			return ErrKeyRevoked
		}
		if s.isExpired(oldKey) {
			return ErrKeyExpired
		}
		namespaces, err := StoredKeyNamespaces(oldKey.Namespaces)
		if err != nil {
			return err
		}
		if undeclared := UndeclaredKeyNamespaces(namespaces); len(undeclared) > 0 {
			return &UndeclaredNamespacesError{Namespaces: undeclared}
		}
		var scope *models.KeyScope
		if len(oldKey.Scope) > 0 {
			scope = &models.KeyScope{}
			if err := json.Unmarshal(oldKey.Scope, scope); err != nil {
				return fmt.Errorf("unmarshal old scope: %w", err)
			}
		}
		if keyWriteHook != nil {
			if err := keyWriteHook(tx, id); err != nil {
				return err
			}
		}

		// The old key's grace: now+gracePeriod, but never later than the
		// expiry it already has. The update applies only while the key is
		// unrevoked and its stored expiry is later than the grace, so a
		// competing change is never overwritten with a later expiry, and a
		// key revoked since it was read gets no replacement.
		graceExpiry := s.nowUTC().Add(gracePeriod)
		if oldKey.ExpiresAt != nil && oldKey.ExpiresAt.Before(graceExpiry) {
			graceExpiry = *oldKey.ExpiresAt
		}
		result := tx.Model(&models.APIKey{}).
			Where("id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", id, graceExpiry).
			Update("expires_at", graceExpiry)
		if result.Error != nil {
			return fmt.Errorf("set grace period: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			var current models.APIKey
			if err := tx.Select("id", "revoked_at").First(&current, "id = ?", id).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return ErrKeyNotFound
				}
				return fmt.Errorf("recheck api key: %w", err)
			}
			if current.IsRevoked() {
				return ErrKeyRevoked
			}
			// Otherwise the stored expiry is already no later than the grace.
		}

		var expiresAt *time.Time
		if oldKey.ExpiresAt != nil {
			expiry := *oldKey.ExpiresAt
			expiresAt = &expiry
		}
		// Namespaces are copied byte for byte: NULL stays cluster-wide and a
		// confined key stays confined.
		resp, err = s.newKey(&CreateKeyRequest{
			Description: oldKey.Description + " (rotated)",
			Role:        oldKey.Role,
			Scope:       scope,
			Namespaces:  oldKey.Namespaces,
			CreatedBy:   actor,
			ExpiresAt:   expiresAt,
		})
		if err != nil {
			return err
		}
		if err := tx.Create(resp.Key).Error; err != nil {
			return fmt.Errorf("persist api key: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// AdminKeyExists returns true if at least one non-revoked, non-expired key
// grants admin cluster-wide with no further restriction. A key confined to
// namespaces, to a job allow-list or to an agent session does not count.
// Candidates are filtered with the same grant logic requests use, so the
// startup check and RBAC cannot disagree about which keys are cluster admins.
func (s *Service) AdminKeyExists() (bool, error) {
	var exists bool
	err := s.withReadRetry(func() error {
		var keys []models.APIKey
		err := s.db.
			Where(
				"role = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)",
				models.RoleAdmin,
				s.nowUTC(),
			).
			Find(&keys).Error
		exists = false
		for i := range keys {
			if keyGrants(&keys[i]).ClusterRole() == models.RoleAdmin && !ScopeRestricts(keys[i].Scope) {
				exists = true
				break
			}
		}
		return err
	})
	if err != nil {
		return false, fmt.Errorf("check admin keys: %w", err)
	}
	return exists, nil
}

// Bootstrap generates the initial admin key on first startup with auth enabled.
// Returns the plaintext key (to be printed to stdout once) or empty string if
// an admin key already exists.
func (s *Service) Bootstrap() (string, error) {
	exists, err := s.AdminKeyExists()
	if err != nil {
		return "", err
	}
	if exists {
		return "", nil
	}

	plaintext, prefix, err := GenerateKey()
	if err != nil {
		return "", fmt.Errorf("bootstrap admin key: %w", err)
	}
	hash, err := HashKey(plaintext, s.keyHashSecret)
	if err != nil {
		return "", fmt.Errorf("bootstrap admin key: %w", err)
	}

	now := s.nowUTC()
	slot := bootstrapAdminSlot
	key := &models.APIKey{
		ID:            uuid.New(),
		KeyPrefix:     prefix,
		KeyHash:       hash,
		BootstrapSlot: &slot,
		Description:   "Bootstrap admin key",
		Role:          models.RoleAdmin,
		CreatedBy:     "system",
		CreatedAt:     now,
	}

	rowsAffected, err := s.tryCreateBootstrapKey(key)
	if err != nil {
		return "", fmt.Errorf("bootstrap admin key: %w", err)
	}
	if rowsAffected == 1 {
		return plaintext, nil
	}

	exists, err = s.AdminKeyExists()
	if err != nil {
		return "", err
	}
	if exists {
		return "", nil
	}

	// The bootstrap slot exists but the key is revoked, expired or confined to
	// namespaces — refresh it with a new UUID so the old audit entries remain
	// unambiguous.
	newID := uuid.New()
	rowsAffected, err = s.tryRefreshBootstrapKey(newID, prefix, hash, now)
	if err != nil {
		return "", fmt.Errorf("bootstrap admin key: %w", err)
	}
	if rowsAffected == 1 {
		return plaintext, nil
	}

	exists, err = s.AdminKeyExists()
	if err != nil {
		return "", err
	}
	if exists {
		return "", nil
	}

	return "", fmt.Errorf("bootstrap admin key: no active admin key found after bootstrap attempt")
}

func (s *Service) nowUTC() time.Time {
	return s.now().UTC()
}

func (s *Service) isExpired(key *models.APIKey) bool {
	return key.ExpiresAt != nil && s.nowUTC().After(*key.ExpiresAt)
}

func (s *Service) applyFailureLatency(startedAt time.Time, err error) {
	if !isValidationFailure(err) || s.validationFailureMinLatency <= 0 {
		return
	}

	if remaining := s.validationFailureMinLatency - s.now().Sub(startedAt); remaining > 0 {
		s.sleep(remaining)
	}
}

func isValidationFailure(err error) bool {
	return errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrKeyRevoked) || errors.Is(err, ErrKeyExpired) ||
		errors.Is(err, ErrUserDisabled) || errors.Is(err, ErrStaleGroups)
}

func (s *Service) tryCreateBootstrapKey(key *models.APIKey) (int64, error) {
	return s.withBootstrapRetry(func() (int64, error) {
		result := s.db.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "bootstrap_slot"}},
			DoNothing: true,
		}).Create(key)
		return result.RowsAffected, result.Error
	})
}

// tryRefreshBootstrapKey restores the bootstrap slot to a fresh cluster admin
// key. It runs only after AdminKeyExists found no cluster admin, and matches a
// bootstrap key that is revoked, expired, confined to namespaces, scoped or
// bound to a user (an active cluster bootstrap key has NULL namespaces, scope
// and user and is never replaced).
func (s *Service) tryRefreshBootstrapKey(newID uuid.UUID, prefix, hash string, now time.Time) (int64, error) {
	return s.withBootstrapRetry(func() (int64, error) {
		result := s.db.Model(&models.APIKey{}).
			Where(
				"bootstrap_slot = ? AND (revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= ?) OR namespaces IS NOT NULL OR scope IS NOT NULL OR user_id IS NOT NULL)",
				bootstrapAdminSlot,
				now,
			).
			Updates(map[string]any{
				"id":           newID,
				"key_prefix":   prefix,
				"key_hash":     hash,
				"description":  "Bootstrap admin key",
				"role":         models.RoleAdmin,
				"scope":        nil,
				"namespaces":   nil,
				"created_by":   "system",
				"created_at":   now,
				"expires_at":   nil,
				"last_used_at": nil,
				"revoked_at":   nil,
				"user_id":      nil,
			})
		return result.RowsAffected, result.Error
	})
}

// withBootstrapRetry retries write operations that fail due to transient DB lock errors.
// SQLite-specific lock messages ("database is locked", "database table is locked",
// "database is busy") are the only recognised retry triggers — other engines are not
// currently supported for the bootstrap path.
func (s *Service) withBootstrapRetry(fn func() (int64, error)) (int64, error) {
	var rows int64
	err := dbretry.Retry(context.Background(), s.lockRetryPolicy(), func() error {
		var err error
		rows, err = fn()
		return err
	})
	if err != nil {
		return 0, err
	}
	return rows, nil
}

// withReadRetry retries read-only operations with the same narrow lock policy.
func (s *Service) withReadRetry(fn func() error) error {
	return dbretry.Retry(context.Background(), s.lockRetryPolicy(), fn)
}

func (s *Service) lockRetryPolicy() dbretry.Policy {
	backoffs := make([]time.Duration, bootstrapRetryAttempts-1)
	for i := range backoffs {
		backoffs[i] = bootstrapRetryDelay
	}
	return dbretry.Policy{
		Backoffs: backoffs, Retryable: isBootstrapLockError,
		Wait: func(_ context.Context, delay time.Duration) error { s.sleep(delay); return nil },
	}
}

func isBootstrapLockError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "database is busy")
}
