package notification

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/authz"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Shared-object semantics through the service (identity-and-access D4).

func newNamespaceTestService(t *testing.T, ctx context.Context) (*gorm.DB, Service) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	return db, NewWithDatabase(ctx, db)
}

func seedChannel(t *testing.T, db *gorm.DB, name, namespace string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.NotificationChannel{ID: id, Namespace: namespace, Name: name, Type: "webhook", Config: []byte(`{}`), Enabled: true}).Error)
	return id
}

func seedPolicy(t *testing.T, db *gorm.DB, name, namespace string, channel uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	require.NoError(t, db.Create(&models.NotificationPolicy{ID: id, Namespace: namespace, Name: name, ChannelID: channel, EventTypes: []byte(`["run_failed"]`), Enabled: true}).Error)
	return id
}

func channelNames(channels []models.NotificationChannel) []string {
	names := make([]string, 0, len(channels))
	for _, ch := range channels {
		names = append(names, ch.Name)
	}
	return names
}

func TestListsAreConfinedToAllowedNamespacesPlusShared(t *testing.T) {
	db, unrestricted := newNamespaceTestService(t, context.Background())
	mkt := seedChannel(t, db, "a-mkt", "marketing")
	fin := seedChannel(t, db, "b-fin", "finance")
	shared := seedChannel(t, db, "c-shared", models.ClusterNamespace)
	seedChannel(t, db, "d-default", "default")
	legacy := seedChannel(t, db, "e-legacy", "default")
	require.NoError(t, db.Exec("UPDATE notification_channels SET namespace = '' WHERE id = ?", legacy).Error)
	seedPolicy(t, db, "p-mkt", "marketing", mkt)
	seedPolicy(t, db, "p-fin", "finance", fin)
	seedPolicy(t, db, "p-shared", models.ClusterNamespace, shared)

	list := func(svc Service, req *ListRequest) []string {
		channels, err := svc.ListChannels(req)
		require.NoError(t, err)
		return channelNames(channels)
	}
	ordered := &ListRequest{OrderBy: []string{"name ASC"}}

	// Unrestricted: every row, order and pagination unchanged.
	require.Equal(t, []string{"a-mkt", "b-fin", "c-shared", "d-default", "e-legacy"}, list(unrestricted, ordered))
	require.Equal(t, []string{"b-fin", "c-shared"}, list(unrestricted, &ListRequest{OrderBy: []string{"name ASC"}, Limit: 2, Offset: 1}))

	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.Equal(t, []string{"a-mkt", "c-shared"}, list(NewWithDatabase(ctx, db), ordered))
	require.True(t, authz.Filtered(ctx), "the list marks the collection filtered")

	ctx = authz.WithAllowedNamespaces(context.Background(), []string{"default", "finance"})
	require.Equal(t, []string{"b-fin", "c-shared", "d-default", "e-legacy"}, list(NewWithDatabase(ctx, db), ordered))
	require.Equal(t, []string{"c-shared"}, list(NewWithDatabase(ctx, db), &ListRequest{OrderBy: []string{"name ASC"}, Limit: 1, Offset: 1}),
		"pagination applies after the filter")

	policies, err := NewWithDatabase(authz.WithAllowedNamespaces(context.Background(), []string{"marketing"}), db).ListPolicies(nil)
	require.NoError(t, err)
	var names []string
	for _, p := range policies {
		names = append(names, p.Name)
	}
	require.ElementsMatch(t, []string{"p-mkt", "p-shared"}, names)
}

