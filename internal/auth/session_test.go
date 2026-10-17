package auth

import (
	"context"
	"slices"
	"strings"
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

func TestSessionStoreValidateHonorsPendingIdleRefresh(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewSessionStore(
		db,
		WithSessionNow(func() time.Time { return now }),
		WithSessionTTLs(10*time.Second, time.Minute),
	)
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)
	require.WithinDuration(t, base.Add(10*time.Second), sess.IdleExpiresAt, time.Millisecond)

	now = base.Add(9 * time.Second)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.NoError(t, err)

	var stale models.Session
	require.NoError(t, db.First(&stale, "id = ?", sess.ID).Error)
	require.WithinDuration(t, base.Add(10*time.Second), stale.IdleExpiresAt, time.Millisecond)

	now = base.Add(11 * time.Second)
	gotSess, _, err := store.Validate(context.Background(), plaintext)
	require.NoError(t, err)
	require.WithinDuration(t, base.Add(19*time.Second), gotSess.IdleExpiresAt, time.Millisecond)

	store.flushSeen(context.Background())

	var flushed models.Session
	require.NoError(t, db.First(&flushed, "id = ?", sess.ID).Error)
	require.NotNil(t, flushed.LastSeenAt)
	require.WithinDuration(t, base.Add(11*time.Second), *flushed.LastSeenAt, time.Millisecond)
	require.WithinDuration(t, base.Add(21*time.Second), flushed.IdleExpiresAt, time.Millisecond)
}

func TestSessionStoreValidatePendingIdleRefreshDoesNotExtendAbsoluteExpiry(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewSessionStore(
		db,
		WithSessionNow(func() time.Time { return now }),
		WithSessionTTLs(10*time.Second, 10*time.Second),
	)
	plaintext, _, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	now = base.Add(9 * time.Second)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.NoError(t, err)

	now = base.Add(11 * time.Second)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.ErrorIs(t, err, ErrSessionExpired)
}

func TestSessionStoreValidateDisabledUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{
		ID:      uuid.New(),
		Issuer:  "oidc",
		Subject: "s",
		Email:   "a@b.com",
		Role:    models.RoleViewer,
	}
	require.NoError(t, db.Create(u).Error)
	store := NewSessionStore(db)
	plaintext, _, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)
	// Disabled behind the session's back (no revocation): Validate still
	// refuses it on every request.
	require.NoError(t, db.Model(&models.User{}).Where("id = ?", u.ID).Update("disabled_at", time.Now().UTC()).Error)

	_, _, err = store.Validate(context.Background(), plaintext)
	require.ErrorIs(t, err, ErrUserDisabled)
}

func TestSessionStoreCreateIsFencedOnTheUser(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	store := NewSessionStore(db)
	store.retry.Wait = nil
	disabledAt := time.Now().UTC()
	disabled := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "d", Email: "d@b.com", Role: models.RoleViewer, DisabledAt: &disabledAt}
	versioned := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "v", Email: "v@b.com", Role: models.RoleViewer, DisableGeneration: 3}
	require.NoError(t, db.Create(disabled).Error)
	require.NoError(t, db.Create(versioned).Error)

	_, _, err := store.Create(t.Context(), CreateSessionRequest{UserID: disabled.ID})
	require.ErrorIs(t, err, ErrUserDisabled)
	_, _, err = store.Create(t.Context(), CreateSessionRequest{UserID: uuid.New()})
	require.ErrorIs(t, err, ErrUserNotFound)
	_, _, err = store.Create(t.Context(), CreateSessionRequest{UserID: versioned.ID, DisableGeneration: 2})
	require.ErrorIs(t, err, ErrUserChanged, "a generation the login did not read refuses the mint")
	_, _, err = store.Create(t.Context(), CreateSessionRequest{UserID: versioned.ID})
	require.ErrorIs(t, err, ErrUserChanged, "the zero value is not a wildcard")

	var count int64
	require.NoError(t, db.Model(&models.Session{}).Count(&count).Error)
	require.Zero(t, count, "no refused mint leaves a row behind")

	_, sess, err := store.Create(t.Context(), CreateSessionRequest{UserID: versioned.ID, DisableGeneration: 3})
	require.NoError(t, err)
	require.Equal(t, versioned.ID, sess.UserID)
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
	store.flushSeen(context.Background())

	var got models.Session
	require.NoError(t, db.First(&got, "id = ?", sess.ID).Error)
	require.NotNil(t, got.LastSeenAt)
	require.False(t, got.IdleExpiresAt.Before(before))
}

