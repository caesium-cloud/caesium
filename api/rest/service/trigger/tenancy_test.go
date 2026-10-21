package trigger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// tenancyCatalog has an HTTP trigger per namespace, each serving one job.
func tenancyCatalog(t *testing.T) (*gorm.DB, map[string]uuid.UUID) {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	svc := &triggerService{ctx: context.Background(), db: db}
	triggers := map[string]uuid.UUID{}
	now := time.Now().UTC()
	for _, ns := range []string{"marketing", "finance"} {
		trigger, err := svc.Create(&CreateRequest{Alias: ns + "-trigger", Type: "http", Configuration: map[string]any{"path": "/hooks/" + ns}})
		require.NoError(t, err)
		triggers[ns] = trigger.ID
		require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: ns + "-job", Namespace: ns, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}).Error)
	}
	return db, triggers
}

func loadTenancyPolicy(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
}

func TestUpdateTriggerTenancy(t *testing.T) {
	db, triggers := tenancyCatalog(t)
	svc := &triggerService{ctx: context.Background(), db: db}
	marketing := internaljobdef.NewTenancyScope([]string{"marketing"}, nil)
	alias := "renamed"

	// A confined writer may edit its own trigger, but not subscribe it to a
	// path a job outside its namespaces is on; the refusal writes nothing.
	updated, err := svc.Update(triggers["marketing"], &UpdateRequest{Alias: &alias, Scope: marketing})
	require.NoError(t, err)
	require.Equal(t, "renamed", updated.Alias)
	_, err = svc.Update(triggers["marketing"], &UpdateRequest{Configuration: map[string]any{"path": "/hooks/finance"}, Scope: marketing})
	nsErr, ok := errors.AsType[*internaljobdef.NamespaceAllowlistError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, internaljobdef.NamespaceReasonWebhookPath, nsErr.Reason)
	var row models.Trigger
	require.NoError(t, db.First(&row, "id = ?", triggers["marketing"]).Error)
	require.Equal(t, "marketing", row.NormalizedPath)

	// A trigger also serving a job outside the scope (moved since the
	// middleware decided) is refused, even for an alias-only edit.
	_, err = svc.Update(triggers["finance"], &UpdateRequest{Alias: &alias, Scope: marketing})
	_, ok = errors.AsType[*internaljobdef.NamespaceAllowlistError](err)
	require.True(t, ok, "%v", err)

	// Without a scope or a policy file the update is the old one.
	_, err = svc.Update(triggers["marketing"], &UpdateRequest{Configuration: map[string]any{"path": "/hooks/finance"}})
	require.NoError(t, err)
}

// The row's type is decided inside the PATCH transaction: a cron trigger an
// apply turns into an HTTP one just before the transaction cannot carry a
// webhook path past the check (the bypass a pre-transaction type read had).
func TestUpdateTriggerTenancyDecidesTypeInTransaction(t *testing.T) {
	db, triggers := tenancyCatalog(t)
	svc := &triggerService{ctx: context.Background(), db: db}
	cron, err := svc.Create(&CreateRequest{Alias: "mkt-cron", Type: "cron", Configuration: map[string]any{"expression": "0 * * * *"}})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Job{ID: uuid.New(), Alias: "mkt-cron-job", Namespace: "marketing", TriggerID: cron.ID, CreatedAt: now, UpdatedAt: now}).Error)

	beforePatchTx = func(id uuid.UUID) {
		require.NoError(t, db.Model(&models.Trigger{}).Where("id = ?", id).Update("type", models.TriggerTypeHTTP).Error)
	}
	t.Cleanup(func() { beforePatchTx = nil })
	marketing := internaljobdef.NewTenancyScope([]string{"marketing"}, nil)
	_, err = svc.Update(cron.ID, &UpdateRequest{Configuration: map[string]any{"path": "/hooks/finance"}, Scope: marketing})
	nsErr, ok := errors.AsType[*internaljobdef.NamespaceAllowlistError](err)
	require.True(t, ok, "%v", err)
	require.Equal(t, internaljobdef.NamespaceReasonWebhookPath, nsErr.Reason)
	var row models.Trigger
	require.NoError(t, db.First(&row, "id = ?", cron.ID).Error)
	require.Empty(t, row.NormalizedPath, "the refused PATCH wrote nothing")
	require.NotEqual(t, triggers["finance"], row.ID)

	// A trigger that stays a cron trigger is edited with no webhook path.
	beforePatchTx = nil
	require.NoError(t, db.Model(&models.Trigger{}).Where("id = ?", cron.ID).Update("type", models.TriggerTypeCron).Error)
	updated, err := svc.Update(cron.ID, &UpdateRequest{Configuration: map[string]any{"expression": "5 * * * *"}, Scope: marketing})
	require.NoError(t, err)
	require.Empty(t, updated.NormalizedPath)
}

func TestUpdateTriggerTenancyWithPolicyFile(t *testing.T) {
	loadTenancyPolicy(t)
	db, triggers := tenancyCatalog(t)
	svc := &triggerService{ctx: context.Background(), db: db}

	// Every writer is held to one namespace per webhook path.
	_, err := svc.Update(triggers["marketing"], &UpdateRequest{Configuration: map[string]any{"path": "/hooks/finance"}})
	require.ErrorIs(t, err, internaljobdef.ErrTriggerTenancy)
	_, err = svc.Update(triggers["marketing"], &UpdateRequest{Configuration: map[string]any{"path": "/hooks/fresh"}})
	require.NoError(t, err)

	// A row serving two namespaces cannot be edited at all (B9 fold 7).
	require.NoError(t, db.Model(&models.Job{}).Where("alias = ?", "finance-job").Update("trigger_id", triggers["marketing"]).Error)
	alias := "anything"
	_, err = svc.Update(triggers["marketing"], &UpdateRequest{Alias: &alias})
	require.ErrorIs(t, err, internaljobdef.ErrTriggerTenancy)
	both := internaljobdef.NewTenancyScope([]string{"marketing", "finance"}, nil)
	_, err = svc.Update(triggers["marketing"], &UpdateRequest{Alias: &alias, Scope: both})
	require.ErrorIs(t, err, internaljobdef.ErrTriggerTenancy)
}
