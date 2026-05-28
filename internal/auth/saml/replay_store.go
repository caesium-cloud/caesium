package saml

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
)

var (
	ErrAssertionReplay    = errors.New("saml assertion replay detected")
	ErrMissingAssertionID = errors.New("saml assertion id is required")
)

const defaultReplayTTL = 5 * time.Minute

// AssertionReplayCache records assertion IDs accepted by the provider.
type AssertionReplayCache interface {
	Record(ctx context.Context, issuer, assertionID string, expiresAt time.Time) error
}

// ReplayStore is a catalog-DB-backed replay cache for SAML assertion IDs.
type ReplayStore struct {
	db  *gorm.DB
	now func() time.Time
}

// ReplayStoreOption customizes replay-store behavior.
type ReplayStoreOption func(*ReplayStore)

// WithReplayNow overrides the replay-store clock. Intended for tests.
func WithReplayNow(now func() time.Time) ReplayStoreOption {
	return func(s *ReplayStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewReplayStore creates a replay cache backed by the given database.
func NewReplayStore(db *gorm.DB, opts ...ReplayStoreOption) *ReplayStore {
	s := &ReplayStore{db: db, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Record stores a verified assertion ID. A duplicate unexpired ID is rejected.
func (s *ReplayStore) Record(ctx context.Context, issuer, assertionID string, expiresAt time.Time) error {
	issuer = strings.TrimSpace(issuer)
	assertionID = strings.TrimSpace(assertionID)
	if assertionID == "" {
		return ErrMissingAssertionID
	}

	now := s.now().UTC()
	if issuer == "" {
		issuer = ProviderName
	}
	if expiresAt.IsZero() || !expiresAt.After(now) {
		expiresAt = now.Add(defaultReplayTTL)
	}
	expiresAt = expiresAt.UTC()

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&models.SAMLAssertionReplay{}).Error; err != nil {
			return fmt.Errorf("reap expired saml assertions: %w", err)
		}
		record := &models.SAMLAssertionReplay{
			Issuer:      issuer,
			AssertionID: assertionID,
			CreatedAt:   now,
			ExpiresAt:   expiresAt,
		}
		if err := tx.Create(record).Error; err != nil {
			if isUniqueConstraintError(err) {
				return ErrAssertionReplay
			}
			return fmt.Errorf("record saml assertion id: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// Reap deletes expired assertion IDs.
func (s *ReplayStore) Reap(ctx context.Context) (int64, error) {
	res := s.db.WithContext(ctx).Where("expires_at <= ?", s.now().UTC()).Delete(&models.SAMLAssertionReplay{})
	if res.Error != nil {
		return 0, fmt.Errorf("reap expired saml assertions: %w", res.Error)
	}
	return res.RowsAffected, nil
}

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}

	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		return sqliteErr.ExtendedCode == sqlite3.ErrConstraintUnique ||
			sqliteErr.ExtendedCode == sqlite3.ErrConstraintPrimaryKey
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		(strings.Contains(msg, "constraint failed") && strings.Contains(msg, "unique")) ||
		strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "duplicate entry")
}
