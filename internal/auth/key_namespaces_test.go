package auth_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// allowKey is the authorizer of a trusted test caller.
func allowKey(*models.APIKey) error { return nil }

func strs(v ...string) *[]string { return &v }

func TestNormalizeKeyNamespaces(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested *[]string
		stored    string // "" = SQL NULL
		effective []string
		wantErr   string
	}{
		{name: "omitted is cluster", requested: nil, effective: []string{"*"}},
		{name: "explicit cluster stores NULL", requested: strs("*"), effective: []string{"*"}},
		{name: "one namespace", requested: strs("marketing"), stored: `["marketing"]`, effective: []string{"marketing"}},
		{name: "trimmed, sorted, deduplicated", requested: strs(" marketing", "finance", "marketing "), stored: `["finance","marketing"]`, effective: []string{"finance", "marketing"}},
		{name: "empty list", requested: strs(), wantErr: "at least one namespace"},
		{name: "blank entry", requested: strs("marketing", "  "), wantErr: "must not be empty"},
		{name: "not a label", requested: strs("Bad_NS"), wantErr: `"Bad_NS"`},
		{name: "cluster mixed with a namespace", requested: strs("*", "marketing"), wantErr: "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored, effective, err := auth.NormalizeKeyNamespaces(tc.requested)
			if tc.wantErr != "" {
				require.ErrorIs(t, err, auth.ErrInvalidKeyNamespaces)
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.effective, effective)
			if tc.stored == "" {
				require.Nil(t, stored)
			} else {
				require.Equal(t, tc.stored, string(stored))
			}
		})
	}
}

func TestStoredKeyNamespaces(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want []string // nil = unusable
	}{
		{"", []string{"*"}},
		{`["*"]`, []string{"*"}},
		{`["marketing","*"]`, []string{"*"}},
		{`["marketing"]`, []string{"marketing"}},
		{`["marketing","finance","marketing"]`, []string{"finance", "marketing"}},
		{`[]`, nil},
		{`null`, nil},
		{`{bad`, nil},
		{`[""]`, nil},
		{`[" marketing"]`, nil},
		{`["Bad_NS"]`, nil},
		{`["*","Bad_NS"]`, nil},
		{`"marketing"`, nil},
	} {
		got, err := auth.StoredKeyNamespaces(datatypes.JSON(tc.raw))
		if tc.want == nil {
			require.ErrorIs(t, err, auth.ErrKeyNamespacesUnusable, tc.raw)
			continue
		}
		require.NoError(t, err, tc.raw)
		require.Equal(t, tc.want, got, tc.raw)
	}
}

func TestKeyVisibleTo(t *testing.T) {
	allowed := []string{"finance", "marketing"}
	require.True(t, auth.KeyVisibleTo(datatypes.JSON(`["marketing"]`), allowed))
	require.True(t, auth.KeyVisibleTo(datatypes.JSON(`["marketing","finance"]`), allowed))
	require.False(t, auth.KeyVisibleTo(datatypes.JSON(`["marketing","sales"]`), allowed))
	require.False(t, auth.KeyVisibleTo(nil, allowed), "a cluster key is never visible to a restricted reader")
	require.False(t, auth.KeyVisibleTo(datatypes.JSON(`["*"]`), allowed))
	require.False(t, auth.KeyVisibleTo(datatypes.JSON(`{bad`), allowed))
	require.False(t, auth.KeyVisibleTo(datatypes.JSON(`["marketing"]`), []string{}))
}

