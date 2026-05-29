# SSO Authentication — Foundation (P0 + P1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the identity/session foundation for native SSO — a `Principal` abstraction that unifies API keys and user sessions, plus server-side sessions in dqlite, group→role mapping, cookie/CSRF handling, and the shared login pipeline — so the OIDC/SAML/LDAP providers (separate plans) only have to produce an `ExternalIdentity`.

**Architecture:** Extend the existing `internal/auth` subsystem and the single auth middleware chokepoint (`api/middleware/auth.go`). P0 introduces a `Principal` and routes today's API-key auth through it with **zero behavior change**. P1 adds `users`/`sessions` tables, a `SessionStore` modeled on the existing `Service` (token hashed at rest, coalesced last-seen flusher), a `RoleMapper`, JIT user provisioning, the provider interface + shared `Complete` tail, cookie + CSRF middleware, the session-cookie branch in the auth middleware, and `/auth/whoami` + `/auth/logout` + an extended `/auth/status`.

**Tech Stack:** Go, GORM over dqlite (`pkg/db` catalog tier), Echo v5, `crypto/rand`/`crypto/hmac`/`crypto/subtle`, React/TypeScript + Vitest (UI).

---

## Reference & scope

- **Spec:** `docs/superpowers/specs/2026-05-27-sso-authentication-design.md` (read §5–§13 before starting).
- **This plan covers:** P0 (Principal refactor) and P1 (identity/session foundation).
- **Out of scope (follow-on plans, see end):** P2 OIDC, P3 SAML, P4 LDAP, P5 docs/hardening. Each gets its own detailed plan written against the real library APIs once this foundation is merged.
- **Definition of done for this plan:** all existing tests stay green; new units are covered; the server starts with SSO env vars set; a session minted by a test fake authenticates against `/v1` exactly like an API key; no provider exists yet so no end-to-end browser login.

## Conventions

