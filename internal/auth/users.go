package auth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/caesium-cloud/caesium/internal/dbretry"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/caesium-cloud/caesium/pkg/sqlerr"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserStore provisions and updates user identities. It also owns each user's
// refresh grant: login replaces it, and the lease and fence primitives below
// are the only way a group refresher may redeem it or record an outcome.
type UserStore struct {
	db     *gorm.DB
	now    func() time.Time
	grants *refreshCipher
	retry  dbretry.Policy

	warnNoCipher sync.Once
}

// UserStoreOption customizes a UserStore.
type UserStoreOption func(*UserStore)

// WithRefreshGrantSecret seals refresh grants under a key derived from secret
// (CAESIUM_AUTH_KEY_HASH_SECRET). Without it, or with an empty secret, logins
// store no grant and groups are never refreshed.
func WithRefreshGrantSecret(secret string) UserStoreOption {
	return func(us *UserStore) {
		grants, err := newRefreshCipher(secret)
		if err != nil {
			us.grants = nil
			return
		}
		us.grants = grants
	}
}

// WithUserStoreNow overrides the store's clock. Intended for tests.
func WithUserStoreNow(now func() time.Time) UserStoreOption {
	return func(us *UserStore) {
		if now != nil {
			us.now = now
		}
	}
}

// NewUserStore creates a user store backed by the given database.
func NewUserStore(db *gorm.DB, opts ...UserStoreOption) *UserStore {
	us := &UserStore{db: db, now: time.Now, retry: catalogRetryPolicy()}
	for _, opt := range opts {
		opt(us)
	}
	return us
}

// userTxRetryBackoffs bounds the whole-unit retry of a user-store write on
// catalog contention (~1.3s in total).
var userTxRetryBackoffs = []time.Duration{
	10 * time.Millisecond,
	20 * time.Millisecond,
	40 * time.Millisecond,
	80 * time.Millisecond,
	160 * time.Millisecond,
	320 * time.Millisecond,
	640 * time.Millisecond,
}

// catalogRetryPolicy retries a whole unit of work (one statement or one
// transaction, never part of one) when the catalog reports transient
// contention: a dqlite/SQLite lock or busy error, or a PostgreSQL
// serialization failure or deadlock.
func catalogRetryPolicy() dbretry.Policy {
	return dbretry.Policy{
		Backoffs:      userTxRetryBackoffs,
		Retryable:     isCatalogConflict,
		Wait:          sleepCatalogRetry,
		BeforeAttempt: true,
		OnRetry:       func(error) { metrics.DBBusyRetriesTotal.Inc() },
	}
}

func isCatalogConflict(err error) bool {
	return dbretry.IsLockContention(err) || dbretry.IsSerializationConflict(err)
}

