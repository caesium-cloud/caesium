package auth

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

const testIssuer = "https://idp.example.com"

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

type refreshFixture struct {
	db       *gorm.DB
	users    *UserStore
	sessions *SessionStore
	clock    *fakeClock
}

// newRefreshFixture runs on in-memory SQLite, or, when
// CAESIUM_TEST_POSTGRES_DSN is set (the repo's opt-in convention), on a fresh
// PostgreSQL schema, so every lease and fence scenario can be checked against
// both dialects' CAS and increment semantics.
func newRefreshFixture(t *testing.T) *refreshFixture {
	t.Helper()
	var db *gorm.DB
	if dsn := strings.TrimSpace(os.Getenv("CAESIUM_TEST_POSTGRES_DSN")); dsn != "" {
		db = openRefreshPostgres(t, dsn)
	} else {
		db = testutil.OpenTestDB(t)
		t.Cleanup(func() { testutil.CloseDB(db) })
	}
	clock := newFakeClock()
	users := NewUserStore(db, WithRefreshGrantSecret(katSecret), WithUserStoreNow(clock.Now))
	users.retry.Wait = nil
	return &refreshFixture{
		db:       db,
		users:    users,
		sessions: NewSessionStore(db, WithSessionNow(clock.Now)),
		clock:    clock,
	}
}

func openRefreshPostgres(t *testing.T, dsn string) *gorm.DB {
	t.Helper()
	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	adminSQL, err := admin.DB()
	require.NoError(t, err)
	schema := "caesium_refresh_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	require.NoError(t, admin.Exec("CREATE SCHEMA "+schema).Error)

	scoped := strings.TrimSpace(dsn) + " search_path=" + schema
	if strings.Contains(dsn, "://") {
		parsed, err := url.Parse(dsn)
		require.NoError(t, err)
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		scoped = parsed.String()
	}
	db, err := gorm.Open(postgres.Open(scoped), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(8)
	t.Cleanup(func() {
		require.NoError(t, sqlDB.Close())
		require.NoError(t, admin.Exec("DROP SCHEMA "+schema+" CASCADE").Error)
		require.NoError(t, adminSQL.Close())
	})
	var current string
	require.NoError(t, db.Raw("SELECT current_schema()").Scan(&current).Error)
	require.Equal(t, schema, current)
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}, &models.AuditLog{}))
	return db
}

func oidcIdentity(subject, handle string, groups ...string) *ExternalIdentity {
	ext := &ExternalIdentity{Issuer: testIssuer, Subject: subject, Email: subject + "@example.com", Groups: groups}
	if handle != "" {
		ext.Refresh = &RefreshGrant{Provider: "oidc", Issuer: testIssuer, Handle: NewRefreshHandle(handle)}
	}
	return ext
}

func (f *refreshFixture) login(t *testing.T, ext *ExternalIdentity) *models.User {
	t.Helper()
	user, err := f.users.Upsert(t.Context(), ext, models.RoleViewer)
	require.NoError(t, err)
	return user
}

func (f *refreshFixture) row(t *testing.T, id uuid.UUID) models.User {
	t.Helper()
	var user models.User
	require.NoError(t, f.db.Where("id = ?", id).Take(&user).Error)
	return user
}

func (f *refreshFixture) claim(t *testing.T, id uuid.UUID, version int64, owner string) bool {
	t.Helper()
	ok, err := f.users.ClaimRefreshLease(t.Context(), id, version, owner, f.clock.Now(), 30*time.Second)
	require.NoError(t, err)
	return ok
}

// handleAt claims a lease at the row's current version and opens the grant.
func (f *refreshFixture) handleAt(t *testing.T, id uuid.UUID) string {
	t.Helper()
	row := f.row(t, id)
	owner := NewRefreshLeaseOwner("probe")
	require.True(t, f.claim(t, id, row.RefreshGrantVersion, owner), "probe claim")
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), id, row.RefreshGrantVersion, owner, f.clock.Now())
	require.NoError(t, err)
	applied, err := f.users.CompleteRefreshTransient(t.Context(), id, row.RefreshGrantVersion, owner)
	require.NoError(t, err)
	require.True(t, applied)
	return req.Grant.Handle.Reveal()
}

func (f *refreshFixture) newSession(t *testing.T, userID uuid.UUID) uuid.UUID {
	t.Helper()
	_, sess, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: userID, AuthMethod: "oidc"})
	require.NoError(t, err)
	return sess.ID
}