- **TDD loop per step:** write failing test → run it, see it fail → minimal implementation → run it, see it pass → commit.
- **Test command (focused loop):** `go test ./internal/auth/ -run <TestName> -v`. If you hit CGO/dqlite linker errors on the host, run inside the project's build toolchain (`just unit-test` uses it). Tests that need a DB use an in-memory GORM handle — see the `newTestDB` helper introduced in Task 1.4 and reuse it.
- **Authoritative gate before every commit that touches Go:** `just unit-test` (race + coverage) must pass. Do not rely on CI for compile errors.
- **Commit messages:** concise imperative subject (repo style, e.g. "Add Principal abstraction to auth middleware"); end every commit message with the trailer `Co-Authored-By: Claude Opus 4.7 <noreply@anthropic.com>`.
- **Progress:** Foundation P0/P1 merged in PR #192. OIDC continued on
  `sso-oidc-provider` (PR #193). SAML continued on `sso-saml-provider`
  (PR #194). LDAP continued on `codex/ldap-sso-provider` (PR #195).
  `codex/sso-hardening-wave` (PR #196) and `codex/sso-fixtures-wave` added P5
  hardening with signed SAML response fixtures, a self-contained containerized
  OpenLDAP fixture, and `user.provisioned` audit events.
  `codex/sso-negative-fixtures-wave` (PR #198) added deeper provider-specific
  negative fixtures for OIDC, SAML, and LDAP. `codex/sso-p5-hardening-wave`
  then covered fail-closed state-cookie callbacks, LDAP malformed search
  results, trusted-proxy same-origin redirects, and secure-cookie checks for
  redirect callbacks. Wave 9 on `codex/sso-remaining-foundation-wave` is scoped
  to remaining foundation hardening and docs cleanup. Wave 10 on
  `codex/sso-post-foundation-hardening` clears one-time OIDC/SAML state cookies
  after redirect callbacks, tightens OIDC multi-audience `azp` checks, validates
  trusted-proxy TLS-guard config, and corrects role-mapping docs/tests.

## File structure

**Created:**
- `internal/auth/principal.go` — `Principal`, `PrincipalKind`, `PrincipalFromKey`, `PrincipalFromUser`.
- `internal/models/user.go` — `User` model.
- `internal/models/session.go` — `Session` model.
- `internal/auth/session.go` — `SessionStore` (create/validate/revoke/reap + coalesced last-seen flusher).
- `internal/auth/rolemap.go` — `RoleMapper` (parse `group=role` config, resolve highest, deny-by-default).
- `internal/auth/users.go` — `UserStore.Upsert` (JIT provisioning).
- `internal/auth/provider.go` — `ExternalIdentity`, `RedirectAuthenticator`, `CredentialAuthenticator`, `SSOService.Complete` (shared login tail).
- `api/middleware/csrf.go` — session-bound CSRF check (synchronizer token).
- `api/rest/controller/auth/sso.go` — `Whoami`, `Logout` handlers + session-cookie helpers.

**Modified:**
- `api/middleware/auth.go` — build `Principal`; add session-cookie branch; `GetPrincipal`.
- `api/middleware/auth_scope.go` — `authorizeScope` takes `scopeJSON []byte` instead of `*models.APIKey`.
- `internal/auth/keygen.go` — add `GenerateToken`.
- `internal/models/models.go` — register `&User{}`, `&Session{}` in `All`.
- `pkg/env/env.go` — add `AUTH_*` SSO/session vars.
- `api/rest/bind/bind.go` — gate auth middleware on "auth enabled (api-key OR sso)"; bind `/auth/whoami`, `/auth/logout`.
- `api/api.go` — extend `authStatus` to advertise methods; mount public SSO routes.
- `cmd/start/start.go` — `initAuth` builds the `SessionStore`/`RoleMapper`/`SSOService`, starts the reaper + flusher.
- `ui/src/lib/auth.ts`, `ui/src/features/auth/AuthGate.tsx` — session (cookie) mode via `/auth/whoami` + CSRF header.

---

# Phase 0 — Principal abstraction (behavior-preserving)

### Task 0.1: Define the `Principal` type

**Files:**
- Create: `internal/auth/principal.go`
- Test: `internal/auth/principal_test.go`

- [x] **Step 1: Write the failing test**

```go
package auth

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestPrincipalFromKey(t *testing.T) {
	id := uuid.New()
	key := &models.APIKey{ID: id, KeyPrefix: "csk_live_ab", Role: models.RoleOperator, Scope: []byte(`{"jobs":["x"]}`)}
	p := PrincipalFromKey(key)
	assert.Equal(t, PrincipalAPIKey, p.Kind)
	assert.Equal(t, models.RoleOperator, p.Role)
	assert.Equal(t, "csk_live_ab", p.Subject)
	assert.Equal(t, []byte(`{"jobs":["x"]}`), p.Scope)
	assert.Equal(t, &id, p.KeyID)
	assert.Nil(t, p.UserID)
}

func TestPrincipalFromUser(t *testing.T) {
	id := uuid.New()
	u := &models.User{ID: id, Email: "a@b.com", Role: models.RoleViewer}
	p := PrincipalFromUser(u)
	assert.Equal(t, PrincipalUser, p.Kind)
	assert.Equal(t, models.RoleViewer, p.Role)
	assert.Equal(t, "a@b.com", p.Subject)
	assert.Equal(t, &id, p.UserID)
	assert.Nil(t, p.Scope) // SSO users unscoped in v1
}
```

(`TestPrincipalFromUser` won't compile until `models.User` exists — Task 1.2. To keep P0 self-contained, write only `TestPrincipalFromKey` now and add `TestPrincipalFromUser` plus `PrincipalFromUser` in Task 1.2. The `principal.go` file below already includes `PrincipalFromUser` referencing `models.User`, so introduce `models.User` first if compiling P0 standalone, **or** temporarily omit `PrincipalFromUser` and add it in Task 1.2. Recommended: omit `PrincipalFromUser` here; add in 1.2.)

- [x] **Step 2: Run test to verify it fails**

Run: `go test ./internal/auth/ -run TestPrincipalFromKey -v`
Expected: FAIL — `undefined: PrincipalFromKey`.

- [x] **Step 3: Write minimal implementation** (`internal/auth/principal.go`)

```go
package auth

import (
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// PrincipalKind distinguishes the credential type behind an authenticated request.
type PrincipalKind string

const (
	PrincipalAPIKey PrincipalKind = "api_key"
	PrincipalUser   PrincipalKind = "user"
)

// Principal is the unified authenticated identity used by RBAC, scope checks,
// and audit. It is produced from either an API key or a user session.
type Principal struct {
	Kind    PrincipalKind
	Role    models.Role
	Scope   []byte // raw KeyScope JSON; nil/empty == unrestricted (see ScopeJobs/CheckScope)
	Subject string // audit actor: key prefix or user email
	UserID  *uuid.UUID
	KeyID   *uuid.UUID
}

// PrincipalFromKey builds a Principal from a validated API key.
func PrincipalFromKey(k *models.APIKey) *Principal {
	id := k.ID
	return &Principal{
		Kind:    PrincipalAPIKey,
		Role:    k.Role,
		Scope:   k.Scope,
		Subject: k.KeyPrefix,
		KeyID:   &id,
	}
}
```

- [x] **Step 4: Run test to verify it passes**

Run: `go test ./internal/auth/ -run TestPrincipalFromKey -v`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add internal/auth/principal.go internal/auth/principal_test.go
git commit   # "Add Principal abstraction to auth package"
```

### Task 0.2: Route the auth middleware through `Principal`

**Files:**
- Modify: `api/middleware/auth.go` (the `Auth` handler body, ~lines 57–106)
- Modify: `api/middleware/auth_scope.go` (`authorizeScope` signature)
- Test: `api/middleware/auth_test.go` (existing — must stay green)

- [x] **Step 1: Change `authorizeScope` to take scope bytes (not the key)**

In `api/middleware/auth_scope.go`, change the signature and the one field access:

```go
// before: func authorizeScope(c *echo.Context, svc *auth.Service, key *models.APIKey, routePath string) (*scopeAuditContext, error)
func authorizeScope(c *echo.Context, svc *auth.Service, scopeJSON []byte, routePath string) (*scopeAuditContext, error) {
	scopeJobs, err := auth.ScopeJobs(scopeJSON)
	// ...rest unchanged, replacing every `key.Scope` with `scopeJSON`...
```

Replace the three `key.Scope` references in that function with `scopeJSON`.

- [x] **Step 2: Build a `Principal` in `Auth` and use it for RBAC/scope**

In `api/middleware/auth.go`, after `key, err := svc.ValidateKey(token)` succeeds, replace the role/scope/context block (currently lines ~85–106) with:

```go
		principal := auth.PrincipalFromKey(key)

		routePath := normalisePath(c)
		required, ok := auth.RequiredRole(c.Request().Method, routePath)
		if !ok {
			return denyAccess(c, auditor, principal.Subject, principal.Role, routePath, "unknown_route", "")
		}
		if !auth.HasRole(principal.Role, required) {
			return denyAccess(c, auditor, principal.Subject, principal.Role, routePath, "insufficient_role", required)
		}

		scopeContext, err := authorizeScope(c, svc, principal.Scope, routePath)
		if err != nil {
			if he, ok := err.(*echo.HTTPError); ok && he.Code == http.StatusForbidden {
				return denyAccess(c, auditor, principal.Subject, principal.Role, routePath, "insufficient_scope", required)
			}
			return err
		}

		c.Set(ContextKeyAuth, key)             // backward compat: GetAuthKey still works
		c.Set(ContextKeyPrincipal, principal)  // new unified identity
		metrics.AuthKeyAgeSeconds.Observe(time.Since(key.CreatedAt).Seconds())
```

Add the new context key constant near `ContextKeyAuth`:

```go
// ContextKeyPrincipal stores the unified *auth.Principal for the request.
const ContextKeyPrincipal = "auth.principal"
```

- [x] **Step 3: Run the existing middleware suite to verify no behavior change**

Run: `go test ./api/middleware/ -v`
Expected: PASS (all existing tests). If any reference `authorizeScope(... key ...)` directly, update them to pass `key.Scope`.

- [x] **Step 4: Run the auth package + full gate**

Run: `go test ./api/middleware/ ./internal/auth/ -v` then `just unit-test`
Expected: PASS.

- [x] **Step 5: Commit**

```bash
git add api/middleware/auth.go api/middleware/auth_scope.go api/middleware/auth_test.go
git commit   # "Resolve API-key auth through Principal (no behavior change)"
```

### Task 0.3: `GetPrincipal` helper

**Files:**
- Modify: `api/middleware/auth.go` (add helper near `GetAuthKey`)
- Test: `api/middleware/auth_test.go`

- [x] **Step 1: Write the failing test**

```go
func TestGetPrincipal(t *testing.T) {
	e := echo.New()
	c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
	assert.Nil(t, GetPrincipal(c))
	p := &auth.Principal{Kind: auth.PrincipalUser, Subject: "a@b.com"}
	c.Set(ContextKeyPrincipal, p)
	assert.Equal(t, p, GetPrincipal(c))
}
```

- [x] **Step 2: Run → FAIL** (`undefined: GetPrincipal`). `go test ./api/middleware/ -run TestGetPrincipal -v`

- [x] **Step 3: Implement**

```go
// GetPrincipal returns the unified authenticated identity, or nil if unauthenticated.
func GetPrincipal(c *echo.Context) *auth.Principal {
	v := c.Get(ContextKeyPrincipal)
	if v == nil {
		return nil
	}
	p, ok := v.(*auth.Principal)
	if !ok {
		return nil
	}
	return p
}
```

- [x] **Step 4: Run → PASS.** `go test ./api/middleware/ -run TestGetPrincipal -v`

- [x] **Step 5: Commit** — `git commit` "Add GetPrincipal context helper"

---

# Phase 1 — Identity & session foundation

### Task 1.1: SSO/session environment configuration

**Files:**
- Modify: `pkg/env/env.go` (Authentication & Authorization block, ~line 136)
- Test: `pkg/env/env_test.go` (add a case if the file tests parsing; otherwise skip the test step)

- [x] **Step 1: Add fields** to the `Environment` struct after the existing `Auth*` fields:

```go
	// SSO / sessions (additive on top of AUTH_MODE)
	AuthPublicBaseURL     string        `envconfig:"AUTH_PUBLIC_BASE_URL" default:""`
	AuthSessionIdleTTL    time.Duration `envconfig:"AUTH_SESSION_IDLE_TTL" default:"8h"`
	AuthSessionAbsoluteTTL time.Duration `envconfig:"AUTH_SESSION_ABSOLUTE_TTL" default:"24h"`
	AuthSessionCookieName string        `envconfig:"AUTH_SESSION_COOKIE_NAME" default:"caesium_session"`
	AuthRoleMapping       string        `envconfig:"AUTH_ROLE_MAPPING" default:""`
	AuthDefaultRole       string        `envconfig:"AUTH_DEFAULT_ROLE" default:""`
	// Provider enable flags are read here; provider-specific config lands in P2-P4.
	AuthOIDCEnabled bool `envconfig:"AUTH_OIDC_ENABLED" default:"false"`
	AuthSAMLEnabled bool `envconfig:"AUTH_SAML_ENABLED" default:"false"`
	AuthLDAPEnabled bool `envconfig:"AUTH_LDAP_ENABLED" default:"false"`
```

- [x] **Step 2: Helper to report whether any SSO provider is enabled** (same file):

```go
// SSOEnabled reports whether any SSO provider is configured.
func (e Environment) SSOEnabled() bool {
	return e.AuthOIDCEnabled || e.AuthSAMLEnabled || e.AuthLDAPEnabled
}
```

- [x] **Step 3: Verify it compiles & parses**

Run: `go build ./pkg/env/ && go test ./pkg/env/ -v`
Expected: PASS (no behavior change to existing vars).

- [x] **Step 4: Commit** — "Add SSO/session environment configuration"

### Task 1.2: `User` and `Session` models + migration registration

**Files:**
- Create: `internal/models/user.go`, `internal/models/session.go`
- Modify: `internal/models/models.go` (register in `All`)
- Modify: `internal/auth/principal.go` (add `PrincipalFromUser`), `internal/auth/principal_test.go` (add `TestPrincipalFromUser` from Task 0.1)
- Test: `internal/models/user_test.go`

- [x] **Step 1: Write `internal/models/user.go`**

```go
package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// User is a human identity provisioned just-in-time from an external IdP.
type User struct {
	ID          uuid.UUID      `gorm:"type:uuid;primaryKey" json:"id"`
	Issuer      string         `gorm:"type:text;not null;uniqueIndex:idx_users_identity" json:"issuer"`
	Subject     string         `gorm:"type:text;not null;uniqueIndex:idx_users_identity" json:"subject"`
	Email       string         `gorm:"type:text;index" json:"email"`
	DisplayName string         `gorm:"type:text" json:"display_name,omitempty"`
	Groups      datatypes.JSON `gorm:"type:json" json:"groups,omitempty"`
	Role        Role           `gorm:"type:text;not null" json:"role"`
	CreatedAt   time.Time      `gorm:"not null" json:"created_at"`
	LastLoginAt *time.Time     `json:"last_login_at,omitempty"`
	DisabledAt  *time.Time     `json:"disabled_at,omitempty"`
}

// IsDisabled reports whether the user account has been disabled.
func (u *User) IsDisabled() bool { return u.DisabledAt != nil }
```

- [x] **Step 2: Write `internal/models/session.go`**

```go
package models

import (
	"time"

	"github.com/google/uuid"
)

// Session is a server-side login session. The opaque token is never stored;
// only its keyed hash (TokenHash) is persisted.
type Session struct {
	ID                uuid.UUID  `gorm:"type:uuid;primaryKey" json:"id"`
	UserID            uuid.UUID  `gorm:"type:uuid;not null;index" json:"user_id"`
	TokenHash         string     `gorm:"type:text;not null;uniqueIndex" json:"-"`
	CSRFToken         string     `gorm:"type:text;not null" json:"-"`
	AuthMethod        string     `gorm:"type:text" json:"auth_method"`
	CreatedAt         time.Time  `gorm:"not null" json:"created_at"`
	IdleExpiresAt     time.Time  `gorm:"not null" json:"idle_expires_at"`
	AbsoluteExpiresAt time.Time  `gorm:"not null" json:"absolute_expires_at"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty"`
	RevokedAt         *time.Time `json:"revoked_at,omitempty"`
	SourceIP          string     `gorm:"type:text" json:"source_ip,omitempty"`
	UserAgent         string     `gorm:"type:text" json:"user_agent,omitempty"`
}

// IsRevoked reports whether the session was explicitly revoked.
func (s *Session) IsRevoked() bool { return s.RevokedAt != nil }
```

- [x] **Step 3: Register in `internal/models/models.go`** — add after `&AuditLog{}` (User before Session for FK ordering):

```go
	&AuditLog{},
	&User{},
	&Session{},
```

- [x] **Step 4: Add `PrincipalFromUser`** to `internal/auth/principal.go`:

```go
// PrincipalFromUser builds a Principal from an authenticated user. SSO users are
// unscoped (nil Scope) in v1.
func PrincipalFromUser(u *models.User) *Principal {
	id := u.ID
	return &Principal{
		Kind:    PrincipalUser,
		Role:    u.Role,
		Subject: u.Email,
		UserID:  &id,
	}
}
```

Add `TestPrincipalFromUser` (from Task 0.1 Step 1) to `internal/auth/principal_test.go`.

- [x] **Step 5: Write `internal/models/user_test.go`** (round-trip + AutoMigrate smoke):

```go
package models

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUserSessionMigrate(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&User{}, &Session{}))

	u := &User{ID: uuid.New(), Issuer: "oidc", Subject: "sub-1", Email: "a@b.com", Role: RoleViewer}
	require.NoError(t, db.Create(u).Error)
	s := &Session{ID: uuid.New(), UserID: u.ID, TokenHash: "h", AuthMethod: "oidc"}
	require.NoError(t, db.Create(s).Error)

	var got User
	require.NoError(t, db.First(&got, "id = ?", u.ID).Error)
	assert.Equal(t, "a@b.com", got.Email)
	assert.False(t, got.IsDisabled())
}
```

> Note: if `gorm.io/driver/sqlite` is not already a dependency, use the project's existing in-memory test DB helper instead (grep `service_test.go` for how `internal/auth` opens its test DB) and mirror that. Do not add a new driver dependency just for tests.

- [x] **Step 6: Run → PASS, then gate**

Run: `go test ./internal/models/ ./internal/auth/ -run 'User|Principal' -v` then `just unit-test`
Expected: PASS.

- [x] **Step 7: Commit** — "Add User and Session models with migration registration"

### Task 1.3: Session token generation

**Files:**
- Modify: `internal/auth/keygen.go`
- Test: `internal/auth/keygen_test.go`

- [x] **Step 1: Failing test**

```go
func TestGenerateToken(t *testing.T) {
	a, err := GenerateToken()
	assert.NoError(t, err)
	b, err := GenerateToken()
	assert.NoError(t, err)
	assert.NotEqual(t, a, b)
	assert.True(t, strings.HasPrefix(a, SessionTokenPrefix))
	assert.Greater(t, len(a), len(SessionTokenPrefix)+20)
}
```

- [x] **Step 2: Run → FAIL.** `go test ./internal/auth/ -run TestGenerateToken -v`

- [x] **Step 3: Implement** (append to `internal/auth/keygen.go`):

```go
// SessionTokenPrefix marks opaque session tokens (distinct from API keys).
const SessionTokenPrefix = "css_"

// GenerateToken produces a new opaque session token. Like API keys, the plaintext
// is shown to the client once (in the cookie) and only its hash is stored.
func GenerateToken() (string, error) {
	random, err := base62Encode(randomBytes)
	if err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return SessionTokenPrefix + random, nil
}
```

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add session token generator"

### Task 1.4: `SessionStore` — create / validate / revoke

**Files:**
- Create: `internal/auth/session.go`
- Test: `internal/auth/session_test.go`

- [x] **Step 1: Failing test** (introduce `newTestDB` helper reused by later tasks):

```go
package auth

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSessionStoreCreateValidate(t *testing.T) {
	db := newTestDB(t) // see note below
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}))
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleOperator}
	require.NoError(t, db.Create(u).Error)

	store := NewSessionStore(db, WithSessionTTLs(8*time.Hour, 24*time.Hour))
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID, AuthMethod: "oidc"})
	require.NoError(t, err)
	assert.NotEmpty(t, plaintext)
	assert.NotEmpty(t, sess.TokenHash)

	gotSess, gotUser, err := store.Validate(context.Background(), plaintext)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, gotSess.ID)
	assert.Equal(t, u.Email, gotUser.Email)

	_, _, err = store.Validate(context.Background(), "css_wrong")
	assert.ErrorIs(t, err, ErrSessionInvalid)
}

