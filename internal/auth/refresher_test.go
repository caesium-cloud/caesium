package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeRefresher is a GroupRefresher whose answers the test controls. With
// rotate set it rotates the handle on every redemption and refuses any but
// the latest one as invalid_grant, as an IdP with refresh-token rotation and
// reuse detection does.
type fakeRefresher struct {
	name string

	mu      sync.Mutex
	calls   int
	groups  []string
	err     error
	rotate  bool
	current string
	known   bool
	// gate, when set, holds every answer until it is closed (or the
	// redemption's context ends, which answers uncertain unless
	// answerAtDeadline is set: then it answers success at the deadline).
	gate             chan struct{}
	answerAtDeadline bool
	seen             []string
}

func newFakeRefresher(groups ...string) *fakeRefresher {
	return &fakeRefresher{name: "oidc", groups: groups, known: true}
}

func (f *fakeRefresher) Name() string { return f.name }

func (f *fakeRefresher) set(fn func(f *fakeRefresher)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeRefresher) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRefresher) RefreshGroups(ctx context.Context, req RefreshRequest) (GroupRefresh, error) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	gate, atDeadline := f.gate, f.answerAtDeadline
	f.seen = append(f.seen, req.Grant.Handle.Reveal())
	f.mu.Unlock()
	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			if !atDeadline {
				return GroupRefresh{}, NewRefreshError(RefreshUncertain, "timeout_after_send", ctx.Err())
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return GroupRefresh{}, f.err
	}
	handle := req.Grant.Handle.Reveal()
	if f.rotate {
		if f.current != "" && handle != f.current {
			return GroupRefresh{}, NewRefreshError(RefreshDefinitive, "invalid_grant", nil)
		}
		f.current = fmt.Sprintf("rotated-%d", n)
		return GroupRefresh{Groups: append([]string(nil), f.groups...), GroupsKnown: f.known, Handle: NewRefreshHandle(f.current), Rotated: true}, nil
	}
	return GroupRefresh{Groups: append([]string(nil), f.groups...), GroupsKnown: f.known, Handle: req.Grant.Handle}, nil
}

type freshnessFixture struct {
	*refreshFixture
	fake     *fakeRefresher
	svc      *GroupRefreshService
	sessions *SessionStore
	keys     *Service
	cancel   context.CancelFunc
}

const (
	testInterval = 15 * time.Minute
	testMaxStale = 24 * time.Hour
)

func newFreshnessFixture(t *testing.T, fake *fakeRefresher) *freshnessFixture {
	t.Helper()
	base := newRefreshFixture(t)
	f := &freshnessFixture{refreshFixture: base, fake: fake}
	f.svc, f.cancel = f.newService(t, "node-a")
	f.sessions = f.sessionStore(f.svc)
	f.keys = NewService(base.db,
		WithKeyHashSecret(katSecret),
		WithNow(base.clock.Now),
		WithUserKeyTTL(MaxUserKeyTTL),
		WithGroupFreshness(f.svc),
	)
	return f
}

func (f *freshnessFixture) newService(t *testing.T, node string, tune ...func(*GroupRefreshConfig)) (*GroupRefreshService, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	opts := []GroupRefreshOption{
		WithGroupRefreshGrantSecret(katSecret),
		WithGroupRefreshNow(f.clock.Now),
		WithGroupRefreshAuditor(NewAuditLogger(f.db)),
	}
	if f.fake != nil {
		opts = append(opts, WithGroupRefreshers(f.fake))
	}
	cfg := GroupRefreshConfig{Interval: testInterval, MaxStaleness: testMaxStale, Timeout: 2 * time.Second, Node: node}
	for _, fn := range tune {
		fn(&cfg)
	}
	svc, err := NewGroupRefreshService(ctx, f.db, cfg, opts...)
	require.NoError(t, err)
	t.Cleanup(func() {
		cancel()
		svc.Wait()
	})
	return svc, cancel
}

func (f *freshnessFixture) sessionStore(svc GroupFreshness) *SessionStore {
	opts := []SessionStoreOption{WithSessionNow(f.clock.Now), WithSessionTTLs(30*24*time.Hour, 30*24*time.Hour)}
	if svc != nil {
		opts = append(opts, WithSessionGroupFreshness(svc))
	}
	return NewSessionStore(f.db, opts...)
}

// browser logs the user in and returns a session cookie value.
func (f *freshnessFixture) browser(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	row := f.row(t, userID)
	token, _, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: userID, AuthMethod: "oidc", DisableGeneration: row.DisableGeneration})
	require.NoError(t, err)
	return token
}

// cli mints a user-bound key and returns its plaintext.
func (f *freshnessFixture) cli(t *testing.T, userID uuid.UUID) string {
	t.Helper()
	row := f.row(t, userID)
	resp, err := f.keys.MintUserBoundKey(t.Context(), MintUserKeyRequest{UserID: userID, DisableGeneration: row.DisableGeneration, CreatedBy: row.Email})
	require.NoError(t, err)
	return resp.Plaintext
}

func (f *freshnessFixture) sessionGroups(t *testing.T, token string) []string {
	t.Helper()
	_, user, err := f.sessions.Validate(t.Context(), token)
	require.NoError(t, err)
	return decodeGroups(t, user.Groups)
}

func (f *freshnessFixture) keyGroups(t *testing.T, plaintext string) []string {
	t.Helper()
	_, principal, err := f.keys.AuthenticateKey(t.Context(), plaintext)
	require.NoError(t, err)
	require.NotNil(t, principal.UserID)
	return decodeGroups(t, f.row(t, *principal.UserID).Groups)
}

func decodeGroups(t *testing.T, raw []byte) []string {
	t.Helper()
	var groups []string
	if len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, &groups))
	}
	return groups
}

