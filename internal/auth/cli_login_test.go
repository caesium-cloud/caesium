package auth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// RFC 7636 Appendix B: the verifier and its S256 challenge.
const (
	rfc7636Verifier  = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	rfc7636Challenge = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
)

func TestS256ChallengeMatchesRFC7636AppendixB(t *testing.T) {
	require.Equal(t, rfc7636Challenge, S256Challenge(rfc7636Verifier))
	require.True(t, ValidCLILoginVerifier(rfc7636Verifier))
	require.False(t, ValidCLILoginVerifier(strings.Repeat("a", 42)), "shorter than 43")
	require.False(t, ValidCLILoginVerifier(strings.Repeat("a", 129)), "longer than 128")
	require.False(t, ValidCLILoginVerifier(strings.Repeat("a", 42)+"!"), "outside the unreserved set")
}

type cliLoginFixture struct {
	*userKeyFixture
	n          int
	sessionIDs map[uuid.UUID]uuid.UUID
}

func newCLILoginFixture(t *testing.T) *cliLoginFixture {
	return &cliLoginFixture{userKeyFixture: newUserKeyFixture(t)}
}

// token returns a distinct valid 43-character state.
func (f *cliLoginFixture) token() string {
	f.n++
	s := strings.Repeat("s", 40) + string(rune('A'+f.n/26)) + string(rune('a'+f.n%26)) + "Z"
	return s
}

func (f *cliLoginFixture) start(t *testing.T, ip string) string {
	t.Helper()
	state := f.token()
	_, err := f.svc.StartCLILogin(t.Context(), StartCLILoginRequest{
		State: state, Challenge: rfc7636Challenge, Port: 40123, HostnameHint: "laptop", SourceIP: ip,
	})
	require.NoError(t, err)
	return state
}

func (f *cliLoginFixture) authorize(t *testing.T, state string, u *models.User) (*AuthorizeCLILoginResult, error) {
	t.Helper()
	return f.svc.AuthorizeCLILogin(t.Context(), AuthorizeCLILoginRequest{
		State: state, UserID: u.ID, DisableGeneration: f.row(t, u.ID).DisableGeneration,
		SessionID: f.session(t, u), Email: u.Email, SourceIP: "10.0.0.1",
	})
}

// session returns a live session of u (one per user, minted on first use).
func (f *cliLoginFixture) session(t *testing.T, u *models.User) uuid.UUID {
	t.Helper()
	if f.sessionIDs == nil {
		f.sessionIDs = map[uuid.UUID]uuid.UUID{}
	}
	if id, ok := f.sessionIDs[u.ID]; ok {
		return id
	}
	_, sess, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: u.ID, DisableGeneration: f.row(t, u.ID).DisableGeneration})
	require.NoError(t, err)
	f.sessionIDs[u.ID] = sess.ID
	return sess.ID
}

func (f *cliLoginFixture) exchange(code, verifier string) (*ExchangeCLILoginResult, error) {
	return f.svc.ExchangeCLILogin(context.Background(), ExchangeCLILoginRequest{Code: code, Verifier: verifier, SourceIP: "10.0.0.1"})
}

func (f *cliLoginFixture) txn(t *testing.T, state string) models.CLILoginTransaction {
	t.Helper()
	hash, err := f.svc.cliLoginHash("state", state)
	require.NoError(t, err)
	var row models.CLILoginTransaction
	require.NoError(t, f.db.Where("state_hash = ?", hash).Take(&row).Error)
	return row
}

func (f *cliLoginFixture) userKeys(t *testing.T, userID uuid.UUID) []models.APIKey {
	t.Helper()
	var keys []models.APIKey
	require.NoError(t, f.db.Where("user_id = ?", userID).Find(&keys).Error)
	return keys
}

func (f *cliLoginFixture) audit(t *testing.T, action string) []models.AuditLog {
	t.Helper()
	var rows []models.AuditLog
	require.NoError(t, f.db.Where("action = ?", action).Order("timestamp").Find(&rows).Error)
	return rows
}

