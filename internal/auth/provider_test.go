package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSSOServiceComplete(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	mapper, err := NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper)

	cookie, sess, err := sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "s1",
		Email:   "a@b.com",
		Groups:  []string{"eng"},
	}, "oidc", "1.2.3.4", "agent")
	require.NoError(t, err)
	require.NotEmpty(t, cookie)
	require.Equal(t, "oidc", sess.AuthMethod)

	var user models.User
	require.NoError(t, db.First(&user, "subject = ?", "s1").Error)
	require.Equal(t, models.RoleOperator, user.Role)

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "s2",
		Groups:  []string{"none"},
	}, "oidc", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)
}

func TestSSOServiceCompleteRejectsInvalidExternalIdentity(t *testing.T) {
	tests := []struct {
		name string
		ext  *ExternalIdentity
	}{
		{
			name: "nil",
			ext:  nil,
		},
		{
			name: "blank issuer",
			ext: &ExternalIdentity{
				Issuer:  "",
				Subject: "sub",
				Groups:  []string{"eng"},
			},
		},
		{
			name: "whitespace issuer",
			ext: &ExternalIdentity{
				Issuer:  " \t\n",
				Subject: "sub",
				Groups:  []string{"eng"},
			},
		},
		{
			name: "blank subject",
			ext: &ExternalIdentity{
				Issuer:  "oidc",
				Subject: "",
				Groups:  []string{"eng"},
			},
		},
		{
			name: "whitespace subject",
			ext: &ExternalIdentity{
				Issuer:  "oidc",
				Subject: " \t\n",
				Groups:  []string{"eng"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)
			mapper, err := NewRoleMapper("eng=operator", "")
			require.NoError(t, err)
			sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper)

			cookie, sess, err := sso.Complete(context.Background(), tt.ext, "oidc", "1.2.3.4", "agent")
			require.ErrorIs(t, err, ErrInvalidExternalIdentity)
			require.Empty(t, cookie)
			require.Nil(t, sess)

			var users int64
			require.NoError(t, db.Model(&models.User{}).Count(&users).Error)
			require.Zero(t, users)

			var sessions int64
			require.NoError(t, db.Model(&models.Session{}).Count(&sessions).Error)
			require.Zero(t, sessions)
		})
	}
}

func TestSSOServiceCompleteRejectsDisabledUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	mapper, err := NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper)

	disabledAt := time.Now().UTC()
	user := &models.User{
		ID:         uuid.New(),
		Issuer:     "oidc",
		Subject:    "disabled-sub",
		Email:      "old@example.com",
		Role:       models.RoleViewer,
		CreatedAt:  disabledAt,
		DisabledAt: &disabledAt,
	}
	require.NoError(t, db.Create(user).Error)

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "disabled-sub",
		Email:   "new@example.com",
		Groups:  []string{"eng"},
	}, "oidc", "1.2.3.4", "agent")
	require.ErrorIs(t, err, ErrLoginDenied)

	var got models.User
	require.NoError(t, db.First(&got, "id = ?", user.ID).Error)
	require.Equal(t, "old@example.com", got.Email)
	require.Equal(t, models.RoleViewer, got.Role)
	require.Nil(t, got.LastLoginAt)

	var sessions int64
	require.NoError(t, db.Model(&models.Session{}).Where("user_id = ?", user.ID).Count(&sessions).Error)
	require.Zero(t, sessions)
}