func TestKeyGrantShortfall(t *testing.T) {
	p := &auth.Principal{Grants: policy.Grants{"*": models.RoleViewer, "marketing": models.RoleAdmin}}
	require.Empty(t, auth.KeyGrantShortfall(p, models.RoleAdmin, []string{"marketing"}))
	require.Equal(t, []string{"finance"}, auth.KeyGrantShortfall(p, models.RoleOperator, []string{"marketing", "finance"}))
	require.Equal(t, []string{"*"}, auth.KeyGrantShortfall(p, models.RoleRunner, []string{"*"}))
	require.Empty(t, auth.KeyGrantShortfall(p, models.RoleViewer, []string{"*", "finance"}), "a cluster grant satisfies every namespace")
	require.Equal(t, []string{"marketing"}, auth.KeyGrantShortfall(nil, models.RoleViewer, []string{"marketing"}))
}

func TestScopeRestricts(t *testing.T) {
	require.False(t, auth.ScopeRestricts(nil))
	require.False(t, auth.ScopeRestricts([]byte(`{}`)))
	require.False(t, auth.ScopeRestricts([]byte(`{"jobs":[]}`)))
	require.True(t, auth.ScopeRestricts([]byte(`{"jobs":["etl"]}`)))
	require.True(t, auth.ScopeRestricts([]byte(`{"agent":{"incident_id":"`+uuid.NewString()+`"}}`)))
	require.True(t, auth.ScopeRestricts([]byte(`{bad`)))
}

// useFilePolicy installs a file-backed process policy holder declaring the
// given namespaces (plus default) for the test.
func useFilePolicy(t *testing.T, namespaces ...string) {
	t.Helper()
	doc := "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  default: {}\n"
	for _, ns := range namespaces {
		doc += "  " + ns + ": {}\n"
	}
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
}

func TestUndeclaredKeyNamespaces(t *testing.T) {
	previous := policy.ProcessHolder()
	policy.SetProcessHolder(nil)
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	require.Empty(t, auth.UndeclaredKeyNamespaces([]string{"marketing"}), "with no policy file every namespace is accepted")

	useFilePolicy(t, "marketing")
	require.Empty(t, auth.UndeclaredKeyNamespaces([]string{"*"}))
	require.Empty(t, auth.UndeclaredKeyNamespaces([]string{"default", "marketing"}))
	require.Equal(t, []string{"finance", "sales"}, auth.UndeclaredKeyNamespaces([]string{"finance", "marketing", "sales"}))
	require.Equal(t, `namespace "finance" is not declared in the access policy`,
		(&auth.UndeclaredNamespacesError{Namespaces: []string{"finance"}}).Error())
	require.Equal(t, `namespaces "finance", "sales" are not declared in the access policy`,
		(&auth.UndeclaredNamespacesError{Namespaces: []string{"finance", "sales"}}).Error())
}

// keyRow reads a key straight from the table.
func keyRow(t *testing.T, db *gorm.DB, id uuid.UUID) models.APIKey {
	t.Helper()
	var key models.APIKey
	require.NoError(t, db.First(&key, "id = ?", id).Error)
	return key
}

func countKeys(t *testing.T, db *gorm.DB) int64 {
	t.Helper()
	var n int64
	require.NoError(t, db.Model(&models.APIKey{}).Count(&n).Error)
	return n
}

func newKeyService(t *testing.T) (*gorm.DB, *auth.Service) {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	return db, auth.NewService(db)
}

// TestRotateKeyKeepsTheRemainingLifetime: the replacement expires when the old
// key would have, and the old key's grace never outlives its own expiry.
func TestRotateKeyKeepsTheRemainingLifetime(t *testing.T) {
	db, svc := newKeyService(t)
	expiry := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Second)
	original, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test", ExpiresAt: &expiry})
	require.NoError(t, err)

	rotated, err := svc.RotateKeyAuthorized(t.Context(), original.Key.ID, 24*time.Hour, "admin", allowKey)
	require.NoError(t, err)
	require.NotNil(t, rotated.Key.ExpiresAt, "a key with a TTL must not rotate into a key without one")
	require.True(t, rotated.Key.ExpiresAt.Equal(expiry))
	require.True(t, keyRow(t, db, rotated.Key.ID).ExpiresAt.Equal(expiry))
	require.True(t, keyRow(t, db, original.Key.ID).ExpiresAt.Equal(expiry),
		"a 24h grace must not extend a key that expires in 30m")

	// A key without an expiry rotates to one without, and gets the grace.
	open, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)
	before := time.Now().UTC()
	rotatedOpen, err := svc.RotateKeyAuthorized(t.Context(), open.Key.ID, time.Hour, "admin", allowKey)
	require.NoError(t, err)
	require.Nil(t, rotatedOpen.Key.ExpiresAt)
	grace := keyRow(t, db, open.Key.ID).ExpiresAt
	require.NotNil(t, grace)
	require.WithinDuration(t, before.Add(time.Hour), *grace, time.Minute)
}

