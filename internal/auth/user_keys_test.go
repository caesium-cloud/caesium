package auth

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// userKeyFixture is the refresh fixture plus a key service on the same
// catalog and clock, resolving grants from a policy that binds marketing-eng
// to operator in marketing and finance-eng to operator in finance.
type userKeyFixture struct {
	*refreshFixture
	svc    *Service
	sleeps []time.Duration
	mu     sync.Mutex
}

const userKeyTestPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing: {}
  finance: {}
bindings:
  - subjects: {groups: [marketing-eng]}
    role: operator
    namespaces: [marketing]
  - subjects: {groups: [finance-eng]}
    role: operator
    namespaces: [finance]
  - subjects: {users: [boss@example.com]}
    role: admin
    namespaces: ["*"]
`

func newUserKeyFixture(t *testing.T, opts ...ServiceOption) *userKeyFixture {
	t.Helper()
	f := &userKeyFixture{refreshFixture: newRefreshFixture(t)}
	parsed, err := policy.Parse([]byte(userKeyTestPolicy))
	require.NoError(t, err)
	base := []ServiceOption{
		WithKeyHashSecret("user-key-test-secret"),
		WithPolicySource(&policy.Static{Policy: parsed}),
		WithNow(f.clock.Now),
		WithSleep(func(d time.Duration) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.sleeps = append(f.sleeps, d)
		}),
	}
	f.svc = NewService(f.db, append(base, opts...)...)
	return f
}

func (f *userKeyFixture) sleepCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sleeps)
}

// user upserts an OIDC user with groups and a verified email.
func (f *userKeyFixture) user(t *testing.T, name string, groups ...string) *models.User {
	t.Helper()
	ext := oidcIdentity(name, "", groups...)
	ext.EmailVerified = true
	return f.login(t, ext)
}

func (f *userKeyFixture) mint(t *testing.T, u *models.User) *CreateKeyResponse {
	t.Helper()
	resp, err := f.svc.MintUserBoundKey(t.Context(), MintUserKeyRequest{
		UserID:            u.ID,
		DisableGeneration: f.row(t, u.ID).DisableGeneration,
		Description:       "caesium login on test-host",
		CreatedBy:         u.Email,
	})
	require.NoError(t, err)
	return resp
}

func (f *userKeyFixture) keyRow(t *testing.T, id uuid.UUID) models.APIKey {
	t.Helper()
	var key models.APIKey
	require.NoError(t, f.db.Where("id = ?", id).Take(&key).Error)
	return key
}

func (f *userKeyFixture) recordedLastUse(id uuid.UUID) bool {
	f.svc.lastUsedMu.Lock()
	defer f.svc.lastUsedMu.Unlock()
	_, ok := f.svc.lastUsed[id]
	return ok
}

func TestMintUserBoundKeyStoresNoGrantAndAlwaysExpires(t *testing.T) {
	require.Equal(t, env.MaxAuthCLIKeyTTL, MaxUserKeyTTL, "the env bound and the mint bound are the same")

	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"default", 0, DefaultUserKeyTTL},
		{"negative is the default", -time.Hour, DefaultUserKeyTTL},
		{"configured", time.Hour, time.Hour},
		{"clamped to seven days", 30 * 24 * time.Hour, MaxUserKeyTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUserKeyFixture(t, WithUserKeyTTL(tc.ttl))
			user := f.user(t, "mia", "marketing-eng")
			resp := f.mint(t, user)

			require.True(t, strings.HasPrefix(resp.Plaintext, KeyPrefixUser))
			require.Equal(t, resp.Plaintext[:displayPrefixLen], resp.Key.KeyPrefix)
			row := f.keyRow(t, resp.Key.ID)
			require.NotContains(t, row.KeyHash, resp.Plaintext, "the plaintext is never stored")
			require.Equal(t, user.ID, *row.UserID)
			require.Equal(t, models.Role(""), row.Role)
			require.Empty(t, row.Namespaces)
			require.Empty(t, row.Scope)
			require.Nil(t, row.RevokedAt)
			require.Equal(t, "caesium login on test-host", row.Description)
			require.Equal(t, user.Email, row.CreatedBy)
			require.NotNil(t, row.ExpiresAt)
			require.True(t, row.ExpiresAt.Equal(f.clock.Now().Add(tc.want)), "expires %s after the mint", tc.want)
		})
	}
}

func TestMintUserBoundKeyRefusesUnmintableUsers(t *testing.T) {
	f := newUserKeyFixture(t)
	user := f.user(t, "noa", "marketing-eng")
	generation := f.row(t, user.ID).DisableGeneration

	mint := func(id uuid.UUID, gen int64) error {
		_, err := f.svc.MintUserBoundKey(t.Context(), MintUserKeyRequest{UserID: id, DisableGeneration: gen})
		return err
	}
	require.ErrorIs(t, mint(uuid.Nil, 0), ErrUserNotFound)
	require.ErrorIs(t, mint(uuid.New(), 0), ErrUserNotFound)
	require.ErrorIs(t, mint(user.ID, generation+1), ErrUserChanged, "a generation the user is not at is refused")

	admin := NewUserAdmin(f.sessions)
	_, err := admin.DisableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.ErrorIs(t, mint(user.ID, generation), ErrUserDisabled)
	_, err = admin.EnableUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.ErrorIs(t, mint(user.ID, generation), ErrUserChanged,
		"a mint authorised before a disable and enable is refused, as a login would be")

	var keys int64
	require.NoError(t, f.db.Model(&models.APIKey{}).Where("user_id = ?", user.ID).Count(&keys).Error)
	require.Zero(t, keys, "no refused mint wrote a key")
	require.NoError(t, mint(user.ID, f.row(t, user.ID).DisableGeneration))
}

func TestUserKeyMintRacingDisableEndsRevoked(t *testing.T) {
	f := newUserKeyFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.user(t, "ola", "marketing-eng")
	generation := f.row(t, user.ID).DisableGeneration

	postgres := f.db.Name() == "postgres"
	var wg sync.WaitGroup
	var disableErr error
	userKeyMintHook = func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, disableErr = admin.DisableUser(context.Background(), user.ID)
		}()
		if postgres {
			waitForBlockedLock(t, f.db)
		}
	}
	resp, err := f.svc.MintUserBoundKey(t.Context(), MintUserKeyRequest{UserID: user.ID, DisableGeneration: generation})
	userKeyMintHook = nil
	require.NoError(t, err, "the mint that won the lock commits")
	wg.Wait()
	require.NoError(t, disableErr)

	require.NotNil(t, f.keyRow(t, resp.Key.ID).RevokedAt, "the disable that waited for the insert revoked the key")
	_, _, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.ErrorIs(t, err, ErrKeyRevoked)
}

func TestAuthenticateUserBoundKeyResolvesTheUsersGrantsPerRequest(t *testing.T) {
	f := newUserKeyFixture(t)
	user := f.user(t, "lou", "marketing-eng")
	resp := f.mint(t, user)

	key, p, err := f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, resp.Key.ID, key.ID)
	require.Equal(t, PrincipalUser, p.Kind)
	require.Equal(t, policy.Grants{"marketing": models.RoleOperator}, p.Grants)
	require.Equal(t, user.Email, p.Subject, "the audit actor is the user's email, never the key prefix")
	require.Equal(t, user.ID, *p.UserID)
	require.Equal(t, resp.Key.ID, *p.UserKeyID)
	require.Nil(t, p.KeyID, "a user principal carries no plain KeyID")
	require.Nil(t, p.Scope)
	require.True(t, f.recordedLastUse(key.ID))
	require.Zero(t, f.sleepCount(), "a success is not padded")

	// Field for field the principal is the one the same user's session gets,
	// plus the key that authenticated it.
	token, _, err := f.sessions.Create(t.Context(), CreateSessionRequest{UserID: user.ID, DisableGeneration: f.row(t, user.ID).DisableGeneration})
	require.NoError(t, err)
	_, sessUser, err := f.sessions.Validate(t.Context(), token)
	require.NoError(t, err)
	sessionPrincipal := PrincipalFromUser(sessUser, f.svc.PolicySource())
	sessionPrincipal.UserKeyID = p.UserKeyID
	require.Equal(t, sessionPrincipal, p)

	// Grants follow the stored groups on the next request, without a new key.
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("groups", datatypes.JSON(`["finance-eng"]`)).Error)
	_, p, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"finance": models.RoleOperator}, p.Grants)

	// A user no binding matches still authenticates, with no grants: RBAC
	// refuses its routes as it does a grant-less session's.
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("groups", datatypes.JSON(`[]`)).Error)
	_, p, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
	require.NotNil(t, p.Grants)
	require.Empty(t, p.Grants)

	// ValidateKey runs the same checks.
	validated, err := f.svc.ValidateKey(resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, resp.Key.ID, validated.ID)
}

func TestAuthenticateUserBoundKeyUnverifiedEmailGetsNoUserBinding(t *testing.T) {
	f := newUserKeyFixture(t)
	ext := oidcIdentity("boss", "")
	ext.Email = "boss@example.com"
	user := f.login(t, ext)
	require.False(t, user.EmailVerified)
	resp := f.mint(t, user)

	_, p, err := f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
	require.Empty(t, p.Grants, "an unverified email never matches a users: binding")
	require.Equal(t, "boss@example.com", p.Subject)

	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("email_verified", true).Error)
	_, p, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, policy.Grants{"*": models.RoleAdmin}, p.Grants)
}

func TestAuthenticateUserBoundKeyRefusals(t *testing.T) {
	f := newUserKeyFixture(t)
	admin := NewUserAdmin(f.sessions)

	refused := func(t *testing.T, plaintext string, keyID uuid.UUID, want error) {
		t.Helper()
		before := f.sleepCount()
		key, p, err := f.svc.AuthenticateKey(t.Context(), plaintext)
		require.ErrorIs(t, err, want)
		require.Nil(t, key)
		require.Nil(t, p)
		require.Equal(t, before+1, f.sleepCount(), "every refusal is padded to the same floor")
		require.False(t, f.recordedLastUse(keyID), "a refused key records no use")
	}

	t.Run("disabled user, key left unrevoked", func(t *testing.T) {
		user := f.user(t, "dee", "marketing-eng")
		resp := f.mint(t, user)
		// Disable without the cascade: validation must refuse on the user
		// row alone.
		require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", user.ID).Update("disabled_at", f.clock.Now()).Error)
		refused(t, resp.Plaintext, resp.Key.ID, ErrUserDisabled)
	})
	t.Run("disabled user through the cascade", func(t *testing.T) {
		user := f.user(t, "dan", "marketing-eng")
		resp := f.mint(t, user)
		change, err := admin.DisableUser(t.Context(), user.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), change.KeysRevoked)
		refused(t, resp.Plaintext, resp.Key.ID, ErrKeyRevoked)
	})
	t.Run("missing user", func(t *testing.T) {
		user := f.user(t, "gus", "marketing-eng")
		resp := f.mint(t, user)
		require.NoError(t, f.db.Exec("DELETE FROM users WHERE id = ?", user.ID).Error)
		refused(t, resp.Plaintext, resp.Key.ID, ErrKeyNotFound)
	})
	t.Run("expired", func(t *testing.T) {
		g := newUserKeyFixture(t, WithUserKeyTTL(time.Minute))
		user := g.user(t, "eve", "marketing-eng")
		resp := g.mint(t, user)
		g.clock.Advance(time.Minute + time.Second)
		_, _, err := g.svc.AuthenticateKey(t.Context(), resp.Plaintext)
		require.ErrorIs(t, err, ErrKeyExpired)
	})
	t.Run("revoked by an admin", func(t *testing.T) {
		user := f.user(t, "rae", "marketing-eng")
		resp := f.mint(t, user)
		require.NoError(t, f.svc.RevokeKey(resp.Key.ID))
		refused(t, resp.Plaintext, resp.Key.ID, ErrKeyRevoked)
	})

	// A user-bound row the mint could not have written is refused, never
	// half-honoured.
	for _, tc := range []struct {
		name   string
		column string
		value  any
	}{
		{"with a role", "role", string(models.RoleAdmin)},
		{"with namespaces", "namespaces", datatypes.JSON(`["*"]`)},
		{"with a scope", "scope", datatypes.JSON(`{"jobs":["a"]}`)},
		{"without an expiry", "expires_at", nil},
		{"bound to the nil user", "user_id", uuid.Nil},
	} {
		t.Run("malformed "+tc.name, func(t *testing.T) {
			user := f.user(t, "mal-"+strings.ReplaceAll(tc.column, "_", ""), "marketing-eng")
			resp := f.mint(t, user)
			require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", resp.Key.ID).Update(tc.column, tc.value).Error)
			refused(t, resp.Plaintext, resp.Key.ID, ErrKeyNotFound)
		})
	}
}

// The stored row decides the kind, never the token's prefix.
func TestAuthenticateKeyKindComesFromTheStoredRow(t *testing.T) {
	f := newUserKeyFixture(t)
	user := f.user(t, "kip", "marketing-eng")

	// A csk_user_ token on a plain row is a plain key.
	plainToken := KeyPrefixUser + "plainrowpresentedwithauserprefix0000000000"
	hash, err := HashKey(plainToken, f.svc.keyHashSecret)
	require.NoError(t, err)
	plain := models.APIKey{ID: uuid.New(), KeyPrefix: plainToken[:displayPrefixLen], KeyHash: hash, Role: models.RoleViewer, CreatedAt: f.clock.Now()}
	require.NoError(t, f.db.Create(&plain).Error)
	_, p, err := f.svc.AuthenticateKey(t.Context(), plainToken)
	require.NoError(t, err)
	require.Equal(t, PrincipalAPIKey, p.Kind)
	require.Equal(t, policy.Grants{"*": models.RoleViewer}, p.Grants)
	require.Nil(t, p.UserID)

	// A csk_live_ token on a user-bound row is that user.
	liveToken := KeyPrefixLive + "userrowpresentedwithaliveprefix00000000000"
	hash, err = HashKey(liveToken, f.svc.keyHashSecret)
	require.NoError(t, err)
	expires := f.clock.Now().Add(time.Hour)
	userID := user.ID
	bound := models.APIKey{ID: uuid.New(), KeyPrefix: liveToken[:displayPrefixLen], KeyHash: hash, CreatedAt: f.clock.Now(), ExpiresAt: &expires, UserID: &userID}
	require.NoError(t, f.db.Create(&bound).Error)
	_, p, err = f.svc.AuthenticateKey(t.Context(), liveToken)
	require.NoError(t, err)
	require.Equal(t, PrincipalUser, p.Kind)
	require.Equal(t, policy.Grants{"marketing": models.RoleOperator}, p.Grants)
	require.Equal(t, bound.ID, *p.UserKeyID)
}

// TestAuthenticateKeyPlainKeysMatchPrincipalFromKeyOracle is the
// byte-identical proof for every plain key shape: AuthenticateKey returns the
// stored row and exactly PrincipalFromKey of it, as the middleware built
// before user-bound keys existed.
func TestAuthenticateKeyPlainKeysMatchPrincipalFromKeyOracle(t *testing.T) {
	f := newUserKeyFixture(t)
	incident := uuid.New()
	for _, tc := range []struct {
		name string
		req  CreateKeyRequest
		raw  datatypes.JSON
	}{
		{name: "cluster admin", req: CreateKeyRequest{Role: models.RoleAdmin}},
		{name: "cluster viewer", req: CreateKeyRequest{Role: models.RoleViewer}},
		{name: "explicit star", req: CreateKeyRequest{Role: models.RoleOperator, Namespaces: datatypes.JSON(`["*"]`)}},
		{name: "namespaced", req: CreateKeyRequest{Role: models.RoleOperator, Namespaces: datatypes.JSON(`["finance","marketing"]`)}},
		{name: "job scoped", req: CreateKeyRequest{Role: models.RoleRunner, Scope: &models.KeyScope{Jobs: []string{"etl"}}}},
		{name: "agent session", req: CreateKeyRequest{Role: models.RoleRunner, Scope: &models.KeyScope{Agent: &models.AgentClaim{IncidentID: incident}}}},
		{name: "corrupt namespaces", req: CreateKeyRequest{Role: models.RoleAdmin}, raw: datatypes.JSON(`not json`)},
		{name: "expiring", req: CreateKeyRequest{Role: models.RoleOperator, ExpiresAt: new(f.clock.Now().Add(time.Hour))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := f.svc.CreateKey(&tc.req)
			require.NoError(t, err)
			if tc.raw != nil {
				require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", resp.Key.ID).Update("namespaces", tc.raw).Error)
			}
			stored := f.keyRow(t, resp.Key.ID)

			key, p, err := f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
			require.NoError(t, err)
			require.Equal(t, stored, *key)
			require.Equal(t, PrincipalFromKey(&stored), p)
			require.Nil(t, p.UserKeyID)
			require.True(t, f.recordedLastUse(key.ID))

			validated, err := f.svc.ValidateKey(resp.Plaintext)
			require.NoError(t, err)
			require.Equal(t, stored, *validated)
		})
	}
}

func TestPrincipalFromKeyAndAdminKeyExistsIgnoreUserBoundRows(t *testing.T) {
	f := newUserKeyFixture(t)
	user := f.user(t, "ari", "marketing-eng")
	resp := f.mint(t, user)
	// Even a tampered user-bound row naming admin grants nothing as a key.
	require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", resp.Key.ID).Update("role", models.RoleAdmin).Error)
	row := f.keyRow(t, resp.Key.ID)
	require.Empty(t, PrincipalFromKey(&row).Grants)

	exists, err := f.svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists, "a user-bound row is never the cluster admin key")
}

func TestBootstrapRefreshClearsAUserBinding(t *testing.T) {
	f := newUserKeyFixture(t)
	plaintext, err := f.svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)
	user := f.user(t, "bea", "marketing-eng")
	require.NoError(t, f.db.Model(&models.APIKey{}).Where("bootstrap_slot = ?", bootstrapAdminSlot).Update("user_id", user.ID).Error)

	refreshed, err := f.svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, refreshed, "a bootstrap row bound to a user is no cluster admin key, so it is refreshed")
	var row models.APIKey
	require.NoError(t, f.db.Where("bootstrap_slot = ?", bootstrapAdminSlot).Take(&row).Error)
	require.Nil(t, row.UserID)
	_, p, err := f.svc.AuthenticateKey(t.Context(), refreshed)
	require.NoError(t, err)
	require.Equal(t, models.RoleAdmin, p.ClusterRole())
}

func TestRotateUserBoundKeyIsRefusedAfterAuthorization(t *testing.T) {
	f := newUserKeyFixture(t)
	user := f.user(t, "roy", "marketing-eng")
	resp := f.mint(t, user)

	var keysBefore int64
	require.NoError(t, f.db.Model(&models.APIKey{}).Count(&keysBefore).Error)

	denial := &rotationDenial{}
	_, err := f.svc.RotateKeyAuthorized(t.Context(), resp.Key.ID, time.Hour, "x", func(*models.APIKey) error { return denial })
	require.ErrorIs(t, err, denial, "a caller that may not administer the key learns nothing about it")

	_, err = f.svc.RotateKeyAuthorized(t.Context(), resp.Key.ID, time.Hour, "x", func(*models.APIKey) error { return nil })
	require.ErrorIs(t, err, ErrUserBoundKeyRotation)

	var keysAfter int64
	require.NoError(t, f.db.Model(&models.APIKey{}).Count(&keysAfter).Error)
	require.Equal(t, keysBefore, keysAfter, "nothing is minted")
	row := f.keyRow(t, resp.Key.ID)
	require.True(t, row.ExpiresAt.Equal(*resp.Key.ExpiresAt), "the old key's expiry is untouched")
	_, _, err = f.svc.AuthenticateKey(t.Context(), resp.Plaintext)
	require.NoError(t, err)
}

type rotationDenial struct{}

func (*rotationDenial) Error() string { return "denied" }

func TestUserLifecycleRevokesUserBoundKeys(t *testing.T) {
	f := newUserKeyFixture(t)
	admin := NewUserAdmin(f.sessions)
	plain, err := f.svc.CreateKey(&CreateKeyRequest{Role: models.RoleAdmin})
	require.NoError(t, err)
	other := f.user(t, "oz", "finance-eng")
	otherKey := f.mint(t, other)

	t.Run("disable", func(t *testing.T) {
		user := f.user(t, "dis", "marketing-eng")
		k1, k2 := f.mint(t, user), f.mint(t, user)
		require.NoError(t, f.svc.RevokeKey(k2.Key.ID))
		f.newSession(t, user.ID)

		change, err := admin.DisableUser(t.Context(), user.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), change.SessionsRevoked)
		require.Equal(t, int64(1), change.KeysRevoked, "only the live key counts")
		require.True(t, f.keyRow(t, k1.Key.ID).RevokedAt.Equal(f.clock.Now()))

		again, err := admin.DisableUser(t.Context(), user.ID)
		require.NoError(t, err)
		require.Zero(t, again.KeysRevoked)
	})
	t.Run("enable sweeps a key inserted while disabled", func(t *testing.T) {
		user := f.user(t, "ena", "marketing-eng")
		_, err := admin.DisableUser(t.Context(), user.ID)
		require.NoError(t, err)
		expires := f.clock.Now().Add(time.Hour)
		id := user.ID
		stray := models.APIKey{ID: uuid.New(), KeyPrefix: "csk_user_str", KeyHash: "stray-" + uuid.NewString(), CreatedAt: f.clock.Now(), ExpiresAt: &expires, UserID: &id}
		require.NoError(t, f.db.Create(&stray).Error)

		change, err := admin.EnableUser(t.Context(), user.ID)
		require.NoError(t, err)
		require.Equal(t, int64(1), change.KeysRevoked)
		require.NotNil(t, f.keyRow(t, stray.ID).RevokedAt)
	})
	t.Run("log out everywhere", func(t *testing.T) {
		user := f.user(t, "out", "marketing-eng")
		k := f.mint(t, user)
		f.newSession(t, user.ID)
		revoked, err := admin.RevokeUserSessions(t.Context(), user.ID)
		require.NoError(t, err)
		require.Equal(t, RevokedCredentials{Sessions: 1, Keys: 1}, revoked)
		require.NotNil(t, f.keyRow(t, k.Key.ID).RevokedAt)
		require.Nil(t, f.row(t, user.ID).DisabledAt, "logging out everywhere does not disable")
	})
	t.Run("definitive refresh denial", func(t *testing.T) {
		ext := oidcIdentity("idp", "idp-refresh-token-0001", "marketing-eng")
		user := f.login(t, ext)
		k := f.mint(t, user)
		row := f.row(t, user.ID)
		require.True(t, f.claim(t, user.ID, row.RefreshGrantVersion, "node-a/1"))
		result, err := f.users.CompleteRefreshDefinitive(t.Context(), user.ID, row.RefreshGrantVersion, "node-a/1", DenialOfGrant, f.clock.Now())
		require.NoError(t, err)
		require.Equal(t, DefinitiveResult{Outcome: DefinitiveRevoked, KeysRevoked: 1}, result)
		require.NotNil(t, f.keyRow(t, k.Key.ID).RevokedAt)
	})

	require.Nil(t, f.keyRow(t, plain.Key.ID).RevokedAt, "a plain key is nobody's credential")
	require.Nil(t, f.keyRow(t, otherKey.Key.ID).RevokedAt, "another user's key is untouched")
}

func TestUserAdminDetailListsActiveUserKeysNewestFirst(t *testing.T) {
	f := newUserKeyFixture(t)
	admin := NewUserAdmin(f.sessions)
	user := f.user(t, "lis", "marketing-eng")
	other := f.user(t, "oth", "marketing-eng")
	f.mint(t, other)

	revoked := f.mint(t, user)
	require.NoError(t, f.svc.RevokeKey(revoked.Key.ID))
	expired := f.mint(t, user)
	require.NoError(t, f.db.Model(&models.APIKey{}).Where("id = ?", expired.Key.ID).Update("expires_at", f.clock.Now().Add(-time.Second)).Error)
	var live []uuid.UUID
	for range DetailSessionLimit + 1 {
		f.clock.Advance(time.Millisecond)
		live = append(live, f.mint(t, user).Key.ID)
	}

	detail, err := admin.GetUser(t.Context(), user.ID)
	require.NoError(t, err)
	require.Equal(t, int64(DetailSessionLimit+1), detail.UserKeysTotal, "revoked and expired keys are not active")
	require.Len(t, detail.UserKeys, DetailSessionLimit)
	require.Equal(t, live[len(live)-1], detail.UserKeys[0].ID, "newest first")
	for _, k := range detail.UserKeys {
		require.Equal(t, user.ID, *k.UserID)
		require.NotEqual(t, revoked.Key.ID, k.ID)
		require.NotEqual(t, expired.Key.ID, k.ID)
	}

	none, err := admin.GetUser(t.Context(), f.user(t, "nil", "marketing-eng").ID)
	require.NoError(t, err)
	require.NotNil(t, none.UserKeys)
	require.Empty(t, none.UserKeys)
	require.Zero(t, none.UserKeysTotal)
}

func TestAPIKeyUserIDSerialisesOnlyWhenSet(t *testing.T) {
	raw, err := json.Marshal(models.APIKey{ID: uuid.New(), Role: models.RoleViewer})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "user_id", "a plain key's JSON is unchanged")
	id := uuid.New()
	raw, err = json.Marshal(models.APIKey{ID: uuid.New(), UserID: &id})
	require.NoError(t, err)
	require.Contains(t, string(raw), `"user_id":"`+id.String()+`"`)
}
