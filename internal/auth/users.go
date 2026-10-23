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
		// A login confirms the groups, so it lifts a stale_groups refusal.
		"stale_refused_at": nil,
	}
	// The write is fenced on the row as this login read it: still enabled and
	// still at the disable generation read above (kept across retries). A
	// user disabled in between is left untouched and the re-read reports it
	// disabled. A generation that moved means a disable committed since the
	// read, perhaps already undone by an enable; the login is refused with
	// ErrUserChanged rather than applied, so an assertion checked before a
	// disable never outlives it. Concurrent logins do not move the
	// generation, so they all apply (the newest grant wins).
	observed := user.DisableGeneration
	var stored models.User
	var applied bool
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		stored, applied = models.User{}, false
		res := tx.Model(&models.User{}).
			Where("id = ? AND disabled_at IS NULL AND COALESCE(disable_generation, 0) = ?", user.ID, observed).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		applied = res.RowsAffected == 1
		return tx.Where("id = ?", user.ID).Take(&stored).Error
	})
	if err != nil {
		return nil, fmt.Errorf("update user: %w", err)
	}
	if !applied && !stored.IsDisabled() {
		return nil, ErrUserChanged
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
	// DefinitiveRevoked: the grant was cleared and every live session and
	// user-bound key of the user revoked, in one transaction. The caller
	// audits auth.sessions_revoked (reason idp_refresh_denied).
	DefinitiveRevoked
)

// DefinitiveResult reports a definitive outcome.
type DefinitiveResult struct {
	Outcome         DefinitiveOutcome
	SessionsRevoked int64
	KeysRevoked     int64
}

// CompleteRefreshDefinitive records a definitive IdP refusal under the lease
// fence. A DenialOfGrant on an uncertain grant may be the echo of an earlier
// redemption, so it only clears the grant; any other definitive refusal (a
// DenialOfGrant on a certain grant, or a DenialOfSubject, which no earlier
// redemption can cause) clears the grant and revokes all the user's sessions
// and user-bound keys in the same transaction, which is retried as a whole on
// catalog contention. Nothing is applied when the fence fails.
func (us *UserStore) CompleteRefreshDefinitive(ctx context.Context, userID uuid.UUID, version int64, owner string, scope DenialScope, now time.Time) (DefinitiveResult, error) {
	now = now.UTC()
	var result DefinitiveResult
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		result = DefinitiveResult{}
		if scope == DenialOfGrant {
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
		}
		res := fenceRefresh(tx, userID, version, owner).Updates(clearGrantUpdates())
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			result.Outcome = DefinitiveDiscarded
			return nil
		}
		revoked, err := revokeUserCredentialsTx(tx, userID, now)
		if err != nil {
			return err
		}
		result = DefinitiveResult{Outcome: DefinitiveRevoked, SessionsRevoked: revoked.Sessions, KeysRevoked: revoked.Keys}
		return nil
	})
	if err != nil {
		return DefinitiveResult{}, fmt.Errorf("record definitive refresh failure: %w", err)
	}
	return result, nil
}

