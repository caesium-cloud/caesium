package auth

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrSessionInvalid = errors.New("session not found")
	ErrSessionRevoked = errors.New("session revoked")
	ErrSessionExpired = errors.New("session expired")
	ErrUserDisabled   = errors.New("user disabled")
	// ErrUserChanged reports that the user was disabled (and perhaps enabled
	// again) after the login read it: its disable generation moved. The
	// login is refused and must be repeated at the IdP.
	ErrUserChanged = errors.New("user changed during login")
)

const sessionFlushBatchSize = 900

// SessionStore manages server-side login sessions in the catalog DB. Tokens
// are hashed at rest and last-seen updates are coalesced.
type SessionStore struct {
	db              *gorm.DB
	tokenHashSecret string
	idleTTL         time.Duration
	absoluteTTL     time.Duration
	now             func() time.Time

	seenMu sync.RWMutex
	seen   map[uuid.UUID]time.Time

	retry dbretry.Policy
	// beforeMint, when set (tests only), runs inside Create's transaction
	// after the user row was locked and checked, before the session insert.
	beforeMint func()
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
		retry:       catalogRetryPolicy(),
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
	// DisableGeneration is the user's disable generation as the login read
	// it. Every disable advances it, so Create refuses a login that was
	// checked before a disable even when the user was enabled again before
	// the insert. A NULL generation compares as 0.
	DisableGeneration int64
}

// Create mints a new session and returns the plaintext token for the cookie.
//
// The insert is fenced on the user's lifecycle (identity-and-access E3): one
// transaction locks the user row (FOR UPDATE on PostgreSQL; the SQLite and
// dqlite dialects drop the clause because their write transactions are
// serialised), requires the user to exist, be enabled and still be at
// req.DisableGeneration, and only then inserts. A concurrent disable therefore
// either commits first (Create returns ErrUserDisabled or ErrUserChanged) or
// waits for the insert and revokes the new session with the others. The whole
// transaction is retried on catalog contention.
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
	err = dbretry.Retry(ctx, s.retry, func() error {
		return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return s.mintTx(tx, req.UserID, req.DisableGeneration, sess)
		})
	})
	switch {
	case err == nil:
		return plaintext, sess, nil
	case errors.Is(err, ErrUserDisabled), errors.Is(err, ErrUserChanged), errors.Is(err, ErrUserNotFound):
		return "", nil, err
	default:
		return "", nil, fmt.Errorf("create session: %w", err)
	}
}