func TestSessionStoreRevoke(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}))
	u := &models.User{ID: uuid.New(), Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db, WithSessionTTLs(time.Hour, time.Hour))
	plaintext, sess, _ := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, store.Revoke(context.Background(), sess.ID))
	_, _, err := store.Validate(context.Background(), plaintext)
	assert.ErrorIs(t, err, ErrSessionRevoked)
}
```

> `newTestDB(t)`: grep `internal/auth/service_test.go` for the existing helper that opens the in-memory test DB and reuse/extract it into a shared `testhelpers_test.go` so both files use one helper (DRY). Match whatever driver the existing auth tests already use.

- [x] **Step 2: Run → FAIL.** `go test ./internal/auth/ -run TestSessionStore -v`

- [x] **Step 3: Implement `internal/auth/session.go`**

```go
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

// SessionStore manages server-side login sessions in the catalog DB. It mirrors
// Service: tokens are hashed at rest and last-seen updates are coalesced.
type SessionStore struct {
	db              *gorm.DB
	tokenHashSecret string
	idleTTL         time.Duration
	absoluteTTL     time.Duration
	now             func() time.Time

	seenMu sync.Mutex
	seen   map[uuid.UUID]time.Time
}

type SessionStoreOption func(*SessionStore)

func WithSessionHashSecret(secret string) SessionStoreOption {
	return func(s *SessionStore) { s.tokenHashSecret = secret }
}
func WithSessionTTLs(idle, absolute time.Duration) SessionStoreOption {
	return func(s *SessionStore) { s.idleTTL, s.absoluteTTL = idle, absolute }
}
func WithSessionNow(now func() time.Time) SessionStoreOption {
	return func(s *SessionStore) { if now != nil { s.now = now } }
}

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

