package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	stdhttp "net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type stubSecretResolver struct {
	resolve func(context.Context, string) (string, error)
}

func (s stubSecretResolver) Resolve(ctx context.Context, ref string) (string, error) {
	return s.resolve(ctx, ref)
}

func TestNewParsesConfiguration(t *testing.T) {
	t.Parallel()

	trigger := &models.Trigger{
		ID:   uuid.New(),
		Type: models.TriggerTypeHTTP,
		Configuration: `{
			"path": "/hooks/run",
			"secret": "shared-secret",
			"signatureScheme": "hmac-sha256",
			"signatureHeader": "X-Signature",
			"paramMapping": {"branch": "$.ref", "commit": "$.after"},
			"defaultParams": {"environment": "staging"}
		}`,
	}

	h, err := New(trigger)
	require.NoError(t, err)
	require.Equal(t, "run", h.config.Path)
	require.Equal(t, "shared-secret", h.config.Secret)
	require.Equal(t, "hmac-sha256", h.config.SignatureScheme)
	require.Equal(t, "X-Signature", h.config.SignatureHeader)
	require.Equal(t, map[string]string{"branch": "$.ref", "commit": "$.after"}, h.config.ParamMapping)
	require.Equal(t, map[string]string{"environment": "staging"}, h.config.DefaultParams)
}

func TestNewAllowsEmptyConfiguration(t *testing.T) {
	t.Parallel()

	h, err := New(&models.Trigger{ID: uuid.New(), Type: models.TriggerTypeHTTP})
	require.NoError(t, err)
	require.Empty(t, h.config.Path)
	require.NotNil(t, h.config.ParamMapping)
	require.NotNil(t, h.config.DefaultParams)
}

func TestValidateSignature(t *testing.T) {
	t.Parallel()

	body := []byte(`{"hello":"world"}`)

	t.Run("no auth when secret empty", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(body))
		require.True(t, validateSignature(req, body, "", "", ""))
	})

	t.Run("hmac sha256", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(body))
		mac := hmac.New(sha256.New, []byte("secret"))
		_, _ = mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		require.True(t, validateSignature(req, body, "secret", "", ""))
	})

	t.Run("hmac sha1", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(body))
		mac := hmac.New(sha1.New, []byte("secret"))
		_, _ = mac.Write(body)
		req.Header.Set("X-Hub-Signature", "sha1="+hex.EncodeToString(mac.Sum(nil)))
		require.True(t, validateSignature(req, body, "secret", "hmac-sha1", ""))
	})

	t.Run("bearer", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret-token")
		require.True(t, validateSignature(req, body, "secret-token", "bearer", ""))
	})

	t.Run("basic", func(t *testing.T) {
		req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(body))
		req.SetBasicAuth("svc", "password")
		require.True(t, validateSignature(req, body, "svc:password", "basic", ""))
	})
}