// MarkStaleRefused records that the user's groups are past the maximum
// staleness and could not be refreshed: from then on every credential of the
// user is refused (stale_groups) until a login clears the mark. It is fenced
// on the grant version the caller read, so a login (or a successful refresh)
// committed since wins, and on the user still being enabled and unmarked.
// It reports whether the mark was written.
func (us *UserStore) MarkStaleRefused(ctx context.Context, userID uuid.UUID, version int64, now time.Time) (bool, error) {
	var applied bool
	err := us.statement(ctx, func(db *gorm.DB) error {
		res := db.Model(&models.User{}).
			Where("id = ? AND COALESCE(refresh_grant_version, 0) = ? AND stale_refused_at IS NULL AND disabled_at IS NULL", userID, version).
			Update("stale_refused_at", now.UTC())
		if res.Error != nil {
			return res.Error
		}
		applied = res.RowsAffected == 1
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("mark stale groups refused: %w", err)
	}
	return applied, nil
}

// ErrUserNotFound reports an administration request for an unknown user.
var ErrUserNotFound = errors.New("user not found")

// UserFilter selects users for administration (identity-and-access E3).
// Issuer matches exactly; Email is a case-insensitive substring (the database LOWER on both sides);
// Disabled, when set, keeps only disabled (true) or enabled (false) users.
// Limit must be positive.
type UserFilter struct {
	Issuer   string
	Email    string
	Disabled *bool
	Limit    int
	Offset   int
}

// UserPage is one page of users, newest first, with the filter's total.
// HasMore comes from the page query's limit+1 probe.
type UserPage struct {
	Users   []models.User
	Total   int64
	HasMore bool
}

// ListUsers returns one page of users matching f, ordered by creation time
// (newest first) with the id as the tie-breaker.
func (us *UserStore) ListUsers(ctx context.Context, f UserFilter) (UserPage, error) {
	if f.Limit <= 0 {
		return UserPage{}, errors.New("list users: limit must be positive")
	}
	if f.Offset < 0 {
		return UserPage{}, errors.New("list users: offset must not be negative")
	}
	scope := func() *gorm.DB {
		q := us.db.WithContext(ctx).Model(&models.User{})
		if f.Issuer != "" {
			q = q.Where("issuer = ?", f.Issuer)
		}
		if f.Email != "" {
			// The database folds both operands with the same LOWER, so the
			// match is consistent on every backend (ASCII-only on SQLite and
			// dqlite, the database locale on PostgreSQL).
			q = q.Where("LOWER(email) LIKE LOWER(?) ESCAPE '!'", "%"+escapeLikePattern(f.Email)+"%")
		}
		if f.Disabled != nil {
			if *f.Disabled {
				q = q.Where("disabled_at IS NOT NULL")
			} else {
				q = q.Where("disabled_at IS NULL")
			}
		}
		return q
	}
	var page UserPage
	if err := scope().Count(&page.Total).Error; err != nil {
		return UserPage{}, fmt.Errorf("count users: %w", err)
	}
	var rows []models.User
	if err := scope().Order("created_at DESC").Order("id DESC").
		Limit(f.Limit + 1).Offset(f.Offset).Find(&rows).Error; err != nil {
		return UserPage{}, fmt.Errorf("list users: %w", err)
	}
	if len(rows) > f.Limit {
		rows = rows[:f.Limit]
		page.HasMore = true
	}
	page.Users = rows
	return page, nil
}

// escapeLikePattern escapes LIKE metacharacters for ESCAPE '!', so a filter
// value matches literally on SQLite, dqlite and PostgreSQL alike.
func escapeLikePattern(s string) string {
	return strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(s)
}

// GetUser loads one user by id.
func (us *UserStore) GetUser(ctx context.Context, id uuid.UUID) (*models.User, error) {
	var user models.User
	err := us.db.WithContext(ctx).Where("id = ?", id).Take(&user).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user: %w", err)
	}
	return &user, nil
}

// UserChange reports a disable or enable: the user as stored afterwards,
// whether this call changed its state, and how many sessions and user-bound
// keys the same transaction revoked.
type UserChange struct {
	User            models.User
	Changed         bool
	SessionsRevoked int64
	KeysRevoked     int64
}

// RevokedCredentials counts what one revocation of a user's credentials
// revoked: its sessions and its user-bound API keys (E4).
type RevokedCredentials struct {
	Sessions int64
	Keys     int64
}