// TestPolicyReferenceDoesNotDiscloseForeignChannels: a foreign channel and a
// missing one fail the same way, on create and on patch.
func TestPolicyReferenceDoesNotDiscloseForeignChannels(t *testing.T) {
	db, svc := newNamespaceTestService(t, context.Background())
	fin := seedChannel(t, db, "fin", "finance")
	shared := seedChannel(t, db, "shared", models.ClusterNamespace)
	own := seedChannel(t, db, "own", "marketing")

	create := func(channel uuid.UUID) error {
		_, err := svc.CreatePolicy(&CreatePolicyRequest{Namespace: "marketing", Name: "p-" + uuid.NewString(), ChannelID: channel, EventTypes: []string{"run_failed"}})
		return err
	}
	foreign, missing := create(fin), create(uuid.New())
	require.ErrorIs(t, foreign, ErrInvalidPolicy)
	require.ErrorIs(t, missing, ErrInvalidPolicy)
	require.NoError(t, create(shared))

	p, err := svc.CreatePolicy(&CreatePolicyRequest{Namespace: "marketing", Name: "patched", ChannelID: own, EventTypes: []string{"run_failed"}})
	require.NoError(t, err)
	_, err = svc.UpdatePolicy(p.ID, &UpdatePolicyRequest{ChannelID: &fin})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	random := uuid.New()
	_, err = svc.UpdatePolicy(p.ID, &UpdatePolicyRequest{ChannelID: &random})
	require.ErrorIs(t, err, ErrInvalidPolicy)
}

func TestCreateRejectsUndeclaredNamespaceWithAPolicyFile(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	db, svc := newNamespaceTestService(t, context.Background())
	shared := seedChannel(t, db, "shared", models.ClusterNamespace)

	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)

	_, err = svc.CreateChannel(&CreateChannelRequest{Namespace: "nowhere", Name: "c", Type: "webhook"})
	undeclared, ok := errors.AsType[*auth.UndeclaredNamespacesError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, []string{"nowhere"}, undeclared.Namespaces)
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Namespace: "nowhere", Name: "p", ChannelID: shared, EventTypes: []string{"run_failed"}})
	_, ok = errors.AsType[*auth.UndeclaredNamespacesError](err)
	require.True(t, ok, "%v", err)

	for _, namespace := range []string{"marketing", "*", "", "default"} {
		_, err := svc.CreateChannel(&CreateChannelRequest{Namespace: namespace, Name: "ok-" + uuid.NewString(), Type: "webhook"})
		require.NoError(t, err, "namespace %q is declared (or shared, or default)", namespace)
	}

	// Without a policy file every namespace is accepted, as before.
	policy.SetProcessHolder(nil)
	_, err = svc.CreateChannel(&CreateChannelRequest{Namespace: "nowhere", Name: "c", Type: "webhook"})
	require.NoError(t, err)
}