func TestExtractParams(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"ref": "refs/heads/main",
		"after": "abc123",
		"sender": {"login": "octocat"},
		"items": [{"name": "first"}, {"name": "second"}]
	}`)

	params := extractParams(body, map[string]string{
		"branch": "$.ref",
		"commit": "$.after",
		"actor":  "$.sender.login",
		"first":  "$.items.0.name",
	})

	require.Equal(t, map[string]string{
		"branch": "refs/heads/main",
		"commit": "abc123",
		"actor":  "octocat",
		"first":  "first",
	}, params)
}

func TestExtractParamsSupportsRootJSONPath(t *testing.T) {
	t.Parallel()

	body := []byte(`{"ref":"refs/heads/main","sender":{"login":"octocat"}}`)

	params := extractParams(body, map[string]string{
		"payload": "$",
	})

	require.Equal(t, map[string]string{
		"payload": `{"ref":"refs/heads/main","sender":{"login":"octocat"}}`,
	}, params)
}

func TestExtractWebhookParams(t *testing.T) {
	t.Parallel()

	trigger := &models.Trigger{
		ID:   uuid.New(),
		Type: models.TriggerTypeHTTP,
		Configuration: `{
			"path": "github/push",
			"secret": "shared-secret",
			"signatureScheme": "bearer",
			"paramMapping": {"branch": "$.ref"}
		}`,
	}

	h, err := New(trigger)
	require.NoError(t, err)

	req := httptest.NewRequest(stdhttp.MethodPost, "/v1/hooks/github/push", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer shared-secret")

	params, err := h.ExtractWebhookParams(context.Background(), req, []byte(`{"ref":"refs/heads/main"}`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"branch": "refs/heads/main"}, params)
}

func TestExtractWebhookParamsResolvesSecretReference(t *testing.T) {
	oldResolver := secretResolver
	t.Cleanup(func() { secretResolver = oldResolver })
	SetSecretResolver(stubSecretResolver{
		resolve: func(_ context.Context, ref string) (string, error) {
			require.Equal(t, "secret://env/webhook_token", ref)
			return "resolved-token", nil
		},
	})

	trigger := &models.Trigger{
		ID:   uuid.New(),
		Type: models.TriggerTypeHTTP,
		Configuration: `{
			"path": "github/push",
			"secret": "secret://env/webhook_token",
			"signatureScheme": "bearer",
			"paramMapping": {"branch": "$.ref"}
		}`,
	}

	h, err := New(trigger)
	require.NoError(t, err)

	req := httptest.NewRequest(stdhttp.MethodPost, "/v1/hooks/github/push", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer resolved-token")

	params, err := h.ExtractWebhookParams(context.Background(), req, []byte(`{"ref":"refs/heads/main"}`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"branch": "refs/heads/main"}, params)
}

func TestExtractWebhookParamsSupportsBasicAuth(t *testing.T) {
	t.Parallel()

	trigger := &models.Trigger{
		ID:   uuid.New(),
		Type: models.TriggerTypeHTTP,
		Configuration: `{
			"path": "ops/debug",
			"secret": "svc:password",
			"signatureScheme": "basic",
			"paramMapping": {"payload": "$"}
		}`,
	}

	h, err := New(trigger)
	require.NoError(t, err)

	req := httptest.NewRequest(stdhttp.MethodPost, "/v1/hooks/ops/debug", bytes.NewReader(nil))
	req.SetBasicAuth("svc", "password")

	params, err := h.ExtractWebhookParams(context.Background(), req, []byte(`{"env":"staging"}`))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"payload": `{"env":"staging"}`}, params)
}

func TestFireWithParamsMergesDefaultAndOverrides(t *testing.T) {
	oldListJobs := listJobs
	oldRunJob := runJob
	t.Cleanup(func() {
		listJobs = oldListJobs
		runJob = oldRunJob
	})

	var (
		seen []map[string]string
		mu   sync.Mutex
		done = make(chan struct{}, 2)
	)
	listJobs = func(ctx context.Context, triggerID string) (models.Jobs, error) {
		return models.Jobs{
			&models.Job{ID: uuid.New(), Alias: "first"},
			&models.Job{ID: uuid.New(), Alias: "second", Paused: true},
			&models.Job{ID: uuid.New(), Alias: "third"},
		}, nil
	}
	runJob = func(ctx context.Context, j *models.Job, params map[string]string) error {
		copied := make(map[string]string, len(params))
		for k, v := range params {
			copied[k] = v
		}
		mu.Lock()
		seen = append(seen, copied)
		mu.Unlock()
		done <- struct{}{}
		return nil
	}

	h := &HTTP{
		id: uuid.New(),
		config: Config{
			DefaultParams: map[string]string{
				"environment": "staging",
				"source":      "webhook",
			},
		},
	}

	err := h.FireWithParams(context.Background(), map[string]string{
		"source": "api",
		"branch": "main",
	})
	require.NoError(t, err)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first job run")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for second job run")
	}

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seen, 2)
	require.Equal(t, map[string]string{
		"environment": "staging",
		"source":      "api",
		"branch":      "main",
	}, seen[0])
	require.Equal(t, seen[0], seen[1])
}

func TestFireUsesDefaultsWhenParamsMissing(t *testing.T) {
	oldListJobs := listJobs
	oldRunJob := runJob
	t.Cleanup(func() {
		listJobs = oldListJobs
		runJob = oldRunJob
	})

	done := make(chan map[string]string, 1)
	listJobs = func(ctx context.Context, triggerID string) (models.Jobs, error) {
		return models.Jobs{&models.Job{ID: uuid.New(), Alias: "only"}}, nil
	}
	runJob = func(ctx context.Context, j *models.Job, params map[string]string) error {
		copied := make(map[string]string, len(params))
		for k, v := range params {
			copied[k] = v
		}
		done <- copied
		return nil
	}

	h := &HTTP{
		id: uuid.New(),
		config: Config{
			DefaultParams: map[string]string{"environment": "prod"},
		},
	}

	err := h.Fire(context.Background())
	require.NoError(t, err)

	select {
	case seen := <-done:
		require.Equal(t, map[string]string{"environment": "prod"}, seen)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for job run")
	}
}

func TestParseConfigRejectsInvalidJSON(t *testing.T) {
	t.Parallel()

	_, err := parseConfig("{")
	require.Error(t, err)
}

func TestResolveJSONPathRejectsInvalidPaths(t *testing.T) {
	t.Parallel()

	_, ok := resolveJSONPath(map[string]any{"a": map[string]any{"b": "c"}}, "$.a..b")
	require.False(t, ok)
}

func TestValidateSignatureRejectsInvalidBearer(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(stdhttp.MethodPost, "/", bytes.NewReader(nil))
	req.Header.Set("Authorization", "Bearer wrong")
	require.False(t, validateSignature(req, nil, "secret", "bearer", ""))
}