// TestRotateKeyCutsALongTTLToTheGrace: rotating a key that still has 90 days
// to live cuts the OLD key's expiry to the grace window, so a leaked secret
// stops working when the grace ends rather than at the end of its TTL; the
// replacement keeps the 90 days.
func TestRotateKeyCutsALongTTLToTheGrace(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	clock := time.Now().UTC().Truncate(time.Second)
	svc := auth.NewService(db, auth.WithNow(func() time.Time { return clock }), auth.WithValidationFailureMinLatency(0))
	ttl := clock.Add(90 * 24 * time.Hour)
	original, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test", ExpiresAt: &ttl})
	require.NoError(t, err)

	rotated, err := svc.RotateKeyAuthorized(t.Context(), original.Key.ID, 24*time.Hour, "admin", allowKey)
	require.NoError(t, err)
	old := keyRow(t, db, original.Key.ID).ExpiresAt
	require.NotNil(t, old)
	require.True(t, old.Equal(clock.Add(24*time.Hour)), "the old key's 90-day expiry must be cut to the 24h grace, got %s", old)
	require.True(t, keyRow(t, db, rotated.Key.ID).ExpiresAt.Equal(ttl), "the replacement keeps the remaining 90 days")

	_, err = svc.ValidateKey(original.Plaintext)
	require.NoError(t, err, "the old key works during the grace")
	clock = clock.Add(25 * time.Hour)
	_, err = svc.ValidateKey(original.Plaintext)
	require.ErrorIs(t, err, auth.ErrKeyExpired, "the old key stops when the grace ends")
	_, err = svc.ValidateKey(rotated.Plaintext)
	require.NoError(t, err, "the replacement outlives the grace")
}

func TestRotateKeyRefusesExpiredKey(t *testing.T) {
	db, svc := newKeyService(t)
	past := time.Now().UTC().Add(-time.Minute)
	expired, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test", ExpiresAt: &past})
	require.NoError(t, err)
	before := countKeys(t, db)

	_, err = svc.RotateKeyAuthorized(t.Context(), expired.Key.ID, time.Hour, "admin", allowKey)
	require.ErrorIs(t, err, auth.ErrKeyExpired)
	require.Equal(t, before, countKeys(t, db), "no replacement is minted for an expired key")
}

// TestRotateKeyRefusesUnusableNamespaces: an unusable stored value is refused
// cleanly, before anything is written (it used to be copied and then fail to
// serialise after both writes landed).
func TestRotateKeyRefusesUnusableNamespaces(t *testing.T) {
	for _, value := range []string{`[]`, `null`, `{bad`, `["Bad_NS"]`} {
		t.Run(value, func(t *testing.T) {
			db, svc := newKeyService(t)
			original, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
			require.NoError(t, err)
			setStoredNamespaces(t, db, original.Key.ID, value)
			before := countKeys(t, db)

			_, err = svc.RotateKeyAuthorized(t.Context(), original.Key.ID, time.Hour, "admin", allowKey)
			require.ErrorIs(t, err, auth.ErrKeyNamespacesUnusable)
			require.Equal(t, before, countKeys(t, db))
			require.Nil(t, keyRow(t, db, original.Key.ID).ExpiresAt, "the old key's expiry must be untouched")
		})
	}
}