func TestCLILoginLifecycleMintsOnlyAtExchange(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "10.0.0.1")

	pending, err := f.svc.LookupCLILogin(t.Context(), state)
	require.NoError(t, err)
	require.Equal(t, models.CLILoginModeLoopback, pending.Mode)
	require.Equal(t, 40123, pending.Port)
	require.Equal(t, "laptop", pending.HostnameHint)

	res, err := f.authorize(t, state, user)
	require.NoError(t, err)
	require.Len(t, res.Code, 43)
	require.Equal(t, f.clock.Now().Add(CLILoginCodeTTL), res.RedeemBy)
	require.Empty(t, f.userKeys(t, user.ID), "authorize mints nothing")

	row := f.txn(t, state)
	require.Equal(t, models.CLILoginAuthorized, row.Status)
	require.Equal(t, user.ID, *row.UserID)
	raw, err := json.Marshal(row)
	require.NoError(t, err)
	for _, secret := range []string{state, res.Code, rfc7636Verifier} {
		require.NotContains(t, string(raw), secret)
		require.NotContains(t, row.StateHash+*row.CodeHash+row.Challenge, secret, "no plaintext secret at rest")
	}

	_, err = f.svc.LookupCLILogin(t.Context(), state)
	require.ErrorIs(t, err, ErrCLILoginConsumed, "the confirmation page refuses a used state")
	_, err = f.authorize(t, state, user)
	require.ErrorIs(t, err, ErrCLILoginConsumed, "a second confirmation is a conflict")

	out, err := f.exchange(res.Code, rfc7636Verifier)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(out.Key.Plaintext, KeyPrefixUser))
	require.Equal(t, user.Email, out.Subject)
	keys := f.userKeys(t, user.ID)
	require.Len(t, keys, 1)
	require.Equal(t, out.Key.Key.ID, keys[0].ID)
	require.Equal(t, user.Email, keys[0].CreatedBy)
	require.Equal(t, "caesium login on laptop", keys[0].Description)

	key, principal, err := f.svc.AuthenticateKey(t.Context(), out.Key.Plaintext)
	require.NoError(t, err)
	require.Equal(t, out.Key.Key.ID, key.ID)
	require.Equal(t, PrincipalUser, principal.Kind)

	row = f.txn(t, state)
	require.Equal(t, models.CLILoginExchanged, row.Status)
	require.Equal(t, out.Key.Key.ID, *row.KeyID)

	_, err = f.exchange(res.Code, rfc7636Verifier)
	require.ErrorIs(t, err, ErrCLILoginCodeInvalid, "a code is redeemed once")
	require.Len(t, f.userKeys(t, user.ID), 1)

	authorized := f.audit(t, ActionAuthCLIAuthorize)
	require.Len(t, authorized, 1)
	require.Equal(t, user.Email, authorized[0].Actor)
	logins := f.audit(t, ActionAuthCLILogin)
	require.Len(t, logins, 1)
	require.Equal(t, OutcomeSuccess, logins[0].Outcome)
	require.Equal(t, out.Key.Key.ID.String(), logins[0].ResourceID)
}

func TestCLILoginManualMode(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.token()
	_, err := f.svc.StartCLILogin(t.Context(), StartCLILoginRequest{State: state, Challenge: rfc7636Challenge, Manual: true})
	require.NoError(t, err)
	res, err := f.authorize(t, state, user)
	require.NoError(t, err)
	require.Equal(t, models.CLILoginModeManual, res.Mode)
	require.Zero(t, res.Port)
}

func TestCLILoginStartValidation(t *testing.T) {
	f := newCLILoginFixture(t)
	for name, req := range map[string]StartCLILoginRequest{
		"port below 1024":     {State: f.token(), Challenge: rfc7636Challenge, Port: 1023},
		"port above 65535":    {State: f.token(), Challenge: rfc7636Challenge, Port: 65536},
		"no port, not manual": {State: f.token(), Challenge: rfc7636Challenge},
		"port and manual":     {State: f.token(), Challenge: rfc7636Challenge, Port: 4000, Manual: true},
		"short state":         {State: "abc", Challenge: rfc7636Challenge, Port: 4000},
		"bad challenge":       {State: f.token(), Challenge: rfc7636Challenge + "x", Port: 4000},
		"state charset":       {State: strings.Repeat("a", 42) + "=", Challenge: rfc7636Challenge, Port: 4000},
	} {
		_, err := f.svc.StartCLILogin(t.Context(), req)
		require.ErrorIs(t, err, ErrCLILoginInvalid, name)
	}
	state := f.start(t, "10.0.0.1")
	_, err := f.svc.StartCLILogin(t.Context(), StartCLILoginRequest{State: state, Challenge: rfc7636Challenge, Port: 4000})
	require.ErrorIs(t, err, ErrCLILoginStateInUse)

	for _, hint := range []string{"bad\x00hint", strings.Repeat("h", 65)} {
		require.Empty(t, SanitizeCLIHostnameHint(hint))
	}
	require.Equal(t, "<b>host</b>", SanitizeCLIHostnameHint(" <b>host</b> "), "kept verbatim; the page escapes it")
}

