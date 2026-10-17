package http

import (
	"bytes"
	"context"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// These tests install the process-wide policy holder, so none is parallel.

func installHookSecretPolicy(t *testing.T, rules map[string][]string) {
	t.Helper()
	view := policy.EmptyPolicy()
	for namespace, allow := range rules {
		view.Namespaces[namespace] = policy.Namespace{Secrets: &policy.SecretRules{Allow: allow}}
	}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
}

func newScopedHook(t *testing.T, jobs models.Jobs, listErr error, listed *int) (*HTTP, *int) {
	t.Helper()
	resolved := 0
	resolver := stubSecretResolver{resolve: func(context.Context, string) (string, error) {
		resolved++
		return "resolved-token", nil
	}}
	trigger := &models.Trigger{
		ID:            uuid.New(),
		Type:          models.TriggerTypeHTTP,
		Configuration: `{"path": "c4/hook", "secret": "secret://env/HOOK_TOKEN", "signatureScheme": "bearer"}`,
	}
	h, err := New(trigger, WithSecretResolver(resolver), WithListJobs(func(_ context.Context, id string) (models.Jobs, error) {
		require.Equal(t, trigger.ID.String(), id)
		*listed++
		return jobs, listErr
	}))
	require.NoError(t, err)
	return h, &resolved
}

func extractWithToken(h *HTTP) error {
	req := httptest.NewRequestWithContext(context.Background(), stdhttp.MethodPost, "/v1/hooks/c4/hook", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer resolved-token")
	_, err := h.ExtractWebhookParams(context.Background(), req, []byte(`{}`))
	return err
}

func TestWebhookSecretFollowsTheJobsNamespaceRules(t *testing.T) {
	installHookSecretPolicy(t, map[string][]string{
		"c4-hooks":  {"env/HOOK_*"},
		"c4-locked": {},
	})
	for _, tc := range []struct {
		name       string
		namespaces []string
		allowed    bool
	}{
		{"allowed namespace", []string{"c4-hooks"}, true},
		{"default namespace without rules", []string{""}, true},
		{"namespace without rules", []string{"c4-unruled"}, true},
		{"no jobs is the default namespace", nil, true},
		{"denying namespace", []string{"c4-locked"}, false},
		{"every job namespace must allow", []string{"c4-hooks", "c4-locked"}, false},
		{"duplicates collapse", []string{"c4-hooks", "c4-hooks", ""}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			jobs := make(models.Jobs, 0, len(tc.namespaces))
			for _, ns := range tc.namespaces {
				jobs = append(jobs, &models.Job{ID: uuid.New(), Namespace: ns})
			}
			listed := 0
			h, resolved := newScopedHook(t, jobs, nil, &listed)
			err := extractWithToken(h)
			require.Equal(t, 1, listed)
			if tc.allowed {
				require.NoError(t, err)
				require.Equal(t, 1, *resolved)
				return
			}
			require.ErrorIs(t, err, secret.ErrSecretDenied)
			require.NotContains(t, err.Error(), "secret://env/HOOK_TOKEN")
			require.Contains(t, err.Error(), "env/HOOK_TOKEN/…")
			require.Zero(t, *resolved, "a denied webhook secret is never resolved")
		})
	}

	// A failure to list the trigger's jobs fails closed.
	listed := 0
	h, resolved := newScopedHook(t, nil, errors.New("db down"), &listed)
	require.ErrorContains(t, extractWithToken(h), "db down")
	require.Zero(t, *resolved)
}

func TestWebhookSecretWithoutPolicyRulesListsNothing(t *testing.T) {
	policy.SetProcessHolder(nil)
	listed := 0
	h, resolved := newScopedHook(t, nil, nil, &listed)
	require.NoError(t, extractWithToken(h))
	require.Zero(t, listed, "with no policy rules the trigger's jobs are not listed")
	require.Equal(t, 1, *resolved)
}