func TestSessionFlushSeenBatchesLargeUpdates(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)

	now := time.Now().UTC()
	sessions := make([]models.Session, 0, sessionFlushBatchSize+5)
	store := NewSessionStore(db, WithSessionTTLs(time.Hour, 24*time.Hour))
	for range sessionFlushBatchSize + 5 {
		id := uuid.New()
		sessions = append(sessions, models.Session{
			ID:                id,
			UserID:            u.ID,
			TokenHash:         id.String(),
			CSRFToken:         "csrf-" + id.String(),
			CreatedAt:         now,
			IdleExpiresAt:     now.Add(time.Hour),
			AbsoluteExpiresAt: now.Add(24 * time.Hour),
		})
		store.recordSeen(id)
	}
	require.NoError(t, db.Create(&sessions).Error)

	store.flushSeen(context.Background())

	var count int64
	require.NoError(t, db.Model(&models.Session{}).Where("last_seen_at IS NOT NULL").Count(&count).Error)
	require.Equal(t, int64(len(sessions)), count)
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

func TestSessionReapFlushesPendingIdleRefreshBeforeDeleting(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewSessionStore(
		db,
		WithSessionNow(func() time.Time { return now }),
		WithSessionTTLs(10*time.Second, time.Minute),
	)
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	now = base.Add(9 * time.Second)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.NoError(t, err)

	now = base.Add(11 * time.Second)
	n, err := store.Reap(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)

	var got models.Session
	require.NoError(t, db.First(&got, "id = ?", sess.ID).Error)
	require.NotNil(t, got.LastSeenAt)
	require.WithinDuration(t, base.Add(11*time.Second), *got.LastSeenAt, time.Millisecond)
	require.WithinDuration(t, base.Add(21*time.Second), got.IdleExpiresAt, time.Millisecond)
}

// TestSessionListActiveAppliesPendingActivityLikeValidate is the boundary
// Validate already honours (persisted idle expiry t+10, activity buffered at
// t+9, still valid at t+11): the listing must count and return the session,
// with the effective expiry, and write nothing.
func TestSessionListActiveAppliesPendingActivityLikeValidate(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	u := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "s", Email: "a@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(u).Error)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewSessionStore(db, WithSessionNow(func() time.Time { return now }), WithSessionTTLs(10*time.Second, time.Minute))
	plaintext, sess, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)
	_, idle, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
	require.NoError(t, err)

	now = base.Add(9 * time.Second)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.NoError(t, err)

	now = base.Add(11 * time.Second)
	page, err := store.ListActive(t.Context(), SessionFilter{UserID: &u.ID, Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total, "the idle session without buffered activity is not active")
	require.Len(t, page.Sessions, 1)
	got := page.Sessions[0]
	require.Equal(t, sess.ID, got.ID)
	require.WithinDuration(t, base.Add(19*time.Second), got.IdleExpiresAt, time.Millisecond, "the effective idle expiry")
	require.NotNil(t, got.LastSeenAt)
	require.WithinDuration(t, base.Add(9*time.Second), *got.LastSeenAt, time.Millisecond)
	_, _, err = store.Validate(context.Background(), plaintext)
	require.NoError(t, err, "the listing and Validate agree")
	_, _, err = store.Create(context.Background(), CreateSessionRequest{UserID: uuid.New()})
	require.ErrorIs(t, err, ErrUserNotFound)

	var persisted models.Session
	require.NoError(t, db.First(&persisted, "id = ?", sess.ID).Error)
	require.WithinDuration(t, base.Add(10*time.Second), persisted.IdleExpiresAt, time.Millisecond, "listing writes nothing")
	require.Nil(t, persisted.LastSeenAt)
	var stale models.Session
	require.NoError(t, db.First(&stale, "id = ?", idle.ID).Error)
	require.Nil(t, stale.RevokedAt)

	// Exactly at the effective expiry Validate still accepts; so does the list.
	now = base.Add(21 * time.Second)
	page, err = store.ListActive(t.Context(), SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, int64(1), page.Total)
	now = base.Add(21*time.Second + time.Nanosecond)
	page, err = store.ListActive(t.Context(), SessionFilter{Limit: 10})
	require.NoError(t, err)
	require.Zero(t, page.Total)
	require.Empty(t, page.Sessions)
}