func TestSSOServiceCompleteAuditsAndRecordsMetrics(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := NewAuditLogger(db)
	mapper, err := NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper, WithSSOAuditLogger(auditor))

	successBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", OutcomeSuccess)
	deniedBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", OutcomeDenied)

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "sub-success",
		Email:   "viewer@example.com",
		Groups:  []string{"eng"},
	}, "oidc", "203.0.113.10", "agent")
	require.NoError(t, err)

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "sub-denied",
		Email:   "denied@example.com",
		Groups:  []string{"unknown"},
	}, "oidc", "203.0.113.11", "agent")
	require.ErrorIs(t, err, ErrLoginDenied)

	require.Equal(t, successBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", OutcomeSuccess))
	require.Equal(t, deniedBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "oidc", OutcomeDenied))

	loginEntries, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLogin, Limit: 10})
	require.NoError(t, err)
	require.Len(t, loginEntries, 1)
	require.Equal(t, "viewer@example.com", loginEntries[0].Actor)
	require.Equal(t, OutcomeSuccess, loginEntries[0].Outcome)
	require.Equal(t, "session", loginEntries[0].ResourceType)

	deniedEntries, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLoginDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, deniedEntries, 1)
	require.Equal(t, "denied@example.com", deniedEntries[0].Actor)
	require.Equal(t, OutcomeDenied, deniedEntries[0].Outcome)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(deniedEntries[0].Metadata, &metadata))
	require.Equal(t, "no_binding", metadata["reason"])
	require.Equal(t, "oidc", metadata["provider"])
}

func TestSSOServiceCompleteAuditsUserProvisionedOnce(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := NewAuditLogger(db)
	mapper, err := NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), mapper, WithSSOAuditLogger(auditor))

	identity := &ExternalIdentity{
		Issuer:      "oidc",
		Subject:     "sub-provisioned",
		Email:       "first@example.com",
		DisplayName: "First Login",
		Groups:      []string{"eng"},
	}
	_, _, err = sso.Complete(context.Background(), identity, "oidc", "203.0.113.20", "agent")
	require.NoError(t, err)

	provisionedEntries, err := auditor.Query(&AuditQueryRequest{Action: ActionUserProvisioned, Limit: 10})
	require.NoError(t, err)
	require.Len(t, provisionedEntries, 1)
	require.Equal(t, "first@example.com", provisionedEntries[0].Actor)
	require.Equal(t, OutcomeSuccess, provisionedEntries[0].Outcome)
	require.Equal(t, "user", provisionedEntries[0].ResourceType)
	require.NotEmpty(t, provisionedEntries[0].ResourceID)

	loginEntries, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLogin, Limit: 10})
	require.NoError(t, err)
	require.Len(t, loginEntries, 1)

	var metadata map[string]any
	require.NoError(t, json.Unmarshal(provisionedEntries[0].Metadata, &metadata))
	require.Equal(t, "oidc", metadata["provider"])
	require.Equal(t, "oidc", metadata["issuer"])
	require.Equal(t, string(models.RoleOperator), metadata["role"])

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer:      identity.Issuer,
		Subject:     identity.Subject,
		Email:       "second@example.com",
		DisplayName: "Second Login",
		Groups:      []string{"eng"},
	}, "oidc", "203.0.113.21", "agent")
	require.NoError(t, err)

	provisionedEntries, err = auditor.Query(&AuditQueryRequest{Action: ActionUserProvisioned, Limit: 10})
	require.NoError(t, err)
	require.Len(t, provisionedEntries, 1)

	loginEntries, err = auditor.Query(&AuditQueryRequest{Action: ActionAuthLogin, Limit: 10})
	require.NoError(t, err)
	require.Len(t, loginEntries, 2)
}

func TestSSOServiceCompletePolicyGroupBindingLogsIn(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: ["CN=Ops,DC=example"]}
    role: operator
    namespaces: ["*"]