// revokeUserCredentialsTx revokes every unrevoked session and user-bound key
// of a user on a caller-supplied handle, so the user lifecycle (disable, the
// enable sweep, log out everywhere, a definitive group-refresh denial) kills
// both credential types in one transaction. It first voids the user's
// confirmed but unredeemed `caesium login` codes (E5), whose exchange would
// otherwise mint a fresh key afterwards; doing that before revoking the keys
// means a concurrent exchange either commits first (and its key is then
// revoked here) or finds its code void, and a confirmation either commits
// first (and is voided here) or finds its session revoked.
func revokeUserCredentialsTx(tx *gorm.DB, userID uuid.UUID, now time.Time) (RevokedCredentials, error) {
	// The user row lock serialises this with a CLI login's confirmation and
	// exchange, which take it first too (cli_login.go).
	if err := lockUserRowTx(tx, userID); err != nil {
		return RevokedCredentials{}, err
	}
	if err := voidUserCLILoginsTx(tx, userID, now); err != nil {
		return RevokedCredentials{}, err
	}
	sessions, err := revokeUserSessionsTx(tx, userID, now)
	if err != nil {
		return RevokedCredentials{}, err
	}
	keys, err := revokeUserKeysTx(tx, userID, now)
	if err != nil {
		return RevokedCredentials{}, err
	}
	return RevokedCredentials{Sessions: sessions, Keys: keys}, nil
}

// DisableUser disables a user and, in the same transaction (retried as a
// whole on catalog contention), revokes every unrevoked session and
// user-bound key of the user, clears the refresh grant and advances the
// disable generation. The generation is the login fence: a login that read
// the user before this commit can no longer apply its profile update
// (UserStore.updateExisting), insert a session (SessionStore.Create) or mint
// a user-bound key (Service.MintUserBoundKey), even after an enable. Clearing
// the grant advances refresh_grant_version, so an in-flight group refresh can
// no longer record its outcome either. Disabling a disabled user changes
// nothing (Changed=false).
func (us *UserStore) DisableUser(ctx context.Context, id uuid.UUID) (UserChange, error) {
	now := us.now().UTC()
	var change UserChange
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		change = UserChange{}
		updates := clearGrantUpdates()
		updates["disabled_at"] = now
		updates["disable_generation"] = gorm.Expr("COALESCE(disable_generation, 0) + 1")
		res := tx.Model(&models.User{}).Where("id = ? AND disabled_at IS NULL", id).Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 {
			revoked, err := revokeUserCredentialsTx(tx, id, now)
			if err != nil {
				return err
			}
			change.Changed, change.SessionsRevoked, change.KeysRevoked = true, revoked.Sessions, revoked.Keys
		}
		return takeUserTx(tx, id, &change.User)
	})
	if err != nil {
		return UserChange{}, wrapUserAdminError("disable user", err)
	}
	return change, nil
}

// EnableUser clears a user's disabled_at. It never un-revokes a session or a
// key. On the transition it also revokes any unrevoked session or user-bound
// key of the user in the same transaction: none should exist (the disable
// revoked them all and the mint fences refuse disabled users), so this is a
// second line of defence should one ever have been inserted while the user
// was disabled. Enabling an enabled user changes nothing (Changed=false).
func (us *UserStore) EnableUser(ctx context.Context, id uuid.UUID) (UserChange, error) {
	now := us.now().UTC()
	var change UserChange
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		change = UserChange{}
		res := tx.Model(&models.User{}).Where("id = ? AND disabled_at IS NOT NULL", id).
			Updates(map[string]any{"disabled_at": nil})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 1 {
			swept, err := revokeUserCredentialsTx(tx, id, now)
			if err != nil {
				return err
			}
			change.Changed, change.SessionsRevoked, change.KeysRevoked = true, swept.Sessions, swept.Keys
		}
		return takeUserTx(tx, id, &change.User)
	})
	if err != nil {
		return UserChange{}, wrapUserAdminError("enable user", err)
	}
	return change, nil
}

// RevokeUserSessions revokes every unrevoked session and user-bound key of an
// existing user ("log out everywhere") and returns how many of each it
// revoked. A user-bound key is a login credential like a session, so logging
// a user out everywhere must not leave its CLI keys alive.
func (us *UserStore) RevokeUserSessions(ctx context.Context, id uuid.UUID) (RevokedCredentials, error) {
	now := us.now().UTC()
	var revoked RevokedCredentials
	err := us.transaction(ctx, func(tx *gorm.DB) error {
		revoked = RevokedCredentials{}
		var user models.User
		if err := takeUserTx(tx, id, &user); err != nil {
			return err
		}
		n, err := revokeUserCredentialsTx(tx, id, now)
		revoked = n
		return err
	})
	if err != nil {
		return RevokedCredentials{}, wrapUserAdminError("revoke user sessions", err)
	}
	return revoked, nil
}