func (f *freshnessFixture) storedGroups(t *testing.T, id uuid.UUID) []string {
	t.Helper()
	return decodeGroups(t, f.row(t, id).Groups)
}

func (f *freshnessFixture) waitIdle(t *testing.T, id uuid.UUID) {
	t.Helper()
	require.Eventually(t, func() bool {
		f.svc.mu.Lock()
		_, running := f.svc.flights[id]
		f.svc.mu.Unlock()
		return !running
	}, 5*time.Second, 5*time.Millisecond)
}

// auditRows returns the user's audit rows of action once the user's flight
// has finished and the audit writes it handed off (afterOutcome) are done.
func (f *freshnessFixture) auditRows(t *testing.T, action string, userID uuid.UUID) []map[string]any {
	t.Helper()
	f.waitIdle(t, userID)
	f.svc.pendingAfter.Wait()
	var rows []models.AuditLog
	require.NoError(t, f.db.Where("action = ? AND resource_id = ?", action, userID.String()).Order("timestamp").Find(&rows).Error)
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		meta := map[string]any{}
		if len(row.Metadata) > 0 {
			require.NoError(t, json.Unmarshal(row.Metadata, &meta))
		}
		meta["_actor"] = row.Actor
		out = append(out, meta)
	}
	return out
}

func refreshCount(t *testing.T, provider, outcome string) float64 {
	t.Helper()
	return metrictestutil.CounterValue(t, metrics.AuthGroupRefreshTotal, provider, outcome)
}

// TestGroupFreshnessLeavesFreshUsersUntouched is the byte-identity oracle: a
// user whose groups are fresh is admitted exactly as without the refresher,
// with no IdP call, for a session and for a user-bound key.
func TestGroupFreshnessLeavesFreshUsersUntouched(t *testing.T) {
	fake := newFakeRefresher("eng")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng", "ops"))
	token := f.browser(t, user.ID)
	key := f.cli(t, user.ID)

	plainSessions := f.sessionStore(nil)
	plainKeys := NewService(f.db, WithKeyHashSecret(katSecret), WithNow(f.clock.Now))
	f.clock.Advance(testInterval)

	sess, got, err := f.sessions.Validate(t.Context(), token)
	require.NoError(t, err)
	wantSess, want, err := plainSessions.Validate(t.Context(), token)
	require.NoError(t, err)
	require.Equal(t, wantSess.ID, sess.ID)
	require.Equal(t, want, got)

	_, principal, err := f.keys.AuthenticateKey(t.Context(), key)
	require.NoError(t, err)
	_, wantPrincipal, err := plainKeys.AuthenticateKey(t.Context(), key)
	require.NoError(t, err)
	require.Equal(t, wantPrincipal, principal)

	require.Zero(t, fake.Calls())
}

// TestGroupRefreshDueRunsOnceInTheBackground: a browser user past the
// interval keeps its stored groups for the requests that find it due, while
// exactly one refresh (single-flight under concurrency) replaces them; the
// change is audited and counted.
func TestGroupRefreshDueRunsOnceInTheBackground(t *testing.T) {
	fake := newFakeRefresher("eng", "payroll")
	gate := make(chan struct{})
	fake.set(func(f *fakeRefresher) { f.gate = gate })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng", "ops"))
	token := f.browser(t, user.ID)
	successBefore := refreshCount(t, "oidc", refreshOutcomeSuccess)

	f.clock.Advance(20 * time.Minute)
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			_, admitted, err := f.sessions.Validate(t.Context(), token)
			if err == nil && len(decodeGroups(t, admitted.Groups)) != 2 {
				err = errors.New("unexpected groups")
			}
			require.NoError(t, err)
		})
	}
	wg.Wait()
	require.Equal(t, []string{"eng", "ops"}, f.sessionGroups(t, token), "a due user proceeds with its stored groups")
	close(gate)
	f.waitIdle(t, user.ID)

	require.Equal(t, 1, fake.Calls(), "single-flight per user")
	require.Equal(t, []string{"eng", "payroll"}, f.sessionGroups(t, token))
	row := f.row(t, user.ID)
	require.Equal(t, f.clock.Now(), row.GroupsRefreshedAt.UTC())
	requireNoLease(t, row)
	require.Equal(t, successBefore+1, refreshCount(t, "oidc", refreshOutcomeSuccess))

	audits := f.auditRows(t, ActionAuthGroupsRefreshed, user.ID)
	require.Len(t, audits, 1)
	require.Equal(t, groupRefreshActor, audits[0]["_actor"])
	require.Equal(t, []any{"payroll"}, audits[0]["added"])
	require.Equal(t, []any{"ops"}, audits[0]["removed"])
	require.Equal(t, "async", audits[0]["trigger"])

	// An unchanged set is refreshed but not audited.
	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)
	require.Equal(t, 2, fake.Calls())
	require.Len(t, f.auditRows(t, ActionAuthGroupsRefreshed, user.ID), 1)
}

// TestGroupRefreshCLIOnlyUser: a user who only ever uses a user-bound key
// is refreshed through the key's validation alone.
func TestGroupRefreshCLIOnlyUser(t *testing.T) {
	fake := newFakeRefresher("finance-eng")
	gate := make(chan struct{})
	releaseGate := sync.OnceFunc(func() { close(gate) })
	defer releaseGate()
	fake.set(func(f *fakeRefresher) { f.gate = gate })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("cli", "token-1", "marketing-eng"))
	key := f.cli(t, user.ID)

	f.clock.Advance(20 * time.Minute)
	require.Equal(t, []string{"marketing-eng"}, f.keyGroups(t, key))
	releaseGate()
	f.waitIdle(t, user.ID)
	require.Equal(t, 1, fake.Calls())
	require.Equal(t, []string{"finance-eng"}, f.keyGroups(t, key))
}