// mintTx is Create's transaction: lock and check the user row, then insert.
func (s *SessionStore) mintTx(tx *gorm.DB, userID uuid.UUID, generation int64, sess *models.Session) error {
	var user models.User
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Select("id", "disabled_at", "disable_generation").
		Where("id = ?", userID).
		Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUserNotFound
	}
	if err != nil {
		return err
	}
	if user.IsDisabled() {
		return ErrUserDisabled
	}
	if user.DisableGeneration != generation {
		return ErrUserChanged
	}
	if s.beforeMint != nil {
		s.beforeMint()
	}
	return tx.Create(sess).Error
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
	if now.After(sess.AbsoluteExpiresAt) {
		return nil, nil, ErrSessionExpired
	}
	idleExpiresAt := sess.IdleExpiresAt
	if seenAt, ok := s.pendingSeen(sess.ID); ok {
		pendingIdleExpiresAt := seenAt.Add(s.idleTTL)
		if pendingIdleExpiresAt.After(idleExpiresAt) {
			idleExpiresAt = pendingIdleExpiresAt
			sess.IdleExpiresAt = idleExpiresAt
		}
	}
	if now.After(idleExpiresAt) {
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
	if _, err := revokeUserSessionsTx(s.db.WithContext(ctx), userID, s.nowUTC()); err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

// revokeUserSessionsTx is the RevokeAllForUser update on a caller-supplied
// handle, so it can run inside a larger transaction (a definitive group
// refresh clears the grant and revokes in one). It returns how many live
// sessions it revoked.
func revokeUserSessionsTx(tx *gorm.DB, userID uuid.UUID, now time.Time) (int64, error) {
	res := tx.Model(&models.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now)
	return res.RowsAffected, res.Error
}

// SessionFilter selects active sessions for administration (E3). A nil
// UserID selects every user's sessions. Limit must be positive.
type SessionFilter struct {
	UserID *uuid.UUID
	Limit  int
	Offset int
}

// SessionPage is one page of active sessions, newest first, and the total
// number of active sessions the filter matches. HasMore is decided by the
// page query itself (a limit+1 probe), not by comparing against Total.
type SessionPage struct {
	Sessions []models.Session
	Total    int64
	HasMore  bool
}

// ListActive returns the sessions Validate on this node would accept now:
// not revoked, within the absolute lifetime, and within the idle lifetime
// once this node's buffered activity is applied, exactly as Validate applies
// it. The buffer is applied before selection, counting and paging, and the
// rows carry the effective idle expiry and last-seen time. Nothing is written:
// flushing would stamp the flush time and so extend sessions. Activity still
// buffered on another node is invisible here, as it is to this node's
// Validate; revocation never filters on expiry, so it is unaffected.
func (s *SessionStore) ListActive(ctx context.Context, f SessionFilter) (SessionPage, error) {
	if f.Limit <= 0 {
		return SessionPage{}, errors.New("list sessions: limit must be positive")
	}
	if f.Offset < 0 {
		return SessionPage{}, errors.New("list sessions: offset must not be negative")
	}
	now := s.nowUTC()
	pending := s.pendingSnapshot()

	// Sessions whose persisted idle expiry has lapsed but whose buffered
	// activity keeps them valid. With the flusher running this set is
	// almost always empty.
	var candidates []uuid.UUID
	for id, seenAt := range pending {
		if !now.After(seenAt.Add(s.idleTTL)) {
			candidates = append(candidates, id)
		}
	}
	slices.SortFunc(candidates, func(a, b uuid.UUID) int { return strings.Compare(a.String(), b.String()) })
	var rescued []uuid.UUID
	for start := 0; start < len(candidates); start += sessionFlushBatchSize {
		end := min(start+sessionFlushBatchSize, len(candidates))
		var ids []uuid.UUID
		q := s.activeScope(s.db.WithContext(ctx), f.UserID, now).
			Where("id IN ? AND idle_expires_at < ?", candidates[start:end], now)
		if err := q.Model(&models.Session{}).Pluck("id", &ids).Error; err != nil {
			return SessionPage{}, fmt.Errorf("list sessions: %w", err)
		}
		rescued = append(rescued, ids...)
	}

	selectActive := func() *gorm.DB {
		q := s.activeScope(s.db.WithContext(ctx).Model(&models.Session{}), f.UserID, now)
		if len(rescued) == 0 {
			return q.Where("idle_expires_at >= ?", now)
		}
		return q.Where("(idle_expires_at >= ? OR id IN ?)", now, rescued)
	}

	var page SessionPage
	if err := selectActive().Count(&page.Total).Error; err != nil {
		return SessionPage{}, fmt.Errorf("count sessions: %w", err)
	}
	var rows []models.Session
	if err := selectActive().
		Order("created_at DESC").Order("id DESC").
		Limit(f.Limit + 1).Offset(f.Offset).
		Find(&rows).Error; err != nil {
		return SessionPage{}, fmt.Errorf("list sessions: %w", err)
	}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		page.HasMore = true
	}
	for i := range rows {
		seenAt, ok := pending[rows[i].ID]
		if !ok {
			continue
		}
		if idle := seenAt.Add(s.idleTTL); idle.After(rows[i].IdleExpiresAt) {
			rows[i].IdleExpiresAt = idle
		}
		if rows[i].LastSeenAt == nil || seenAt.After(*rows[i].LastSeenAt) {
			seen := seenAt
			rows[i].LastSeenAt = &seen
		}
	}
	page.Sessions = rows
	return page, nil
}

// activeScope narrows q to unrevoked sessions within their absolute lifetime
// (Validate rejects only once now is after the expiry), optionally of one
// user.
func (s *SessionStore) activeScope(q *gorm.DB, userID *uuid.UUID, now time.Time) *gorm.DB {
	q = q.Where("revoked_at IS NULL AND absolute_expires_at >= ?", now)
	if userID != nil {
		q = q.Where("user_id = ?", *userID)
	}
	return q
}

// pendingSnapshot copies this node's buffered session activity.
func (s *SessionStore) pendingSnapshot() map[uuid.UUID]time.Time {
	s.seenMu.RLock()
	defer s.seenMu.RUnlock()
	return maps.Clone(s.seen)
}

// RunLastSeenFlusher periodically flushes buffered session activity.
func (s *SessionStore) RunLastSeenFlusher(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flushSeen(context.Background())
			return
		case <-ticker.C:
			s.flushSeen(ctx)
		}
	}
}

// Reap deletes expired sessions and sessions revoked more than an hour ago.
func (s *SessionStore) Reap(ctx context.Context) (int64, error) {
	s.flushSeen(ctx)

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

func (s *SessionStore) flushSeen(ctx context.Context) {
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
	for start := 0; start < len(ids); start += sessionFlushBatchSize {
		end := min(start+sessionFlushBatchSize, len(ids))
		if err := s.db.WithContext(ctx).Model(&models.Session{}).Where("id IN ?", ids[start:end]).Updates(map[string]any{
			"last_seen_at":    now,
			"idle_expires_at": now.Add(s.idleTTL),
		}).Error; err != nil {
			log.Warn("failed to flush session last_seen batch", "error", err)
		}
	}
}

func (s *SessionStore) recordSeen(id uuid.UUID) {
	s.seenMu.Lock()
	s.seen[id] = s.nowUTC()
	s.seenMu.Unlock()
}

func (s *SessionStore) pendingSeen(id uuid.UUID) (time.Time, bool) {
	s.seenMu.RLock()
	seenAt, ok := s.seen[id]
	s.seenMu.RUnlock()
	return seenAt, ok
}

func (s *SessionStore) nowUTC() time.Time {
	return s.now().UTC()
}