func takeUserTx(tx *gorm.DB, id uuid.UUID, out *models.User) error {
	err := tx.Where("id = ?", id).Take(out).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrUserNotFound
	}
	return err
}

func wrapUserAdminError(op string, err error) error {
	if errors.Is(err, ErrUserNotFound) {
		return ErrUserNotFound
	}
	return fmt.Errorf("%s: %w", op, err)
}

// UserAdmin is the cluster-admin surface over users and their sessions
// (identity-and-access E3, spec §11.2). It is where the user lifecycle
// cascades meet: user-bound API keys (E4) are listed in the detail and
// revoked with the sessions (revokeUserCredentialsTx).
type UserAdmin struct {
	users    *UserStore
	sessions *SessionStore
}

// NewUserAdmin builds the administration surface over the session store's
// catalog connection and clock. It returns nil when sessions is nil.
func NewUserAdmin(sessions *SessionStore) *UserAdmin {
	if sessions == nil {
		return nil
	}
	return &UserAdmin{
		users:    NewUserStore(sessions.db, WithUserStoreNow(sessions.now)),
		sessions: sessions,
	}
}

// DetailSessionLimit caps the sessions embedded in a user detail; the rest
// are paged through ListSessions.
const DetailSessionLimit = 100

// UserDetail is one user with the newest of its active sessions and active
// user-bound keys (each capped at DetailSessionLimit, with their totals).
type UserDetail struct {
	User          models.User
	Sessions      []models.Session
	SessionsTotal int64
	UserKeys      []models.APIKey
	UserKeysTotal int64
}

// ListUsers pages users matching f.
func (a *UserAdmin) ListUsers(ctx context.Context, f UserFilter) (UserPage, error) {
	return a.users.ListUsers(ctx, f)
}

// GetUser returns a user, at most DetailSessionLimit of its newest active
// sessions and at most DetailSessionLimit of its newest active user-bound
// keys, with their totals.
func (a *UserAdmin) GetUser(ctx context.Context, id uuid.UUID) (UserDetail, error) {
	user, err := a.users.GetUser(ctx, id)
	if err != nil {
		return UserDetail{}, err
	}
	page, err := a.sessions.ListActive(ctx, SessionFilter{UserID: &id, Limit: DetailSessionLimit})
	if err != nil {
		return UserDetail{}, err
	}
	keys, keysTotal, err := activeUserKeys(ctx, a.users.db, id, a.users.now().UTC(), DetailSessionLimit)
	if err != nil {
		return UserDetail{}, err
	}
	return UserDetail{
		User:          *user,
		Sessions:      page.Sessions,
		SessionsTotal: page.Total,
		UserKeys:      keys,
		UserKeysTotal: keysTotal,
	}, nil
}

// DisableUser disables a user and revokes its sessions and user-bound keys
// (UserStore.DisableUser).
func (a *UserAdmin) DisableUser(ctx context.Context, id uuid.UUID) (UserChange, error) {
	return a.users.DisableUser(ctx, id)
}

// EnableUser re-enables a user (UserStore.EnableUser).
func (a *UserAdmin) EnableUser(ctx context.Context, id uuid.UUID) (UserChange, error) {
	return a.users.EnableUser(ctx, id)
}

// RevokeUserSessions logs a user out everywhere, sessions and user-bound keys
// alike (UserStore.RevokeUserSessions).
func (a *UserAdmin) RevokeUserSessions(ctx context.Context, id uuid.UUID) (RevokedCredentials, error) {
	return a.users.RevokeUserSessions(ctx, id)
}

// ListSessions pages active sessions (SessionStore.ListActive).
func (a *UserAdmin) ListSessions(ctx context.Context, f SessionFilter) (SessionPage, error) {
	return a.sessions.ListActive(ctx, f)
}