func TestCLILoginPendingCapPerIP(t *testing.T) {
	f := newCLILoginFixture(t)
	for i := 0; i < CLILoginMaxPendingPerIP; i++ {
		f.start(t, "10.0.0.9")
	}
	_, err := f.svc.StartCLILogin(t.Context(), StartCLILoginRequest{State: f.token(), Challenge: rfc7636Challenge, Port: 4000, SourceIP: "10.0.0.9"})
	require.ErrorIs(t, err, ErrCLILoginTooMany)
	f.start(t, "10.0.0.10")

	f.clock.Advance(CLILoginTransactionTTL)
	f.start(t, "10.0.0.9")
	var remaining int64
	require.NoError(t, f.db.Model(&models.CLILoginTransaction{}).Count(&remaining).Error)
	require.EqualValues(t, 1, remaining, "the start swept every row past its deadline")
}

func TestCLILoginPendingCapHoldsUnderConcurrency(t *testing.T) {
	f := newCLILoginFixture(t)
	states := make([]string, 3*CLILoginMaxPendingPerIP)
	for i := range states {
		states[i] = f.token()
	}
	var wg sync.WaitGroup
	errs := make([]error, len(states))
	for i, state := range states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.svc.StartCLILogin(context.Background(), StartCLILoginRequest{State: state, Challenge: rfc7636Challenge, Port: 4000, SourceIP: "10.1.1.1"})
		}()
	}
	wg.Wait()
	admitted := 0
	for _, err := range errs {
		if err == nil {
			admitted++
		} else {
			require.ErrorIs(t, err, ErrCLILoginTooMany)
		}
	}
	require.Equal(t, CLILoginMaxPendingPerIP, admitted)
}

func TestCLILoginDeadlines(t *testing.T) {
	t.Run("transaction expires before confirmation", func(t *testing.T) {
		f := newCLILoginFixture(t)
		user := f.user(t, "mia", "marketing-eng")
		state := f.start(t, "")
		f.clock.Advance(CLILoginTransactionTTL)
		_, err := f.svc.LookupCLILogin(t.Context(), state)
		require.ErrorIs(t, err, ErrCLILoginExpired)
		_, err = f.authorize(t, state, user)
		require.ErrorIs(t, err, ErrCLILoginExpired)
	})
	t.Run("code expires 60s after confirmation", func(t *testing.T) {
		f := newCLILoginFixture(t)
		user := f.user(t, "mia", "marketing-eng")
		res, err := f.authorize(t, f.start(t, ""), user)
		require.NoError(t, err)
		f.clock.Advance(CLILoginCodeTTL)
		_, err = f.exchange(res.Code, rfc7636Verifier)
		require.ErrorIs(t, err, ErrCLILoginCodeInvalid)
		require.Empty(t, f.userKeys(t, user.ID))
	})
	t.Run("confirmation near the transaction expiry keeps its deadline", func(t *testing.T) {
		f := newCLILoginFixture(t)
		user := f.user(t, "mia", "marketing-eng")
		state := f.start(t, "")
		expiry := f.txn(t, state).ExpiresAt
		f.clock.Advance(CLILoginTransactionTTL - 10*time.Second)
		res, err := f.authorize(t, state, user)
		require.NoError(t, err)
		require.True(t, res.RedeemBy.Equal(expiry), "the code's deadline is min(expiry, authorize+60s)")
		f.clock.Advance(10 * time.Second)
		_, err = f.exchange(res.Code, rfc7636Verifier)
		require.ErrorIs(t, err, ErrCLILoginCodeInvalid, "the code dies with its transaction")
		require.Empty(t, f.userKeys(t, user.ID))
	})
	t.Run("a code redeemed within both deadlines mints", func(t *testing.T) {
		f := newCLILoginFixture(t)
		user := f.user(t, "mia", "marketing-eng")
		res, err := f.authorize(t, f.start(t, ""), user)
		require.NoError(t, err)
		f.clock.Advance(CLILoginCodeTTL - time.Second)
		_, err = f.exchange(res.Code, rfc7636Verifier)
		require.NoError(t, err)
	})
}