`))
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), &policy.Static{Policy: p})

	// Policy-file groups are byte-exact: the env adapter's trimming does not apply.
	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "padded", Groups: []string{" CN=Ops,DC=example "},
	}, "oidc", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)

	cookie, _, err := sso.Complete(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "exact", Email: "ops@example.com", Groups: []string{"CN=Ops,DC=example"},
	}, "oidc", "", "")
	require.NoError(t, err)
	require.NotEmpty(t, cookie)

	var user models.User
	require.NoError(t, db.First(&user, "subject = ?", "exact").Error)
	require.Equal(t, models.RoleOperator, user.Role)
}

func TestSSOServiceCompleteEmailOnlyBindingRequiresVerifiedEmail(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	auditor := NewAuditLogger(db)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), emailOnlyAdminSource(t), WithSSOAuditLogger(auditor))

	ext := &ExternalIdentity{Issuer: "oidc", Subject: "admin-sub", Email: "admin@example.com", EmailVerified: false}
	cookie, sess, err := sso.Complete(context.Background(), ext, "oidc", "203.0.113.30", "agent")
	require.ErrorIs(t, err, ErrLoginDenied)
	require.Empty(t, cookie)
	require.Nil(t, sess)
	requireUserAndSessionCounts(t, db, 0, 0)

	denied, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLoginDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, denied, 1)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(denied[0].Metadata, &metadata))
	require.Equal(t, "no_binding", metadata["reason"])

	ext.EmailVerified = true
	cookie, _, err = sso.Complete(context.Background(), ext, "oidc", "203.0.113.30", "agent")
	require.NoError(t, err)
	require.NotEmpty(t, cookie)

	var user models.User
	require.NoError(t, db.First(&user, "subject = ?", "admin-sub").Error)
	require.Equal(t, models.RoleAdmin, user.Role)
	require.True(t, user.EmailVerified)
}

func TestSSOServiceCompleteNoBindingDenied(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), emailOnlyAdminSource(t))

	deniedBefore := metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", OutcomeDenied)
	_, _, err := sso.Complete(context.Background(), &ExternalIdentity{
		Issuer: "ldap", Subject: "carol", Email: "carol@example.com", EmailVerified: true, Groups: []string{"admins"},
	}, "ldap", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)
	require.Equal(t, deniedBefore+1, metrictestutil.CounterValue(t, metrics.SSOLoginsTotal, "ldap", OutcomeDenied))
	requireUserAndSessionCounts(t, db, 0, 0)
}

func TestSSOServiceCompleteConfinedOnlyStoresEmptyClusterRole(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
namespaces: {marketing: {}}
bindings:
  - subjects: {groups: [marketing-eng]}
    role: operator
    namespaces: [marketing]
`))
	require.NoError(t, err)
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), &policy.Static{Policy: p})

	_, _, err = sso.Complete(context.Background(), &ExternalIdentity{
		Issuer: "oidc", Subject: "marketer", Groups: []string{"marketing-eng"},
	}, "oidc", "", "")
	require.NoError(t, err)

	var user models.User
	require.NoError(t, db.First(&user, "subject = ?", "marketer").Error)
	require.Equal(t, models.Role(""), user.Role)
	requireUserAndSessionCounts(t, db, 1, 1)
}

func TestSSOServiceCompleteNilSourceDenies(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	var typedNil *RoleMapper
	for _, src := range []policy.Source{nil, typedNil} {
		sso := NewSSOService(NewUserStore(db), NewSessionStore(db), src)
		_, _, err := sso.Complete(context.Background(), &ExternalIdentity{
			Issuer: "oidc", Subject: "anyone", Email: "a@b.com", EmailVerified: true, Groups: []string{"eng"},
		}, "oidc", "", "")
		require.ErrorIs(t, err, ErrLoginDenied)
	}
	requireUserAndSessionCounts(t, db, 0, 0)
}

