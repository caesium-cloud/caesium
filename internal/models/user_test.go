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

// TestUserLegacyTableGainsNullableEmailVerified proves the upgrade path: a
// users table created before email_verified existed gains a nullable column,
// keeps its rows and indexes across repeated migrations, and reads the
// missing value as untrusted.
func TestUserLegacyTableGainsNullableEmailVerified(t *testing.T) {
	db := openEmptyModelTestDB(t)
	require.NoError(t, db.Exec("CREATE TABLE `users` (`id` uuid,`issuer` text NOT NULL,`subject` text NOT NULL,"+
		"`email` text,`display_name` text,`groups` json,`role` text NOT NULL,`created_at` datetime NOT NULL,"+
		"`last_login_at` datetime,`disabled_at` datetime,PRIMARY KEY (`id`))").Error)
	require.NoError(t, db.Exec("CREATE UNIQUE INDEX `idx_users_identity` ON `users`(`issuer`,`subject`)").Error)
	require.NoError(t, db.Exec("CREATE INDEX `idx_users_email` ON `users`(`email`)").Error)

	now := time.Now().UTC()
	legacy := []User{
		{ID: uuid.New(), Issuer: "oidc", Subject: "legacy-1", Email: "one@example.com", DisplayName: "One",
			Groups: []byte(`["eng"]`), Role: RoleOperator},
		{ID: uuid.New(), Issuer: "ldap", Subject: "legacy-2", Email: "two@example.com", Role: RoleViewer},
	}
	for _, u := range legacy {
		require.NoError(t, db.Exec(
			"INSERT INTO users (id, issuer, subject, email, display_name, groups, role, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			u.ID, u.Issuer, u.Subject, u.Email, u.DisplayName, u.Groups, string(u.Role), now,
		).Error)
	}
	require.False(t, db.Migrator().HasColumn(&User{}, "EmailVerified"))

	for range 2 {
		require.NoError(t, db.AutoMigrate(&User{}))
	}
	require.True(t, db.Migrator().HasColumn(&User{}, "EmailVerified"))
	require.True(t, db.Migrator().HasIndex(&User{}, "idx_users_identity"))
	require.True(t, db.Migrator().HasIndex(&User{}, "idx_users_email"))

	var nulls int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM users WHERE email_verified IS NULL").Scan(&nulls).Error)
	require.Equal(t, int64(2), nulls, "existing rows stay NULL rather than gaining a default")

	for _, want := range legacy {
		var got User
		require.NoError(t, db.First(&got, "id = ?", want.ID).Error)
		require.Equal(t, want.Issuer, got.Issuer)
		require.Equal(t, want.Subject, got.Subject)
		require.Equal(t, want.Email, got.Email)
		require.Equal(t, want.DisplayName, got.DisplayName)
		require.Equal(t, want.Role, got.Role)
		require.JSONEq(t, string(orJSONNull(want.Groups)), string(orJSONNull(got.Groups)))
		require.False(t, got.EmailVerified, "NULL email_verified must read as untrusted")
	}

	// The column is nullable, so a writer that omits it still succeeds.
	rawID := uuid.New()
	require.NoError(t, db.Exec(
		"INSERT INTO users (id, issuer, subject, email, role, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		rawID, "oidc", "raw-insert", "raw@example.com", string(RoleViewer), now,
	).Error)
	var raw User
	require.NoError(t, db.First(&raw, "id = ?", rawID).Error)
	require.False(t, raw.EmailVerified)

	created := &User{ID: uuid.New(), Issuer: "oidc", Subject: "fresh", Email: "fresh@example.com", Role: RoleViewer}
	require.NoError(t, db.Create(created).Error)
	var fresh User
	require.NoError(t, db.First(&fresh, "id = ?", created.ID).Error)
	require.False(t, fresh.EmailVerified)
	require.NoError(t, db.Model(&User{}).Where("id = ?", created.ID).Updates(map[string]any{"email_verified": true}).Error)
	require.NoError(t, db.First(&fresh, "id = ?", created.ID).Error)
	require.True(t, fresh.EmailVerified)
}

func orJSONNull(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

func openModelTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db := openEmptyModelTestDB(t)
	require.NoError(t, db.AutoMigrate(&User{}, &Session{}))
	return db
}

func openEmptyModelTestDB(t *testing.T) *gorm.DB {
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
	return db
}