func (s *SessionStore) nowUTC() time.Time { return s.now().UTC() }

// CreateSessionRequest holds parameters for minting a session.
type CreateSessionRequest struct {
	UserID     uuid.UUID
	AuthMethod string
	SourceIP   string
	UserAgent  string
}

// Create mints a new session and returns the plaintext token (cookie value).
func (s *SessionStore) Create(ctx context.Context, req CreateSessionRequest) (string, *models.Session, error) {
	plaintext, err := GenerateToken()
	if err != nil {
		return "", nil, err
	}
	hash, err := HashKey(plaintext, s.tokenHashSecret)
	if err != nil {
		return "", nil, err
	}
	csrf, err := base62Encode(randomBytes) // per-session synchronizer CSRF token (kept server-side)
	if err != nil {
		return "", nil, err
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

// Validate resolves a plaintext token to its live session + user, or an error.
func (s *SessionStore) Validate(ctx context.Context, plaintext string) (*models.Session, *models.User, error) {
	hash, err := HashKey(plaintext, s.tokenHashSecret)
	if err != nil {
		return nil, nil, err
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
		Where("id = ? AND revoked_at IS NULL", id).Update("revoked_at", s.nowUTC())
	if res.Error != nil {
		return fmt.Errorf("revoke session: %w", res.Error)
	}
	if res.RowsAffected == 0 {
		return ErrSessionInvalid
	}
	return nil
}

// RevokeAllForUser revokes every live session for a user (log out everywhere).
func (s *SessionStore) RevokeAllForUser(ctx context.Context, userID uuid.UUID) error {
	return s.db.WithContext(ctx).Model(&models.Session{}).
		Where("user_id = ? AND revoked_at IS NULL", userID).
		Update("revoked_at", s.nowUTC()).Error
}

func (s *SessionStore) recordSeen(id uuid.UUID) {
	s.seenMu.Lock()
	s.seen[id] = s.nowUTC()
	s.seenMu.Unlock()
}
```

- [x] **Step 4: Run → PASS.** `go test ./internal/auth/ -run TestSessionStore -v`
- [x] **Step 5: Commit** — "Add SessionStore create/validate/revoke"

### Task 1.5: Session reaper + coalesced last-seen flusher

**Files:**
- Modify: `internal/auth/session.go`
- Test: `internal/auth/session_test.go`

- [x] **Step 1: Failing tests**

```go
func TestSessionFlushSeen(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}))
	u := &models.User{ID: uuid.New(), Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db, WithSessionTTLs(time.Hour, 24*time.Hour))
	_, sess, _ := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	before := sess.IdleExpiresAt
	store.recordSeen(sess.ID)
	store.flushSeen()
	var got models.Session
	require.NoError(t, db.First(&got, "id = ?", sess.ID).Error)
	assert.NotNil(t, got.LastSeenAt)
	assert.False(t, got.IdleExpiresAt.Before(before)) // idle window bumped, not shrunk
}

func TestSessionReap(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}))
	u := &models.User{ID: uuid.New(), Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	past := time.Now().UTC().Add(-time.Hour)
	store := NewSessionStore(db, WithSessionNow(func() time.Time { return past }), WithSessionTTLs(time.Minute, time.Minute))
	_, sess, _ := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	store.now = time.Now // jump to present; the session is now expired
	n, err := store.Reap(context.Background())
	require.NoError(t, err)
	assert.GreaterOrEqual(t, n, int64(1))
	var count int64
	db.Model(&models.Session{}).Where("id = ?", sess.ID).Count(&count)
	assert.Equal(t, int64(0), count)
}
```

- [x] **Step 2: Run → FAIL.** `go test ./internal/auth/ -run 'TestSessionFlushSeen|TestSessionReap' -v`

- [x] **Step 3: Implement** (append to `internal/auth/session.go`)

```go
// flushSeen writes buffered last-seen timestamps and bumps idle expiry. Mirrors
// Service.flushLastUsed — keeps per-request writes off the hot path.
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

// RunLastSeenFlusher flushes buffered activity every 30s until ctx is cancelled.
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

// Reap deletes sessions past their absolute expiry or revoked over an hour ago.
func (s *SessionStore) Reap(ctx context.Context) (int64, error) {
	now := s.nowUTC()
	res := s.db.WithContext(ctx).
		Where("absolute_expires_at < ? OR (revoked_at IS NOT NULL AND revoked_at < ?)", now, now.Add(-time.Hour)).
		Delete(&models.Session{})
	return res.RowsAffected, res.Error
}

// RunReaper sweeps expired sessions every 10 minutes until ctx is cancelled.
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
```

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add session reaper and coalesced last-seen flusher"

### Task 1.6: `RoleMapper` — group→role resolution

**Files:**
- Create: `internal/auth/rolemap.go`
- Test: `internal/auth/rolemap_test.go`

- [x] **Step 1: Failing test**

```go
func TestRoleMapperResolve(t *testing.T) {
	m, err := NewRoleMapper("caesium-admins=admin;data-eng=operator;*=viewer", "")
	require.NoError(t, err)

	r, ok := m.Resolve([]string{"data-eng"})
	assert.True(t, ok)
	assert.Equal(t, models.RoleOperator, r)

	// highest wins
	r, ok = m.Resolve([]string{"data-eng", "caesium-admins"})
	assert.True(t, ok)
	assert.Equal(t, models.RoleAdmin, r)

	// wildcard default
	r, ok = m.Resolve([]string{"unknown"})
	assert.True(t, ok)
	assert.Equal(t, models.RoleViewer, r)
}

func TestRoleMapperDenyByDefault(t *testing.T) {
	m, err := NewRoleMapper("admins=admin", "") // no wildcard, no default
	require.NoError(t, err)
	_, ok := m.Resolve([]string{"nobody"})
	assert.False(t, ok) // deny
}

func TestRoleMapperRejectsBadRole(t *testing.T) {
	_, err := NewRoleMapper("g=superuser", "")
	assert.Error(t, err)
}
```

- [x] **Step 2: Run → FAIL.** `go test ./internal/auth/ -run TestRoleMapper -v`

- [x] **Step 3: Implement `internal/auth/rolemap.go`**

```go
package auth

import (
	"fmt"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
)

// RoleMapper resolves a set of IdP groups to a Caesium role. Highest role wins;
// "*" is an explicit wildcard. With no match and no default, access is denied.
type RoleMapper struct {
	byGroup     map[string]models.Role
	wildcard    *models.Role
	defaultRole *models.Role
}

// NewRoleMapper parses "group=role;group2=role2[;*=role]" plus an optional
// default role. Entries are ';'-separated and split on the LAST '=', so a group
// key may itself contain '=' and ',' (LDAP/AD DNs). Unknown roles are rejected.
func NewRoleMapper(mapping, defaultRole string) (*RoleMapper, error) {
	m := &RoleMapper{byGroup: map[string]models.Role{}}
	for _, entry := range strings.Split(mapping, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		eq := strings.LastIndex(entry, "=")
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("invalid role mapping entry %q (want group=role)", entry)
		}
		group := strings.TrimSpace(entry[:eq])
		role := models.Role(strings.TrimSpace(entry[eq+1:]))
		if !models.ValidRole(string(role)) {
			return nil, fmt.Errorf("invalid role %q in mapping", role)
		}
		if group == "*" {
			r := role
			m.wildcard = &r
			continue
		}
		m.byGroup[group] = role
	}
	if dr := strings.TrimSpace(defaultRole); dr != "" {
		if !models.ValidRole(dr) {
			return nil, fmt.Errorf("invalid default role %q", dr)
		}
		r := models.Role(dr)
		m.defaultRole = &r
	}
	return m, nil
}