// TestProlongedTransientFailureEndsInStaleGroupsUntilLogin: transient IdP
// failures keep the stored groups (with an asynchronous back-off); past the
// maximum staleness one synchronous attempt fails and the refusal is
// persisted for every credential of the user, survives a recovered IdP and
// a restart, and is lifted only by a login.
func TestProlongedTransientFailureEndsInStaleGroupsUntilLogin(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) {
		f.err = NewRefreshError(RefreshTransient, "dial_failed", errors.New("connection refused"))
	})
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	key := f.cli(t, user.ID)
	transientBefore := refreshCount(t, "oidc", refreshOutcomeTransient)

	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)
	require.Equal(t, 1, fake.Calls())
	require.Equal(t, transientBefore+1, refreshCount(t, "oidc", refreshOutcomeTransient))

	// Within the back-off nothing is retried; after it, the next request is.
	f.clock.Advance(10 * time.Second)
	f.keyGroups(t, key)
	f.waitIdle(t, user.ID)
	require.Equal(t, 1, fake.Calls())
	f.clock.Advance(asyncRefreshBackoffCap)
	f.keyGroups(t, key)
	f.waitIdle(t, user.ID)
	require.Equal(t, 2, fake.Calls())
	require.Nil(t, f.row(t, user.ID).StaleRefusedAt)

	// Past the bound, the first request (inside the back-off window: the
	// throttle never suppresses it) attempts one synchronous refresh.
	f.clock.Advance(testMaxStale)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Equal(t, 3, fake.Calls())
	require.NotNil(t, f.row(t, user.ID).StaleRefusedAt)

	// Every credential is refused without contacting the IdP again, even
	// once it recovered.
	fake.set(func(f *fakeRefresher) { f.err = nil })
	_, _, err = f.keys.AuthenticateKey(t.Context(), key)
	require.ErrorIs(t, err, ErrStaleGroups)
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Equal(t, 3, fake.Calls())

	// A restarted process (a new service, another node) reads the refusal.
	other, _ := f.newService(t, "node-b")
	_, _, err = f.sessionStore(other).Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Equal(t, 3, fake.Calls())

	// A login clears it.
	f.login(t, oidcIdentity("ada", "token-2", "eng"))
	require.Nil(t, f.row(t, user.ID).StaleRefusedAt)
	require.Equal(t, []string{"eng"}, f.sessionGroups(t, token))
	require.Equal(t, []string{"eng"}, f.keyGroups(t, key))
}

// TestPastBoundSynchronousSuccessAdmitsWithNewGroups: the first request past
// the bound waits for the refresh and is admitted with the refreshed groups.
func TestPastBoundSynchronousSuccessAdmitsWithNewGroups(t *testing.T) {
	fake := newFakeRefresher("ops")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	key := f.cli(t, user.ID)

	f.clock.Advance(testMaxStale + time.Minute)
	require.Equal(t, []string{"ops"}, f.sessionGroups(t, token))
	require.Equal(t, 1, fake.Calls())
	audits := f.auditRows(t, ActionAuthGroupsRefreshed, user.ID)
	require.Len(t, audits, 1)
	require.Equal(t, "sync", audits[0]["trigger"])

	f.clock.Advance(testMaxStale + time.Minute)
	fake.set(func(f *fakeRefresher) { f.groups = []string{"ops", "payroll"} })
	require.Equal(t, []string{"ops", "payroll"}, f.keyGroups(t, key))
}

// TestNoGrantPastBoundIsRefusedWithoutAnAttempt: a user without a grant
// (SAML, LDAP before E1b, a cleared grant) cannot be refreshed, so the bound
// refuses it at once and only a login brings it back.
func TestNoGrantPastBoundIsRefusedWithoutAnAttempt(t *testing.T) {
	fake := newFakeRefresher("eng")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("saml-like", "", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)
	require.Zero(t, fake.Calls(), "nothing to redeem")

	f.clock.Advance(testMaxStale)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Zero(t, fake.Calls())
	require.NotNil(t, f.row(t, user.ID).StaleRefusedAt)
}

// TestPreE1UserFallsBackToLastLogin: a user row from before E1 has no
// GroupsRefreshedAt; its groups were confirmed at its last login.
func TestPreE1UserFallsBackToLastLogin(t *testing.T) {
	f := newFreshnessFixture(t, newFakeRefresher("eng"))
	user := f.login(t, oidcIdentity("legacy", "", "eng"))
	token := f.browser(t, user.ID)
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("groups_refreshed_at", nil).Error)
	f.sessionGroups(t, token)

	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Updates(map[string]any{"last_login_at": nil}).Error)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups, "with neither timestamp the groups are stale")
}

// TestDefinitiveRefusalRevokesSessionsAndKeys: invalid_grant on a certain
// grant revokes every session and user-bound key in one transaction, audits
// it, and the credentials then fail as revoked.
func TestDefinitiveRefusalRevokesSessionsAndKeys(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) { f.err = NewRefreshError(RefreshDefinitive, "invalid_grant", nil) })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	key := f.cli(t, user.ID)
	before := refreshCount(t, "oidc", refreshOutcomeDefinitive)

	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)

	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrSessionRevoked)
	_, _, err = f.keys.AuthenticateKey(t.Context(), key)
	require.ErrorIs(t, err, ErrKeyRevoked)
	require.Nil(t, f.row(t, user.ID).RefreshGrant)
	require.Equal(t, before+1, refreshCount(t, "oidc", refreshOutcomeDefinitive))
	audits := f.auditRows(t, ActionSessionsRevoked, user.ID)
	require.Len(t, audits, 1)
	require.Equal(t, SessionsRevokedReasonIdPRefreshDenied, audits[0]["reason"])
	require.EqualValues(t, 1, audits[0]["count"])
	require.EqualValues(t, 1, audits[0]["keys_revoked"])
}

