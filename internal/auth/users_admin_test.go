package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// The user administration tests run on the refresh fixture: in-memory SQLite
// by default (one connection, so concurrent work is serialised by the pool),
// or PostgreSQL when CAESIUM_TEST_POSTGRES_DSN is set, where the row-lock
// cases really interleave.

func (f *refreshFixture) seedUser(t *testing.T, issuer, email string, created time.Time, disabled bool) models.User {
	t.Helper()
	u := models.User{
		ID:        uuid.New(),
		Issuer:    issuer,
		Subject:   uuid.NewString(),
		Email:     email,
		Role:      models.RoleViewer,
		CreatedAt: created,
	}
	if disabled {
		at := created
		u.DisabledAt = &at
	}
	require.NoError(t, f.db.Create(&u).Error)
	return u
}

func userIDs(users []models.User) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(users))
	for _, u := range users {
		ids = append(ids, u.ID)
	}
	return ids
}

func TestUserAdminListUsersFiltersAndPages(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	base := f.clock.Now()
	alice := f.seedUser(t, "https://a.example", "Alice@Example.com", base, false)
	bob := f.seedUser(t, "https://a.example", "bob_smith@example.com", base.Add(time.Second), true)
	carol := f.seedUser(t, "https://b.example", "carol%x@example.com", base.Add(2*time.Second), false)
	// Two users created at the same instant order by id (descending).
	tieA := f.seedUser(t, "https://b.example", "tie-a@corp.test", base.Add(3*time.Second), false)
	tieB := f.seedUser(t, "https://b.example", "tie-b@corp.test", base.Add(3*time.Second), false)
	ties := []uuid.UUID{tieA.ID, tieB.ID}
	if tieA.ID.String() < tieB.ID.String() {
		ties = []uuid.UUID{tieB.ID, tieA.ID}
	}

	list := func(filter UserFilter) UserPage {
		t.Helper()
		if filter.Limit == 0 {
			filter.Limit = 100
		}
		page, err := admin.ListUsers(t.Context(), filter)
		require.NoError(t, err)
		return page
	}

	all := list(UserFilter{})
	require.Equal(t, int64(5), all.Total)
	require.False(t, all.HasMore)
	require.Equal(t, append(ties, carol.ID, bob.ID, alice.ID), userIDs(all.Users), "newest first, id breaks ties")

	require.Equal(t, []uuid.UUID{bob.ID, alice.ID}, userIDs(list(UserFilter{Issuer: "https://a.example"}).Users))
	require.Empty(t, list(UserFilter{Issuer: "https://a.example/"}).Users, "issuer matches exactly")
	require.Equal(t, []uuid.UUID{alice.ID}, userIDs(list(UserFilter{Email: "aLiCe@"}).Users), "email is case-insensitive")
	require.Equal(t, []uuid.UUID{bob.ID}, userIDs(list(UserFilter{Email: "b_s"}).Users), "_ is literal")
	require.Empty(t, list(UserFilter{Email: "bxs"}).Users, "_ is not a wildcard")
	require.Equal(t, []uuid.UUID{carol.ID}, userIDs(list(UserFilter{Email: "%x"}).Users), "% is literal")
	require.Empty(t, list(UserFilter{Email: "!"}).Users, "the escape character itself is literal")

	disabled, enabled := true, false
	require.Equal(t, []uuid.UUID{bob.ID}, userIDs(list(UserFilter{Disabled: &disabled}).Users))
	onlyEnabled := list(UserFilter{Disabled: &enabled})
	require.Equal(t, int64(4), onlyEnabled.Total)
	require.NotContains(t, userIDs(onlyEnabled.Users), bob.ID)

	first := list(UserFilter{Limit: 2})
	require.Equal(t, ties, userIDs(first.Users), "the page boundary splits nothing between equal timestamps")
	require.True(t, first.HasMore)
	require.Equal(t, int64(5), first.Total)
	second := list(UserFilter{Limit: 2, Offset: 2})
	require.Equal(t, []uuid.UUID{carol.ID, bob.ID}, userIDs(second.Users))
	require.True(t, second.HasMore)
	last := list(UserFilter{Limit: 2, Offset: 4})
	require.Equal(t, []uuid.UUID{alice.ID}, userIDs(last.Users))
	require.False(t, last.HasMore)

	_, err := admin.ListUsers(t.Context(), UserFilter{})
	require.Error(t, err, "a zero limit is refused rather than unbounded")
	_, err = admin.ListUsers(t.Context(), UserFilter{Limit: 1, Offset: -1})
	require.Error(t, err)
}