// TestSharedObjectNamesSurviveTombstones: a delete releases the name, a
// tombstone left before that never blocks a create or rename, and losing the
// race for a name is a conflict, never a 500.
func TestSharedObjectNamesSurviveTombstones(t *testing.T) {
	db, svc := newNamespaceTestService(t, context.Background())
	shared := seedChannel(t, db, "shared", models.ClusterNamespace)

	// Policies: delete then re-create and rename onto the freed name.
	p, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "squat", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.NoError(t, err)
	require.NoError(t, svc.DeletePolicy(p.ID))
	again, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "squat", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.NoError(t, err, "a deleted policy's name can be created again")
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "squat", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.ErrorIs(t, err, ErrPolicyNameConflict, "a live name is a conflict")
	require.NoError(t, svc.DeletePolicy(again.ID))
	other, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "other", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.NoError(t, err)
	squat := "squat"
	_, err = svc.UpdatePolicy(other.ID, &UpdatePolicyRequest{Name: &squat})
	require.NoError(t, err, "a rename onto a deleted policy's name succeeds")

	// A legacy tombstone (soft-deleted before deletes released names) holding
	// the plain name.
	legacy := seedPolicy(t, db, "legacy", "default", shared)
	require.NoError(t, db.Delete(&models.NotificationPolicy{}, "id = ?", legacy).Error)
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: "legacy", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.NoError(t, err, "a legacy tombstone no longer holds its name")
	var tombstone models.NotificationPolicy
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", legacy).Error)
	require.Equal(t, models.TombstoneName("legacy", legacy), tombstone.Name)
	require.True(t, tombstone.DeletedAt.Valid)

	legacyRename := seedPolicy(t, db, "legacy-rename", "default", shared)
	require.NoError(t, db.Delete(&models.NotificationPolicy{}, "id = ?", legacyRename).Error)
	target := "legacy-rename"
	_, err = svc.UpdatePolicy(other.ID, &UpdatePolicyRequest{Name: &target})
	require.NoError(t, err)

	// Channels (hard-deleted) and a legacy soft-deleted channel row.
	ch, err := svc.CreateChannel(&CreateChannelRequest{Name: "pager", Type: "webhook"})
	require.NoError(t, err)
	_, err = svc.CreateChannel(&CreateChannelRequest{Name: "pager", Type: "webhook"})
	require.ErrorIs(t, err, ErrChannelNameConflict)
	require.NoError(t, svc.DeleteChannel(ch.ID))
	_, err = svc.CreateChannel(&CreateChannelRequest{Name: "pager", Type: "webhook"})
	require.NoError(t, err)
	oldChannel := seedChannel(t, db, "old-channel", "default")
	require.NoError(t, db.Delete(&models.NotificationChannel{}, "id = ?", oldChannel).Error)
	_, err = svc.CreateChannel(&CreateChannelRequest{Name: "old-channel", Type: "webhook"})
	require.NoError(t, err)

	// No live object may take a tombstone's name, so a delete's rename never
	// collides: a name carrying the marker is invalid on create and rename.
	reserved := models.TombstoneName("pager", ch.ID)
	_, err = svc.CreateChannel(&CreateChannelRequest{Name: reserved, Type: "webhook"})
	require.ErrorIs(t, err, ErrInvalidChannel)
	live, err := svc.CreateChannel(&CreateChannelRequest{Name: "live-channel", Type: "webhook"})
	require.NoError(t, err)
	_, err = svc.UpdateChannel(live.ID, &UpdateChannelRequest{Name: &reserved})
	require.ErrorIs(t, err, ErrInvalidChannel)
	_, err = svc.CreatePolicy(&CreatePolicyRequest{Name: models.TombstoneName("other", other.ID), ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.ErrorIs(t, err, ErrInvalidPolicy)
	_, err = svc.UpdatePolicy(other.ID, &UpdatePolicyRequest{Name: &reserved})
	require.ErrorIs(t, err, ErrInvalidPolicy)

	// A write that loses a race for the name surfaces as the conflict.
	require.ErrorIs(t, nameConflictError(errors.New("UNIQUE constraint failed: notification_policies.name"), ErrPolicyNameConflict, "x"), ErrPolicyNameConflict)
	require.ErrorIs(t, nameConflictError(errors.New("UNIQUE constraint failed: notification_channels.name"), ErrChannelNameConflict, "x"), ErrChannelNameConflict)
	plain := errors.New("disk I/O error")
	require.Equal(t, plain, nameConflictError(plain, ErrPolicyNameConflict, "x"))
}

// TestSharedObjectWritesRetryContention: a name-claiming write is one
// transaction, retried whole on transient contention (pkg/db TransactionOn)
// rather than surfacing it as an error.
func TestSharedObjectWritesRetryContention(t *testing.T) {
	db, svc := newNamespaceTestService(t, context.Background())
	shared := seedChannel(t, db, "shared", models.ClusterNamespace)

	failures := map[string]int{"notification_policies": 1, "notification_channels": 1}
	inject := func(tx *gorm.DB) {
		if failures[tx.Statement.Table] > 0 {
			failures[tx.Statement.Table]--
			_ = tx.AddError(errors.New("database is locked"))
		}
	}
	require.NoError(t, db.Callback().Create().Before("gorm:create").Register("test:contention", inject))
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:contention", inject))

	p, err := svc.CreatePolicy(&CreatePolicyRequest{Name: "contended", ChannelID: shared, EventTypes: []string{"run_failed"}})
	require.NoError(t, err, "contention on the insert retries the whole claim")
	ch, err := svc.CreateChannel(&CreateChannelRequest{Name: "contended", Type: "webhook"})
	require.NoError(t, err)

	failures["notification_policies"] = 1
	require.NoError(t, svc.DeletePolicy(p.ID), "contention on the tombstone rename retries the delete")
	var live int64
	require.NoError(t, db.Model(&models.NotificationPolicy{}).Where("name = ?", "contended").Count(&live).Error)
	require.Zero(t, live)

	failures["notification_channels"] = 1
	renamed := "contended-2"
	_, err = svc.UpdateChannel(ch.ID, &UpdateChannelRequest{Name: &renamed})
	require.NoError(t, err)
}
