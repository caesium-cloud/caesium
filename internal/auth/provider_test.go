package auth

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
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
