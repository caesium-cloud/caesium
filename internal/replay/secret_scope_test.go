package replay

import (
	"context"
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// TestReplaySecretVerificationDenialIsTypedAndRedacted: a pinned Vault
// reference resolved while its namespace allowed it, then denied when replay
// re-verifies the baseline identity (the rule was revoked in between). The
// failure keeps ErrSecretDenied for the executor's terminal handling and never
// re-attaches the raw reference.
func TestReplaySecretVerificationDenialIsTypedAndRedacted(t *testing.T) {
	const ref = "secret://vault/secret/data/finance/payroll/password"
	expected := []models.TaskExecutionSecretRef{{
		Ref: ref, EnvKey: "TOKEN", Provider: "vault", Verifiable: true,
		Identity: datatypes.JSONMap{"version": "3", "keyId": "k1"},
	}}
	actual := []jobdefruntime.ResolvedSecretIdentity{{
		EnvKey: "TOKEN", Ref: ref,
		Identity: secret.Identity{Provider: "vault", Ref: ref, Version: "3", KeyID: "k1", Verifiable: true},
	}}
	inner := secret.NewMultiResolver(map[string]secret.Resolver{"vault": secret.NewVaultResolverWithLogical(nil)})
	revoked := secret.NewScopedResolver(inner, "marketing", &policy.SecretRules{Allow: []string{}}, "")

	err := VerifyReplaySecretIdentities(context.Background(), revoked, expected, actual, map[string]string{"TOKEN": "value"})
	require.ErrorIs(t, err, secret.ErrSecretDenied)
	require.Contains(t, err.Error(), "replay secret for env TOKEN:")
	require.Contains(t, err.Error(), "vault/secret/…")
	require.False(t, strings.Contains(err.Error(), "payroll"), err.Error())
	require.False(t, strings.Contains(err.Error(), ref), err.Error())
}

// TestReplayChecksSecretRulesBeforeReusingACachedStep: an unchanged step is
// reused from its baseline result without reaching an executor, so the
// constructor runs the namespace allow-check first. While the namespace allows
// the reference the replay is a cache hit; once it no longer does, the replay
// is refused (typed, redacted) and nothing is materialized. Not parallel: it
// installs the process holder.
func TestReplayChecksSecretRulesBeforeReusingACachedStep(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const ref = "secret://env/C4_REPLAY_TOKEN"
	rules := func(allow ...string) *policy.Holder {
		view := policy.EmptyPolicy()
		view.Namespaces[models.DefaultNamespace] = policy.Namespace{Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}}
		return policy.NewEnvHolder(view, "", nil)
	}
	f := newReplayFixture(t)
	taskID := f.seedTask(t, seedTaskConfig{name: "use", result: "success", spec: container.Spec{Env: map[string]string{"TOKEN": ref}}})
	resolver := WithSecretResolver(secret.NewMultiResolver(map[string]secret.Resolver{"env": secret.NewEnvResolver()}))

	policy.SetProcessHolder(rules("env/C4_REPLAY_*"))
	allowed, err := New(f.store, &recordingDispatcher{}, resolver).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.NoError(t, err)
	require.Len(t, allowed.Decisions, 1)
	require.True(t, allowed.Decisions[0].CacheHit, "while allowed, the unchanged step is reused")

	policy.SetProcessHolder(rules())
	dispatcher := &recordingDispatcher{}
	_, err = New(f.store, dispatcher, resolver).Replay(context.Background(), Request{BaselineRunID: f.runID})
	require.ErrorIs(t, err, secret.ErrSecretDenied)
	require.ErrorIs(t, err, ErrSecretIdentity)
	require.Contains(t, err.Error(), `step "use"`)
	require.Contains(t, err.Error(), "env/C4_REPLAY_TOKEN/…")
	require.NotContains(t, err.Error(), ref)
	require.Empty(t, dispatcher.calls)
	var count int64
	require.NoError(t, f.db.Model(&models.TaskRun{}).
		Where("job_run_id NOT IN ? AND task_id = ?", []uuid.UUID{f.runID, allowed.Run.ID}, taskID).
		Count(&count).Error)
	require.Zero(t, count, "a refused replay materializes nothing")
}
