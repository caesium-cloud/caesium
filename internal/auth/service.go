package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrKeyNotFound = errors.New("api key not found")
	ErrKeyRevoked  = errors.New("api key revoked")
	ErrKeyExpired  = errors.New("api key expired")
	ErrForbidden   = errors.New("insufficient permissions")
)

// Service provides API key management and validation.
type Service struct {
	db *gorm.DB

	// lastUsedMu protects the async last_used_at update buffer.
	lastUsedMu sync.Mutex
	lastUsed   map[uuid.UUID]time.Time
}

// NewService creates a new auth service backed by the given database.
func NewService(db *gorm.DB) *Service {
	s := &Service{
		db:       db,
		lastUsed: make(map[uuid.UUID]time.Time),
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

	if err := s.db.Model(&models.APIKey{}).Where("id IN ?", ids).Update("last_used_at", time.Now().UTC()).Error; err != nil {
		log.Warn("failed to batch update api key last_used_at", "error", err)
	}
}

func (s *Service) recordLastUsed(id uuid.UUID) {
	s.lastUsedMu.Lock()
	s.lastUsed[id] = time.Now().UTC()
	s.lastUsedMu.Unlock()
}

// ValidateKey looks up a plaintext API key, verifies it is active, and returns
// the key record. On success it asynchronously updates last_used_at.
func (s *Service) ValidateKey(plaintext string) (*models.APIKey, error) {
	hash := HashKey(plaintext)

	var key models.APIKey
	if err := s.db.Where("key_hash = ?", hash).First(&key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKeyNotFound
		}
		return nil, fmt.Errorf("lookup api key: %w", err)
	}

	if key.IsRevoked() {
		return nil, ErrKeyRevoked
	}
	if key.IsExpired() {
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
	plaintext, prefix, hash, err := GenerateKey()
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
		CreatedAt:   time.Now().UTC(),
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
	graceExpiry := time.Now().UTC().Add(gracePeriod)
	if err := s.db.Model(&models.APIKey{}).Where("id = ?", id).Update("expires_at", graceExpiry).Error; err != nil {
		return nil, fmt.Errorf("set grace period: %w", err)
	}

	return resp, nil
}

// AdminKeyExists returns true if at least one non-revoked, non-expired admin key exists.
func (s *Service) AdminKeyExists() (bool, error) {
	var count int64
	now := time.Now().UTC()
	err := s.db.Model(&models.APIKey{}).
		Where(
			"role = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)",
			models.RoleAdmin,
			now,
		).
		Count(&count).Error
	if err != nil {
		return false, fmt.Errorf("check admin keys: %w", err)
	}
	return count > 0, nil
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

	resp, err := s.CreateKey(&CreateKeyRequest{
		Description: "Bootstrap admin key",
		Role:        models.RoleAdmin,
		CreatedBy:   "system",
	})
	if err != nil {
		return "", fmt.Errorf("bootstrap admin key: %w", err)
	}

	return resp.Plaintext, nil
}
