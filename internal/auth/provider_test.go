package auth

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
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
