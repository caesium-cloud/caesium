package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrKeyNotFound = errors.New("api key not found")
	ErrKeyRevoked  = errors.New("api key revoked")
	ErrKeyExpired  = errors.New("api key expired")
	ErrForbidden   = errors.New("insufficient permissions")
)

const (
	defaultValidationFailureMinLatency = 25 * time.Millisecond
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

	// lastUsedMu protects the async last_used_at update buffer.
	lastUsedMu sync.Mutex
	lastUsed   map[uuid.UUID]time.Time
}

// ServiceOption customizes auth service behavior.
type ServiceOption func(*Service)

// WithKeyHashSecret configures the server-side secret used for keyed API-key hashes.
func WithKeyHashSecret(secret string) ServiceOption {
	return func(s *Service) {
		s.keyHashSecret = secret
	}
}

// WithValidationFailureMinLatency configures the minimum latency for auth failures.
func WithValidationFailureMinLatency(d time.Duration) ServiceOption {
	return func(s *Service) {
		s.validationFailureMinLatency = d
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

// StartLastUsedFlusher runs a background goroutine that periodically flushes
// buffered last_used_at timestamps to the database. This keeps the hot auth
// path free of writes.
func (s *Service) StartLastUsedFlusher(ctx context.Context) {
	go func() {
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
	}()
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

// ValidateKey looks up a plaintext API key, verifies it is active, and returns
// the key record. On success it asynchronously updates last_used_at.
func (s *Service) ValidateKey(plaintext string) (_ *models.APIKey, retErr error) {
	startedAt := s.now()
	defer func() {
		s.applyFailureLatency(startedAt, retErr)
	}()

	hashes, err := HashLookupCandidates(plaintext, s.keyHashSecret)
	if err != nil {
		return nil, fmt.Errorf("hash api key: %w", err)
	}

	var key models.APIKey
	if err := s.db.Where("key_hash IN ?", hashes).Order("created_at DESC").First(&key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}

	if key.IsRevoked() {
		return nil, ErrKeyRevoked
	}
	if s.isExpired(&key) {
		return nil, ErrKeyExpired
	}

	s.recordLastUsed(key.ID)
	return &key, nil
}

// CreateKeyRequest holds the parameters for creating a new API key.
type CreateKeyRequest struct {
	Description string
	Role        models.Role
	Scope       *models.KeyScope
	CreatedBy   string
	ExpiresAt   *time.Time
}

// CreateKeyResponse is returned on key creation — the only time the plaintext is available.
type CreateKeyResponse struct {
	Plaintext string         `json:"key"`
	Key       *models.APIKey `json:"api_key"`
}

// CreateKey generates a new API key and persists its hash.
func (s *Service) CreateKey(req *CreateKeyRequest) (*CreateKeyResponse, error) {
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
		CreatedBy:   req.CreatedBy,
		CreatedAt:   s.nowUTC(),
		ExpiresAt:   req.ExpiresAt,
	}

	if err := s.db.Create(key).Error; err != nil {
		return nil, fmt.Errorf("persist api key: %w", err)
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

// RevokeKey sets revoked_at on the specified key.
func (s *Service) RevokeKey(id uuid.UUID) error {
	now := time.Now().UTC()
	result := s.db.Model(&models.APIKey{}).Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", now)
	if result.Error != nil {
		return fmt.Errorf("revoke api key: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// RotateKey creates a new key and sets expires_at on the old key to allow a grace period.
func (s *Service) RotateKey(id uuid.UUID, gracePeriod time.Duration, actor string) (*CreateKeyResponse, error) {
	var oldKey models.APIKey
	if err := s.db.First(&oldKey, id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup old key: %w", err)
	}

	if oldKey.IsRevoked() {
		return nil, ErrKeyRevoked
	}

	// Create new key with same role and scope.
	var scope *models.KeyScope
	if len(oldKey.Scope) > 0 {
		scope = &models.KeyScope{}
		if err := json.Unmarshal(oldKey.Scope, scope); err != nil {
			return nil, fmt.Errorf("unmarshal old scope: %w", err)
		}
	}

	resp, err := s.CreateKey(&CreateKeyRequest{
		Description: oldKey.Description + " (rotated)",
		Role:        oldKey.Role,
		Scope:       scope,
		CreatedBy:   actor,
	})
	if err != nil {
		return nil, err
	}

	// Set grace period on old key.
	graceExpiry := s.nowUTC().Add(gracePeriod)
	if err := s.db.Model(&models.APIKey{}).Where("id = ?", id).Update("expires_at", graceExpiry).Error; err != nil {
		return nil, fmt.Errorf("set grace period: %w", err)
	}

	return resp, nil
}

// AdminKeyExists returns true if at least one non-revoked, non-expired admin key exists.
func (s *Service) AdminKeyExists() (bool, error) {
	var exists bool
	_, err := s.withBootstrapRetry(func() (int64, error) {
		var count int64
		now := s.nowUTC()
		err := s.db.Model(&models.APIKey{}).
			Where(
				"role = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)",
				models.RoleAdmin,
				now,
			).
			Count(&count).Error
		exists = count > 0
		return 1, err
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

	rowsAffected, err = s.tryRefreshBootstrapKey(prefix, hash, now)
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
	return errors.Is(err, ErrKeyNotFound) || errors.Is(err, ErrKeyRevoked) || errors.Is(err, ErrKeyExpired)
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

func (s *Service) tryRefreshBootstrapKey(prefix, hash string, now time.Time) (int64, error) {
	return s.withBootstrapRetry(func() (int64, error) {
		result := s.db.Model(&models.APIKey{}).
			Where(
				"bootstrap_slot = ? AND (revoked_at IS NOT NULL OR (expires_at IS NOT NULL AND expires_at <= ?))",
				bootstrapAdminSlot,
				now,
			).
			Updates(map[string]any{
				"key_prefix":   prefix,
				"key_hash":     hash,
				"description":  "Bootstrap admin key",
				"role":         models.RoleAdmin,
				"scope":        nil,
				"created_by":   "system",
				"created_at":   now,
				"expires_at":   nil,
				"last_used_at": nil,
				"revoked_at":   nil,
			})
		return result.RowsAffected, result.Error
	})
}

func (s *Service) withBootstrapRetry(fn func() (int64, error)) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < bootstrapRetryAttempts; attempt++ {
		rowsAffected, err := fn()
		if err == nil {
			return rowsAffected, nil
		}
		if !isBootstrapLockError(err) {
			return 0, err
		}
		lastErr = err
		if attempt < bootstrapRetryAttempts-1 {
			s.sleep(bootstrapRetryDelay)
		}
	}
	return 0, lastErr
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