func (f *refreshFixture) liveSessions(t *testing.T, userID uuid.UUID) int64 {
	t.Helper()
	var n int64
	require.NoError(t, f.db.Model(&models.Session{}).Where("user_id = ? AND revoked_at IS NULL", userID).Count(&n).Error)
	return n
}

func requireNoLease(t *testing.T, row models.User) {
	t.Helper()
	require.Nil(t, row.RefreshLeaseOwner)
	require.Nil(t, row.RefreshLeaseUntil)
}

func TestLoginStoresSealedGrant(t *testing.T) {
	f := newRefreshFixture(t)
	const token = "login-refresh-token-0001"
	user := f.login(t, oidcIdentity("ada", token, "eng"))

	row := f.row(t, user.ID)
	require.NotEmpty(t, row.RefreshGrant)
	require.False(t, bytes.Contains(row.RefreshGrant, []byte(token)), "the stored column must not contain the token")
	var raw []byte
	require.NoError(t, f.db.Raw("SELECT refresh_grant FROM users WHERE id = ?", user.ID).Row().Scan(&raw))
	require.False(t, bytes.Contains(raw, []byte(token)))
	require.Equal(t, int64(1), row.RefreshGrantVersion)
	require.Equal(t, int64(1), user.RefreshGrantVersion)
	require.NotNil(t, row.GroupsRefreshedAt)
	require.True(t, row.GroupsRefreshedAt.Equal(f.clock.Now()))
	requireNoLease(t, row)
	require.False(t, row.RefreshUncertain)

	owner := NewRefreshLeaseOwner("node-a")
	require.True(t, f.claim(t, user.ID, 1, owner))
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, owner, f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, user.ID, req.UserID)
	require.Equal(t, testIssuer, req.Issuer)
	require.Equal(t, "ada", req.Subject)
	require.Equal(t, "oidc", req.Grant.Provider)
	require.Equal(t, testIssuer, req.Grant.Issuer)
	require.Equal(t, token, req.Grant.Handle.Reveal())
}

func TestReloginReplacesGrantClearsLeaseAndBumpsVersion(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	owner := NewRefreshLeaseOwner("node-a")
	require.True(t, f.claim(t, user.ID, 1, owner))
	applied, err := f.users.CompleteRefreshUncertain(t.Context(), user.ID, 1, owner)
	require.NoError(t, err)
	require.True(t, applied)
	require.True(t, f.claim(t, user.ID, 1, owner+"-again"))

	later := f.clock.Advance(time.Minute)
	relogged := f.login(t, oidcIdentity("ada", "token-2", "ops"))
	require.Equal(t, user.ID, relogged.ID)
	require.Equal(t, int64(2), relogged.RefreshGrantVersion)

	row := f.row(t, user.ID)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
	require.False(t, row.RefreshUncertain, "a new grant starts certain")
	require.True(t, row.GroupsRefreshedAt.Equal(later))
	require.JSONEq(t, `["ops"]`, string(row.Groups))
	require.Equal(t, "token-2", f.handleAt(t, user.ID))
}

func TestLoginWithoutGrantStoresNullAndBumpsVersion(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	// A SAML login, or an IdP that issued no refresh token, replaces the
	// grant with none: in-flight outcomes for the old grant are discarded.
	owner := NewRefreshLeaseOwner("node-a")
	require.True(t, f.claim(t, user.ID, 1, owner))
	f.login(t, oidcIdentity("ada", ""))

	row := f.row(t, user.ID)
	require.Nil(t, row.RefreshGrant)
	var nulls int64
	require.NoError(t, f.db.Raw("SELECT COUNT(*) FROM users WHERE id = ? AND refresh_grant IS NULL", user.ID).Scan(&nulls).Error)
	require.Equal(t, int64(1), nulls, "no grant is stored as NULL")
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
	require.False(t, f.claim(t, user.ID, 2, NewRefreshLeaseOwner("node-b")), "no grant, no lease")
	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, owner, []string{"x"}, nil, f.clock.Now())
	require.NoError(t, err)
	require.False(t, applied)
}