// Resolve returns the effective role for the groups and whether access is allowed.
func (m *RoleMapper) Resolve(groups []string) (models.Role, bool) {
	var best models.Role
	matched := false
	for _, g := range groups {
		if role, ok := m.byGroup[strings.TrimSpace(g)]; ok {
			if !matched || models.RoleLevel(role) > models.RoleLevel(best) {
				best, matched = role, true
			}
		}
	}
	if matched {
		return best, true
	}
	if m.wildcard != nil {
		return *m.wildcard, true
	}
	if m.defaultRole != nil {
		return *m.defaultRole, true
	}
	return "", false
}
```

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add group-to-role mapper"

### Task 1.7: `UserStore.Upsert` — JIT provisioning

**Files:**
- Create: `internal/auth/users.go`
- Test: `internal/auth/users_test.go`

- [x] **Step 1: Failing test**

```go
func TestUserStoreUpsert(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.User{}))
	us := NewUserStore(db)

	u1, err := us.Upsert(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "sub-1", Email: "a@b.com", DisplayName: "A", Groups: []string{"x"},
	}, models.RoleViewer)
	require.NoError(t, err)
	assert.Equal(t, models.RoleViewer, u1.Role)
	assert.NotNil(t, u1.LastLoginAt)

	// second login updates role/email, keeps same row
	u2, err := us.Upsert(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "sub-1", Email: "a2@b.com", Groups: []string{"y"},
	}, models.RoleOperator)
	require.NoError(t, err)
	assert.Equal(t, u1.ID, u2.ID)
	assert.Equal(t, "a2@b.com", u2.Email)
	assert.Equal(t, models.RoleOperator, u2.Role)
}
```

- [x] **Step 2: Run → FAIL.** (`ExternalIdentity` is defined in Task 1.8; if writing 1.7 first, add the struct stub now and the interfaces in 1.8 — recommended order: do Task 1.8 Step 3's `ExternalIdentity` definition first, then this task.)

- [x] **Step 3: Implement `internal/auth/users.go`**

```go
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// UserStore provisions and updates user identities.
type UserStore struct {
	db  *gorm.DB
	now func() time.Time
}

func NewUserStore(db *gorm.DB) *UserStore { return &UserStore{db: db, now: time.Now} }

// Upsert provisions a user on first login and refreshes profile/role/last-login
// on subsequent logins, keyed on (issuer, subject).
func (us *UserStore) Upsert(ctx context.Context, ext *ExternalIdentity, role models.Role) (*models.User, error) {
	now := us.now().UTC()
	groupsJSON, err := json.Marshal(ext.Groups)
	if err != nil {
		return nil, fmt.Errorf("marshal groups: %w", err)
	}
	var user models.User
	err = us.db.WithContext(ctx).Where("issuer = ? AND subject = ?", ext.Issuer, ext.Subject).First(&user).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		user = models.User{
			ID: uuid.New(), Issuer: ext.Issuer, Subject: ext.Subject, Email: ext.Email,
			DisplayName: ext.DisplayName, Groups: groupsJSON, Role: role, CreatedAt: now, LastLoginAt: &now,
		}
		if err := us.db.WithContext(ctx).Create(&user).Error; err != nil {
			return nil, fmt.Errorf("create user: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("lookup user: %w", err)
	default:
		updates := map[string]any{
			"email": ext.Email, "display_name": ext.DisplayName, "groups": groupsJSON,
			"role": role, "last_login_at": now,
		}
		if err := us.db.WithContext(ctx).Model(&user).Updates(updates).Error; err != nil {
			return nil, fmt.Errorf("update user: %w", err)
		}
		user.Email, user.DisplayName, user.Groups, user.Role, user.LastLoginAt = ext.Email, ext.DisplayName, groupsJSON, role, &now
	}
	return &user, nil
}
```

> Add `import "time"` to the block. Place `ExternalIdentity` (Task 1.8) before this file compiles.

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add JIT user provisioning"

### Task 1.8: Provider interfaces + shared login pipeline

**Files:**
- Create: `internal/auth/provider.go`
- Test: `internal/auth/provider_test.go`

- [x] **Step 1: Failing test** (a fake identity exercises the full provision→map→mint tail)

```go
func TestSSOServiceComplete(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.AutoMigrate(&models.User{}, &models.Session{}))
	mapper, _ := NewRoleMapper("eng=operator", "")
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper)

	cookie, sess, err := sso.Complete(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "s1", Email: "a@b.com", Groups: []string{"eng"},
	}, "oidc", "1.2.3.4", "agent")
	require.NoError(t, err)
	assert.NotEmpty(t, cookie)
	assert.Equal(t, "oidc", sess.AuthMethod)

	// denied when no group maps
	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{Issuer: "oidc", Subject: "s2", Groups: []string{"none"}}, "oidc", "", "")
	assert.ErrorIs(t, err, ErrLoginDenied)
}
```

- [x] **Step 2: Run → FAIL.** `go test ./internal/auth/ -run TestSSOServiceComplete -v`

- [x] **Step 3: Implement `internal/auth/provider.go`**

```go
package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/internal/models"
)

// ErrLoginDenied is returned when an external identity maps to no role.
var ErrLoginDenied = errors.New("login denied: no role mapping")

// ExternalIdentity is the normalized identity every provider produces.
type ExternalIdentity struct {
	Issuer      string
	Subject     string
	Email       string
	DisplayName string
	Groups      []string
}

// RedirectAuthenticator is implemented by browser-redirect providers (OIDC, SAML).
type RedirectAuthenticator interface {
	Name() string
	Begin(w http.ResponseWriter, r *http.Request, returnTo string) (redirectURL string, err error)
	Complete(r *http.Request) (*ExternalIdentity, error)
}

// CredentialAuthenticator is implemented by credential providers (LDAP).
type CredentialAuthenticator interface {
	Name() string
	Authenticate(ctx context.Context, username, password string) (*ExternalIdentity, error)
}

// SSOService is the shared tail: provision the user, map a role, mint a session.
type SSOService struct {
	users    *UserStore
	sessions *SessionStore
	roles    *RoleMapper
}

func NewSSOService(users *UserStore, sessions *SessionStore, roles *RoleMapper) *SSOService {
	return &SSOService{users: users, sessions: sessions, roles: roles}
}

// Complete turns an authenticated ExternalIdentity into a session, returning the
// cookie value. Returns ErrLoginDenied when no role maps.
func (s *SSOService) Complete(ctx context.Context, ext *ExternalIdentity, method, ip, ua string) (string, *models.Session, error) {
	role, ok := s.roles.Resolve(ext.Groups)
	if !ok {
		return "", nil, ErrLoginDenied
	}
	user, err := s.users.Upsert(ctx, ext, role)
	if err != nil {
		return "", nil, err
	}
	return s.sessions.Create(ctx, CreateSessionRequest{
		UserID: user.ID, AuthMethod: method, SourceIP: ip, UserAgent: ua,
	})
}
```

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add provider interfaces and shared SSO login pipeline"

### Task 1.9: Session-bound CSRF (synchronizer token)

**Files:**
- Create: `api/middleware/csrf.go`
- Test: `api/middleware/csrf_test.go`

CSRF is bound to the server-side session (synchronizer-token pattern): each session carries a
`csrf_token` (Tasks 1.2/1.4), and for cookie-authenticated unsafe methods the request must echo
it in `X-CSRF-Token`. The check runs *inside* the auth middleware (Task 1.10) once the session is
resolved, comparing against server-side state — there is **no readable CSRF cookie**, so subdomain
cookie injection cannot forge it. This file provides the check helper, method classification, and
the context plumbing that `/auth/whoami` uses to surface the token.

- [x] **Step 1: Failing test**

```go
func TestEnforceSessionCSRF(t *testing.T) {
	// safe method: allowed regardless of header
	c, _ := newCtx(http.MethodGet, "/v1/jobs", nil)
	assert.NoError(t, EnforceSessionCSRF(c, "tok123"))

	// unsafe + matching header: allowed
	c, _ = newCtx(http.MethodPost, "/v1/jobs", map[string]string{"X-CSRF-Token": "tok123"})
	assert.NoError(t, EnforceSessionCSRF(c, "tok123"))

	// unsafe + missing header: 403
	c, _ = newCtx(http.MethodPost, "/v1/jobs", nil)
	assert.Error(t, EnforceSessionCSRF(c, "tok123"))

	// unsafe + mismatched header: 403
	c, _ = newCtx(http.MethodPost, "/v1/jobs", map[string]string{"X-CSRF-Token": "wrong"})
	err := EnforceSessionCSRF(c, "tok123")
	he, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	assert.Equal(t, http.StatusForbidden, he.Code)
}
```

> `newCtx` helper: build an Echo context from method/path/headers (mirror the setup already used in `api/middleware/auth_test.go`; extract a shared helper if convenient).

- [x] **Step 2: Run → FAIL.** `go test ./api/middleware/ -run TestEnforceSessionCSRF -v`

- [x] **Step 3: Implement `api/middleware/csrf.go`**

```go
package middleware

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