// TestSynchronousDefinitiveRefusalRefusesTheWaitingCredential: the request
// that waited for a refresh the IdP refused re-validates its credential and
// finds it revoked.
func TestSynchronousDefinitiveRefusalRefusesTheWaitingCredential(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) { f.err = NewRefreshDenial(DenialOfSubject, "user_not_found") })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	key := f.cli(t, user.ID)

	f.clock.Advance(testMaxStale + time.Minute)
	_, _, err := f.keys.AuthenticateKey(t.Context(), key)
	require.ErrorIs(t, err, ErrKeyRevoked)
}

// TestAlternatingBrowserAndCLIRefreshWithRotation: one grant per user, shared
// by its session and its user-bound key; every refresh rotates it and the
// IdP refuses a stale copy, so alternating the two credentials must always
// redeem the latest grant and never revoke.
func TestAlternatingBrowserAndCLIRefreshWithRotation(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) { f.rotate = true; f.current = "token-1" })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	key := f.cli(t, user.ID)

	for i := range 6 {
		f.clock.Advance(20 * time.Minute)
		if i%2 == 0 {
			f.sessionGroups(t, token)
		} else {
			f.keyGroups(t, key)
		}
		f.waitIdle(t, user.ID)
		require.Equal(t, i+1, fake.Calls())
		fake.mu.Lock()
		current := fake.current
		fake.mu.Unlock()
		require.Equal(t, current, f.handleAt(t, user.ID), "the stored grant is the latest rotation")
	}
	// Past the bound, the synchronous path uses the same grant.
	f.clock.Advance(testMaxStale + time.Minute)
	require.Equal(t, []string{"eng"}, f.sessionGroups(t, token))
	require.EqualValues(t, 1, f.liveSessions(t, user.ID))
	_, _, err := f.keys.AuthenticateKey(t.Context(), key)
	require.NoError(t, err)
}

