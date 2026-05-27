package models

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestUserSessionMigrate(t *testing.T) {
	db := openModelTestDB(t)

	u := &User{
		ID:      uuid.New(),
		Issuer:  "oidc",
		Subject: "sub-1",
		Email:   "a@b.com",
		Role:    RoleViewer,
	}
	require.NoError(t, db.Create(u).Error)

	s := &Session{
		ID:         uuid.New(),
		UserID:     u.ID,
		TokenHash:  "h",
		CSRFToken:  "csrf",
		AuthMethod: "oidc",
	}
	require.NoError(t, db.Create(s).Error)

	var got User
	require.NoError(t, db.First(&got, "id = ?", u.ID).Error)
	require.Equal(t, "a@b.com", got.Email)
	require.False(t, got.IsDisabled())
}

func TestSessionIsRevoked(t *testing.T) {
	s := &Session{}
	require.False(t, s.IsRevoked())
	now := time.Now()
	s.RevokedAt = &now
	require.True(t, s.IsRevoked())
}

func openModelTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared&_busy_timeout=5000"), &gorm.Config{})
	require.NoError(t, err)
	t.Cleanup(func() {
		sqlDB, err := db.DB()
		require.NoError(t, err)
		require.NoError(t, sqlDB.Close())
	})

	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)

	require.NoError(t, db.AutoMigrate(&User{}, &Session{}))
	return db
}