func TestCLILoginWrongVerifierBurnsTheCode(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "")
	res, err := f.authorize(t, state, user)
	require.NoError(t, err)

	_, err = f.exchange(res.Code, strings.Repeat("w", 43))
	require.ErrorIs(t, err, ErrCLILoginCodeInvalid)
	refusal, ok := err.(*CLILoginRefusal)
	require.True(t, ok)
	require.Equal(t, "verifier_mismatch", refusal.Reason)
	require.True(t, refusal.Audited)
	require.Equal(t, models.CLILoginFailed, f.txn(t, state).Status)

	_, err = f.exchange(res.Code, rfc7636Verifier)
	require.ErrorIs(t, err, ErrCLILoginCodeInvalid, "the right verifier cannot redeem a burned code")
	require.Empty(t, f.userKeys(t, user.ID), "nothing was minted")
	denied := f.audit(t, ActionAuthCLILogin)
	require.Len(t, denied, 1)
	require.Equal(t, OutcomeDenied, denied[0].Outcome)
}

func TestCLILoginExchangeRefusesUnknownAndMalformed(t *testing.T) {
	f := newCLILoginFixture(t)
	for _, tc := range []struct{ code, verifier, reason string }{
		{strings.Repeat("c", 43), rfc7636Verifier, "unknown"},
		{"short", rfc7636Verifier, "malformed"},
		{strings.Repeat("c", 43), "short", "malformed"},
	} {
		_, err := f.exchange(tc.code, tc.verifier)
		require.ErrorIs(t, err, ErrCLILoginCodeInvalid)
		refusal, ok := err.(*CLILoginRefusal)
		require.True(t, ok)
		require.Equal(t, tc.reason, refusal.Reason)
		require.False(t, refusal.Audited)
	}
}

func TestCLILoginDisableBetweenConfirmationAndExchangeRefusesTheMint(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	res, err := f.authorize(t, f.start(t, ""), user)
	require.NoError(t, err)

	admin := NewUserAdmin(f.sessions)
	_, err = admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)
	_, err = admin.EnableUser(t.Context(), user.ID)
	require.NoError(t, err)

	_, err = f.exchange(res.Code, rfc7636Verifier)
	require.ErrorIs(t, err, ErrCLILoginCodeInvalid, "the recorded generation fences the mint")
	require.Empty(t, f.userKeys(t, user.ID))
}

// Log out everywhere (and a definitive refresh denial, which shares
// revokeUserCredentialsTx) advances no disable generation, so the generation
// fence alone would let a code confirmed before it mint a fresh key after it.
func TestCLILoginLogOutEverywhereVoidsConfirmedCodes(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "")
	res, err := f.authorize(t, state, user)
	require.NoError(t, err)
	pending := f.start(t, "")

	_, err = NewUserAdmin(f.sessions).RevokeUserSessions(t.Context(), user.ID)
	require.NoError(t, err)
	require.Equal(t, models.CLILoginFailed, f.txn(t, state).Status)
	require.Equal(t, models.CLILoginPending, f.txn(t, pending).Status, "an unconfirmed login names no user yet")

	_, err = f.exchange(res.Code, rfc7636Verifier)
	require.ErrorIs(t, err, ErrCLILoginCodeInvalid)
	require.Empty(t, f.userKeys(t, user.ID), "no key after log out everywhere")
}

func TestCLILoginAuthorizeFencesTheUser(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "")
	_, err := f.svc.AuthorizeCLILogin(t.Context(), AuthorizeCLILoginRequest{
		State: state, UserID: user.ID, DisableGeneration: f.row(t, user.ID).DisableGeneration + 1, Email: user.Email,
	})
	require.ErrorIs(t, err, ErrUserChanged)
	require.Equal(t, models.CLILoginPending, f.txn(t, state).Status, "a refused confirmation consumes nothing")
	_, err = f.svc.AuthorizeCLILogin(t.Context(), AuthorizeCLILoginRequest{State: f.token(), UserID: user.ID, SessionID: f.session(t, user)})
	require.ErrorIs(t, err, ErrCLILoginNotFound)
}

