package auth

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
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
	require.Equal(t, "no_role_mapping", metadata["reason"])
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
