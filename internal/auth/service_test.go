package auth_test

import (
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

func TestCreateKeyAndValidate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Description: "test key",
		Role:        models.RoleOperator,
		CreatedBy:   "test",
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Plaintext)
	require.True(t, strings.HasPrefix(resp.Plaintext, auth.KeyPrefixLive))
	require.NotEmpty(t, resp.Key.ID)
	require.Equal(t, models.RoleOperator, resp.Key.Role)
	require.Equal(t, "test key", resp.Key.Description)
	require.Equal(t, "test", resp.Key.CreatedBy)
	require.Nil(t, resp.Key.RevokedAt)
	require.Nil(t, resp.Key.ExpiresAt)

	// Validate the key succeeds.
	key, err := svc.ValidateKey(resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, resp.Key.ID, key.ID)
	require.Equal(t, models.RoleOperator, key.Role)
}

func TestValidateKeyNotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	_, err := svc.ValidateKey("csk_live_doesnotexist")
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

func TestValidateKeyRevoked(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "test",
	})
	require.NoError(t, err)

	err = svc.RevokeKey(resp.Key.ID)
	require.NoError(t, err)

	_, err = svc.ValidateKey(resp.Plaintext)
	require.ErrorIs(t, err, auth.ErrKeyRevoked)
}

func TestValidateKeyExpired(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	past := time.Now().UTC().Add(-1 * time.Hour)
	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "test",
		ExpiresAt: &past,
	})
	require.NoError(t, err)

	_, err = svc.ValidateKey(resp.Plaintext)
	require.ErrorIs(t, err, auth.ErrKeyExpired)
}

func TestCreateKeyWithScope(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	scope := &models.KeyScope{Jobs: []string{"etl-daily", "etl-hourly"}}
	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleRunner,
		Scope:     scope,
		CreatedBy: "test",
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.Key.Scope)

	// Verify scope is persisted and retrievable.
	key, err := svc.ValidateKey(resp.Plaintext)
	require.NoError(t, err)
	require.True(t, auth.CheckScope(key.Scope, "etl-daily"))
	require.True(t, auth.CheckScope(key.Scope, "etl-hourly"))
	require.False(t, auth.CheckScope(key.Scope, "other-job"))
}

func TestCreateKeyWithExpiry(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	future := time.Now().UTC().Add(24 * time.Hour)
	resp, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "test",
		ExpiresAt: &future,
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Key.ExpiresAt)

	// Still valid — hasn't expired.
	key, err := svc.ValidateKey(resp.Plaintext)
	require.NoError(t, err)
	require.Equal(t, resp.Key.ID, key.ID)
}

func TestListKeys(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	// Start empty.
	keys, err := svc.ListKeys()
	require.NoError(t, err)
	require.Empty(t, keys)

	// Create two keys.
	_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
	require.NoError(t, err)
	_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	keys, err = svc.ListKeys()
	require.NoError(t, err)
	require.Len(t, keys, 2)

	// Most recent first.
	require.Equal(t, models.RoleViewer, keys[0].Role)
	require.Equal(t, models.RoleAdmin, keys[1].Role)
}

func TestRevokeKeyNotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	err := svc.RevokeKey([16]byte{}) // zero UUID
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

func TestRevokeKeyIdempotent(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	err = svc.RevokeKey(resp.Key.ID)
	require.NoError(t, err)

	// Second revoke should return not found (already revoked, WHERE clause excludes it).
	err = svc.RevokeKey(resp.Key.ID)
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

func TestRotateKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	original, err := svc.CreateKey(&auth.CreateKeyRequest{
		Description: "original",
		Role:        models.RoleOperator,
		CreatedBy:   "test",
	})
	require.NoError(t, err)

	// Rotate with 1-hour grace period.
	rotated, err := svc.RotateKey(original.Key.ID, time.Hour, "admin")
	require.NoError(t, err)
	require.NotEmpty(t, rotated.Plaintext)
	require.NotEqual(t, original.Plaintext, rotated.Plaintext)
	require.Equal(t, models.RoleOperator, rotated.Key.Role)
	require.Contains(t, rotated.Key.Description, "rotated")
	require.Equal(t, "admin", rotated.Key.CreatedBy)

	// New key works.
	key, err := svc.ValidateKey(rotated.Plaintext)
	require.NoError(t, err)
	require.Equal(t, rotated.Key.ID, key.ID)

	// Old key still works during grace period.
	key, err = svc.ValidateKey(original.Plaintext)
	require.NoError(t, err)
	require.Equal(t, original.Key.ID, key.ID)
}

func TestRotateKeyRevokedFails(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	err = svc.RevokeKey(resp.Key.ID)
	require.NoError(t, err)

	_, err = svc.RotateKey(resp.Key.ID, time.Hour, "admin")
	require.ErrorIs(t, err, auth.ErrKeyRevoked)
}

func TestRotateKeyNotFound(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	_, err := svc.RotateKey([16]byte{}, time.Hour, "admin")
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

func TestBootstrapCreatesAdminKey(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	plaintext, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)
	require.True(t, strings.HasPrefix(plaintext, auth.KeyPrefixLive))

	// Validate the bootstrap key works.
	key, err := svc.ValidateKey(plaintext)
	require.NoError(t, err)
	require.Equal(t, models.RoleAdmin, key.Role)
	require.Equal(t, "system", key.CreatedBy)
}

func TestBootstrapNoopWhenAdminExists(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	// First bootstrap creates a key.
	first, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, first)

	// Second bootstrap returns empty (admin already exists).
	second, err := svc.Bootstrap()
	require.NoError(t, err)
	require.Empty(t, second)
}

func TestAdminKeyExists(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	exists, err := svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists)

	_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	// Viewer key doesn't count.
	exists, err = svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists)

	_, err = svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
	require.NoError(t, err)

	exists, err = svc.AdminKeyExists()
	require.NoError(t, err)
	require.True(t, exists)
}

func TestAdminKeyExistsExcludesRevoked(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	resp, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test"})
	require.NoError(t, err)

	exists, err := svc.AdminKeyExists()
	require.NoError(t, err)
	require.True(t, exists)

	err = svc.RevokeKey(resp.Key.ID)
	require.NoError(t, err)

	exists, err = svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists)
}

func TestAdminKeyExistsExcludesExpired(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	past := time.Now().UTC().Add(-time.Hour)
	_, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleAdmin,
		CreatedBy: "test",
		ExpiresAt: &past,
	})
	require.NoError(t, err)

	exists, err := svc.AdminKeyExists()
	require.NoError(t, err)
	require.False(t, exists)
}

func TestBootstrapCreatesAdminKeyWhenOnlyExpiredAdminExists(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	svc := auth.NewService(db)

	past := time.Now().UTC().Add(-time.Hour)
	_, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role:      models.RoleAdmin,
		CreatedBy: "test",
		ExpiresAt: &past,
	})
	require.NoError(t, err)

	plaintext, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)

	key, err := svc.ValidateKey(plaintext)
	require.NoError(t, err)
	require.Equal(t, models.RoleAdmin, key.Role)
}