func TestSSOServiceCompleteDowngradesEmailTrust(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	p, err := policy.Parse([]byte(`apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: [viewers]}
    role: viewer
    namespaces: ["*"]
  - subjects: {users: [admin@example.com]}
    role: admin
    namespaces: ["*"]
`))
	require.NoError(t, err)
	src := &policy.Static{Policy: p}
	sso := NewSSOService(NewUserStore(db), NewSessionStore(db), src)

	ext := &ExternalIdentity{
		Issuer: "oidc", Subject: "trust-sub", Email: "admin@example.com", EmailVerified: true, Groups: []string{"viewers"},
	}
	_, _, err = sso.Complete(context.Background(), ext, "oidc", "", "")
	require.NoError(t, err)
	var user models.User
	require.NoError(t, db.First(&user, "subject = ?", "trust-sub").Error)
	require.True(t, user.EmailVerified)
	require.Equal(t, models.RoleAdmin, user.Role)
	require.Equal(t, models.RoleAdmin, PrincipalFromUser(&user, src).ClusterRole())

	ext.EmailVerified = false
	_, _, err = sso.Complete(context.Background(), ext, "oidc", "", "")
	require.NoError(t, err)
	user = models.User{}
	require.NoError(t, db.First(&user, "subject = ?", "trust-sub").Error)
	require.False(t, user.EmailVerified, "an unverified login must downgrade the stored trust")
	require.Equal(t, models.RoleViewer, user.Role)
	require.Equal(t, models.RoleViewer, PrincipalFromUser(&user, src).ClusterRole(),
		"the session principal must lose the email-only grant")
}

func newRefreshSSO(t *testing.T) (*refreshFixture, *SSOService, *AuditLogger) {
	t.Helper()
	f := newRefreshFixture(t)
	mapper, err := NewRoleMapper("eng=operator", "")
	require.NoError(t, err)
	auditor := NewAuditLogger(f.db)
	return f, NewSSOService(f.users, f.sessions, mapper, WithSSOAuditLogger(auditor)), auditor
}

func TestSSOServiceCompleteReplacesTheGrantOnEveryLogin(t *testing.T) {
	f, sso, auditor := newRefreshSSO(t)
	const firstToken, secondToken = "first-refresh-token-xyz", "second-refresh-token-xyz"

	cookie, sess, err := sso.Complete(t.Context(), oidcIdentity("ada", firstToken, "eng"), "oidc", "203.0.113.1", "agent")
	require.NoError(t, err)
	require.NotEmpty(t, cookie)
	user := f.row(t, sess.UserID)
	require.Equal(t, int64(1), user.RefreshGrantVersion)
	require.True(t, user.GroupsRefreshedAt.Equal(f.clock.Now()))
	require.False(t, bytes.Contains(user.RefreshGrant, []byte(firstToken)))
	require.Equal(t, firstToken, f.handleAt(t, user.ID))

	f.clock.Advance(time.Hour)
	_, sess2, err := sso.Complete(t.Context(), oidcIdentity("ada", secondToken, "eng"), "oidc", "203.0.113.1", "agent")
	require.NoError(t, err)
	require.Equal(t, user.ID, sess2.UserID)
	user = f.row(t, user.ID)
	require.Equal(t, int64(2), user.RefreshGrantVersion)
	require.True(t, user.GroupsRefreshedAt.Equal(f.clock.Now()))
	require.Equal(t, secondToken, f.handleAt(t, user.ID))

	// A SAML login carries no grant: it clears the stored one and advances
	// the version, so any refresh of the old grant is discarded.
	_, _, err = sso.Complete(t.Context(), oidcIdentity("ada", "", "eng"), "saml", "203.0.113.1", "agent")
	require.NoError(t, err)
	user = f.row(t, user.ID)
	require.Nil(t, user.RefreshGrant)
	require.Equal(t, int64(3), user.RefreshGrantVersion)

	// No audit row carries a token.
	entries, err := auditor.Query(&AuditQueryRequest{Limit: 100})
	require.NoError(t, err)
	require.NotEmpty(t, entries)
	for _, entry := range entries {
		raw, err := json.Marshal(entry)
		require.NoError(t, err)
		require.NotContains(t, string(raw), firstToken)
		require.NotContains(t, string(raw), secondToken)
	}
}