// TestDisableOrRevokeDuringSynchronousRefresh: a synchronous refresh that
// succeeds after an admin disabled the user or revoked the credential must
// not admit the waiting request.
func TestDisableOrRevokeDuringSynchronousRefresh(t *testing.T) {
	cases := map[string]struct {
		act  func(t *testing.T, f *freshnessFixture, userID uuid.UUID, sessionID uuid.UUID)
		want error
		key  bool
	}{
		// An admin disable revokes the credentials in the same transaction.
		"disable/session": {act: func(t *testing.T, f *freshnessFixture, id, _ uuid.UUID) {
			_, err := f.users.DisableUser(context.Background(), id)
			require.NoError(t, err)
		}, want: ErrSessionRevoked},
		"disable/key": {act: func(t *testing.T, f *freshnessFixture, id, _ uuid.UUID) {
			_, err := f.users.DisableUser(context.Background(), id)
			require.NoError(t, err)
		}, want: ErrKeyRevoked, key: true},
		// A disable whose cascade the credential never saw: the user row
		// alone refuses it.
		"disabled-row/session": {act: func(t *testing.T, f *freshnessFixture, id, _ uuid.UUID) {
			require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", id).Update("disabled_at", f.clock.Now()).Error)
		}, want: ErrUserDisabled},
		"disabled-row/key": {act: func(t *testing.T, f *freshnessFixture, id, _ uuid.UUID) {
			require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", id).Update("disabled_at", f.clock.Now()).Error)
		}, want: ErrUserDisabled, key: true},
		"revoke/session": {act: func(t *testing.T, f *freshnessFixture, _, sessionID uuid.UUID) {
			require.NoError(t, f.sessions.Revoke(context.Background(), sessionID))
		}, want: ErrSessionRevoked},
		"revoke/key": {act: func(t *testing.T, f *freshnessFixture, id, _ uuid.UUID) {
			require.NoError(t, f.db.Model(&models.APIKey{}).Where("user_id = ?", id).Update("revoked_at", f.clock.Now()).Error)
		}, want: ErrKeyRevoked, key: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fake := newFakeRefresher("eng", "ops")
			f := newFreshnessFixture(t, fake)
			user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
			token := f.browser(t, user.ID)
			key := f.cli(t, user.ID)
			sess, _, err := f.sessions.Validate(t.Context(), token)
			require.NoError(t, err)
			f.svc.beforeOutcome = func(id uuid.UUID) { tc.act(t, f, id, sess.ID) }

			f.clock.Advance(testMaxStale + time.Minute)
			if tc.key {
				_, _, err = f.keys.AuthenticateKey(t.Context(), key)
			} else {
				_, _, err = f.sessions.Validate(t.Context(), token)
			}
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// TestWaiterCancellationDoesNotLoseTheRotatedGrant: the request that started
// a synchronous refresh leaves; the redemption still completes on the
// service's context and its rotated grant and groups are stored.
func TestWaiterCancellationDoesNotLoseTheRotatedGrant(t *testing.T) {
	fake := newFakeRefresher("ops")
	gate := make(chan struct{})
	fake.set(func(f *fakeRefresher) { f.rotate = true; f.current = "token-1"; f.gate = gate })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(testMaxStale + time.Minute)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, _, err := f.sessions.Validate(ctx, token)
		done <- err
	}()
	require.Eventually(t, func() bool { return fake.Calls() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	close(gate)
	f.waitIdle(t, user.ID)

	require.Equal(t, []string{"ops"}, f.storedGroups(t, user.ID))
	require.Equal(t, "rotated-1", f.handleAt(t, user.ID))
	require.Nil(t, f.row(t, user.ID).StaleRefusedAt)
}

// TestShutdownDuringSynchronousRefreshDoesNotRefuse: a synchronous refresh
// that fails because the process is shutting down persists no refusal.
func TestShutdownDuringSynchronousRefreshDoesNotRefuse(t *testing.T) {
	fake := newFakeRefresher("ops")
	fake.set(func(f *fakeRefresher) { f.gate = make(chan struct{}) })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(testMaxStale + time.Minute)
	done := make(chan error, 1)
	go func() {
		_, _, err := f.sessions.Validate(context.Background(), token)
		done <- err
	}()
	require.Eventually(t, func() bool { return fake.Calls() == 1 }, 5*time.Second, 5*time.Millisecond)
	f.cancel()
	require.ErrorIs(t, <-done, ErrStaleGroups, "the waiting request is refused")
	f.svc.Wait()
	row := f.row(t, user.ID)
	require.Nil(t, row.StaleRefusedAt, "but the user is not marked refused")
	require.True(t, row.RefreshUncertain, "the interrupted redemption is recorded as uncertain")
}

// TestSynchronousWaitIsBoundedByTheTimeout: an IdP that never answers holds
// the request no longer than CAESIUM_AUTH_GROUP_REFRESH_TIMEOUT; the failed
// attempt then refuses the user.
func TestSynchronousWaitIsBoundedByTheTimeout(t *testing.T) {
	fake := newFakeRefresher("ops")
	fake.set(func(f *fakeRefresher) { f.gate = make(chan struct{}) })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(testMaxStale + time.Minute)
	started := time.Now()
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Less(t, time.Since(started), 2*time.Second+500*time.Millisecond, "held at most the timeout")
	require.Eventually(t, func() bool { return f.row(t, user.ID).StaleRefusedAt != nil }, 5*time.Second, 10*time.Millisecond)
	require.True(t, f.row(t, user.ID).RefreshUncertain)
}

// TestLateSuccessAfterTheTimeoutDoesNotLiftTheRefusal: the IdP answers but
// the outcome is recorded after the request's timeout. The request is
// refused and so is the user (persisted); the late success still stores the
// rotated grant, yet only a login lifts the refusal.
func TestLateSuccessAfterTheTimeoutDoesNotLiftTheRefusal(t *testing.T) {
	fake := newFakeRefresher("ops")
	fake.set(func(f *fakeRefresher) { f.rotate = true; f.current = "token-1" })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	release := make(chan struct{})
	f.svc.beforeOutcome = func(uuid.UUID) { <-release }

	f.clock.Advance(testMaxStale + time.Minute)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Eventually(t, func() bool { return f.row(t, user.ID).StaleRefusedAt != nil }, 5*time.Second, 10*time.Millisecond)
	close(release)
	f.waitIdle(t, user.ID)

	require.Equal(t, []string{"ops"}, f.storedGroups(t, user.ID), "the late outcome is recorded")
	require.Equal(t, "rotated-1", f.handleAt(t, user.ID), "with its rotated grant")
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups, "but the user stays refused until a login")
	f.login(t, oidcIdentity("ada", "token-2", "eng"))
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.NoError(t, err)
}

// TestExhaustedLeaseWaitRefuses: another node holds the lease for the whole
// synchronous budget; that is this request's failed attempt, and it is
// persisted, so a later release of the lease does not recover the user
// without a login.
func TestExhaustedLeaseWaitRefuses(t *testing.T) {
	fake := newFakeRefresher("ops")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	f.clock.Advance(testMaxStale + time.Minute)
	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))

	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Eventually(t, func() bool { return f.row(t, user.ID).StaleRefusedAt != nil }, 5*time.Second, 10*time.Millisecond)
	require.Zero(t, fake.Calls())

	applied, err := f.users.CompleteRefreshTransient(t.Context(), user.ID, 1, "node-b/1")
	require.NoError(t, err)
	require.True(t, applied)
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Zero(t, fake.Calls())
}

// TestFullQueueNeverFinishesAFlightARequestStarted: with the queue full, a
// request past the bound may start the very flight the overflow path is
// about to drop; only the flight's runner finishes it (no double close).
func TestFullQueueNeverFinishesAFlightARequestStarted(t *testing.T) {
	fake := newFakeRefresher("ops")
	gate := make(chan struct{})
	fake.set(func(f *fakeRefresher) { f.gate = gate })
	f := newFreshnessFixture(t, fake)
	f.svc, f.cancel = f.newService(t, "node-q", func(c *GroupRefreshConfig) { c.Workers, c.QueueSize = 1, 1 })
	f.sessions = f.sessionStore(f.svc)

	busy := f.login(t, oidcIdentity("busy", "token-1", "eng"))
	queued := f.login(t, oidcIdentity("queued", "token-1", "eng"))
	racer := f.login(t, oidcIdentity("racer", "token-1", "eng"))
	busyToken, queuedToken, racerToken := f.browser(t, busy.ID), f.browser(t, queued.ID), f.browser(t, racer.ID)
	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, busyToken)
	require.Eventually(t, func() bool { return fake.Calls() == 1 }, 5*time.Second, 5*time.Millisecond, "the only worker is busy")
	f.sessionGroups(t, queuedToken) // fills the queue

	var joined *refreshFlight
	f.svc.beforeEnqueue = func(id uuid.UUID) {
		if id == racer.ID {
			joined = f.svc.joinSync(id)
		}
	}
	f.sessionGroups(t, racerToken)
	require.NotNil(t, joined)
	close(gate)
	select {
	case <-joined.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the racing flight never finished")
	}
	f.waitIdle(t, queued.ID)
	require.Equal(t, []string{"ops"}, f.storedGroups(t, racer.ID))
}

