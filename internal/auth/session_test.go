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

func TestSessionStoreCreateValidate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleOperator}
	require.NoError(t, db.Create(u).Error)

	store := NewSessionStore(db, WithSessionTTLs(8*time.Hour, 24*time.Hour))
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID, AuthMethod: "oidc"})
	require.NoError(t, err)
	require.NotEmpty(t, plaintext)
	require.NotEmpty(t, sess.TokenHash)
	require.NotEqual(t, plaintext, sess.TokenHash)
	require.NotEmpty(t, sess.CSRFToken)

	gotSess, gotUser, err := store.Validate(context.Background(), plaintext)
	require.NoError(t, err)
	require.Equal(t, sess.ID, gotSess.ID)
	require.Equal(t, u.Email, gotUser.Email)

	_, _, err = store.Validate(context.Background(), "css_wrong")
	require.ErrorIs(t, err, ErrSessionInvalid)
}

func TestSessionStoreRevoke(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db, WithSessionTTLs(time.Hour, time.Hour))
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)
	require.NoError(t, store.Revoke(context.Background(), sess.ID))
	_, _, err = store.Validate(context.Background(), plaintext)
	require.ErrorIs(t, err, ErrSessionRevoked)
}

func TestSessionStoreValidateExpired(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	past := time.Now().UTC().Add(-2 * time.Hour)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(
		db,
		WithSessionNow(func() time.Time { return past }),
		WithSessionTTLs(time.Minute, time.Minute),
	)
	plaintext, _, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	store.now = time.Now
	_, _, err = store.Validate(context.Background(), plaintext)
	require.ErrorIs(t, err, ErrSessionExpired)
}

func TestSessionStoreValidateDisabledUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	disabledAt := time.Now().UTC()
	u := &models.User{
		ID:         uuid.New(),
		Issuer:     "oidc",
		Subject:    "s",
		Email:      "a@b.com",
		Role:       models.RoleViewer,
		DisabledAt: &disabledAt,
	}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db)
	plaintext, _, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	_, _, err = store.Validate(context.Background(), plaintext)
	require.ErrorIs(t, err, ErrUserDisabled)
}

func TestSessionFlushSeen(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db, WithSessionTTLs(time.Hour, 24*time.Hour))
	_, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)
	before := sess.IdleExpiresAt

	store.recordSeen(sess.ID)
	store.flushSeen()

	var got models.Session
	require.NoError(t, db.First(&got, "id = ?", sess.ID).Error)
	require.NotNil(t, got.LastSeenAt)
	require.False(t, got.IdleExpiresAt.Before(before))
}

func TestSessionReap(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)
	past := time.Now().UTC().Add(-time.Hour)
	store := NewSessionStore(
		db,
		WithSessionNow(func() time.Time { return past }),
		WithSessionTTLs(time.Minute, time.Minute),
	)
	_, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	store.now = time.Now
	n, err := store.Reap(context.Background())
	require.NoError(t, err)
	require.GreaterOrEqual(t, n, int64(1))
	var count int64
	require.NoError(t, db.Model(&models.Session{}).Where("id = ?", sess.ID).Count(&count).Error)
	require.Equal(t, int64(0), count)
}
