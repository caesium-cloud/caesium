package auth

import (
	"context"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestUserStoreUpsert(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	us := NewUserStore(db)

	u1, err := us.Upsert(context.Background(), &ExternalIdentity{
		Issuer:      "oidc",
		Subject:     "sub-1",
		Email:       "a@b.com",
		DisplayName: "A",
		Groups:      []string{"x"},
	}, models.RoleViewer)
	require.NoError(t, err)
	require.Equal(t, models.RoleViewer, u1.Role)
	require.NotNil(t, u1.LastLoginAt)

	u2, err := us.Upsert(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "sub-1",
		Email:   "a2@b.com",
		Groups:  []string{"y"},
	}, models.RoleOperator)
	require.NoError(t, err)
	require.Equal(t, u1.ID, u2.ID)
	require.Equal(t, "a2@b.com", u2.Email)
	require.Equal(t, models.RoleOperator, u2.Role)
}
