package auth

import (
	"context"
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
	ErrSessionInvalid = errors.New("session not found")
	ErrSessionRevoked = errors.New("session revoked")
	ErrSessionExpired = errors.New("session expired")
	ErrUserDisabled   = errors.New("user disabled")
)

// SessionStore manages server-side login sessions in the catalog DB. Tokens
// are hashed at rest and last-seen updates are coalesced.
type SessionStore struct {
	db              *gorm.DB
	tokenHashSecret string
	idleTTL         time.Duration
	absoluteTTL     time.Duration
	now             func() time.Time

	seenMu sync.Mutex
	seen   map[uuid.UUID]time.Time
}

// SessionStoreOption customizes session-store behavior.
type SessionStoreOption func(*SessionStore)

// WithSessionHashSecret configures the server-side secret for session-token hashes.
func WithSessionHashSecret(secret string) SessionStoreOption {
	return func(s *SessionStore) {
		s.tokenHashSecret = secret
	}
}

// WithSessionTTLs configures idle and absolute session lifetimes.
func WithSessionTTLs(idle, absolute time.Duration) SessionStoreOption {
	return func(s *SessionStore) {
		s.idleTTL = idle
		s.absoluteTTL = absolute
	}
}

// WithSessionNow overrides the session-store clock. Intended for tests.
func WithSessionNow(now func() time.Time) SessionStoreOption {
	return func(s *SessionStore) {
		if now != nil {
			s.now = now
		}
	}
}

// NewSessionStore creates a new session store backed by the given database.
func NewSessionStore(db *gorm.DB, opts ...SessionStoreOption) *SessionStore {
	s := &SessionStore{
		db:          db,
		idleTTL:     8 * time.Hour,
		absoluteTTL: 24 * time.Hour,
		now:         time.Now,
		seen:        make(map[uuid.UUID]time.Time),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// CreateSessionRequest holds parameters for minting a session.
type CreateSessionRequest struct {
	UserID     uuid.UUID
	AuthMethod string
	SourceIP   string
	UserAgent  string
}

// Create mints a new session and returns the plaintext token for the cookie.
func (s *SessionStore) Create(ctx context.Context, req CreateSessionRequest) (string, *models.Session, error) {
	plaintext, err := GenerateToken()
	if err != nil {
		return "", nil, err
	}
	hash, err := HashKey(plaintext, s.tokenHashSecret)
	if err != nil {
		return "", nil, fmt.Errorf("hash session token: %w", err)
	}
	csrf, err := base62Encode(randomBytes)
	if err != nil {
		return "", nil, fmt.Errorf("generate csrf token: %w", err)
	}

	now := s.nowUTC()
	sess := &models.Session{
		ID:                uuid.New(),
		UserID:            req.UserID,
		TokenHash:         hash,
		CSRFToken:         csrf,
		AuthMethod:        req.AuthMethod,
		CreatedAt:         now,
		IdleExpiresAt:     now.Add(s.idleTTL),
		AbsoluteExpiresAt: now.Add(s.absoluteTTL),
		SourceIP:          req.SourceIP,
		UserAgent:         req.UserAgent,
	}
	if err := s.db.WithContext(ctx).Create(sess).Error; err != nil {
		return "", nil, fmt.Errorf("create session: %w", err)
	}
	return plaintext, sess, nil
}

// Validate resolves a plaintext token to its live session and user.
func (s *SessionStore) Validate(ctx context.Context, plaintext string) (*models.Session, *models.User, error) {
	hash, err := HashKey(plaintext, s.tokenHashSecret)
	if err != nil {
		return nil, nil, fmt.Errorf("hash session token: %w", err)
	}

	var sess models.Session
	if err := s.db.WithContext(ctx).Where("token_hash = ?", hash).First(&sess).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, ErrSessionInvalid
		}
		return nil, nil, fmt.Errorf("lookup session: %w", err)
	}
	if sess.IsRevoked() {
		return nil, nil, ErrSessionRevoked
	}
	now := s.nowUTC()
	if now.After(sess.AbsoluteExpiresAt) || now.After(sess.IdleExpiresAt) {
		return nil, nil, ErrSessionExpired
	}

	var user models.User
	if err := s.db.WithContext(ctx).First(&user, "id = ?", sess.UserID).Error; err != nil {
		return nil, nil, fmt.Errorf("load session user: %w", err)
	}
	if user.IsDisabled() {
		return nil, nil, ErrUserDisabled
	}

	s.recordSeen(sess.ID)
	return &sess, &user, nil
}

// Revoke marks a single session revoked.
func (s *SessionStore) Revoke(ctx context.Context, id uuid.UUID) error {
	res := s.db.WithContext(ctx).Model(&models.Session{}).
		Where("id = ? AND revoked_at IS NULL", id).
		Update("revoked_at", s.nowUTC())
	if res.Error != nil {
		return fmt.Errorf("revoke session: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSessionInvalid
	}
	return nil
}

// RevokeAllForUser revokes every live session for a user.
func (s *SessionStore) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	if err := s.db.WithContext(ctx).Model(&models.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", s.nowUTC()).Error; err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// RunLastSeenFlusher periodically flushes buffered session activity.
func (s *SessionStore) RunLastSeenFlusher(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushSeen()
			return
		case <-ticker.C:
			s.flushSeen()
		}
	}
}

// Reap deletes expired sessions and sessions revoked more than an hour ago.
func (s *SessionStore) Reap(ctx context.Context) (int64, error) {
	now := s.nowUTC()
	res := s.db.WithContext(ctx).
		Where(
			"absolute_expires_at < ? OR idle_expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)",
			now,
			now,
			now.Add(-time.Hour),
		).
		Delete(&models.Session{})
	if res.Error != nil {
		return 0, fmt.Errorf("reap sessions: %w", res.Error)
	}
	return res.RowsAffected, nil
}

// RunReaper sweeps expired sessions until ctx is cancelled.
func (s *SessionStore) RunReaper(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := s.Reap(ctx); err != nil {
				log.Warn("session reap failed", "error", err)
			}
		}
	}
}

func (s *SessionStore) flushSeen() {
	s.seenMu.Lock()
	pending := s.seen
	s.seen = make(map[uuid.UUID]time.Time, len(pending))
	s.seenMu.Unlock()
	if len(pending) == 0 {
		return
	}

	ids := make([]uuid.UUID, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	now := s.nowUTC()
	if err := s.db.Model(&models.Session{}).Where("id IN ?", ids).Updates(map[string]any{
		"last_seen_at":    now,
		"idle_expires_at": now.Add(s.idleTTL),
	}).Error; err != nil {
		log.Warn("failed to flush session last_seen", "error", err)
	}
}

func (s *SessionStore) recordSeen(id uuid.UUID) {
	s.seenMu.Lock()
	s.seen[id] = s.nowUTC()
	s.seenMu.Unlock()
}

func (s *SessionStore) nowUTC() time.Time {
	return s.now().UTC()
}