func sleepCatalogRetry(ctx context.Context, base time.Duration) error {
	wait := base
	if jitter := int64(base / 5); jitter > 0 {
		wait -= time.Duration(mathrand.Int64N(jitter + 1))
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// transaction runs fn in one transaction and retries the whole transaction on
// catalog contention. A rolled-back attempt leaves nothing behind, so fn must
// reset any result it reports at its start.
func (us *UserStore) transaction(ctx context.Context, fn func(tx *gorm.DB) error) error {
	return dbretry.Retry(ctx, us.retry, func() error {
		return us.db.WithContext(ctx).Transaction(fn)
	})
}

// statement runs one autocommit statement with the same retry.
func (us *UserStore) statement(ctx context.Context, fn func(db *gorm.DB) error) error {
	return dbretry.Retry(ctx, us.retry, func() error {
		return fn(us.db.WithContext(ctx))
	})
}

// Upsert provisions a user on first login and refreshes profile, role, and
// last-login fields on subsequent logins, keyed on (issuer, subject).
func (us *UserStore) Upsert(ctx context.Context, ext *ExternalIdentity, role models.Role) (*models.User, error) {
	user, _, err := us.upsert(ctx, ext, role)
	return user, err
}

func (us *UserStore) upsert(ctx context.Context, ext *ExternalIdentity, role models.Role) (*models.User, bool, error) {
	now := us.now().UTC()
	groupsJSON, err := json.Marshal(ext.Groups)
	if err != nil {
		return nil, false, fmt.Errorf("marshal groups: %w", err)
	}

	var user models.User
	err = us.db.WithContext(ctx).Where("issuer = ? AND subject = ?", ext.Issuer, ext.Subject).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		id := uuid.New()
		// A first login starts the grant at version 1 and confirms the
		// groups now (§11.1). The grant is sealed for this row's ID.
		user = models.User{
			ID:                  id,
			Issuer:              ext.Issuer,
			Subject:             ext.Subject,
			Email:               ext.Email,
			DisplayName:         ext.DisplayName,
			Groups:              groupsJSON,
			Role:                role,
			EmailVerified:       ext.EmailVerified,
			CreatedAt:           now,
			LastLoginAt:         &now,
			RefreshGrant:        us.sealLoginGrant(id, ext),
			RefreshGrantVersion: 1,
			GroupsRefreshedAt:   &now,
		}
		if err := us.db.WithContext(ctx).Create(&user).Error; err != nil {
			if sqlerr.IsUniqueConstraint(err) {
				if existing, lookupErr := us.lookupByIdentity(ctx, ext); lookupErr == nil {
					updated, err := us.updateExisting(ctx, &existing, ext, role, groupsJSON, now)
					return updated, false, err
				}
			}
			return nil, false, fmt.Errorf("create user: %w", err)
		}
	case err != nil:
		return nil, false, fmt.Errorf("lookup user: %w", err)
	default:
		updated, err := us.updateExisting(ctx, &user, ext, role, groupsJSON, now)
		return updated, false, err
	}

	return &user, true, nil
}

func (us *UserStore) lookupByIdentity(ctx context.Context, ext *ExternalIdentity) (models.User, error) {
	var user models.User
	err := us.db.WithContext(ctx).Where("issuer = ? AND subject = ?", ext.Issuer, ext.Subject).First(&user).Error
	return user, err
}

// updateExisting applies a login to an existing user. A disabled user's login
// changes nothing, refresh state included; the caller denies it.
func (us *UserStore) updateExisting(ctx context.Context, user *models.User, ext *ExternalIdentity, role models.Role, groupsJSON []byte, now time.Time) (*models.User, error) {
	if user.IsDisabled() {
		return user, nil
	}

	// The login replaces the grant (the newest IdP session wins) with a
	// database increment of its version, so an in-flight refresh of the
	// previous grant is fenced out whatever it learns, and it clears any
	// lease and uncertainty, which belonged to the previous grant. A map
	// update writes false too, so a later unverified login downgrades the
	// stored email trust.
	var grant any
	if sealed := us.sealLoginGrant(user.ID, ext); sealed != nil {
		grant = sealed
	}
	updates := map[string]any{
		"email":                 ext.Email,
		"display_name":          ext.DisplayName,
		"groups":                groupsJSON,
		"role":                  role,
		"email_verified":        ext.EmailVerified,
		"last_login_at":         now,
		"refresh_grant":         grant,
		"refresh_grant_version": nextGrantVersion(),
		"groups_refreshed_at":   now,
		"refresh_lease_owner":   nil,
		"refresh_lease_until":   nil,
		"refresh_uncertain":     false,
	}
	var stored models.User
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		stored = models.User{}
		// disabled_at IS NULL closes the window between the read above and
		// this write: a user disabled in between is left untouched and the
		// re-read reports it disabled.
		if err := tx.Model(&models.User{}).
			Where("id = ? AND disabled_at IS NULL", user.ID).
			Updates(updates).Error; err != nil {
			return err
		}
		return tx.Where("id = ?", user.ID).Take(&stored).Error
	})
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	return &stored, nil
}

// sealLoginGrant seals the login's refresh grant for userID, or returns nil
// (store no grant) when the provider gave none, the grant does not belong to
// the login's issuer, or no secret is configured. Nil is always safe: the
// user then refreshes nothing and the staleness bound forces a re-login.
func (us *UserStore) sealLoginGrant(userID uuid.UUID, ext *ExternalIdentity) []byte {
	if ext == nil || ext.Refresh == nil {
		return nil
	}
	grant := *ext.Refresh
	if !grant.complete() || grant.Issuer != ext.Issuer {
		log.Warn("discarding a refresh grant that does not match the login identity", "provider", grant.Provider)
		return nil
	}
	if us.grants == nil {
		us.warnNoCipher.Do(func() {
			log.Warn("refresh grants are not stored: the user store has no CAESIUM_AUTH_KEY_HASH_SECRET")
		})
		return nil
	}
	sealed, err := us.grants.seal(userID, grant)
	if err != nil {
		log.Warn("failed to seal a refresh grant; storing none", "provider", grant.Provider, "error", err)
		return nil
	}
	return sealed
}