// TestRotatedAnswerAtTheDeadlineIsStored: the IdP's rotated answer arrives as
// the redemption's deadline expires; the outcome is still recorded (on its
// own bounded context), so the rotated grant is never stranded.
func TestRotatedAnswerAtTheDeadlineIsStored(t *testing.T) {
	fake := newFakeRefresher("ops")
	fake.set(func(f *fakeRefresher) {
		f.rotate = true
		f.current = "token-1"
		f.gate = make(chan struct{})
		f.answerAtDeadline = true
	})
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)
	require.Equal(t, "rotated-1", f.handleAt(t, user.ID))
	require.Equal(t, []string{"ops"}, f.storedGroups(t, user.ID))
}

// TestLoginDuringRefreshDiscardsTheOutcome: a login that replaces the grant
// while a refresh is in flight wins; the refresh's outcome is discarded.
func TestLoginDuringRefreshDiscardsTheOutcome(t *testing.T) {
	fake := newFakeRefresher("stale-answer")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	before := refreshCount(t, "oidc", refreshOutcomeDiscardedCAS)
	f.svc.beforeOutcome = func(uuid.UUID) {
		_, err := f.users.Upsert(context.Background(), oidcIdentity("ada", "token-2", "fresh-login"), models.RoleViewer)
		require.NoError(t, err)
	}

	f.clock.Advance(20 * time.Minute)
	f.sessionGroups(t, token)
	f.waitIdle(t, user.ID)
	require.Equal(t, []string{"fresh-login"}, f.storedGroups(t, user.ID))
	require.Equal(t, "token-2", f.handleAt(t, user.ID))
	require.Equal(t, before+1, refreshCount(t, "oidc", refreshOutcomeDiscardedCAS))
}

// TestRefreshOutcomeLabels drives the remaining outcomes and checks their
// counters: uncertain, groups_unknown, unusable_grant and a provider that is
// not configured (transient, labelled by the grant's provider).
func TestRefreshOutcomeLabels(t *testing.T) {
	t.Run("uncertain", func(t *testing.T) {
		fake := newFakeRefresher("eng")
		fake.set(func(f *fakeRefresher) { f.err = NewRefreshError(RefreshUncertain, "timeout_after_send", nil) })
		f := newFreshnessFixture(t, fake)
		user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
		token := f.browser(t, user.ID)
		before := refreshCount(t, "oidc", refreshOutcomeUncertain)
		f.clock.Advance(20 * time.Minute)
		f.sessionGroups(t, token)
		f.waitIdle(t, user.ID)
		require.Equal(t, before+1, refreshCount(t, "oidc", refreshOutcomeUncertain))
		require.True(t, f.row(t, user.ID).RefreshUncertain)
	})
	t.Run("groups_unknown", func(t *testing.T) {
		fake := newFakeRefresher("ignored")
		fake.set(func(f *fakeRefresher) { f.known = false })
		f := newFreshnessFixture(t, fake)
		user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
		token := f.browser(t, user.ID)
		before := refreshCount(t, "oidc", refreshOutcomeGroupsUnknown)
		f.clock.Advance(20 * time.Minute)
		f.sessionGroups(t, token)
		f.waitIdle(t, user.ID)
		require.Equal(t, before+1, refreshCount(t, "oidc", refreshOutcomeGroupsUnknown))
		require.Equal(t, []string{"eng"}, f.storedGroups(t, user.ID))
		// Unknown groups never confirm them: past the bound, the user is
		// refused.
		f.clock.Advance(testMaxStale)
		_, _, err := f.sessions.Validate(t.Context(), token)
		require.ErrorIs(t, err, ErrStaleGroups)
	})
	t.Run("unusable_grant", func(t *testing.T) {
		fake := newFakeRefresher("eng")
		f := newFreshnessFixture(t, fake)
		user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
		token := f.browser(t, user.ID)
		require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("refresh_grant", []byte("tampered")).Error)
		before := refreshCount(t, providerOther, refreshOutcomeUnusableGrant)
		f.clock.Advance(20 * time.Minute)
		f.sessionGroups(t, token)
		f.waitIdle(t, user.ID)
		require.Equal(t, before+1, refreshCount(t, providerOther, refreshOutcomeUnusableGrant))
		require.Nil(t, f.row(t, user.ID).RefreshGrant)
		require.Zero(t, fake.Calls())
	})
	t.Run("provider_not_configured", func(t *testing.T) {
		fake := newFakeRefresher("eng")
		f := newFreshnessFixture(t, fake)
		ext := oidcIdentity("ldap-user", "uid=x,ou=people,dc=example,dc=org", "eng")
		ext.Issuer = "ldap"
		ext.Refresh.Provider, ext.Refresh.Issuer, ext.Refresh.Login = "ldap", "ldap", "x"
		user := f.login(t, ext)
		token := f.browser(t, user.ID)
		before := refreshCount(t, "ldap", refreshOutcomeTransient)
		f.clock.Advance(20 * time.Minute)
		f.sessionGroups(t, token)
		f.waitIdle(t, user.ID)
		require.Equal(t, before+1, refreshCount(t, "ldap", refreshOutcomeTransient))
		require.NotNil(t, f.row(t, user.ID).RefreshGrant, "a transient failure keeps the grant")
	})
}

// TestSynchronousRefreshWaitsForAnotherNodesLease: a request past the bound
// on node A that finds node B's live lease waits for it and is admitted with
// B's result instead of refusing.
func TestSynchronousRefreshWaitsForAnotherNodesLease(t *testing.T) {
	fake := newFakeRefresher("eng")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	f.clock.Advance(testMaxStale + time.Minute)

	require.True(t, f.claim(t, user.ID, 1, "node-b/1"))
	var released atomic.Bool
	go func() {
		time.Sleep(3 * staleLeasePoll)
		applied, err := f.users.CompleteRefreshSuccess(context.Background(), user.ID, 1, "node-b/1", []string{"from-b"}, nil, f.clock.Now())
		if err == nil && applied {
			released.Store(true)
		}
	}()
	require.Equal(t, []string{"from-b"}, f.sessionGroups(t, token))
	require.True(t, released.Load())
	require.Zero(t, fake.Calls())
	require.Nil(t, f.row(t, user.ID).StaleRefusedAt)
}

