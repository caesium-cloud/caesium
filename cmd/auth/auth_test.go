package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKeyCreateCommandSendsScopeAndPrintsKey(t *testing.T) {
	var authHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/auth/keys", r.URL.Path)
		authHeader = r.Header.Get("Authorization")

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "operator", body["role"])
		require.Equal(t, "CI deploy key", body["description"])
		require.Equal(t, "48h", body["expires_in"])
		require.Equal(t, map[string]any{"jobs": []any{"etl-daily", "etl-hourly"}}, body["scope"])

		_, _ = w.Write([]byte(`{"key":"csk_live_secret","api_key":{"id":"abc","role":"operator"}}`))
	}))
	defer server.Close()

	createRole = "operator"
	createDescription = "CI deploy key"
	createExpiresIn = "48h"
	createServer = server.URL
	createAPIKey = "admin-token"
	createScopeJobs = []string{"etl-daily", "etl-hourly"}

	var out bytes.Buffer
	keyCreateCmd.SetOut(&out)
	keyCreateCmd.SetErr(&out)
	keyCreateCmd.SetContext(context.Background())

	err := keyCreateCmd.RunE(keyCreateCmd, nil)
	require.NoError(t, err)
	require.Equal(t, "Bearer admin-token", authHeader)
	require.Contains(t, out.String(), "csk_live_secret")
	require.Contains(t, out.String(), `"role": "operator"`)
}

func TestKeyListCommandPrintsFormattedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/auth/keys", r.URL.Path)
		_, _ = w.Write([]byte(`[{"id":"abc","role":"viewer"}]`))
	}))
	defer server.Close()

	listServer = server.URL
	listAPIKey = "admin-token"

	var out bytes.Buffer
	keyListCmd.SetOut(&out)
	keyListCmd.SetErr(&out)
	keyListCmd.SetContext(context.Background())

	err := keyListCmd.RunE(keyListCmd, nil)
	require.NoError(t, err)
	require.Contains(t, out.String(), `"role": "viewer"`)
}

func TestKeyRevokeCommandTargetsRevokeEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"status":"revoked"}`))
	}))
	defer server.Close()

	revokeID = "1234"
	revokeServer = server.URL
	revokeAPIKey = "admin-token"

	var out bytes.Buffer
	keyRevokeCmd.SetOut(&out)
	keyRevokeCmd.SetErr(&out)
	keyRevokeCmd.SetContext(context.Background())

	err := keyRevokeCmd.RunE(keyRevokeCmd, nil)
	require.NoError(t, err)
	require.Equal(t, "/v1/auth/keys/1234/revoke", gotPath)
	require.Contains(t, out.String(), `"status": "revoked"`)
}

func TestKeyRotateCommandSendsGracePeriodAndPrintsNewKey(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, _ = w.Write([]byte(`{"key":"csk_live_rotated","api_key":{"id":"new","role":"runner"}}`))
	}))
	defer server.Close()

	rotateID = "1234"
	rotateGracePeriod = "6h"
	rotateServer = server.URL
	rotateAPIKey = "admin-token"

	var out bytes.Buffer
	keyRotateCmd.SetOut(&out)
	keyRotateCmd.SetErr(&out)
	keyRotateCmd.SetContext(context.Background())

	err := keyRotateCmd.RunE(keyRotateCmd, nil)
	require.NoError(t, err)
	require.Equal(t, map[string]any{"grace_period": "6h"}, body)
	require.Contains(t, out.String(), "csk_live_rotated")
}

func TestAuditCommandBuildsQueryString(t *testing.T) {
	var query string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		_, _ = w.Write([]byte(`[]`))
	}))
	defer server.Close()

	auditSince = "24h"
	auditActor = "alpha"
	auditAction = "api_key.create"
	auditLimit = 10
	auditServer = server.URL
	auditAPIKey = "admin-token"

	var out bytes.Buffer
	auditCmd.SetOut(&out)
	auditCmd.SetErr(&out)
	auditCmd.SetContext(context.Background())

	err := auditCmd.RunE(auditCmd, nil)
	require.NoError(t, err)
	require.Contains(t, query, "since=24h")
	require.Contains(t, query, "actor=alpha")
	require.Contains(t, query, "action=api_key.create")
	require.Contains(t, query, "limit=10")
	require.Contains(t, out.String(), "[]")
}