// TestUserAdminEmailFilterFoldsBothSidesAlike: the email filter folds the
// stored value and the pattern with the same database LOWER, so an exact
// non-ASCII email is always found; on PostgreSQL, whose LOWER folds non-ASCII
// letters, a different case is found too.
func TestUserAdminEmailFilterFoldsBothSidesAlike(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	ada := f.seedUser(t, "https://a.example", "Äda@Example.com", f.clock.Now(), false)
	f.seedUser(t, "https://a.example", "other@example.com", f.clock.Now(), false)

	for _, query := range []string{"Äda@Example.com", "Äda@example.COM", "Äda"} {
		page, err := admin.ListUsers(t.Context(), UserFilter{Email: query, Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{ada.ID}, userIDs(page.Users), "query %q", query)
	}
	if f.db.Name() == "postgres" {
		page, err := admin.ListUsers(t.Context(), UserFilter{Email: "äda@", Limit: 10})
		require.NoError(t, err)
		require.Equal(t, []uuid.UUID{ada.ID}, userIDs(page.Users), "PostgreSQL folds non-ASCII on both sides")
	}
}

func TestUserAdminDisableRevokesSessionsAndClearsGrant(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("dora", "dora-refresh-token-0001", "eng"))
	other := f.login(t, oidcIdentity("otto", "otto-refresh-token-0001", "eng"))
	live1 := f.newSession(t, user.ID)
	live2 := f.newSession(t, user.ID)
	gone := f.newSession(t, user.ID)
	require.NoError(t, f.sessions.Revoke(t.Context(), gone))
	var goneBefore models.Session
	require.NoError(t, f.db.Where("id = ?", gone).Take(&goneBefore).Error)
	otherSession := f.newSession(t, other.ID)
	before := f.row(t, user.ID)
	require.NotEmpty(t, before.RefreshGrant)

	f.clock.Advance(time.Minute)
	change, err := admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.True(t, change.Changed)
	require.Equal(t, int64(2), change.SessionsRevoked, "only the live sessions count")
	require.NotNil(t, change.User.DisabledAt)
	require.True(t, change.User.DisabledAt.Equal(f.clock.Now()))

	row := f.row(t, user.ID)
	require.Nil(t, row.RefreshGrant, "a disabled user keeps no refresh grant")
	require.Equal(t, before.RefreshGrantVersion+1, row.RefreshGrantVersion, "clearing the grant fences in-flight refreshes")
	require.Equal(t, before.DisableGeneration+1, row.DisableGeneration, "the disable advances the login fence")
	requireNoLease(t, row)
	require.False(t, row.RefreshUncertain)
	require.Zero(t, f.liveSessions(t, user.ID))
	for _, id := range []uuid.UUID{live1, live2} {
		var s models.Session
		require.NoError(t, f.db.Where("id = ?", id).Take(&s).Error)
		require.NotNil(t, s.RevokedAt)
		require.True(t, s.RevokedAt.Equal(f.clock.Now()))
	}
	var goneAfter models.Session
	require.NoError(t, f.db.Where("id = ?", gone).Take(&goneAfter).Error)
	require.True(t, goneAfter.RevokedAt.Equal(*goneBefore.RevokedAt), "an earlier revocation is left as it was")
	require.Equal(t, int64(1), f.liveSessions(t, other.ID))
	var otherRow models.Session
	require.NoError(t, f.db.Where("id = ?", otherSession).Take(&otherRow).Error)
	require.Nil(t, otherRow.RevokedAt)

	again, err := admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.False(t, again.Changed)
	require.Zero(t, again.SessionsRevoked)
	require.Equal(t, row.RefreshGrantVersion, f.row(t, user.ID).RefreshGrantVersion, "a repeat changes nothing")
	require.Equal(t, row.DisableGeneration, f.row(t, user.ID).DisableGeneration)

	_, err = admin.DisableUser(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestUserAdminDisableFencesAnInFlightRefresh(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("ivy", "ivy-refresh-token-0001", "eng"))
	owner := NewRefreshLeaseOwner("node-a")
	require.True(t, f.claim(t, user.ID, user.RefreshGrantVersion, owner))

	_, err := admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)

	applied, err := f.users.CompleteRefreshSuccess(t.Context(), user.ID, user.RefreshGrantVersion, owner, []string{"admins"}, nil, f.clock.Now())
	require.NoError(t, err)
	require.False(t, applied, "a refresh claimed before the disable records nothing")
	result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, user.RefreshGrantVersion, owner, f.clock.Now())
	require.NoError(t, err)
	require.Equal(t, DefinitiveDiscarded, result.Outcome)
	require.JSONEq(t, `["eng"]`, string(f.row(t, user.ID).Groups))
}

