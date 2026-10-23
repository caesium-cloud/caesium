package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// User-bound API keys (identity-and-access E4, design §7.3). A user-bound key
// is the credential `caesium login` (E5) stores: it belongs to a user, stores
// no role, namespaces or scope, and authenticates as that user, whose grants
// are resolved from the policy on every request exactly as for the user's
// browser session. It always expires, and it dies with the user: disabling the
// user, logging the user out everywhere, or a definitive group-refresh denial
// revokes it in the same transaction as the user's sessions
// (revokeUserCredentialsTx), and validation refuses it while the user is
// disabled even if it was somehow left unrevoked.

const (
	// DefaultUserKeyTTL is the lifetime of a user-bound key when
	// CAESIUM_AUTH_CLI_KEY_TTL is unset.
	DefaultUserKeyTTL = 12 * time.Hour
	// MaxUserKeyTTL bounds every user-bound key's lifetime.
	MaxUserKeyTTL = 7 * 24 * time.Hour
)

// WithUserKeyTTL configures the lifetime of minted user-bound keys
// (CAESIUM_AUTH_CLI_KEY_TTL). A non-positive value means DefaultUserKeyTTL;
// a value above MaxUserKeyTTL is clamped to it.
func WithUserKeyTTL(ttl time.Duration) ServiceOption {
	return func(s *Service) {
		s.userKeyTTL = ttl
	}
}

func (s *Service) effectiveUserKeyTTL() time.Duration {
	switch {
	case s.userKeyTTL <= 0:
		return DefaultUserKeyTTL
	case s.userKeyTTL > MaxUserKeyTTL:
		return MaxUserKeyTTL
	default:
		return s.userKeyTTL
	}
}

// userKeyMintHook, when set (tests only), runs inside MintUserBoundKey's
// transaction after the user row is checked and before the key is inserted,
// so a test can race a disable against the mint deterministically.
var userKeyMintHook func()

// MintUserKeyRequest asks for a user-bound key.
type MintUserKeyRequest struct {
	// UserID is the user the key authenticates.
	UserID uuid.UUID
	// DisableGeneration is the user's disable generation as read when the
	// minting request was authorised (the session's user). The mint is fenced
	// on it, as SessionStore.Create is: a disable committed since then refuses
	// the mint even if the user was enabled again.
	DisableGeneration int64
	// Description labels the key (for example the CLI host it was minted for).
	Description string
	// CreatedBy is the audit actor of the mint (the user's email).
	CreatedBy string
}

// MintUserBoundKey mints a user-bound key for a user (the seam E5's
// /auth/cli/authorize calls; nothing in E4 exposes it as a route). In one
// transaction, retried as a whole on catalog contention, it locks the user
// row (FOR UPDATE on PostgreSQL; the SQLite and dqlite dialects drop the
// clause because their write transactions are serialised), requires the user
// to exist, be enabled and still be at req.DisableGeneration, and only then
// inserts the key, so a concurrent disable either commits first (the mint is
// refused with ErrUserDisabled or ErrUserChanged) or waits for the insert and
// revokes the new key with the user's other credentials. The key expires after
// the configured TTL. The plaintext is returned once and never stored.
func (s *Service) MintUserBoundKey(ctx context.Context, req MintUserKeyRequest) (*CreateKeyResponse, error) {
	if req.UserID == uuid.Nil {
		return nil, ErrUserNotFound
	}
	var resp *CreateKeyResponse
	err := s.keyTransaction(ctx, func(tx *gorm.DB) error {
		minted, err := s.mintUserBoundKeyTx(tx, req)
		resp = minted
		return err
	})
	return mintUserKeyResult(resp, err)
}

// mintUserKeyResult maps a mint transaction's outcome: the fence errors pass
// through unwrapped, anything else is wrapped.
func mintUserKeyResult(resp *CreateKeyResponse, err error) (*CreateKeyResponse, error) {
	switch {
	case err == nil:
		return resp, nil
	case errors.Is(err, ErrUserDisabled), errors.Is(err, ErrUserChanged), errors.Is(err, ErrUserNotFound):
		return nil, err
	default:
		return nil, fmt.Errorf("mint user-bound api key: %w", err)
	}
}

// mintUserBoundKeyTx is the one mint path for user-bound keys, on a
// caller-supplied transaction: MintUserBoundKey runs it in a transaction of
// its own, and the CLI login exchange (cli_login.go) runs it in the same
// transaction that consumes the one-time code. It generates a fresh key on
// every call (a retried transaction never reuses an earlier attempt's key),
// fences on the user row (lockMintableUser) and inserts the key.
func (s *Service) mintUserBoundKeyTx(tx *gorm.DB, req MintUserKeyRequest) (*CreateKeyResponse, error) {
	if req.UserID == uuid.Nil {
		return nil, ErrUserNotFound
	}
	plaintext, prefix, err := GenerateUserKey()
	if err != nil {
		return nil, err
	}
	hash, err := HashKey(plaintext, s.keyHashSecret)
	if err != nil {
		return nil, err
	}
	now := s.nowUTC()
	expiresAt := now.Add(s.effectiveUserKeyTTL())
	userID := req.UserID
	key := &models.APIKey{
		ID:          uuid.New(),
		KeyPrefix:   prefix,
		KeyHash:     hash,
		Description: req.Description,
		CreatedBy:   req.CreatedBy,
		CreatedAt:   now,
		ExpiresAt:   &expiresAt,
		UserID:      &userID,
	}
	if err := lockMintableUser(tx, req.UserID, req.DisableGeneration); err != nil {
		return nil, err
	}
	if userKeyMintHook != nil {
		userKeyMintHook()
	}
	if err := tx.Create(key).Error; err != nil {
		return nil, err
	}
	return &CreateKeyResponse{Plaintext: plaintext, Key: key}, nil
}