func TestRotateKeyRefusesUndeclaredNamespaces(t *testing.T) {
	db, svc := newKeyService(t)
	original, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role: models.RoleOperator, CreatedBy: "test", Namespaces: datatypes.JSON(`["finance","marketing"]`),
	})
	require.NoError(t, err)

	useFilePolicy(t, "marketing")
	before := countKeys(t, db)
	_, err = svc.RotateKeyAuthorized(t.Context(), original.Key.ID, time.Hour, "admin", allowKey)
	undeclared, ok := errors.AsType[*auth.UndeclaredNamespacesError](err)
	require.True(t, ok, "got %v", err)
	require.Equal(t, []string{"finance"}, undeclared.Namespaces)
	require.Equal(t, before, countKeys(t, db))

	useFilePolicy(t, "marketing", "finance")
	_, err = svc.RotateKeyAuthorized(t.Context(), original.Key.ID, time.Hour, "admin", allowKey)
	require.NoError(t, err)
}

// TestAuthorizedKeyOperationsDenyWithoutWriting: the authorizer sees the stored
// row inside the transaction, and its refusal leaves everything as it was.
func TestAuthorizedKeyOperationsDenyWithoutWriting(t *testing.T) {
	db, svc := newKeyService(t)
	key, err := svc.CreateKey(&auth.CreateKeyRequest{
		Role: models.RoleOperator, CreatedBy: "test", Namespaces: datatypes.JSON(`["finance"]`),
	})
	require.NoError(t, err)
	denied := errors.New("denied")
	var seen []string
	deny := func(k *models.APIKey) error {
		seen = append(seen, string(k.Namespaces))
		return denied
	}
	before := countKeys(t, db)

	_, err = svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, deny)
	require.ErrorIs(t, err, denied)
	_, err = svc.RotateKeyAuthorized(t.Context(), key.Key.ID, time.Hour, "admin", deny)
	require.ErrorIs(t, err, denied)

	require.Equal(t, []string{`["finance"]`, `["finance"]`}, seen)
	row := keyRow(t, db, key.Key.ID)
	require.Nil(t, row.RevokedAt)
	require.Nil(t, row.ExpiresAt)
	require.Equal(t, before, countKeys(t, db))

	_, err = svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, nil)
	require.Error(t, err, "an authorized operation never runs without an authorizer")
	_, err = svc.RotateKeyAuthorized(t.Context(), key.Key.ID, time.Hour, "admin", nil)
	require.Error(t, err)
	require.Nil(t, keyRow(t, db, key.Key.ID).RevokedAt)
}

func TestRevokeKeyAuthorized(t *testing.T) {
	db, svc := newKeyService(t)
	key, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)

	revoked, err := svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, allowKey)
	require.NoError(t, err)
	require.Equal(t, key.Key.ID, revoked.ID)
	require.NotNil(t, revoked.RevokedAt)
	require.NotNil(t, keyRow(t, db, key.Key.ID).RevokedAt)

	_, err = svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, allowKey)
	require.ErrorIs(t, err, auth.ErrKeyNotFound, "an already revoked key answers not found, as before")
	_, err = svc.RevokeKeyAuthorized(t.Context(), uuid.New(), allowKey)
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

// The interleaving tests run a competing change between the authorized read
// and the writes through the test hook. The unit database is one SQLite
// connection, so the competing change runs inside the same transaction: this
// proves the conditional writes and the rollback, not PostgreSQL's row lock
// (SELECT ... FOR UPDATE), which closes the window there.