func TestLoginStoresNoGrantForMismatchedIssuerOrMissingSecret(t *testing.T) {
	f := newRefreshFixture(t)
	ext := oidcIdentity("ada", "token-1")
	ext.Refresh.Issuer = "https://other-idp.example.com"
	user := f.login(t, ext)
	require.Nil(t, f.row(t, user.ID).RefreshGrant)

	noSecret := NewUserStore(f.db, WithUserStoreNow(f.clock.Now))
	created, err := noSecret.Upsert(t.Context(), oidcIdentity("bob", "token-2"), models.RoleViewer)
	require.NoError(t, err)
	require.Nil(t, f.row(t, created.ID).RefreshGrant)
	require.Equal(t, int64(1), f.row(t, created.ID).RefreshGrantVersion)

	emptySecret := NewUserStore(f.db, WithRefreshGrantSecret(""))
	require.Nil(t, emptySecret.grants)
}

func TestLegacyUserWithNullVersionLogsInAndClaims(t *testing.T) {
	f := newRefreshFixture(t)
	id := uuid.New()
	require.NoError(t, f.db.Exec(
		"INSERT INTO users (id, issuer, subject, email, role, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, testIssuer, "legacy", "legacy@example.com", string(models.RoleViewer), f.clock.Now(),
	).Error)
	var nulls int64
	require.NoError(t, f.db.Raw("SELECT COUNT(*) FROM users WHERE id = ? AND refresh_grant_version IS NULL", id).Scan(&nulls).Error)
	require.Equal(t, int64(1), nulls)
	require.Zero(t, f.row(t, id).RefreshGrantVersion)
	require.Nil(t, f.row(t, id).GroupsRefreshedAt)
	require.False(t, f.claim(t, id, 0, NewRefreshLeaseOwner("node-a")), "a legacy row has no grant to lease")

	user := f.login(t, oidcIdentity("legacy", "token-1"))
	require.Equal(t, id, user.ID)
	require.Equal(t, int64(1), f.row(t, id).RefreshGrantVersion, "COALESCE(NULL, 0) + 1")
	require.True(t, f.claim(t, id, 1, NewRefreshLeaseOwner("node-a")))
}

func TestConcurrentLoginsGetDistinctVersionsAndTheLastGrantWins(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-0"))

	const logins = 8
	type result struct {
		version int64
		handle  string
	}
	results := make([]result, logins)
	var wg sync.WaitGroup
	for i := range logins {
		wg.Go(func() {
			handle := fmt.Sprintf("token-%d", i+1)
			got, err := f.users.Upsert(context.Background(), oidcIdentity("ada", handle), models.RoleViewer)
			if err != nil {
				t.Errorf("login %d: %v", i, err)
				return
			}
			results[i] = result{version: got.RefreshGrantVersion, handle: handle}
		})
	}
	wg.Wait()

	seen := map[int64]string{}
	var last result
	for _, r := range results {
		require.NotContains(t, seen, r.version, "two logins got the same grant version")
		seen[r.version] = r.handle
		if r.version > last.version {
			last = r
		}
	}
	for v := int64(2); v <= logins+1; v++ {
		require.Contains(t, seen, v)
	}
	row := f.row(t, user.ID)
	require.Equal(t, int64(logins+1), row.RefreshGrantVersion)
	require.Equal(t, last.version, row.RefreshGrantVersion)
	require.Equal(t, last.handle, f.handleAt(t, user.ID), "the stored grant is the one written with the final version")
}

func TestClaimRefreshLeaseHasExactlyOneWinner(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))

	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	require.False(t, f.claim(t, user.ID, 1, "node-b/1"))
	require.Equal(t, "node-a/1", *f.row(t, user.ID).RefreshLeaseOwner)

	other := f.login(t, oidcIdentity("bob", "token-2"))
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			ok, err := f.users.ClaimRefreshLease(context.Background(), other.ID, 1, fmt.Sprintf("node-%d/x", i), f.clock.Now(), time.Minute)
			if err != nil {
				t.Errorf("claim: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		})
	}
	wg.Wait()
	require.Equal(t, int32(1), wins.Load())
}