// ContextKeyCSRFToken stores the resolved session's CSRF token for handlers (e.g. whoami).
const ContextKeyCSRFToken = "auth.csrf_token"

// EnforceSessionCSRF validates the synchronizer CSRF token for cookie-authenticated unsafe
// requests. `expected` is the resolved session's csrf_token. Safe methods always pass. The auth
// middleware calls this only for session principals; Bearer/API-key requests are exempt at the
// call site (no ambient cookie → not forgeable cross-site).
func EnforceSessionCSRF(c *echo.Context, expected string) error {
	if isSafeMethod(c.Request().Method) {
		return nil
	}
	header := c.Request().Header.Get("X-CSRF-Token")
	if expected == "" || header == "" ||
		subtle.ConstantTimeCompare([]byte(header), []byte(expected)) != 1 {
		return echo.NewHTTPError(http.StatusForbidden, "invalid csrf token")
	}
	return nil
}

// GetCSRFToken returns the session CSRF token stashed by the auth middleware, or "".
func GetCSRFToken(c *echo.Context) string {
	if v, ok := c.Get(ContextKeyCSRFToken).(string); ok {
		return v
	}
	return ""
}

func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	}
	return false
}

func hasBearer(c *echo.Context) bool {
	h := c.Request().Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "Bearer ")
}
```

(`hasBearer` is used by the Task 1.10 auth middleware to take the API-key branch.)

- [x] **Step 4: Run → PASS.** **Step 5: Commit** — "Add session-bound CSRF synchronizer check"

### Task 1.10: Resolve session-cookie principals in the auth middleware

**Files:**
- Modify: `api/middleware/auth.go` (`Auth` signature + body)
- Modify: `api/rest/bind/bind.go` (pass new deps; gate on auth-enabled)
- Test: `api/middleware/auth_test.go`

- [x] **Step 1: Failing test** — a valid session cookie authenticates a `GET /v1/jobs` as its user's role

```go
func TestAuthAcceptsSessionCookie(t *testing.T) {
	// Build svc + sessions over a shared test DB; create a viewer user + session.
	// Construct Auth(AuthDeps{Service: svc, Sessions: sessions, CookieName: "caesium_session", ...}).
	// Issue GET /v1/jobs with Cookie: caesium_session=<plaintext>; expect the request
	// to reach the handler with GetPrincipal(c).Kind == PrincipalUser and Role viewer.
	// (Mirror the existing API-key happy-path test in this file for wiring.)
}
```

Flesh this out following the existing API-key test's harness in the same file (reuse its DB/echo setup; add a session via `SessionStore.Create`).

- [x] **Step 2: Run → FAIL.**

- [x] **Step 3: Implement.** Introduce a deps struct so the signature doesn't grow unbounded:

```go
// AuthDeps bundles the dependencies the auth middleware needs.
type AuthDeps struct {
	Service    *auth.Service
	Auditor    *auth.AuditLogger
	Limiter    *auth.RateLimiter
	Sessions   *auth.SessionStore // nil when SSO disabled
	CookieName string
}

func Auth(d AuthDeps) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			path := c.Request().URL.Path
			if skipPaths[path] {
				return next(c)
			}
			ip := c.RealIP()
			if d.Limiter.IsLimited(ip) { /* unchanged rate-limit block */ }

			var principal *auth.Principal
			var key *models.APIKey

			if token := extractBearerToken(c); token != "" {
				k, err := d.Service.ValidateKey(token)
				if err != nil { /* unchanged failure handling using d.Auditor/d.Limiter */ }
				key = k
				principal = auth.PrincipalFromKey(k)
			} else if d.Sessions != nil {
				if cookie, err := c.Request().Cookie(d.CookieName); err == nil && cookie.Value != "" {
					sess, user, verr := d.Sessions.Validate(c.Request().Context(), cookie.Value)
					if verr != nil {
						metrics.AuthFailuresTotal.WithLabelValues("session_invalid").Inc()
						return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired session")
					}
					// Session-bound CSRF for cookie auth (Bearer path is exempt — no cookie).
					if cerr := EnforceSessionCSRF(c, sess.CSRFToken); cerr != nil {
						return cerr
					}
					principal = auth.PrincipalFromUser(user)
					c.Set(ContextKeyCSRFToken, sess.CSRFToken) // surfaced via /auth/whoami
				}
			}

			if principal == nil {
				metrics.AuthFailuresTotal.WithLabelValues("missing").Inc()
				return echo.NewHTTPError(http.StatusUnauthorized, "missing credentials")
			}

			// RBAC + scope on principal (as refactored in Task 0.2), then:
			if key != nil {
				c.Set(ContextKeyAuth, key)
			}
			c.Set(ContextKeyPrincipal, principal)
			// ...continue (next, metrics, audit) using principal.Subject/Role...
		}
	}
}
```

Keep the existing rate-limit, failure-audit, RBAC, and scope blocks — only the credential-resolution part changes. Update `bind.go`:

```go
	if env.Variables().AuthMode == "api-key" || env.Variables().SSOEnabled() {
		deps := authmw.AuthDeps{Service: authSvc, Auditor: auditor, Limiter: limiter, Sessions: sessions, CookieName: env.Variables().AuthSessionCookieName}
		protected.Use(authmw.Auth(deps)) // session-bound CSRF is enforced inside Auth
		if authSvc != nil {
			bindAuth(protected, authctrl.New(authSvc, auditor))
		}
	}
```

This requires `All(...)` to receive `sessions *auth.SessionStore`. Add it to the `bind.All` signature and thread it from `api.Start`.

- [x] **Step 4: Run the full middleware + auth suites + gate.** Update every existing `authmw.Auth(svc, auditor, limiter)` call site (tests + `api.go` metrics) to the new `AuthDeps` form. `go test ./api/... ./internal/auth/ -v && just unit-test`.

- [x] **Step 5: Commit** — "Resolve session-cookie principals in auth middleware"

### Task 1.11: `/auth/whoami`, `/auth/logout`, extended `/auth/status`

**Files:**
- Create: `api/rest/controller/auth/sso.go`
- Modify: `api/api.go` (`authStatus`; mount public SSO routes), `api/rest/bind/bind.go` (`bindAuth` adds whoami/logout)
- Test: `api/rest/controller/auth/sso_test.go`

- [x] **Step 1: Failing test** — whoami returns 401 without a session and the principal's identity with one; logout revokes.

```go
func TestWhoamiLogout(t *testing.T) {
	// With SSOController over a test DB + session: GET /auth/whoami with a valid
	// cookie returns {email, role, kind:"user"}; POST /auth/logout revokes the
	// session and clears the cookie (Set-Cookie max-age<=0); subsequent whoami → 401.
}
```

- [x] **Step 2: Run → FAIL.**

- [x] **Step 3: Implement `api/rest/controller/auth/sso.go`**

```go
package auth

import (
	"net/http"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/labstack/echo/v5"
)

// SSOController serves session-aware endpoints (whoami/logout). Login/callback
// handlers are added per-provider in P2-P4.
type SSOController struct {
	sessions   *iauth.SessionStore
	cookieName string
}

func NewSSO(sessions *iauth.SessionStore, cookieName string) *SSOController {
	return &SSOController{sessions: sessions, cookieName: cookieName}
}

func (s *SSOController) Whoami(c *echo.Context) error {
	p := authmw.GetPrincipal(c)
	if p == nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "not authenticated")
	}
	body := map[string]any{"kind": p.Kind, "subject": p.Subject, "role": p.Role}
	if csrf := authmw.GetCSRFToken(c); csrf != "" { // session principals carry a CSRF token
		body["csrf_token"] = csrf
	}
	return c.JSON(http.StatusOK, body)
}

