package auth

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
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

func TestUserStoreUpsertHandlesConcurrentCreateConflict(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	us := NewUserStore(db.Session(&gorm.Session{SkipDefaultTransaction: true}))

	const callbackName = "test:insert_racing_user"
	inserted := false
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callbackName, func(tx *gorm.DB) {
		if inserted || tx.Statement.Schema == nil || tx.Statement.Schema.Name != "User" {
			return
		}
		inserted = true
		now := time.Now().UTC()
		tx.Exec(
			"INSERT INTO users (id, issuer, subject, email, role, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			uuid.New(), "oidc", "sub-race", "raced@example.com", string(models.RoleViewer), now,
		)
	}))

	u, err := us.Upsert(context.Background(), &ExternalIdentity{
		Issuer:  "oidc",
		Subject: "sub-race",
		Email:   "winner@example.com",
		Groups:  []string{"eng"},
	}, models.RoleOperator)
	require.NoError(t, err)
	require.True(t, inserted)
	require.Equal(t, "winner@example.com", u.Email)
	require.Equal(t, models.RoleOperator, u.Role)

	var count int64
	require.NoError(t, db.Model(&models.User{}).Where("issuer = ? AND subject = ?", "oidc", "sub-race").Count(&count).Error)
	require.Equal(t, int64(1), count)
}