func TestRotateKeyRacingRevokeMintsNothing(t *testing.T) {
	db, svc := newKeyService(t)
	key, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)
	before := countKeys(t, db)

	t.Cleanup(auth.SetKeyWriteHookForTest(func(tx *gorm.DB, id uuid.UUID) error {
		return tx.Model(&models.APIKey{}).Where("id = ?", id).Update("revoked_at", time.Now().UTC()).Error
	}))
	_, err = svc.RotateKeyAuthorized(t.Context(), key.Key.ID, time.Hour, "admin", allowKey)
	require.ErrorIs(t, err, auth.ErrKeyRevoked)
	require.Equal(t, before, countKeys(t, db), "a key revoked under the rotation gets no replacement")
}

func TestRotateKeyNeverLengthensAShorterExpiry(t *testing.T) {
	db, svc := newKeyService(t)
	key, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)
	shorter := time.Now().UTC().Add(5 * time.Minute).Truncate(time.Second)

	t.Cleanup(auth.SetKeyWriteHookForTest(func(tx *gorm.DB, id uuid.UUID) error {
		// A competing rotation already gave the key a 5m grace.
		return tx.Model(&models.APIKey{}).Where("id = ?", id).Update("expires_at", shorter).Error
	}))
	_, err = svc.RotateKeyAuthorized(t.Context(), key.Key.ID, 24*time.Hour, "admin", allowKey)
	require.NoError(t, err)
	stored := keyRow(t, db, key.Key.ID).ExpiresAt
	require.NotNil(t, stored)
	require.True(t, stored.Equal(shorter), "the 24h grace must not overwrite the shorter %s with %s", shorter, stored)
}

func TestRevokeKeyAuthorizedRacingRevoke(t *testing.T) {
	_, svc := newKeyService(t)
	key, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)

	t.Cleanup(auth.SetKeyWriteHookForTest(func(tx *gorm.DB, id uuid.UUID) error {
		return tx.Model(&models.APIKey{}).Where("id = ?", id).Update("revoked_at", time.Now().UTC()).Error
	}))
	_, err = svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, allowKey)
	require.ErrorIs(t, err, auth.ErrKeyNotFound)
}

// TestAuthorizedKeyOperationsRetryCatalogContention: a contention error in an
// attempt (here injected by the hook) rolls it back and the whole transaction
// runs again, so a valid revoke or rotation is not lost to a busy catalog.
func TestAuthorizedKeyOperationsRetryCatalogContention(t *testing.T) {
	db, svc := newKeyService(t)
	rotatable, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)
	revocable, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
	require.NoError(t, err)

	attempts := 0
	t.Cleanup(auth.SetKeyWriteHookForTest(func(*gorm.DB, uuid.UUID) error {
		attempts++
		if attempts%2 == 1 {
			return errors.New("database is locked")
		}
		return nil
	}))

	before := countKeys(t, db)
	rotated, err := svc.RotateKeyAuthorized(t.Context(), rotatable.Key.ID, time.Hour, "admin", allowKey)
	require.NoError(t, err)
	require.Equal(t, 2, attempts, "the contended attempt is retried once")
	require.Equal(t, before+1, countKeys(t, db), "the rolled-back attempt left no replacement behind")
	require.Equal(t, rotated.Key.ID, keyRow(t, db, rotated.Key.ID).ID)

	_, err = svc.RevokeKeyAuthorized(t.Context(), revocable.Key.ID, allowKey)
	require.NoError(t, err)
	require.Equal(t, 4, attempts)
	require.NotNil(t, keyRow(t, db, revocable.Key.ID).RevokedAt)

	// A non-contention error is not retried.
	attempts = 0
	t.Cleanup(auth.SetKeyWriteHookForTest(func(*gorm.DB, uuid.UUID) error {
		attempts++
		return errors.New("boom")
	}))
	_, err = svc.RevokeKeyAuthorized(t.Context(), rotated.Key.ID, allowKey)
	require.ErrorContains(t, err, "boom")
	require.Equal(t, 1, attempts)
}