func (s *SSOController) Logout(c *echo.Context) error {
	if cookie, err := c.Request().Cookie(s.cookieName); err == nil && cookie.Value != "" {
		if sess, _, verr := s.sessions.Validate(c.Request().Context(), cookie.Value); verr == nil {
			_ = s.sessions.Revoke(c.Request().Context(), sess.ID)
		}
	}
	c.SetCookie(&http.Cookie{Name: s.cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode})
	return c.NoContent(http.StatusNoContent)
}
```

(`Whoami` is mounted behind the protected group so `GetPrincipal` is populated; `Logout` does its own cookie read so it works regardless.)

- [x] **Step 4: Extend `authStatus` in `api/api.go`:**

```go
func authStatus(vars env.Environment) echo.HandlerFunc {
	return func(c *echo.Context) error {
		methods := []map[string]string{}
		if vars.AuthMode == "api-key" {
			methods = append(methods, map[string]string{"type": "api-key"})
		}
		if vars.AuthOIDCEnabled {
			methods = append(methods, map[string]string{"type": "oidc", "loginUrl": "/auth/sso/oidc/login"})
		}
		if vars.AuthSAMLEnabled {
			methods = append(methods, map[string]string{"type": "saml", "loginUrl": "/auth/sso/saml/login"})
		}
		if vars.AuthLDAPEnabled {
			methods = append(methods, map[string]string{"type": "ldap"})
		}
		return c.JSON(http.StatusOK, map[string]any{
			"enabled": vars.AuthMode == "api-key" || vars.SSOEnabled(),
			"methods": methods,
		})
	}
}
```

Add `g.GET("/auth/whoami", ssoCtrl.Whoami)` to `bindAuth` (protected) and `e.POST("/auth/logout", ssoCtrl.Logout)` near `/auth/status` in `api.Start` (public — reads cookie directly).

- [x] **Step 5: Run → PASS, gate, commit** — "Add whoami/logout endpoints and method-aware auth status"

### Task 1.12: Wire session services into startup

**Files:**
- Modify: `cmd/start/start.go` (`initAuth` + its return/threading), `api/api.go` (`Start` signature), `api/rest/bind/bind.go` (`All` signature)
- Test: covered by an integration smoke (server boots with SSO env) — add to `test/` if a server-boot harness exists; otherwise verify via `just unit-test` compile + a manual boot.

- [x] **Step 1:** In `initAuth`, after building `authSvc/auditor/limiter`, when `vars.AuthMode == "api-key" || vars.SSOEnabled()` build the session stack and return it (extend the return tuple or, preferably, return an `auth.Deps`-style bundle):

```go
	sessions := auth.NewSessionStore(conn,
		auth.WithSessionHashSecret(vars.AuthKeyHashSecret),
		auth.WithSessionTTLs(vars.AuthSessionIdleTTL, vars.AuthSessionAbsoluteTTL),
	)
	if vars.SSOEnabled() {
		if vars.AuthMode == "none" || vars.AuthMode == "" {
			log.Fatal("SSO providers require authentication to be enabled (set CAESIUM_AUTH_MODE=api-key)")
		}
		mapper, err := auth.NewRoleMapper(vars.AuthRoleMapping, vars.AuthDefaultRole)
		if err != nil {
			log.Fatal("invalid CAESIUM_AUTH_ROLE_MAPPING", "error", err)
		}
		_ = auth.NewSSOService(auth.NewUserStore(conn), sessions, mapper) // passed to provider controllers in P2-P4
		runAsync(func() { sessions.RunLastSeenFlusher(ctx) })
		runAsync(func() { sessions.RunReaper(ctx) })
	}
```

- [x] **Step 2:** Thread `sessions` (and later the SSO service + providers) through `api.Start(...)` → `bind.All(...)`. Update both signatures and the call site at `cmd/start/start.go:527`.

- [x] **Step 3:** `go build ./... ` (via the project toolchain) and `just unit-test`.

- [x] **Step 4: Manual boot smoke** (document the command in the PR):

```bash
CAESIUM_AUTH_MODE=api-key CAESIUM_AUTH_KEY_HASH_SECRET=$(openssl rand -hex 32) \
CAESIUM_AUTH_REQUIRE_TLS=false CAESIUM_AUTH_OIDC_ENABLED=true \
CAESIUM_AUTH_ROLE_MAPPING='*=viewer' just run   # expect: boots, /auth/status lists oidc
```

- [x] **Step 5: Commit** — "Wire session store, reaper, and flusher into startup"

### Task 1.13: UI session (cookie) mode

**Files:**
- Modify: `ui/src/lib/auth.ts`, `ui/src/features/auth/AuthGate.tsx`
- Test: `ui/src/lib/__tests__/auth.test.ts`, `ui/src/features/auth/__tests__/AuthGate.test.tsx`

- [x] **Step 1: Failing test** — `AuthGate` treats an established session (`/auth/whoami` 200) as authed without the API-key box; `withAuthHeaders` adds `X-CSRF-Token` from the token cached from `/auth/whoami`.

- [x] **Step 2: Run → FAIL.** `cd ui && npx vitest run src/lib/__tests__/auth.test.ts`

- [x] **Step 3: Implement.** In `auth.ts`, add session detection + CSRF header (from the in-memory token `checkSession()` captures from `/auth/whoami`, not a cookie) and a `checkSession()` that calls `/auth/whoami` with `credentials: "include"`. In `AuthGate.tsx`, after the `/auth/status` probe, also call `/auth/whoami`; if it returns 200, render children (cookie session active). Keep the in-memory API-key path intact. SSO "Sign in with…" buttons + LDAP form are added in the provider plans (they need login URLs from `/auth/status.methods`).

```ts
// auth.ts additions — CSRF token cached from /auth/whoami (synchronizer pattern; never a cookie)
let csrfToken: string | null = null;

export function csrfHeader(): Record<string, string> {
  return csrfToken ? { "X-CSRF-Token": csrfToken } : {};
}