func TestClaimRefreshLeaseRequiresGrantVersionAndValidArguments(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	require.False(t, f.claim(t, user.ID, 2, "node-a/1"), "wrong version")
	require.False(t, f.claim(t, uuid.New(), 1, "node-a/1"), "unknown user")
	_, err := f.users.ClaimRefreshLease(t.Context(), user.ID, 1, " ", f.clock.Now(), time.Minute)
	require.Error(t, err)
	_, err = f.users.ClaimRefreshLease(t.Context(), user.ID, 1, "node-a/1", f.clock.Now(), 0)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = f.users.ClaimRefreshLease(ctx, user.ID, 1, "node-a/1", f.clock.Now(), time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	requireNoLease(t, f.row(t, user.ID))
}

func TestLeaseExpiryLetsASecondClaimantInAndTakeoverIsUncertain(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))

	f.clock.Advance(29 * time.Second)
	require.False(t, f.claim(t, user.ID, 1, "node-b/1"), "the lease has not expired")
	require.False(t, f.row(t, user.ID).RefreshUncertain)

	f.clock.Advance(2 * time.Second)
	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	row := f.row(t, user.ID)
	require.Equal(t, "node-b/1", *row.RefreshLeaseOwner)
	require.True(t, row.RefreshUncertain, "an abandoned lease's holder may have redeemed the grant")

	// The previous holder is fenced out of every outcome.
	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-a/1", []string{"x"}, nil, f.clock.Now())
	require.NoError(t, err)
	require.False(t, applied)
	_, err = f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshLeaseLost)
}

// TestAStalledHolderCannotLoadAfterItsLeaseExpires: a holder late enough that
// another claimant may already be redeeming must not start its own
// redemption, even before anyone takes the lease over.
func TestAStalledHolderCannotLoadAfterItsLeaseExpires(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	_, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now().Add(29*time.Second))
	require.NoError(t, err)
	_, err = f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now().Add(30*time.Second))
	require.ErrorIs(t, err, ErrRefreshLeaseLost)
	// It can still record that it redeemed nothing.
	applied, err := f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)
}

func TestClaimOverAReleasedLeaseKeepsUncertainty(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))

	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	applied, err := f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)
	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	require.False(t, f.row(t, user.ID).RefreshUncertain, "a released lease is not an abandoned one")

	applied, err = f.users.CompleteRefreshUncertain(t.Context(), user.ID, 1, "node-b/1")
	require.NoError(t, err)
	require.True(t, applied)
	require.True(t, f.claim(t, user.ID, 1, "node-c/1"))
	require.True(t, f.row(t, user.ID).RefreshUncertain, "a claim over a released lease keeps the uncertainty")
}

// TestRedeemerReadsTheGrantItClaimed is R1: a worker that read the user row
// (and so the grant) at version V, paused, and claims after another worker
// rotated the grant fails its claim instead of redeeming a dead token.
func TestRedeemerReadsTheGrantItClaimed(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-T"))
	staleRow := f.row(t, user.ID) // worker A reads version 1 and pauses

	// Worker B claims, redeems T, and the IdP rotates it to T2.
	require.True(t, f.claim(t, user.ID, staleRow.RefreshGrantVersion, "node-b/1"))
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-b/1", f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, "token-T", req.Grant.Handle.Reveal())
	rotated := req.Grant
	rotated.Handle = NewRefreshHandle("token-T2")
	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-b/1", []string{"eng"}, &rotated, f.clock.Now())
	require.NoError(t, err)
	require.True(t, applied)

	// Worker A resumes with its stale version: the claim fails, so it never
	// redeems T (which the IdP would answer with invalid_grant).
	require.False(t, f.claim(t, user.ID, staleRow.RefreshGrantVersion, "node-a/1"))
	_, err = f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, staleRow.RefreshGrantVersion, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshLeaseLost)
	require.Equal(t, "token-T2", f.handleAt(t, user.ID))
}

func TestLoginDuringRefreshDiscardsEveryOutcome(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	sessionID := f.newSession(t, user.ID)
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))

	f.clock.Advance(time.Second)
	f.login(t, oidcIdentity("ada", "token-2", "ops"))
	afterLogin := f.row(t, user.ID)

	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-a/1", []string{"stale"}, &RefreshGrant{
		Provider: "oidc", Issuer: testIssuer, Handle: NewRefreshHandle("token-1-rotated"),
	}, f.clock.Now())
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = f.users.CompleteRefreshGroupsUnknown(t.Context(), user.ID, 1, "node-a/1", nil)
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = f.users.CompleteRefreshUncertain(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.False(t, applied)
	applied, err = f.users.CompleteRefreshDiscard(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.False(t, applied)
	result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, DefinitiveDiscarded, result.Outcome)

	row := f.row(t, user.ID)
	require.Equal(t, afterLogin.RefreshGrant, row.RefreshGrant)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	require.False(t, row.RefreshUncertain)
	require.JSONEq(t, `["ops"]`, string(row.Groups))
	require.Equal(t, int64(1), f.liveSessions(t, user.ID))
	_, _, err = f.sessions.Validate(t.Context(), "unused") // sanity: store still usable
	require.ErrorIs(t, err, ErrSessionInvalid)
	var sess models.Session
	require.NoError(t, f.db.Where("id = ?", sessionID).Take(&sess).Error)
	require.Nil(t, sess.RevokedAt)
	require.Equal(t, "token-2", f.handleAt(t, user.ID))
}