// TestAuthorizedKeyOperationsSurviveASnapshotConflict drives the real failure
// with two independent connections to one WAL database: after the authorized
// read, another connection commits an unrelated change, so the attempt's read
// snapshot can no longer become a write (SQLITE_BUSY). The retry runs the
// whole transaction again and the operation succeeds.
func TestAuthorizedKeyOperationsSurviveASnapshotConflict(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "catalog.db") + "?_journal_mode=WAL&_busy_timeout=0"
	open := func() *gorm.DB {
		conn, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: logger.Discard})
		require.NoError(t, err)
		sqlDB, err := conn.DB()
		require.NoError(t, err)
		sqlDB.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = sqlDB.Close() })
		return conn
	}
	db := open()
	require.NoError(t, db.AutoMigrate(&models.APIKey{}))
	peer := open()
	svc := auth.NewService(db)

	bystander, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleViewer, CreatedBy: "test"})
	require.NoError(t, err)
	attempts, conflicts := 0, 0
	t.Cleanup(auth.SetKeyWriteHookForTest(func(*gorm.DB, uuid.UUID) error {
		attempts++
		if attempts%2 == 1 {
			conflicts++
			// The peer's write itself must succeed: the attempt holds only a
			// read snapshot, so the failure that follows is the attempt's own.
			require.NoError(t, peer.Model(&models.APIKey{}).Where("id = ?", bystander.Key.ID).
				Update("description", fmt.Sprintf("peer commit %d", conflicts)).Error)
		}
		return nil
	}))

	for _, op := range []string{"rotate", "revoke"} {
		key, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleOperator, CreatedBy: "test"})
		require.NoError(t, err)
		start := attempts
		if op == "rotate" {
			_, err = svc.RotateKeyAuthorized(t.Context(), key.Key.ID, time.Hour, "admin", allowKey)
		} else {
			_, err = svc.RevokeKeyAuthorized(t.Context(), key.Key.ID, allowKey)
		}
		require.NoError(t, err, op)
		require.Equal(t, 2, attempts-start, "%s: the peer's commit must have failed the first attempt, which is retried", op)
	}
}

// TestAdminKeyExistsIgnoresScopedAdmins: a job-scoped or agent-session admin
// key is not a cluster admin, so bootstrap still mints one.
func TestAdminKeyExistsIgnoresScopedAdmins(t *testing.T) {
	for _, scope := range []*models.KeyScope{
		{Jobs: []string{"etl"}},
		{Agent: &models.AgentClaim{IncidentID: uuid.New()}},
	} {
		db, svc := newKeyService(t)
		_, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test", Scope: scope})
		require.NoError(t, err)
		exists, err := svc.AdminKeyExists()
		require.NoError(t, err)
		require.False(t, exists)

		plaintext, err := svc.Bootstrap()
		require.NoError(t, err)
		require.NotEmpty(t, plaintext, "bootstrap must mint a cluster admin past a scoped one")
		exists, err = svc.AdminKeyExists()
		require.NoError(t, err)
		require.True(t, exists)
		require.Equal(t, int64(2), countKeys(t, db))
	}

	_, svc := newKeyService(t)
	_, err := svc.CreateKey(&auth.CreateKeyRequest{Role: models.RoleAdmin, CreatedBy: "test", Scope: &models.KeyScope{}})
	require.NoError(t, err)
	exists, err := svc.AdminKeyExists()
	require.NoError(t, err)
	require.True(t, exists, "an empty scope is unrestricted")
}

func TestBootstrapRefreshesAScopedSlot(t *testing.T) {
	db, svc := newKeyService(t)
	first, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, first)
	require.NoError(t, db.Exec(`UPDATE api_keys SET scope = ? WHERE bootstrap_slot IS NOT NULL`, `{"jobs":["etl"]}`).Error)

	refreshed, err := svc.Bootstrap()
	require.NoError(t, err)
	require.NotEmpty(t, refreshed)
	key, err := svc.ValidateKey(refreshed)
	require.NoError(t, err)
	require.Empty(t, key.Scope)
	require.Equal(t, int64(1), countKeys(t, db))
}