func TestUserAdminConcurrentDisablesChangeOnce(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("jay", "", "eng"))
	f.newSession(t, user.ID)

	var wg sync.WaitGroup
	changes := make([]UserChange, 4)
	errs := make([]error, 4)
	for i := range changes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			changes[i], errs[i] = admin.DisableUser(t.Context(), user.ID)
		}(i)
	}
	wg.Wait()
	changed, revoked := 0, int64(0)
	for i := range changes {
		require.NoError(t, errs[i])
		if changes[i].Changed {
			changed++
		}
		revoked += changes[i].SessionsRevoked
	}
	require.Equal(t, 1, changed, "exactly one disable takes effect")
	require.Equal(t, int64(1), revoked)
}

func TestUserAdminEnableNeverRevivesAndSweepsStrays(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("kim", "", "eng"))
	token, _, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: user.ID, DisableGeneration: f.row(t, user.ID).DisableGeneration})
	require.NoError(t, err)

	_, err = admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)
	// A row inserted while the user is disabled (bypassing the mint fence)
	// must not come alive when the user is enabled.
	f.clock.Advance(time.Second)
	stray := models.Session{
		ID: uuid.New(), UserID: user.ID, TokenHash: "stray-" + uuid.NewString(), CSRFToken: "c",
		CreatedAt: f.clock.Now(), IdleExpiresAt: f.clock.Now().Add(time.Hour), AbsoluteExpiresAt: f.clock.Now().Add(time.Hour),
	}
	require.NoError(t, f.db.Create(&stray).Error)

	change, err := admin.EnableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.True(t, change.Changed)
	require.Nil(t, change.User.DisabledAt)
	require.Equal(t, int64(1), change.SessionsRevoked, "the stray is swept")
	require.Zero(t, f.liveSessions(t, user.ID))

	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrSessionRevoked, "enable does not revive a session the disable revoked")

	again, err := admin.EnableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.False(t, again.Changed)
	require.Zero(t, again.SessionsRevoked)

	// Enabled again, the user can sign in normally.
	f.newSession(t, user.ID)
	require.Equal(t, int64(1), f.liveSessions(t, user.ID))

	_, err = admin.EnableUser(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestUserAdminRevokeUserSessions(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("lee", "", "eng"))
	f.newSession(t, user.ID)
	f.newSession(t, user.ID)

	revoked, err := admin.RevokeUserSessions(t.Context(), user.ID)
	require.NoError(t, err)
	require.Equal(t, RevokedCredentials{Sessions: 2}, revoked)
	revoked, err = admin.RevokeUserSessions(t.Context(), user.ID)
	require.NoError(t, err)
	require.Zero(t, revoked)
	require.Nil(t, f.row(t, user.ID).DisabledAt, "logging out everywhere does not disable")

	_, err = admin.RevokeUserSessions(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrUserNotFound)
	_, err = admin.GetUser(t.Context(), uuid.New())
	require.ErrorIs(t, err, ErrUserNotFound)
}

