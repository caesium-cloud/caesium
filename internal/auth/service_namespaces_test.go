package auth_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// storedNamespaces reads the raw api_keys.namespaces column; Valid=false is SQL NULL.
func storedNamespaces(t *testing.T, db *gorm.DB, id uuid.UUID) sql.NullString {
	t.Helper()
	var ns sql.NullString
	require.NoError(t, db.Raw("SELECT namespaces FROM api_keys WHERE id = ?", id).Row().Scan(&ns))
	return ns
}

func setStoredNamespaces(t *testing.T, db *gorm.DB, id uuid.UUID, value string) {
	t.Helper()
	require.NoError(t, db.Exec("UPDATE api_keys SET namespaces = ? WHERE id = ?", value, id).Error)
}

// TestRotateKeyPreservesNamespaces: rotation must never widen (or narrow) a
// key. The replacement row carries the old namespaces byte-for-byte.
// Unusable values are refused (TestRotateKeyRefusesUnusableNamespaces).
func TestRotateKeyPreservesNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name       string
		namespaces *string // nil = SQL NULL
	}{
		{"null is cluster", nil},
		{"confined", ptr(`["marketing"]`)},
		{"explicit cluster", ptr(`["*"]`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)
			svc := auth.NewService(db)

			original, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
			require.NoError(t, err)
			if tc.namespaces != nil {
				setStoredNamespaces(t, db, original.Key.ID, *tc.namespaces)
			}
			before := storedNamespaces(t, db, original.Key.ID)

			rotated, err := svc.RotateKeyAuthorized(t.Context(), original.Key.ID, time.Hour, "admin", allowKey)
			require.NoError(t, err)
			after := storedNamespaces(t, db, rotated.Key.ID)
			require.Equal(t, before, after, "rotation must copy namespaces verbatim")
			if tc.namespaces == nil {
				require.False(t, after.Valid, "a NULL-namespaces key must rotate to NULL")
			}

			oldKey, err := svc.ValidateKey(original.Plaintext)
			require.NoError(t, err)
			newKey, err := svc.ValidateKey(rotated.Plaintext)
			require.NoError(t, err)
			require.Equal(t, auth.PrincipalFromKey(oldKey).Grants, auth.PrincipalFromKey(newKey).Grants)
		})
	}
}

func TestCreateKeyPersistsNamespaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	confined, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role: models.RoleOperator, CreatedBy: "test", Namespaces: datatypes.JSON(`["marketing"]`),
	})
	require.NoError(t, err)
	require.Equal(t, sql.NullString{String: `["marketing"]`, Valid: true}, storedNamespaces(t, db, confined.Key.ID))

	for _, ns := range []datatypes.JSON{nil, {}} {
		cluster, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test", Namespaces: ns})
		require.NoError(t, err)
		require.False(t, storedNamespaces(t, db, cluster.Key.ID).Valid, "empty namespaces must store SQL NULL")
	}
}

func TestAdminKeyExistsIgnoresConfinedKeys(t *testing.T) {
	for _, tc := range []struct {
		name       string
		namespaces *string
		want       bool
	}{
		{"null is cluster admin", nil, true},
		{"explicit cluster admin", ptr(`["*"]`), true},
		{"cluster among others", ptr(`["marketing","*"]`), true},
		{"confined admin", ptr(`["marketing"]`), false},
		{"empty list", ptr(`[]`), false},
		{"json null", ptr(`null`), false},
		{"malformed", ptr(`{bad`), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.OpenTestDB(t)
			defer testutil.CloseDB(db)
			svc := auth.NewService(db)

			_, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
			require.NoError(t, err)
			admin, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
			require.NoError(t, err)
			if tc.namespaces != nil {
				setStoredNamespaces(t, db, admin.Key.ID, *tc.namespaces)
			}

			exists, err := svc.AdminKeyExists()
			require.NoError(t, err)
			require.Equal(t, tc.want, exists)
		})
	}

	t.Run("one cluster admin among confined admins", func(t *testing.T) {
		db := testutil.OpenTestDB(t)
		defer testutil.CloseDB(db)
		svc := auth.NewService(db)
		for range 3 {
			confined, err := svc.CreateKey(&auth.CreateKeyRequest{
				Role: models.RoleAdmin, CreatedBy: "test", Namespaces: datatypes.JSON(`["marketing"]`),
			})
			require.NoError(t, err)
			require.NotNil(t, confined)
		}
		exists, err := svc.AdminKeyExists()
		require.NoError(t, err)
		require.False(t, exists)

		past := time.Now().UTC().Add(-time.Hour)
		_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test", ExpiresAt: &past})
		require.NoError(t, err)
		exists, err = svc.AdminKeyExists()
		require.NoError(t, err)
		require.False(t, exists, "an expired cluster admin still does not count")

		_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
		require.NoError(t, err)
		exists, err = svc.AdminKeyExists()
		require.NoError(t, err)
		require.True(t, exists)
	})
}

func TestBootstrapRefreshClearsNamespaces(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db, auth.WithKeyHashSecret("server-side-secret"))

	first, err := svc.Bootstrap()
	require.NoError(t, err)
	key, err := svc.ValidateKey(first)
	require.NoError(t, err)
	require.False(t, storedNamespaces(t, db, key.ID).Valid)

	setStoredNamespaces(t, db, key.ID, `["marketing"]`)
	require.NoError(t, svc.RevokeKey(key.ID))

	second, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, second)
	testutil.AssertCount(t, db, &models.APIKey{}, 1)

	refreshed, err := svc.ValidateKey(second)
	require.NoError(t, err)
	require.False(t, storedNamespaces(t, db, refreshed.ID).Valid, "the refreshed bootstrap key must be cluster-wide again")
	require.Equal(t, models.RoleAdmin, auth.PrincipalFromKey(refreshed).ClusterRole())
}

// TestBootstrapRestoresConfinedBootstrapKey: when the only admin is a bootstrap
// key confined to namespaces, there is no cluster admin, so startup restores the
// bootstrap slot to a fresh cluster admin key instead of failing.
func TestBootstrapRestoresConfinedBootstrapKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db, auth.WithKeyHashSecret("server-side-secret"))

	first, err := svc.Bootstrap()
	require.NoError(t, err)
	key, err := svc.ValidateKey(first)
	require.NoError(t, err)
	setStoredNamespaces(t, db, key.ID, `["marketing"]`)

	exists, err := svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists)

	second, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, second)
	require.NotEqual(t, first, second)
	testutil.AssertCount(t, db, &models.APIKey{}, 1)

	_, err = svc.ValidateKey(first)
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
	refreshed, err := svc.ValidateKey(second)
	require.NoError(t, err)
	require.Equal(t, models.RoleAdmin, auth.PrincipalFromKey(refreshed).ClusterRole())

	// An active cluster bootstrap key is never replaced.
	third, err := svc.Bootstrap()
	require.NoError(t, err)
	require.Empty(t, third)
	_, err = svc.ValidateKey(second)
	require.NoError(t, err)
}

func ptr(s string) *string { return &s }