func TestUncertainThenInvalidGrantClearsGrantWithoutRevoking(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	f.newSession(t, user.ID)
	f.newSession(t, user.ID)

	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	applied, err := f.users.CompleteRefreshUncertain(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)
	row := f.row(t, user.ID)
	require.True(t, row.RefreshUncertain)
	requireNoLease(t, row)
	require.Equal(t, int64(1), row.RefreshGrantVersion)

	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-b/1", f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, DefinitiveResult{Outcome: DefinitiveGrantCleared}, result)

	row = f.row(t, user.ID)
	require.Nil(t, row.RefreshGrant)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	require.False(t, row.RefreshUncertain)
	requireNoLease(t, row)
	require.Equal(t, int64(2), f.liveSessions(t, user.ID), "an invalid_grant on an uncertain grant revokes nothing")
}

func TestDefinitiveOnCertainGrantRevokesOnlyThatUsersSessions(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	other := f.login(t, oidcIdentity("bob", "token-2"))
	f.newSession(t, user.ID)
	f.newSession(t, user.ID)
	revokedEarlier := f.newSession(t, user.ID)
	require.NoError(t, f.sessions.Revoke(t.Context(), revokedEarlier))
	var earlier models.Session
	require.NoError(t, f.db.Where("id = ?", revokedEarlier).Take(&earlier).Error)
	otherSession := f.newSession(t, other.ID)

	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	denialAt := f.clock.Advance(time.Minute)
	result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-a/1", denialAt)
	require.NoError(t, err)
	require.Equal(t, DefinitiveResult{Outcome: DefinitiveRevoked, SessionsRevoked: 2}, result)

	row := f.row(t, user.ID)
	require.Nil(t, row.RefreshGrant)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
	require.Zero(t, f.liveSessions(t, user.ID))
	var sessions []models.Session
	require.NoError(t, f.db.Where("user_id = ?", user.ID).Find(&sessions).Error)
	for _, s := range sessions {
		require.NotNil(t, s.RevokedAt)
		if s.ID == revokedEarlier {
			require.True(t, s.RevokedAt.Equal(*earlier.RevokedAt), "an already-revoked session keeps its timestamp")
		} else {
			require.True(t, s.RevokedAt.Equal(denialAt))
		}
	}
	var otherSess models.Session
	require.NoError(t, f.db.Where("id = ?", otherSession).Take(&otherSess).Error)
	require.Nil(t, otherSess.RevokedAt, "another user's session is untouched")
	require.NotNil(t, f.row(t, other.ID).RefreshGrant)
}

// TestCrashAfterRedemptionNeverRevokes is R2: a holder that redeemed (and
// perhaps rotated) the grant and died without recording anything leaves an
// expired lease with its owner set; the next claimant's invalid_grant then
// clears the grant and revokes nothing.
func TestCrashAfterRedemptionNeverRevokes(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-T"))
	f.newSession(t, user.ID)

	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	_, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.NoError(t, err)
	// node-a redeems token-T at the IdP (rotating it) and crashes here.

	f.clock.Advance(31 * time.Second)
	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-b/1", f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, "token-T", req.Grant.Handle.Reveal())
	// The IdP answers node-b's redemption of the rotated-away token with
	// invalid_grant.
	result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-b/1", f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, DefinitiveGrantCleared, result.Outcome)
	require.Equal(t, int64(1), f.liveSessions(t, user.ID))
	require.Nil(t, f.row(t, user.ID).RefreshGrant)
}