// checkSession reports whether a cookie session is active and captures its CSRF token.
export async function checkSession(): Promise<boolean> {
  try {
    const r = await fetch("/auth/whoami", { credentials: "include" });
    if (!r.ok) return false;
    const body = (await r.json()) as { csrf_token?: string };
    csrfToken = body.csrf_token ?? null;
    return true;
  } catch {
    return false;
  }
}
```

Merge `csrfHeader()` into `withAuthHeaders` for unsafe requests, and include `credentials: "include"` on same-origin fetches in the API client.

- [x] **Step 4: Run → PASS.** `cd ui && npx vitest run`
- [x] **Step 5: Commit** — "Add UI cookie-session mode and CSRF header"

---

## Phase 1 exit criteria

- `just unit-test` green; `cd ui && npx vitest run` green.
- Server boots with `CAESIUM_AUTH_OIDC_ENABLED=true` and `/auth/status` lists `oidc`.
- A session created via `SessionStore.Create` authenticates a `/v1` request as its user's role (proven in Task 1.10's test).
- No provider exists yet — browser login is delivered by the next plan.

---

## Follow-on plans (P2–P5) — roadmap

Each becomes its own `docs/superpowers/plans/2026-…-sso-<phase>.md`, written just-in-time against the real library APIs once this foundation merges. Summary of scope so reviewers see the whole arc:

### P2 — OIDC provider (`coreos/go-oidc/v3` + `golang.org/x/oauth2`)
- [x] **Wave 2 status (PR #193):** Implemented the OIDC redirect provider, `AUTH_OIDC_*`
  config, login/callback route wiring, session-cookie completion, UI "Sign in with OIDC"
  affordance, and focused mock-provider tests.
- [x] **Wave 7 status:** Added OIDC ID-token audience mismatch coverage,
  asserting callbacks fail with `ErrInvalidIDToken`.
- [x] **Wave 8 status:** Added OIDC callback hardening for tampered and
  expired pre-login state cookies plus early authorization-error/missing-code
  returns, asserting no token exchange occurs on fail-closed paths.
- **Files:** `internal/auth/oidc/provider.go` (implements `RedirectAuthenticator`), provider config in `pkg/env/env.go` (`AUTH_OIDC_*`), login/callback handlers in `api/rest/controller/auth/sso.go`, route mounts in `api.Start`, UI "Sign in with OIDC" button.
- **Key work:** discovery; Authorization Code + PKCE; `state`/`nonce` in a short-lived signed pre-login cookie; ID-token signature + `iss`/`aud`/`exp`/`nonce` verification; groups from `AUTH_OIDC_GROUPS_CLAIM` → `ExternalIdentity` → `SSOService.Complete` (mints session + per-session CSRF token) → set session cookie → redirect to validated `returnTo`.
- **Tests:** against a mock OIDC provider (in-process JWKS); negative cases
  (bad state, tampered/expired state cookie, early callback errors, bad nonce,
  expired token, audience mismatch).

### P3 — SAML provider (`crewjam/saml`)  *(highest risk)*
- [x] **Wave 3 status (PR #194):** Implemented the SAML redirect provider foundation:
  `AUTH_SAML_*` config, HTTPS-only IdP metadata loading, SP metadata, login/ACS
  route wiring, RelayState return targets, dqlite-backed assertion replay
  storage, group-attribute mapping into the shared SSO tail, and UI rendering
  for advertised redirect methods.
- [x] **Wave 6 status:** Added self-contained signed SAMLResponse fixture
  coverage through `Provider.CompleteWithReturnTo`, including accepted signed
  assertions, tampered response rejection, replay rejection, and expired
  assertion rejection.
- [x] **Wave 7 status:** Added signed SAMLResponse negative fixture coverage
  for wrong audience restrictions, asserting the response is rejected before
  replay state is recorded.
- [x] **Wave 8 status:** Added SAML callback hardening for tampered and
  expired pre-login state cookies, asserting invalid state is rejected before
  SAMLResponse validation.
- **Files:** `internal/auth/saml/provider.go`, `AUTH_SAML_*` config, ACS + metadata routes.
- **Key work:** SP metadata; IdP metadata fetched **HTTPS-only with TLS certificate verification**; XML-dsig verification; `Audience`/`Recipient`/`NotOnOrAfter` with clock-skew leeway; **dqlite-backed** assertion replay cache (`saml_assertion_ids`, not per-node in-memory); RelayState = validated `returnTo`; groups from attribute → shared tail.
- **Tests:** signed assertion fixtures incl. tampered/expired/replayed/wrong
  audience; tampered/expired state cookies; SP metadata round-trip.

### P4 — LDAP provider (`go-ldap/ldap/v3`)
- [x] **Wave 4 status:** Implemented the LDAP `CredentialAuthenticator`,
  `AUTH_LDAP_*` config, startup provider initialization, `/auth/sso/ldap/login`
  with shared session-cookie completion and per-IP failed-login limiting, and
  the LDAP username/password UI. Unit coverage exercises config validation,
  LDAP filter escaping, search-then-bind flow, group mapping, route mounting,
  and credential-form behavior.
- [x] **Wave 5 status:** Strengthened the LDAP integration test contract so a
  seeded real directory can assert profile fields, groups, and role resolution;
  added hermetic coverage for full-DN role mapping from LDAP group search
  results.
- [x] **Wave 6 status:** Added a self-contained OpenLDAP integration fixture
  seeded from checked-in LDIF. It starts `osixia/openldap:1.5.0`, authenticates
  through the real LDAP provider, and asserts profile fields, full-DN groups,
  and role mapping; it skips cleanly when Docker is unavailable.
- [x] **Wave 7 status:** Added fail-closed LDAP negative coverage for group
  search failures after successful user credential verification.
- [x] **Wave 8 status:** Added fail-closed LDAP edge coverage for nil user
  search results and user entries with missing DNs before any user bind.
- **Files:** `internal/auth/ldap/provider.go` (implements `CredentialAuthenticator`), `AUTH_LDAP_*` config, `POST /auth/sso/ldap/login` handler (rate-limited), UI username/password form.
- **Key work:** LDAPS/StartTLS; service bind → user search (`USER_FILTER`) → rebind to verify; group query (`GROUP_FILTER`); **escape all user-supplied input with `ldap.EscapeFilter` before substitution** (LDAP-injection guard); reject empty-password/anonymous bind.
- **Tests:** real-directory LDAP integration assertions (skipped unless
  `CAESIUM_TEST_LDAP_*` is set) plus hermetic group/role edge-case coverage;
  self-contained OpenLDAP fixture coverage.

### P5 — Docs + hardening
- [x] **Docs draft status:** Created `docs/sso-authentication.md` for LDAP
  operator setup, role mapping, session TTL/cookie tuning, TLS prerequisites,
  LDAP filter placeholders, and OIDC/SAML/LDAP setup.
- [x] **Wave 5 status:** Added security regressions for unsafe encoded
  same-origin redirects, LDAP non-redirect credential completion, CSRF header
  enforcement, readable-cookie CSRF bypass attempts, API-key precedence over
  session cookies, `/auth/whoami` session CSRF surfacing, SSO audit actions
  (`auth.login`, `auth.login_denied`, `auth.logout`, `auth.session_revoked`),
  and Prometheus SSO login/logout metrics.
- [x] **Wave 6 status:** Added signed SAML fixture coverage, a
  self-contained OpenLDAP fixture, and the `user.provisioned` audit event for
  first-time SSO user creation.
- [x] **Wave 7 status:** Added provider-specific negative fixtures for OIDC
  audience mismatch, SAML wrong audience restrictions, and LDAP group-search
  fail-closed behavior.
- [x] **Wave 8 status:** Completed the remaining P5 hardening pass by covering
  fail-closed OIDC/SAML pre-login state handling, malformed LDAP search results,
  trusted-proxy same-origin return targets, and secure session-cookie flags for
  redirect-provider callbacks. Prior P5 waves covered CSRF, audit, metrics, and
  operator docs.
- [x] **Wave 9 status:** Covered remaining foundation hardening: fail-closed
  shared identity validation, session idle-refresh correctness, malformed
  `Authorization` header rejection before cookie-session fallback, OIDC/SAML
  provider identity and state-cookie checks, UI cookie-session fail-closed/logout
  handling, and operator docs/roadmap cleanup.
- [x] **Wave 10 status:** Added one-time cleanup for OIDC/SAML pre-login state
  cookies after redirect callbacks, preserving provider cookie policy on expiry,
  required OIDC `azp` for multi-audience ID tokens, rejected invalid trusted
  proxy entries in the auth TLS startup guard, and corrected SSO role-mapping
  docs/tests to include the supported `runner` role.
- [x] **Wave 11 status:** Documented the visible browser-auth endpoint
  contract for `/auth/status` method objects, `/auth/whoami` session CSRF
  surfacing, `/auth/logout` CSRF requirements, API-key/SSO coexistence, and
  the operator-console sign-out affordance.
- **Files:** `docs/sso-authentication.md` (operator setup for all three + role mapping + session tuning + TLS), README/roadmap updates.
- **Key work:** end-to-end security pass; verify cookie flags/CSRF/open-redirect guards across providers; finalize metrics + audit actions (`auth.login`, `auth.logout`, `auth.session_revoked`, `user.provisioned`, `auth.login_denied`).

---

## Self-review notes

- **Spec coverage:** §5.1 Principal → Tasks 0.1–0.3, 1.10; §6 data model → 1.2; §7 config → 1.1; §9 role mapping → 1.6; §10 sessions → 1.4/1.5; §11 middleware/CSRF/endpoints → 1.9–1.11; §5.2 provider interface + login tail → 1.8; UI → 1.13; providers §8 → P2–P4 plans; docs §17 → P5.
- **Type consistency:** `Principal.Scope` is `[]byte` (matches `auth.ScopeJobs`/`CheckScope` and `models.APIKey.Scope` = `datatypes.JSON`); `SSOService.Complete` returns `(cookieValue string, *models.Session, error)` consistent with `SessionStore.Create`; `RoleMapper.Resolve` returns `(models.Role, bool)` consumed by `Complete`.
- **Open follow-through for the executor:** `ExternalIdentity` (Task 1.8) must exist before Tasks 1.7/1.8 tests compile — do Task 1.8's struct first if ordering causes a compile gap. Reuse one `newTestDB`/`newCtx` helper across `internal/auth` and `api/middleware` tests rather than duplicating.