// The confirmation re-checks its session inside the transaction: a session
// revoked after the request was authenticated (log out everywhere landing
// while the body is still in flight) cannot confirm.
func TestCLILoginAuthorizeRequiresTheSessionStillLive(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	other := f.user(t, "fin", "finance-eng")
	state := f.start(t, "")
	req := func(session uuid.UUID) error {
		_, err := f.svc.AuthorizeCLILogin(t.Context(), AuthorizeCLILoginRequest{
			State: state, UserID: user.ID, DisableGeneration: f.row(t, user.ID).DisableGeneration, SessionID: session, Email: user.Email,
		})
		return err
	}
	require.ErrorIs(t, req(uuid.Nil), ErrSessionInvalid)
	require.ErrorIs(t, req(uuid.New()), ErrSessionInvalid)
	require.ErrorIs(t, req(f.session(t, other)), ErrSessionInvalid, "another user's session")

	live := f.session(t, user)
	_, err := NewUserAdmin(f.sessions).RevokeUserSessions(t.Context(), user.ID)
	require.NoError(t, err)
	require.ErrorIs(t, req(live), ErrSessionRevoked)
	require.Equal(t, models.CLILoginPending, f.txn(t, state).Status)
}

func TestCLILoginConcurrentConfirmationsAndExchanges(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "")
	gen := f.row(t, user.ID).DisableGeneration
	sessionID := f.session(t, user)

	var wg sync.WaitGroup
	results := make([]*AuthorizeCLILoginResult, 8)
	errs := make([]error, 8)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = f.svc.AuthorizeCLILogin(context.Background(), AuthorizeCLILoginRequest{State: state, UserID: user.ID, DisableGeneration: gen, SessionID: sessionID, Email: user.Email})
		}()
	}
	wg.Wait()
	var code string
	for i, err := range errs {
		if err == nil {
			require.Empty(t, code, "exactly one confirmation wins")
			code = results[i].Code
		} else {
			require.ErrorIs(t, err, ErrCLILoginConsumed)
		}
	}
	require.NotEmpty(t, code)

	minted := make([]error, 8)
	for i := range minted {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, minted[i] = f.exchange(code, rfc7636Verifier)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range minted {
		if err == nil {
			wins++
		} else {
			require.ErrorIs(t, err, ErrCLILoginCodeInvalid)
		}
	}
	require.Equal(t, 1, wins, "exactly one exchange mints")
	require.Len(t, f.userKeys(t, user.ID), 1)
}

func TestCLILoginExchangeRollsBackTheMintWhenTheAuditWriteFails(t *testing.T) {
	f := newCLILoginFixture(t)
	user := f.user(t, "mia", "marketing-eng")
	state := f.start(t, "")
	res, err := f.authorize(t, state, user)
	require.NoError(t, err)

	require.NoError(t, f.db.Exec("DROP TABLE audit_logs").Error)
	_, err = f.exchange(res.Code, rfc7636Verifier)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrCLILoginCodeInvalid)
	require.Empty(t, f.userKeys(t, user.ID), "the mint rolled back with the audit row")
	require.Equal(t, models.CLILoginAuthorized, f.txn(t, state).Status, "the code is still redeemable")
}

func TestRevokeOwnUserKeyRevokesOnlyThatKeyOfThatUser(t *testing.T) {
	f := newCLILoginFixture(t)
	mia := f.user(t, "mia", "marketing-eng")
	fin := f.user(t, "fin", "finance-eng")
	k1, k2, other := f.mint(t, mia), f.mint(t, mia), f.mint(t, fin)

	require.NoError(t, f.svc.RevokeOwnUserKey(t.Context(), other.Key.ID, mia.ID))
	require.Nil(t, f.keyRow(t, other.Key.ID).RevokedAt, "a key of another user is untouched")

	require.NoError(t, f.svc.RevokeOwnUserKey(t.Context(), k1.Key.ID, mia.ID))
	require.NotNil(t, f.keyRow(t, k1.Key.ID).RevokedAt)
	require.Nil(t, f.keyRow(t, k2.Key.ID).RevokedAt)
	require.NoError(t, f.svc.RevokeOwnUserKey(t.Context(), k1.Key.ID, mia.ID), "idempotent")
	require.ErrorIs(t, f.svc.RevokeOwnUserKey(t.Context(), uuid.Nil, mia.ID), ErrKeyNotFound)
}