func TestSuccessStoresGroupsAndTheRotatedHandle(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.NoError(t, err)

	refreshedAt := f.clock.Advance(10 * time.Minute)
	rotated := req.Grant
	rotated.Handle = NewRefreshHandle("token-2")
	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-a/1", []string{"eng", "ops"}, &rotated, refreshedAt)
	require.NoError(t, err)
	require.True(t, applied)

	row := f.row(t, user.ID)
	require.JSONEq(t, `["eng","ops"]`, string(row.Groups))
	require.True(t, row.GroupsRefreshedAt.Equal(refreshedAt))
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
	require.False(t, row.RefreshUncertain)
	require.Equal(t, "token-2", f.handleAt(t, user.ID))

	applied, err = f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-a/1", []string{"again"}, nil, refreshedAt)
	require.NoError(t, err)
	require.False(t, applied, "an outcome is recorded once")
}

func TestSuccessWithoutRotationKeepsTheHandle(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	before := f.row(t, user.ID)
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	// An uncertain grant that redeems fine is certain again.
	f.clock.Advance(time.Minute)
	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	require.True(t, f.row(t, user.ID).RefreshUncertain)

	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, 1, "node-b/1", []string{"ops"}, nil, f.clock.Now())
	require.NoError(t, err)
	require.True(t, applied)
	row := f.row(t, user.ID)
	require.Equal(t, before.RefreshGrant, row.RefreshGrant)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	require.False(t, row.RefreshUncertain)
	require.Equal(t, "token-1", f.handleAt(t, user.ID))
}

func TestGroupsUnknownPersistsOnlyTheRotation(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	f.newSession(t, user.ID)
	before := f.row(t, user.ID)
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	req, err := f.users.LoadLeasedRefreshGrant(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.NoError(t, err)

	f.clock.Advance(10 * time.Minute)
	rotated := req.Grant
	rotated.Handle = NewRefreshHandle("token-2")
	applied, err := f.users.CompleteRefreshGroupsUnknown(t.Context(), user.ID, 1, "node-a/1", &rotated)
	require.NoError(t, err)
	require.True(t, applied)

	row := f.row(t, user.ID)
	require.JSONEq(t, string(before.Groups), string(row.Groups))
	require.True(t, row.GroupsRefreshedAt.Equal(*before.GroupsRefreshedAt), "unknown groups are not fresh groups")
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
	require.Equal(t, int64(1), f.liveSessions(t, user.ID))
	require.Equal(t, "token-2", f.handleAt(t, user.ID))
}

func TestTransientReleasesTheLeaseAndKeepsEverythingElse(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	before := f.row(t, user.ID)
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	applied, err := f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-b/1")
	require.NoError(t, err)
	require.False(t, applied, "only the holder records an outcome")
	applied, err = f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)

	row := f.row(t, user.ID)
	requireNoLease(t, row)
	require.Equal(t, before.RefreshGrant, row.RefreshGrant)
	require.Equal(t, before.RefreshGrantVersion, row.RefreshGrantVersion)
	require.Equal(t, before.RefreshUncertain, row.RefreshUncertain)
	require.True(t, row.GroupsRefreshedAt.Equal(*before.GroupsRefreshedAt))
}

func TestLoadLeasedRefreshGrantRejectsUnusableGrants(t *testing.T) {
	f := newRefreshFixture(t)
	ada := f.login(t, oidcIdentity("ada", "token-ada"))
	bob := f.login(t, oidcIdentity("bob", "token-bob"))
	require.True(t, f.claim(t, ada.ID, 1, "node-a/1"))
	_, err := f.users.LoadLeasedRefreshGrant(t.Context(), ada.ID, 1, "node-z/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshLeaseLost)

	// A rotated hash secret cannot open the grant: treat it as none.
	rotatedSecret := NewUserStore(f.db, WithRefreshGrantSecret(katSecret+"-rotated"))
	_, err = rotatedSecret.LoadLeasedRefreshGrant(t.Context(), ada.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)
	noSecret := NewUserStore(f.db)
	_, err = noSecret.LoadLeasedRefreshGrant(t.Context(), ada.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)

	// bob's ciphertext copied onto ada's row does not open as ada's.
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", ada.ID).
		Update("refresh_grant", f.row(t, bob.ID).RefreshGrant).Error)
	_, err = f.users.LoadLeasedRefreshGrant(t.Context(), ada.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)

	// A grant sealed for ada but naming another issuer is refused too.
	foreign, err := f.users.grants.seal(ada.ID, RefreshGrant{Provider: "oidc", Issuer: "https://evil", Handle: NewRefreshHandle("t")})
	require.NoError(t, err)
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", ada.ID).Update("refresh_grant", foreign).Error)
	_, err = f.users.LoadLeasedRefreshGrant(t.Context(), ada.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, ErrRefreshGrantInvalid)

	applied, err := f.users.CompleteRefreshDiscard(t.Context(), ada.ID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)
	row := f.row(t, ada.ID)
	require.Nil(t, row.RefreshGrant)
	require.Equal(t, int64(2), row.RefreshGrantVersion)
	requireNoLease(t, row)
}