// lockMintableUser is the user-row fence of MintUserBoundKey; it mirrors
// SessionStore.mintTx.
func lockMintableUser(tx *gorm.DB, userID uuid.UUID, generation int64) error {
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
	return nil
}

// validateUserKey authenticates an active, unexpired user-bound key as its
// user. A malformed row (one carrying a role, namespaces or scope, or no
// expiry, none of which the mint writes) and a missing user are
// ErrKeyNotFound; a disabled user is ErrUserDisabled. The caller answers all
// of them with the same 401 a revoked key gets, after the same latency floor.
func (s *Service) validateUserKey(ctx context.Context, key *models.APIKey) (*Principal, error) {
	if !wellFormedUserKey(key) {
		log.Warn("user-bound api key row is malformed; refusing it", "key_prefix", key.KeyPrefix)
		return nil, ErrKeyNotFound
	}
	var user models.User
	err := s.db.WithContext(ctx).Where("id = ?", *key.UserID).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrKeyNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load api key user: %w", err)
	}
	if user.IsDisabled() {
		return nil, ErrUserDisabled
	}
	// Group refresh (identity-and-access §11.1, E2): a due refresh is queued,
	// and past the maximum staleness the key is refused (stale_groups) unless
	// one synchronous refresh succeeds.
	if s.groupFreshness != nil {
		admitted, synced, err := s.groupFreshness.Admit(ctx, &user)
		if synced {
			// The request waited: re-check the key on a fresh row (a
			// definitive refusal or an admin may have revoked it); a
			// cancelled request is refused, never admitted unchecked.
			if rerr := syncedRevalidation(ctx, err, func() error { return s.revalidateKey(ctx, key) }); rerr != nil {
				return nil, rerr
			}
		}
		if err != nil {
			return nil, err
		}
		user = *admitted
	}
	return PrincipalFromUserKey(key, &user, s.policySource), nil
}

// revalidateKey re-reads a user-bound key after a synchronous group refresh.
func (s *Service) revalidateKey(ctx context.Context, key *models.APIKey) error {
	var current models.APIKey
	err := s.db.WithContext(ctx).Where("id = ?", key.ID).Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrKeyNotFound
	}
	if err != nil {
		return fmt.Errorf("reload api key: %w", err)
	}
	if current.IsRevoked() {
		return ErrKeyRevoked
	}
	if s.isExpired(&current) {
		return ErrKeyExpired
	}
	return nil
}

// wellFormedUserKey reports whether a user-bound row has exactly the shape
// MintUserBoundKey writes: a user, an expiry, and no role, namespaces or
// scope. Anything else is refused rather than half-honoured: a stored scope
// would otherwise be silently dropped, and a key without an expiry would
// never die on its own.
func wellFormedUserKey(k *models.APIKey) bool {
	return k.UserID != nil && *k.UserID != uuid.Nil &&
		k.ExpiresAt != nil &&
		k.Role == "" &&
		len(k.Namespaces) == 0 &&
		len(k.Scope) == 0
}

// revokeUserKeysTx revokes every unrevoked user-bound key of a user on a
// caller-supplied handle and returns how many it revoked.
func revokeUserKeysTx(tx *gorm.DB, userID uuid.UUID, now time.Time) (int64, error) {
	res := tx.Model(&models.APIKey{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", now)
	return res.RowsAffected, res.Error
}

// activeUserKeys returns at most limit of a user's active (unrevoked,
// unexpired) user-bound keys, newest first, and how many there are.
func activeUserKeys(ctx context.Context, db *gorm.DB, userID uuid.UUID, now time.Time, limit int) ([]models.APIKey, int64, error) {
	scope := func() *gorm.DB {
		return db.WithContext(ctx).Model(&models.APIKey{}).
			Where("user_id = ? AND revoked_at IS NULL AND expires_at > ?", userID, now)
	}
	var total int64
	if err := scope().Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count user-bound api keys: %w", err)
	}
	keys := []models.APIKey{}
	if total > 0 {
		if err := scope().Order("created_at DESC").Order("id DESC").Limit(limit).Find(&keys).Error; err != nil {
			return nil, 0, fmt.Errorf("list user-bound api keys: %w", err)
		}
	}
	return keys, total, nil
}
