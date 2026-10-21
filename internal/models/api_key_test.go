package models

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestIsRevokedFalseByDefault(t *testing.T) {
	key := &APIKey{}
	require.False(t, key.IsRevoked())
}

func TestIsRevokedTrueWhenSet(t *testing.T) {
	now := time.Now()
	key := &APIKey{RevokedAt: &now}
	require.True(t, key.IsRevoked())
}

func TestIsExpiredFalseByDefault(t *testing.T) {
	key := &APIKey{}
	require.False(t, key.IsExpired())
}

func TestIsExpiredFalseWhenFuture(t *testing.T) {
	future := time.Now().Add(time.Hour)
	key := &APIKey{ExpiresAt: &future}
	require.False(t, key.IsExpired())
}

func TestIsExpiredTrueWhenPast(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	key := &APIKey{ExpiresAt: &past}
	require.True(t, key.IsExpired())
}

func TestRoleLevelOrdering(t *testing.T) {
	require.Greater(t, RoleLevel(RoleAdmin), RoleLevel(RoleOperator))
	require.Greater(t, RoleLevel(RoleOperator), RoleLevel(RoleRunner))
	require.Greater(t, RoleLevel(RoleRunner), RoleLevel(RoleViewer))
	require.Equal(t, 0, RoleLevel(Role("unknown")))
}

func TestValidRole(t *testing.T) {
	require.True(t, ValidRole("admin"))
	require.True(t, ValidRole("operator"))
	require.True(t, ValidRole("runner"))
	require.True(t, ValidRole("viewer"))
	require.False(t, ValidRole("superuser"))
	require.False(t, ValidRole(""))
}

// A key minted without namespaces persists SQL NULL and reads back empty,
// which PrincipalFromKey treats as cluster-wide. Nothing normalizes it.
func TestAPIKeyNamespacesNullRoundTrip(t *testing.T) {
	db := openEmptyModelTestDB(t)
	require.NoError(t, db.AutoMigrate(&APIKey{}))

	key := &APIKey{ID: uuid.New(), KeyPrefix: "csk_live_ns", KeyHash: "hash", Role: RoleViewer, CreatedAt: time.Now().UTC()}
	require.NoError(t, db.Create(key).Error)

	var nulls int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM api_keys WHERE namespaces IS NULL").Scan(&nulls).Error)
	require.Equal(t, int64(1), nulls)

	var got APIKey
	require.NoError(t, db.First(&got, "id = ?", key.ID).Error)
	require.Empty(t, got.Namespaces)

	require.NoError(t, db.Model(&APIKey{}).Where("id = ?", key.ID).Update("namespaces", datatypes.JSON(`["marketing"]`)).Error)
	require.NoError(t, db.First(&got, "id = ?", key.ID).Error)
	require.JSONEq(t, `["marketing"]`, string(got.Namespaces))
}

// preUserBoundAPIKey is api_keys as it was before user-bound keys (E4).
type preUserBoundAPIKey struct {
	ID            uuid.UUID      `gorm:"type:uuid;primaryKey"`
	KeyPrefix     string         `gorm:"type:text;size:12;not null;index"`
	KeyHash       string         `gorm:"type:text;not null;uniqueIndex"`
	BootstrapSlot *string        `gorm:"type:text;uniqueIndex"`
	Description   string         `gorm:"type:text"`
	Role          Role           `gorm:"type:text;not null"`
	Scope         datatypes.JSON `gorm:"type:json"`
	Namespaces    datatypes.JSON `gorm:"type:json"`
	CreatedBy     string         `gorm:"type:text"`
	CreatedAt     time.Time      `gorm:"not null"`
	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	RevokedAt     *time.Time
}

func (preUserBoundAPIKey) TableName() string { return "api_keys" }

// TestAPIKeyTableGainsNullableUserID: the E4 column is added to an existing
// table as nullable with no default and an index, existing keys read NULL
// (plain keys), and the migration is idempotent.
func TestAPIKeyTableGainsNullableUserID(t *testing.T) {
	db := openEmptyModelTestDB(t)
	require.NoError(t, db.AutoMigrate(&preUserBoundAPIKey{}))
	legacy := preUserBoundAPIKey{ID: uuid.New(), KeyPrefix: "csk_live_old", KeyHash: "old-hash", Role: RoleAdmin, CreatedAt: time.Now().UTC()}
	require.NoError(t, db.Create(&legacy).Error)
	require.False(t, db.Migrator().HasColumn(&APIKey{}, "user_id"))
	for range 2 {
		require.NoError(t, db.AutoMigrate(&APIKey{}))
	}
	require.True(t, db.Migrator().HasColumn(&APIKey{}, "user_id"))
	require.True(t, db.Migrator().HasIndex(&APIKey{}, "UserID"))

	var nulls int64
	require.NoError(t, db.Raw("SELECT COUNT(*) FROM api_keys WHERE user_id IS NULL").Scan(&nulls).Error)
	require.Equal(t, int64(1), nulls, "existing keys stay NULL rather than gain a default")
	columnTypes, err := db.Migrator().ColumnTypes(&APIKey{})
	require.NoError(t, err)
	for _, ct := range columnTypes {
		if ct.Name() != "user_id" {
			continue
		}
		if nullable, ok := ct.Nullable(); ok {
			require.True(t, nullable)
		}
		_, hasDefault := ct.DefaultValue()
		require.False(t, hasDefault)
	}
	var got APIKey
	require.NoError(t, db.First(&got, "id = ?", legacy.ID).Error)
	require.Nil(t, got.UserID)
	require.False(t, got.IsUserBound())
	require.Equal(t, RoleAdmin, got.Role)
}
