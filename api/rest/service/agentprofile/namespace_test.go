package agentprofile

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

// Agent profiles as shared objects through the service (identity-and-access D4).

func newProfileNamespaceTest(t *testing.T) *gorm.DB {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	return db
}

func TestProfileListIsConfinedToAllowedNamespacesPlusShared(t *testing.T) {
	db := newProfileNamespaceTest(t)
	require.NoError(t, SeedDefaults(context.Background(), db))
	unrestricted := NewWithDatabase(context.Background(), db)
	for name, namespace := range map[string]string{"a-mkt": "marketing", "b-fin": "finance", "c-shared": "*"} {
		_, err := unrestricted.Create(&CreateRequest{Namespace: namespace, Name: name, Image: "img"})
		require.NoError(t, err)
	}
	names := func(svc Service) []string {
		profiles, err := svc.List(&ListRequest{OrderBy: []string{"name ASC"}})
		require.NoError(t, err)
		out := make([]string, 0, len(profiles))
		for _, p := range profiles {
			out = append(out, p.Name)
		}
		return out
	}
	require.Equal(t, []string{"a-mkt", "b-fin", "c-shared", DefaultTriageOnlyProfileName}, names(unrestricted))
	ctx := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	require.Equal(t, []string{"a-mkt", "c-shared", DefaultTriageOnlyProfileName}, names(NewWithDatabase(ctx, db)),
		"marketing's profiles and every shared one, the built-in triage-only among them")
	require.True(t, authz.Filtered(ctx))
}

func TestProfileCreateRejectsUndeclaredNamespaceWithAPolicyFile(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	svc := NewWithDatabase(context.Background(), newProfileNamespaceTest(t))
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)

	_, err = svc.Create(&CreateRequest{Namespace: "nowhere", Name: "p", Image: "img"})
	undeclared, ok := errors.AsType[*auth.UndeclaredNamespacesError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, []string{"nowhere"}, undeclared.Namespaces)
	for _, namespace := range []string{"marketing", "*", ""} {
		_, err := svc.Create(&CreateRequest{Namespace: namespace, Name: "ok-" + uuid.NewString(), Image: "img"})
		require.NoError(t, err, "namespace %q", namespace)
	}
	policy.SetProcessHolder(nil)
	_, err = svc.Create(&CreateRequest{Namespace: "nowhere", Name: "p", Image: "img"})
	require.NoError(t, err, "without a policy file every namespace is accepted")
}

func TestProfileNamesSurviveTombstones(t *testing.T) {
	db := newProfileNamespaceTest(t)
	svc := NewWithDatabase(context.Background(), db)

	created, err := svc.Create(&CreateRequest{Name: "squat", Image: "img"})
	require.NoError(t, err)
	_, err = svc.Create(&CreateRequest{Name: "squat", Image: "img"})
	require.ErrorIs(t, err, ErrProfileNameConflict)
	require.NoError(t, svc.Delete(created.ID))
	_, err = svc.Create(&CreateRequest{Name: "squat", Image: "img"})
	require.NoError(t, err, "a deleted profile's name can be created again")
	var tombstone models.AgentProfile
	require.NoError(t, db.Unscoped().First(&tombstone, "id = ?", created.ID).Error)
	require.Equal(t, models.TombstoneName("squat", created.ID), tombstone.Name, "the tombstone keeps its row for session history")
	require.True(t, tombstone.DeletedAt.Valid)

	// A legacy tombstone holding the plain name blocks neither a create nor a rename.
	legacy := models.AgentProfile{ID: uuid.New(), Name: "legacy", Image: "img", Engine: models.AtomEngineDocker}
	require.NoError(t, db.Create(&legacy).Error)
	require.NoError(t, db.Delete(&models.AgentProfile{}, "id = ?", legacy.ID).Error)
	other, err := svc.Create(&CreateRequest{Name: "other", Image: "img"})
	require.NoError(t, err)
	name := "legacy"
	_, err = svc.Update(other.ID, &UpdateRequest{Name: &name})
	require.NoError(t, err)

	// No live profile may take a tombstone's name (a delete's rename would
	// collide with it).
	reserved := models.TombstoneName("other", other.ID)
	_, err = svc.Create(&CreateRequest{Name: reserved, Image: "img"})
	require.ErrorIs(t, err, ErrInvalidProfile)
	_, err = svc.Update(other.ID, &UpdateRequest{Name: &reserved})
	require.ErrorIs(t, err, ErrInvalidProfile)

	require.ErrorIs(t, nameConflictError(errors.New("UNIQUE constraint failed: agent_profiles.name"), "x"), ErrProfileNameConflict)
}

// TestSeedDefaultsLeavesADeletedBuiltinDeleted: deleting the built-in
// triage-only profile releases its name but does not resurrect it.
func TestSeedDefaultsLeavesADeletedBuiltinDeleted(t *testing.T) {
	db := newProfileNamespaceTest(t)
	require.NoError(t, SeedDefaults(context.Background(), db))
	var builtin models.AgentProfile
	require.NoError(t, db.First(&builtin, "name = ?", DefaultTriageOnlyProfileName).Error)
	require.NoError(t, NewWithDatabase(context.Background(), db).Delete(builtin.ID))

	require.NoError(t, SeedDefaults(context.Background(), db))
	var live int64
	require.NoError(t, db.Model(&models.AgentProfile{}).Where("name = ?", DefaultTriageOnlyProfileName).Count(&live).Error)
	require.Zero(t, live)
}
