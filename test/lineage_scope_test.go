//go:build integration

package test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	lineagectrl "github.com/caesium-cloud/caesium/api/rest/controller/lineage"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

func TestLineageImpactScopeRequiresUnscopedPrincipal(t *testing.T) {
	t.Setenv("CAESIUM_DATABASE_PATH", t.TempDir())
	t.Setenv("CAESIUM_DATABASE_SHARDS", "1")
	t.Setenv("CAESIUM_DATABASE_STANDBYS", "0")
	t.Setenv("CAESIUM_NODE_ADDRESS", freeLoopbackAddress(t))
	t.Setenv("CAESIUM_AUTH_KEY_HASH_SECRET", "0123456789abcdef0123456789abcdef")
	require.NoError(t, env.Process())
	require.NoError(t, db.Migrate())

	conn := db.Connection()
	authSvc := iauth.NewService(conn, iauth.WithKeyHashSecret(env.Variables().AuthKeyHashSecret))
	auditor := iauth.NewAuditLogger(conn)
	limiter := iauth.NewRateLimiter(10, time.Minute)

	unscoped, err := authSvc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		CreatedBy: "integration-test",
	})
	require.NoError(t, err)
	scoped, err := authSvc.CreateKey(&iauth.CreateKeyRequest{
		Role:      models.RoleViewer,
		Scope:     &models.KeyScope{Jobs: []string{"alpha"}},
		CreatedBy: "integration-test",
	})
	require.NoError(t, err)

	e := echo.New()
	protected := e.Group("/v1")
	protected.Use(authmw.Auth(authmw.AuthDeps{
		Service: authSvc,
		Auditor: auditor,
		Limiter: limiter,
	}))
	protected.GET("/lineage/impact", lineagectrl.Impact)

	server := httptest.NewServer(e)
	defer server.Close()

	target := server.URL + "/v1/lineage/impact?namespace=caesium&name=missing"

	status, body := getWithBearer(t, target, scoped.Plaintext)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, authmw.LineageImpactScopedDenyMessage)

	status, body = getWithBearer(t, target, unscoped.Plaintext)
	require.Equal(t, http.StatusOK, status, body)

	var result struct {
		RootNamespace string `json:"root_namespace"`
		RootName      string `json:"root_name"`
		Downstream    []any  `json:"downstream"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &result))
	require.Equal(t, "caesium", result.RootNamespace)
	require.Equal(t, "missing", result.RootName)
	require.Empty(t, result.Downstream)
}

func getWithBearer(t *testing.T, target, token string) (int, string) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func freeLoopbackAddress(t *testing.T) string {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().String()
}