func TestUserAdminDetailCapsSessions(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("max", "", "eng"))
	for range DetailSessionLimit + 1 {
		f.newSession(t, user.ID)
		f.clock.Advance(time.Millisecond)
	}
	detail, err := admin.GetUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.Equal(t, user.ID, detail.User.ID)
	require.Len(t, detail.Sessions, DetailSessionLimit)
	require.Equal(t, int64(DetailSessionLimit+1), detail.SessionsTotal)
	require.True(t, detail.Sessions[0].CreatedAt.After(detail.Sessions[DetailSessionLimit-1].CreatedAt), "newest first")
}

// TestSSOLoginCheckedBeforeDisableCannotMint pauses a login between its
// disabled check and the session insert, disables and re-enables the user
// (with the enable's sweep) in that window, then lets the login finish: the
// version fence refuses the insert, so no session exists afterwards.
func TestSSOLoginCheckedBeforeDisableCannotMint(t *testing.T) {
	for _, tc := range []struct {
		name   string
		enable bool
	}{
		{name: "disabled in the window", enable: false},
		{name: "disabled and enabled in the window", enable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRefreshFixture(t)
			admin := NewUserAdmin(f.sessions)
			mapper, err := NewRoleMapper("eng=operator", "")
			require.NoError(t, err)
			auditor := NewAuditLogger(f.db)
			sso := NewSSOService(f.users, f.sessions, mapper, WithSSOAuditLogger(auditor))
			ext := oidcIdentity("nia", "nia-refresh-token-0001", "eng")
			user := f.login(t, ext)

			sso.beforeMint = func() {
				_, err := admin.DisableUser(t.Context(), user.ID)
				require.NoError(t, err)
				if tc.enable {
					_, err = admin.EnableUser(t.Context(), user.ID)
					require.NoError(t, err)
				}
			}
			_, _, err = sso.Complete(t.Context(), ext, "oidc", "203.0.113.9", "test")
			require.ErrorIs(t, err, ErrLoginDenied)

			var sessions int64
			require.NoError(t, f.db.Model(&models.Session{}).Where("user_id = ?", user.ID).Count(&sessions).Error)
			require.Zero(t, sessions, "the login checked before the disable minted nothing")

			denials, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLoginDenied})
			require.NoError(t, err)
			require.Len(t, denials, 1)
			var meta map[string]any
			require.NoError(t, json.Unmarshal(denials[0].Metadata, &meta))
			want := "user_changed"
			if !tc.enable {
				want = "user_disabled"
			}
			require.Equal(t, want, meta["reason"])

			if tc.enable {
				// A fresh login after the enable succeeds.
				sso.beforeMint = nil
				_, sess, err := sso.Complete(t.Context(), ext, "oidc", "203.0.113.9", "test")
				require.NoError(t, err)
				require.Equal(t, user.ID, sess.UserID)
			}
		})
	}
}

// TestSSOLoginReadBeforeDisableAndEnableIsRefused pauses a login after it
// read the user (enabled) and just before its profile UPDATE, then disables
// and re-enables the user: the UPDATE is fenced on the version the login read,
// so the login is refused (user_changed) instead of re-reading a fresh version
// and minting a session after both revocations. On PostgreSQL the disable and
// enable commit on other connections; on the one-connection SQLite test DB they
// run as statements inside the paused transaction, which the UPDATE sees the
// same way.
func TestSSOLoginReadBeforeDisableAndEnableIsRefused(t *testing.T) {
	f, sso, auditor := newRefreshSSO(t)
	admin := NewUserAdmin(f.sessions)
	ext := oidcIdentity("pia", "pia-refresh-token-0001", "eng")
	_, first, err := sso.Complete(t.Context(), ext, "oidc", "", "")
	require.NoError(t, err)
	userID := first.UserID

	postgres := f.db.Name() == "postgres"
	const callback = "test:disable_enable_before_login_update"
	fired := false
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if fired || tx.Statement.Table != "users" {
			return
		}
		fired = true
		if postgres {
			_, err := admin.DisableUser(t.Context(), userID)
			require.NoError(t, err)
			_, err = admin.EnableUser(t.Context(), userID)
			require.NoError(t, err)
			return
		}
		raw := tx.Session(&gorm.Session{NewDB: true})
		now := f.clock.Now()
		require.NoError(t, raw.Exec(`UPDATE users SET disabled_at = ?, refresh_grant = NULL,
			refresh_grant_version = COALESCE(refresh_grant_version, 0) + 1,
			disable_generation = COALESCE(disable_generation, 0) + 1 WHERE id = ?`, now, userID).Error)
		require.NoError(t, raw.Exec(`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`, now, userID).Error)
		require.NoError(t, raw.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, userID).Error)
	}))
	t.Cleanup(func() { _ = f.db.Callback().Update().Remove(callback) })

	_, _, err = sso.Complete(t.Context(), ext, "oidc", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)
	require.True(t, fired)
	require.Zero(t, f.liveSessions(t, userID), "no session outlives the disable")

	denials, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLoginDenied})
	require.NoError(t, err)
	require.Len(t, denials, 1)
	var meta map[string]any
	require.NoError(t, json.Unmarshal(denials[0].Metadata, &meta))
	require.Equal(t, "user_changed", meta["reason"])

	// A fresh login after the enable is accepted.
	require.NoError(t, f.db.Callback().Update().Remove(callback))
	_, again, err := sso.Complete(t.Context(), ext, "oidc", "", "")
	require.NoError(t, err)
	require.Equal(t, userID, again.UserID)
}