// TestMarkStaleRefusedIsFenced: the refusal is written only at the grant
// version its writer read, only once, and never on a disabled user.
func TestMarkStaleRefusedIsFenced(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	f.login(t, oidcIdentity("ada", "token-2", "eng")) // version 2

	marked, err := f.users.MarkStaleRefused(t.Context(), user.ID, 1, f.clock.Now())
	require.NoError(t, err)
	require.False(t, marked, "a login since the read wins")
	marked, err = f.users.MarkStaleRefused(t.Context(), user.ID, 2, f.clock.Now())
	require.NoError(t, err)
	require.True(t, marked)
	marked, err = f.users.MarkStaleRefused(t.Context(), user.ID, 2, f.clock.Now())
	require.NoError(t, err)
	require.False(t, marked, "already refused")

	other := f.login(t, oidcIdentity("bo", "token-1", "eng"))
	_, err = f.users.DisableUser(t.Context(), other.ID)
	require.NoError(t, err)
	marked, err = f.users.MarkStaleRefused(t.Context(), other.ID, f.row(t, other.ID).RefreshGrantVersion, f.clock.Now())
	require.NoError(t, err)
	require.False(t, marked)
}

func TestGroupRefreshConfigValidation(t *testing.T) {
	db := newRefreshFixture(t).db
	for _, cfg := range []GroupRefreshConfig{
		{Interval: 0, MaxStaleness: time.Hour, Timeout: time.Second},
		{Interval: time.Hour, MaxStaleness: time.Minute, Timeout: time.Second},
		{Interval: time.Hour, MaxStaleness: time.Hour, Timeout: 0},
	} {
		_, err := NewGroupRefreshService(t.Context(), db, cfg)
		require.Error(t, err)
	}
}

func TestSAMLSessionAbsoluteTTLOverridesTheGeneralOne(t *testing.T) {
	f := newRefreshFixture(t)
	user := f.login(t, oidcIdentity("ada", "", "eng"))
	store := NewSessionStore(f.db, WithSessionNow(f.clock.Now), WithSessionTTLs(time.Hour, 24*time.Hour), WithSAMLSessionAbsoluteTTL(8*time.Hour))
	_, saml, err := store.Create(t.Context(), CreateSessionRequest{UserID: user.ID, AuthMethod: "saml"})
	require.NoError(t, err)
	require.Equal(t, f.clock.Now().Add(8*time.Hour), saml.AbsoluteExpiresAt)
	_, oidc, err := store.Create(t.Context(), CreateSessionRequest{UserID: user.ID, AuthMethod: "oidc"})
	require.NoError(t, err)
	require.Equal(t, f.clock.Now().Add(24*time.Hour), oidc.AbsoluteExpiresAt)
}

func TestGroupDelta(t *testing.T) {
	added, removed := groupDelta([]string{"b", "a", "a"}, []string{"c", "b", "c"})
	require.Equal(t, []string{"c"}, added)
	require.Equal(t, []string{"a"}, removed)
	added, removed = groupDelta(nil, nil)
	require.NotNil(t, added)
	require.NotNil(t, removed)
}

// cancelAfterSyncedAdmit runs the real Admit, then revokes the credential and
// cancels the request before the caller's re-check: the window in which a
// successful synchronous refresh used to skip that re-check.
type cancelAfterSyncedAdmit struct {
	inner  GroupFreshness
	act    func()
	cancel context.CancelFunc
}

func (c *cancelAfterSyncedAdmit) Admit(ctx context.Context, user *models.User) (*models.User, bool, error) {
	admitted, synced, err := c.inner.Admit(ctx, user)
	c.act()
	c.cancel()
	return admitted, synced, err
}

// TestCancelledRequestAfterSynchronousSuccessIsRefused: a request cancelled
// after a successful synchronous refresh cannot re-check its credential, so
// it is refused (here the credential was revoked meanwhile), never admitted.
func TestCancelledRequestAfterSynchronousSuccessIsRefused(t *testing.T) {
	for _, key := range []bool{false, true} {
		t.Run(map[bool]string{false: "session", true: "key"}[key], func(t *testing.T) {
			fake := newFakeRefresher("eng", "ops")
			f := newFreshnessFixture(t, fake)
			user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
			token := f.browser(t, user.ID)
			plaintext := f.cli(t, user.ID)
			sess, _, err := f.sessions.Validate(t.Context(), token)
			require.NoError(t, err)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			stub := &cancelAfterSyncedAdmit{inner: f.svc, cancel: cancel, act: func() {
				if key {
					require.NoError(t, f.db.Model(&models.APIKey{}).Where("user_id = ?", user.ID).Update("revoked_at", f.clock.Now()).Error)
				} else {
					require.NoError(t, f.sessions.Revoke(context.Background(), sess.ID))
				}
			}}
			f.clock.Advance(testMaxStale + time.Minute)
			if key {
				keys := NewService(f.db, WithKeyHashSecret(katSecret), WithNow(f.clock.Now),
					WithUserKeyTTL(MaxUserKeyTTL), WithGroupFreshness(stub))
				p, _, err := keys.AuthenticateKey(ctx, plaintext)
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, p)
			} else {
				s, u, err := f.sessionStore(stub).Validate(ctx, token)
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, s)
				require.Nil(t, u)
			}
			require.Equal(t, 1, fake.Calls(), "the synchronous refresh ran and succeeded")
		})
	}
}