// nextGrantVersion advances refresh_grant_version in the database, treating a
// NULL (a user from before E1) as 0. Never increment a value read earlier.
func nextGrantVersion() any {
	return gorm.Expr("COALESCE(refresh_grant_version, 0) + 1")
}

// ErrRefreshLeaseLost reports that the caller no longer holds the refresh
// lease at that grant version: a login replaced the grant, another holder
// took the lease over, or an outcome was already recorded.
var ErrRefreshLeaseLost = errors.New("refresh lease not held")

// NewRefreshLeaseOwner returns a lease owner for one refresh attempt: the
// node identity plus a random suffix, so two attempts never share an owner,
// even on one node.
func NewRefreshLeaseOwner(node string) string {
	node = strings.TrimSpace(node)
	if node == "" {
		node = "node"
	}
	return node + "/" + rand.Text()
}

// ClaimRefreshLease takes the user's refresh lease for grant version, until
// now+ttl, with one compare-and-swap. It succeeds only when the row still
// holds a grant at that version and no unexpired lease. Only the holder may
// redeem the grant (LoadLeasedRefreshGrant) and record an outcome.
//
// Every outcome clears the lease owner, so an owner still set on an expired
// lease means its holder died or never recorded an outcome, possibly after
// redeeming (and rotating) the grant. Taking such a lease over therefore
// marks the grant uncertain in the same statement; a claim over a released
// lease leaves the uncertainty as it is.
func (us *UserStore) ClaimRefreshLease(ctx context.Context, userID uuid.UUID, version int64, owner string, now time.Time, ttl time.Duration) (bool, error) {
	if strings.TrimSpace(owner) == "" {
		return false, errors.New("refresh lease owner is required")
	}
	if ttl <= 0 {
		return false, errors.New("refresh lease ttl must be positive")
	}
	now = now.UTC()
	var claimed bool
	err := us.statement(ctx, func(db *gorm.DB) error {
		res := db.Model(&models.User{}).
			Where("id = ? AND COALESCE(refresh_grant_version, 0) = ? AND refresh_grant IS NOT NULL", userID, version).
			Where("(refresh_lease_until IS NULL OR refresh_lease_until < ?)", now).
			Updates(map[string]any{
				"refresh_lease_owner": owner,
				"refresh_lease_until": now.Add(ttl),
				// SET expressions read the row as it was before this
				// update, so this sees the previous owner.
				"refresh_uncertain": gorm.Expr("CASE WHEN refresh_lease_owner IS NOT NULL THEN ? ELSE refresh_uncertain END", true),
			})
		if res.Error != nil {
			return res.Error
		}
		claimed = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("claim refresh lease: %w", err)
	}
	return claimed, nil
}

// LoadLeasedRefreshGrant reads and opens the grant the caller's lease covers.
// Read it only after the claim succeeds: every grant change advances the
// version, so the grant read here is exactly the one the claim fenced, never
// one a concurrent refresher has since redeemed and replaced. The lease must
// also still be live at now: a holder that stalled past its lease must not
// start a redemption another claimant may already have started. The caller
// bounds the redemption itself by the lease (a context deadline before
// RefreshLeaseUntil).
//
// It returns ErrRefreshLeaseLost when the fence no longer holds or the lease
// has expired, and ErrRefreshGrantInvalid when the grant cannot be opened (a
// rotated secret, tampering, a grant sealed for another user, or one naming
// another issuer); treat that as "no grant" and discard it with
// CompleteRefreshDiscard.
func (us *UserStore) LoadLeasedRefreshGrant(ctx context.Context, userID uuid.UUID, version int64, owner string, now time.Time) (RefreshRequest, error) {
	now = now.UTC()
	var row models.User
	err := us.statement(ctx, func(db *gorm.DB) error {
		row = models.User{}
		return fenceRefresh(db, userID, version, owner).
			Where("refresh_lease_until > ?", now).
			Select("id", "issuer", "subject", "refresh_grant").
			Take(&row).Error
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return RefreshRequest{}, ErrRefreshLeaseLost
	}
	if err != nil {
		return RefreshRequest{}, fmt.Errorf("load refresh grant: %w", err)
	}
	if len(row.RefreshGrant) == 0 {
		return RefreshRequest{}, ErrRefreshGrantInvalid
	}
	grant, err := us.grants.open(row.ID, row.RefreshGrant)
	if err != nil {
		return RefreshRequest{}, err
	}
	if grant.Issuer != row.Issuer {
		return RefreshRequest{}, ErrRefreshGrantInvalid
	}
	return RefreshRequest{UserID: row.ID, Issuer: row.Issuer, Subject: row.Subject, Grant: grant}, nil
}

// fenceRefresh restricts an update or read to the row while the caller holds
// the lease at that grant version.
func fenceRefresh(db *gorm.DB, userID uuid.UUID, version int64, owner string) *gorm.DB {
	return db.Model(&models.User{}).
		Where("id = ? AND COALESCE(refresh_grant_version, 0) = ? AND refresh_lease_owner = ?", userID, version, owner)
}

// releaseLease is part of every outcome: completion always clears the owner.
func releaseLease(updates map[string]any) map[string]any {
	updates["refresh_lease_owner"] = nil
	updates["refresh_lease_until"] = nil
	return updates
}

// sealFor seals a refresh-produced grant for userID.
func (us *UserStore) sealFor(userID uuid.UUID, grant RefreshGrant) ([]byte, error) {
	if us.grants == nil {
		return nil, errors.New("refresh grants are not configured")
	}
	return us.grants.seal(userID, grant)
}

// fencedUpdate applies updates under the lease fence and reports whether it
// applied (false: a login or another holder moved the row on).
func (us *UserStore) fencedUpdate(ctx context.Context, op string, userID uuid.UUID, version int64, owner string, updates map[string]any) (bool, error) {
	var applied bool
	err := us.statement(ctx, func(db *gorm.DB) error {
		res := fenceRefresh(db, userID, version, owner).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		applied = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("%s: %w", op, err)
	}
	return applied, nil
}

// CompleteRefreshSuccess records a redemption that returned the user's
// groups: it writes the groups and GroupsRefreshedAt, stores rotated when
// the IdP rotated the handle (nil keeps the stored grant), advances the grant
// version, clears the uncertainty (the grant demonstrably works) and releases
// the lease. It applies only while the caller holds the lease at version.
// groups is the known set; nil is the empty set and is stored as [], never
// null.
func (us *UserStore) CompleteRefreshSuccess(ctx context.Context, userID uuid.UUID, version int64, owner string, groups []string, rotated *RefreshGrant, now time.Time) (bool, error) {
	if groups == nil {
		groups = []string{}
	}
	groupsJSON, err := json.Marshal(groups)
	if err != nil {
		return false, fmt.Errorf("marshal groups: %w", err)
	}
	updates := map[string]any{
		"groups":              groupsJSON,
		"groups_refreshed_at": now.UTC(),
	}
	if err := us.applyRedeemed(userID, rotated, updates); err != nil {
		return false, err
	}
	return us.fencedUpdate(ctx, "record refresh success", userID, version, owner, updates)
}

// CompleteRefreshGroupsUnknown records a redemption the IdP accepted without
// returning verifiable groups (an OIDC refresh with no ID token and no usable
// userinfo answer). It stores rotated (nil keeps the stored grant), advances
// the version, clears the uncertainty and releases the lease, but keeps the
// stored groups and GroupsRefreshedAt, so the staleness bound still applies.
// It revokes nothing.
func (us *UserStore) CompleteRefreshGroupsUnknown(ctx context.Context, userID uuid.UUID, version int64, owner string, rotated *RefreshGrant) (bool, error) {
	updates := map[string]any{}
	if err := us.applyRedeemed(userID, rotated, updates); err != nil {
		return false, err
	}
	return us.fencedUpdate(ctx, "record refresh rotation", userID, version, owner, updates)
}

func (us *UserStore) applyRedeemed(userID uuid.UUID, rotated *RefreshGrant, updates map[string]any) error {
	if rotated != nil {
		sealed, err := us.sealFor(userID, *rotated)
		if err != nil {
			return fmt.Errorf("seal rotated refresh grant: %w", err)
		}
		updates["refresh_grant"] = sealed
	}
	updates["refresh_grant_version"] = nextGrantVersion()
	updates["refresh_uncertain"] = false
	releaseLease(updates)
	return nil
}

// CompleteRefreshTransient records a failure that redeemed nothing: it
// releases the lease and keeps everything else.
func (us *UserStore) CompleteRefreshTransient(ctx context.Context, userID uuid.UUID, version int64, owner string) (bool, error) {
	return us.fencedUpdate(ctx, "record transient refresh failure", userID, version, owner, releaseLease(map[string]any{}))
}

// CompleteRefreshUncertain records a redemption that may have reached the IdP
// without a definitive answer: it marks the grant uncertain and releases the
// lease.
func (us *UserStore) CompleteRefreshUncertain(ctx context.Context, userID uuid.UUID, version int64, owner string) (bool, error) {
	return us.fencedUpdate(ctx, "record uncertain refresh", userID, version, owner,
		releaseLease(map[string]any{"refresh_uncertain": true}))
}

// CompleteRefreshDiscard clears a grant that cannot be used (it no longer
// opens) and releases the lease. It revokes nothing.
func (us *UserStore) CompleteRefreshDiscard(ctx context.Context, userID uuid.UUID, version int64, owner string) (bool, error) {
	return us.fencedUpdate(ctx, "discard refresh grant", userID, version, owner, clearGrantUpdates())
}

// clearGrantUpdates drops the grant (a grant change, so the version advances)
// and releases the lease.
func clearGrantUpdates() map[string]any {
	return releaseLease(map[string]any{
		"refresh_grant":         nil,
		"refresh_grant_version": nextGrantVersion(),
		"refresh_uncertain":     false,
	})
}

// DefinitiveOutcome says what CompleteRefreshDefinitive did.
type DefinitiveOutcome uint8

const (
	// DefinitiveDiscarded: the fence failed (a login replaced the grant or the
	// lease was taken over); nothing was applied and nothing revoked.
	DefinitiveDiscarded DefinitiveOutcome = iota
	// DefinitiveGrantCleared: the grant was uncertain, so the denial may be
	// the echo of an earlier redemption; the grant was cleared and nothing
	// revoked. The staleness bound then forces a re-login.
	DefinitiveGrantCleared
	// DefinitiveRevoked: the grant was cleared and every live session of the
	// user revoked, in one transaction. The caller audits
	// auth.sessions_revoked (reason idp_refresh_denied).
	DefinitiveRevoked
)

// DefinitiveResult reports a definitive outcome.
type DefinitiveResult struct {
	Outcome         DefinitiveOutcome
	SessionsRevoked int64
}

// CompleteRefreshDefinitive records a definitive IdP refusal under the lease
// fence. On an uncertain grant the refusal may be the echo of an earlier
// redemption, so it only clears the grant; on a certain grant it clears the
// grant and revokes all the user's sessions in the same transaction, which is
// retried as a whole on catalog contention. Nothing is applied when the fence
// fails.
func (us *UserStore) CompleteRefreshDefinitive(ctx context.Context, userID uuid.UUID, version int64, owner string, now time.Time) (DefinitiveResult, error) {
	now = now.UTC()
	var result DefinitiveResult
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		result = DefinitiveResult{}
		res := fenceRefresh(tx, userID, version, owner).
			Where("refresh_uncertain = ?", true).
			Updates(clearGrantUpdates())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 {
			result.Outcome = DefinitiveGrantCleared
			return nil
		}
		res = fenceRefresh(tx, userID, version, owner).Updates(clearGrantUpdates())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			result.Outcome = DefinitiveDiscarded
			return nil
		}
		revoked, err := revokeUserSessionsTx(tx, userID, now)
		if err != nil {
			return err
		}
		result = DefinitiveResult{Outcome: DefinitiveRevoked, SessionsRevoked: revoked}
		return nil
	})
	if err != nil {
		return DefinitiveResult{}, fmt.Errorf("record definitive refresh failure: %w", err)
	}
	return result, nil
}