type pgConflictError struct{}

func (pgConflictError) Error() string    { return "ERROR: could not serialize access (SQLSTATE 40001)" }
func (pgConflictError) SQLState() string { return "40001" }

var failUpdateSeq atomic.Uint64

// failUpdates fails the next n updates of table with err.
func failUpdates(t *testing.T, db *gorm.DB, table string, n int, err error) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	remaining := int32(n)
	name := fmt.Sprintf("test:fail_%s_updates:%s:%d", table, t.Name(), failUpdateSeq.Add(1))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register(name, func(tx *gorm.DB) {
		if tx.Statement.Table != table {
			return
		}
		calls.Add(1)
		if remaining > 0 {
			remaining--
			_ = tx.AddError(err)
		}
	}))
	t.Cleanup(func() { _ = db.Callback().Update().Remove(name) })
	return &calls
}

func TestDefinitiveRetriesTheWholeTransaction(t *testing.T) {
	for name, injected := range map[string]error{
		"sqlite busy":        errors.New("database is locked"),
		"postgres conflict":  pgConflictError{},
		"wrapped checkpoint": fmt.Errorf("exec: %w", errors.New("checkpoint in progress")),
	} {
		t.Run(name, func(t *testing.T) {
			f := newRefreshFixture(t)
			user := f.login(t, oidcIdentity("ada", "token-1"))
			f.newSession(t, user.ID)
			require.True(t, f.claim(t, user.ID, 1, "node-a/1"))

			sessionUpdates := failUpdates(t, f.db, "sessions", 1, injected)
			result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
			require.NoError(t, err)
			require.Equal(t, DefinitiveResult{Outcome: DefinitiveRevoked, SessionsRevoked: 1}, result,
				"the retry must see the first attempt's grant clear rolled back, or its fence would fail")
			require.Equal(t, int32(2), sessionUpdates.Load())
			row := f.row(t, user.ID)
			require.Equal(t, int64(2), row.RefreshGrantVersion, "the version advanced exactly once")
			require.Nil(t, row.RefreshGrant)
			require.Zero(t, f.liveSessions(t, user.ID))
		})
	}
}

func TestDefinitiveRollsBackOnFailure(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	f.newSession(t, user.ID)
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	before := f.row(t, user.ID)

	boom := errors.New("disk I/O error")
	sessionUpdates := failUpdates(t, f.db, "sessions", 100, boom)
	_, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, 1, "node-a/1", f.clock.Now())
	require.ErrorIs(t, err, boom)
	require.Equal(t, int32(1), sessionUpdates.Load(), "a non-contention error is not retried")

	row := f.row(t, user.ID)
	require.Equal(t, before.RefreshGrant, row.RefreshGrant, "the grant clear rolled back with the revocation")
	require.Equal(t, int64(1), row.RefreshGrantVersion)
	require.Equal(t, "node-a/1", *row.RefreshLeaseOwner, "the lease is still held; the holder may retry")
	require.Equal(t, int64(1), f.liveSessions(t, user.ID))
}

func TestRefreshStatementsRetryOnContention(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1"))
	userUpdates := failUpdates(t, f.db, "users", 2, errors.New("database is locked"))
	require.True(t, f.claim(t, user.ID, 1, "node-a/1"))
	require.Equal(t, int32(3), userUpdates.Load())

	userUpdates = failUpdates(t, f.db, "users", len(userTxRetryBackoffs)+1, pgConflictError{})
	_, err := f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-a/1")
	require.ErrorAs(t, err, new(pgConflictError))
	require.Equal(t, int32(len(userTxRetryBackoffs)+1), userUpdates.Load(), "the retry budget is bounded")
}

func TestNewRefreshLeaseOwnerIsUniquePerAttempt(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		owner := NewRefreshLeaseOwner("node-a")
		require.Regexp(t, `^node-a/[A-Z2-7]{26}$`, owner)
		require.False(t, seen[owner])
		seen[owner] = true
	}
	require.Regexp(t, `^node/`, NewRefreshLeaseOwner("  "))
}