// TestBlockedAuditWriteIsBoundedByThePersistenceBudget: the audit row a
// refresh writes neither delays the request the refresh admits (the groups
// are committed before it is written) nor outlives its own bounded budget,
// so a stuck audit insert cannot block Wait.
func TestBlockedAuditWriteIsBoundedByThePersistenceBudget(t *testing.T) {
	fake := newFakeRefresher("eng", "ops")
	f := newFreshnessFixture(t, fake)
	svc, cancel := f.newService(t, "node-b", func(c *GroupRefreshConfig) { c.Timeout = 300 * time.Millisecond })
	sessions := f.sessionStore(svc)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var blocked atomic.Int32
	// Block before the insert's transaction begins, so the stalled audit
	// write holds no connection (the test database has exactly one).
	require.NoError(t, f.db.Callback().Create().Before("gorm:begin_transaction").Register("test:block-audit", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil || tx.Statement.Schema.Name != "AuditLog" {
			return
		}
		blocked.Add(1)
		select {
		case <-tx.Statement.Context.Done():
			_ = tx.AddError(tx.Statement.Context.Err())
		case <-release:
		}
	}))

	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token, _, err := sessions.Create(t.Context(), CreateSessionRequest{UserID: user.ID, AuthMethod: "oidc", DisableGeneration: f.row(t, user.ID).DisableGeneration})
	require.NoError(t, err)
	f.clock.Advance(testMaxStale + time.Minute)
	_, admitted, err := sessions.Validate(t.Context(), token)
	require.NoError(t, err, "a successful synchronous refresh admits even while its audit write stalls")
	var groups []string
	require.NoError(t, json.Unmarshal(admitted.Groups, &groups))
	require.ElementsMatch(t, []string{"eng", "ops"}, groups)
	require.Eventually(t, func() bool { return blocked.Load() == 1 }, 5*time.Second, 5*time.Millisecond, "the groups changed, so the refresh audits")

	cancel()
	waited := make(chan struct{})
	go func() { svc.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked audit write outlived its persistence budget")
	}
}

// TestQueuedRefreshSkipsGroupsALoginConfirmed: a refresh queued from a row
// read before a re-login must not redeem the login's new grant (whose
// definitive refusal would revoke credentials while the login still mints
// its session); the login already confirmed the groups.
func TestQueuedRefreshSkipsGroupsALoginConfirmed(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) { f.err = NewRefreshError(RefreshDefinitive, "invalid_grant", nil) })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)

	f.clock.Advance(testInterval + time.Minute)
	before := f.row(t, user.ID) // read by a request before the re-login
	f.login(t, oidcIdentity("ada", "token-2", "eng"))

	_, synced, err := f.svc.Admit(t.Context(), &before)
	require.NoError(t, err)
	require.False(t, synced)
	require.Eventually(t, func() bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		_, running := f.svc.flights[user.ID]
		return !running
	}, 5*time.Second, 5*time.Millisecond, "the queued flight ran")

	require.Zero(t, fake.Calls(), "fresh groups are not redeemed")
	require.NotEmpty(t, f.row(t, user.ID).RefreshGrant, "the login's grant is kept")
	_, _, err = f.sessionStore(nil).Validate(t.Context(), token)
	require.NoError(t, err, "nothing was revoked")
}

// TestUnconcludedSynchronousAttemptsPersistTheRefusal: when both synchronous
// flights end without a conclusion (here every lease claim fails), the
// request's attempt failed: the refusal is persisted, so recovery of the
// catalog does not restore access without a login.
func TestUnconcludedSynchronousAttemptsPersistTheRefusal(t *testing.T) {
	fake := newFakeRefresher("eng")
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	var failClaims atomic.Bool
	failClaims.Store(true)
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register("test:fail-claims", func(tx *gorm.DB) {
		if !failClaims.Load() {
			return
		}
		if m, ok := tx.Statement.Dest.(map[string]any); ok {
			if owner, ok := m["refresh_lease_owner"].(string); ok && owner != "" {
				_ = tx.AddError(errors.New("catalog refused the lease claim"))
			}
		}
	}))

	f.clock.Advance(testMaxStale + time.Minute)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.Zero(t, fake.Calls())
	require.NotNil(t, f.row(t, user.ID).StaleRefusedAt, "the failed attempt is persisted before the answer")

	failClaims.Store(false)
	_, _, err = f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups, "recovery does not lift the refusal; only a login does")
	require.Zero(t, fake.Calls())
}

// TestRefusalSurvivesALostVersionFence: another node advancing the grant
// version without refreshing the groups (here: between the re-read and the
// fenced write) must not make the refusal disappear; it is retried on the
// re-read row while the user stays stale.
func TestRefusalSurvivesALostVersionFence(t *testing.T) {
	fake := newFakeRefresher("eng")
	fake.set(func(f *fakeRefresher) { f.err = NewRefreshError(RefreshTransient, "idp_unavailable", nil) })
	f := newFreshnessFixture(t, fake)
	user := f.login(t, oidcIdentity("ada", "token-1", "eng"))
	token := f.browser(t, user.ID)
	var bumps atomic.Int32
	f.svc.beforeMarkStale = func(id uuid.UUID) {
		if bumps.Add(1) == 1 {
			// As CompleteRefreshGroupsUnknown on another node: the version
			// moves, the groups stay stale.
			require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", id).
				Update("refresh_grant_version", gorm.Expr("COALESCE(refresh_grant_version, 0) + 1")).Error)
		}
	}

	f.clock.Advance(testMaxStale + time.Minute)
	_, _, err := f.sessions.Validate(t.Context(), token)
	require.ErrorIs(t, err, ErrStaleGroups)
	require.EqualValues(t, 2, bumps.Load(), "the lost fence was retried on the re-read row")
	require.NotNil(t, f.row(t, user.ID).StaleRefusedAt, "the refusal is persisted despite the lost fence")
}