func TestSessionListActiveFiltersOrdersAndPages(t *testing.T) {
	db := testutil.OpenTestDB(t)
	defer testutil.CloseDB(db)
	alice := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "a", Email: "a@b.com", Role: models.RoleViewer}
	bob := &models.User{ID: uuid.New(), Issuer: "oidc", Subject: "b", Email: "b@b.com", Role: models.RoleViewer}
	require.NoError(t, db.Create(alice).Error)
	require.NoError(t, db.Create(bob).Error)

	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := base
	store := NewSessionStore(db, WithSessionNow(func() time.Time { return now }), WithSessionTTLs(time.Hour, 2*time.Hour))
	mint := func(u *models.User) uuid.UUID {
		t.Helper()
		_, s, err := store.Create(context.Background(), CreateSessionRequest{UserID: u.ID})
		require.NoError(t, err)
		return s.ID
	}
	// Three of alice's sessions share a creation instant: they order by id.
	same := []uuid.UUID{mint(alice), mint(alice), mint(alice)}
	slices.SortFunc(same, func(a, b uuid.UUID) int { return strings.Compare(b.String(), a.String()) })
	now = base.Add(time.Minute)
	newest := mint(alice)
	revoked := mint(alice)
	require.NoError(t, store.Revoke(context.Background(), revoked))
	bobs := mint(bob)
	// Absolute-expired: created long enough ago.
	require.NoError(t, db.Create(&models.Session{
		ID: uuid.New(), UserID: alice.ID, TokenHash: "old", CSRFToken: "c",
		CreatedAt: base.Add(-3 * time.Hour), IdleExpiresAt: base.Add(time.Hour), AbsoluteExpiresAt: base.Add(-time.Hour),
	}).Error)

	page, err := store.ListActive(t.Context(), SessionFilter{UserID: &alice.ID, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, int64(4), page.Total, "revoked and expired sessions are not active")
	require.True(t, page.HasMore)
	require.Equal(t, []uuid.UUID{newest, same[0]}, sessionIDs(page.Sessions))
	page, err = store.ListActive(t.Context(), SessionFilter{UserID: &alice.ID, Limit: 2, Offset: 2})
	require.NoError(t, err)
	require.False(t, page.HasMore)
	require.Equal(t, []uuid.UUID{same[1], same[2]}, sessionIDs(page.Sessions), "ties split across the page boundary in id order")

	all, err := store.ListActive(t.Context(), SessionFilter{Limit: 100})
	require.NoError(t, err)
	require.Equal(t, int64(5), all.Total)
	require.Contains(t, sessionIDs(all.Sessions), bobs)

	_, err = store.ListActive(t.Context(), SessionFilter{})
	require.Error(t, err)
}

func sessionIDs(sessions []models.Session) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(sessions))
	for _, s := range sessions {
		ids = append(ids, s.ID)
	}
	return ids
}