// TestConcurrentSSOLoginsAllMint: the login fence is the disable generation,
// which logins never move, so concurrent logins of one user all succeed (the
// newest grant wins, as E1 requires) instead of refusing each other.
func TestConcurrentSSOLoginsAllMint(t *testing.T) {
	f, sso, _ := newRefreshSSO(t)
	_, first, err := sso.Complete(t.Context(), oidcIdentity("quin", "quin-token-0", "eng"), "oidc", "", "")
	require.NoError(t, err)

	const logins = 6
	errs := make([]error, logins)
	var wg sync.WaitGroup
	for i := range logins {
		wg.Go(func() {
			_, _, errs[i] = sso.Complete(context.Background(), oidcIdentity("quin", fmt.Sprintf("quin-token-%d", i+1), "eng"), "oidc", "", "")
		})
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "login %d", i)
	}
	require.Equal(t, int64(logins+1), f.liveSessions(t, first.UserID))
}

// TestSessionMintRacingDisableEndsRevoked pauses Create inside its
// transaction, after the user row was locked and checked, and starts a
// disable there. On PostgreSQL the disable blocks on the row lock until the
// insert commits and then revokes the new session; the test waits until the
// disable is really blocked before letting the insert proceed. On SQLite the
// single test connection serialises the two transactions the same way. Either
// way no live session of a disabled user survives.
func TestSessionMintRacingDisableEndsRevoked(t *testing.T) {
	f := newRefreshFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.login(t, oidcIdentity("ola", "", "eng"))
	generation := f.row(t, user.ID).DisableGeneration

	postgres := f.db.Name() == "postgres"
	var wg sync.WaitGroup
	var disableErr error
	f.sessions.beforeMint = func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, disableErr = admin.DisableUser(t.Context(), user.ID)
		}()
		if postgres {
			waitForBlockedLock(t, f.db)
		}
	}
	token, sess, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: user.ID, DisableGeneration: generation})
	f.sessions.beforeMint = nil
	require.NoError(t, err, "the mint that won the lock commits")
	wg.Wait()
	require.NoError(t, disableErr)

	var row models.Session
	require.NoError(t, f.db.Where("id = ?", sess.ID).Take(&row).Error)
	require.NotNil(t, row.RevokedAt, "the disable that waited for the insert revoked it")
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrSessionRevoked)
}

// waitForBlockedLock waits until some backend is waiting on a lock.
func waitForBlockedLock(t *testing.T, db *gorm.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int64
		require.NoError(t, db.Raw("SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting).Error)
		if waiting > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	require.FailNow(t, "the concurrent disable never blocked on the user row lock")
}

func TestNewUserAdminNilSessions(t *testing.T) {
	require.Nil(t, NewUserAdmin(nil))
	require.True(t, errors.Is(wrapUserAdminError("x", fmt.Errorf("wrapped: %w", ErrUserNotFound)), ErrUserNotFound))
}
