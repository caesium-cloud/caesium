package start

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	internaljobdef "github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func patchDefinition(alias, namespace string) schema.Definition {
	return schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       schema.KindJob,
		Metadata:   schema.Metadata{Alias: alias, Namespace: namespace},
		Trigger:    schema.Trigger{Type: schema.TriggerCron, Configuration: map[string]any{"cron": "0 0 31 2 *"}},
		Steps:      []schema.Step{{Name: "ok", Engine: schema.EngineDocker, Image: "busybox:1.36.1", Command: []string{"true"}}},
	}
}

// TestApplyJobdefPatchRefusesUndeclaredNamespaceOnBothRoutes: with a policy
// file loaded, an agent patch that moves a job into an undeclared namespace is
// refused on the dry-run (git-synced, escalate) route exactly as on the direct
// route, before anything is rendered for a human; a declared namespace still
// renders, and with no policy file every valid namespace is accepted (B2).
func TestApplyJobdefPatchRefusesUndeclaredNamespaceOnBothRoutes(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ctx := context.Background()

	const alias = "git-synced-ingest"
	encoded, err := json.Marshal(patchDefinition(alias, "marketing"))
	require.NoError(t, err)
	var applied schema.Definition
	require.NoError(t, json.Unmarshal(encoded, &applied))
	_, err = internaljobdef.NewImporter(db).Apply(ctx, &applied)
	require.NoError(t, err)
	var job models.Job
	require.NoError(t, db.Where("alias = ?", alias).First(&job).Error)

	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte("apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing: {}\n  finance: {}\n"), 0o600))
	holder, err := policy.LoadFile(t.Context(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	previous := policy.ProcessHolder()
	t.Cleanup(func() { policy.SetProcessHolder(previous) })
	policy.SetProcessHolder(holder)

	ops := newIncidentActionOps(db, nil, nil)
	patch := func(namespace string) json.RawMessage {
		raw, err := json.Marshal(patchDefinition(alias, namespace))
		require.NoError(t, err)
		return raw
	}

	_, dryRunErr := ops.ApplyJobdefPatch(ctx, job.ID, patch("sales"), true)
	require.ErrorIs(t, dryRunErr, internaljobdef.ErrNamespaceNotDeclared, "the git-synced route must not escalate an undeclared namespace")
	require.Contains(t, dryRunErr.Error(), `metadata.namespace "sales" of job "git-synced-ingest" is not declared`)
	_, directErr := ops.ApplyJobdefPatch(ctx, job.ID, patch("sales"), false)
	require.ErrorIs(t, directErr, internaljobdef.ErrNamespaceNotDeclared)
	require.Equal(t, dryRunErr.Error(), directErr.Error(), "both provenance routes refuse with the same error")

	rendered, err := ops.ApplyJobdefPatch(ctx, job.ID, patch("finance"), true)
	require.NoError(t, err, "a declared namespace still renders for escalation")
	require.Contains(t, string(rendered), alias)
	require.NoError(t, db.First(&job, "id = ?", job.ID).Error)
	require.Equal(t, "marketing", job.Namespace, "a dry run writes nothing")

	policy.SetProcessHolder(nil)
	_, err = ops.ApplyJobdefPatch(ctx, job.ID, patch("sales"), true)
	require.NoError(t, err, "with no policy file every valid namespace is accepted")
}