func TestSSOServiceCompleteDisabledUserLeavesRefreshStateUntouched(t *testing.T) {
	f, sso, auditor := newRefreshSSO(t)
	_, sess, err := sso.Complete(t.Context(), oidcIdentity("ada", "token-1", "eng"), "oidc", "", "")
	require.NoError(t, err)
	require.True(t, f.claim(t, sess.UserID, 1, "node-a/1"))
	applied, err := f.users.CompleteRefreshUncertain(t.Context(), sess.UserID, 1, "node-a/1")
	require.NoError(t, err)
	require.True(t, applied)
	require.True(t, f.claim(t, sess.UserID, 1, "node-b/1"))
	require.NoError(t, f.db.Model(&models.User{}).Where("id = ?", sess.UserID).Update("disabled_at", f.clock.Now()).Error)
	before := f.row(t, sess.UserID)

	f.clock.Advance(time.Hour)
	cookie, denied, err := sso.Complete(t.Context(), oidcIdentity("ada", "token-2", "eng"), "oidc", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)
	require.Empty(t, cookie)
	require.Nil(t, denied)
	requireSameRefreshState(t, before, f.row(t, sess.UserID))

	deniedEntries, err := auditor.Query(&AuditQueryRequest{Action: ActionAuthLoginDenied, Limit: 10})
	require.NoError(t, err)
	require.Len(t, deniedEntries, 1)
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(deniedEntries[0].Metadata, &metadata))
	require.Equal(t, "user_disabled", metadata["reason"])
}

// TestSSOServiceCompleteUserDisabledMidLogin covers a user disabled between
// the login's read and its write: the write is fenced on disabled_at, so the
// login is denied and nothing rotates.
func TestSSOServiceCompleteUserDisabledMidLogin(t *testing.T) {
	f, sso, _ := newRefreshSSO(t)
	_, sess, err := sso.Complete(t.Context(), oidcIdentity("ada", "token-1", "eng"), "oidc", "", "")
	require.NoError(t, err)
	userID := sess.UserID
	before := f.row(t, userID)

	const callback = "test:disable_user_mid_login"
	disabled := false
	require.NoError(t, f.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if disabled || tx.Statement.Table != "users" {
			return
		}
		disabled = true
		tx.Session(&gorm.Session{NewDB: true}).Exec("UPDATE users SET disabled_at = ? WHERE id = ?", f.clock.Now(), userID)
	}))
	t.Cleanup(func() { _ = f.db.Callback().Update().Remove(callback) })

	_, _, err = sso.Complete(t.Context(), oidcIdentity("ada", "token-2", "eng"), "oidc", "", "")
	require.ErrorIs(t, err, ErrLoginDenied)
	require.True(t, disabled)
	after := f.row(t, userID)
	require.NotNil(t, after.DisabledAt)
	requireSameRefreshState(t, before, after)
	require.JSONEq(t, string(before.Groups), string(after.Groups))
	require.Equal(t, int64(1), f.liveSessions(t, userID), "no session minted for the disabled login")
}

func requireSameRefreshState(t *testing.T, before, after models.User) {
	t.Helper()
	require.Equal(t, before.RefreshGrant, after.RefreshGrant)
	require.Equal(t, before.RefreshGrantVersion, after.RefreshGrantVersion)
	require.Equal(t, before.RefreshUncertain, after.RefreshUncertain)
	require.Equal(t, before.RefreshLeaseOwner, after.RefreshLeaseOwner)
	require.Equal(t, before.RefreshLeaseUntil == nil, after.RefreshLeaseUntil == nil)
	if before.RefreshLeaseUntil != nil {
		require.True(t, before.RefreshLeaseUntil.Equal(*after.RefreshLeaseUntil))
	}
	require.True(t, before.GroupsRefreshedAt.Equal(*after.GroupsRefreshedAt))
	require.Equal(t, before.LastLoginAt.Unix(), after.LastLoginAt.Unix())
}

func requireUserAndSessionCounts(t *testing.T, db *gorm.DB, users, sessions int64) {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&models.User{}).Count(&count).Error)
	require.Equal(t, users, count, "users")
	require.NoError(t, db.Model(&models.Session{}).Count(&count).Error)
	require.Equal(t, sessions, count, "sessions")
}
