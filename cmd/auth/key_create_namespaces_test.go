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

// runKeyCreate runs `auth key create` with the package flag variables as set
// by the caller and returns the JSON body the server received.
func runKeyCreate(t *testing.T) map[string]any {
	t.Helper()
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		_, _ = w.Write([]byte(`{"key":"csk_live_secret","api_key":{"id":"abc","role":"operator"}}`))
	}))
	defer server.Close()

	createServer = server.URL
	createAPIKey = "admin-token"
	var out bytes.Buffer
	keyCreateCmd.SetOut(&out)
	keyCreateCmd.SetErr(&out)
	keyCreateCmd.SetContext(context.Background())
	require.NoError(t, keyCreateCmd.RunE(keyCreateCmd, nil))
	return body
}

func resetKeyCreateFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		createRole, createDescription, createExpiresIn = "", "", ""
		createScopeJobs, createJobs, createNamespaces = nil, nil, nil
		for _, name := range []string{"jobs", "scope-jobs", "namespace"} {
			flag := keyCreateCmd.Flags().Lookup(name)
			_ = flag.Value.(interface{ Replace([]string) error }).Replace(nil)
			flag.Changed = false
		}
	}
	reset()
	t.Cleanup(reset)
}

func TestKeyCreateCommandSendsNamespaces(t *testing.T) {
	resetKeyCreateFlags(t)
	createRole = "operator"
	require.NoError(t, keyCreateCmd.ParseFlags([]string{"--namespace", "marketing", "--namespace", "finance,sales", "--jobs", "etl-daily,etl-hourly"}))
	require.NoError(t, keyCreateCmd.ValidateFlagGroups())

	body := runKeyCreate(t)
	require.Equal(t, []any{"marketing", "finance", "sales"}, body["namespaces"])
	require.Equal(t, map[string]any{"jobs": []any{"etl-daily", "etl-hourly"}}, body["scope"], "--jobs is --scope-jobs")
}

func TestKeyCreateCommandWithoutNamespacesSendsTodaysBody(t *testing.T) {
	resetKeyCreateFlags(t)
	createRole = "viewer"

	body := runKeyCreate(t)
	require.Equal(t, map[string]any{"role": "viewer", "description": ""}, body,
		"without --namespace or --jobs the request body is unchanged (a cluster-wide key)")
}

// TestKeyCreateCommandSendsAnEmptyNamespace: `--namespace "$NS"` with an empty
// NS must reach the server (which refuses it), not mint a cluster-wide key.
func TestKeyCreateCommandSendsAnEmptyNamespace(t *testing.T) {
	resetKeyCreateFlags(t)
	createRole = "admin"
	require.NoError(t, keyCreateCmd.ParseFlags([]string{"--namespace", ""}))

	body := runKeyCreate(t)
	require.Equal(t, []any{""}, body["namespaces"])
}

func TestKeyCreateCommandRefusesEmptyJobs(t *testing.T) {
	for _, args := range [][]string{
		{"--jobs", ""}, {"--scope-jobs", ""}, {"--jobs", ","}, {"--jobs", " "}, {"--scope-jobs", " , "},
	} {
		t.Run(args[0]+"="+args[1], func(t *testing.T) {
			resetKeyCreateFlags(t)
			createRole = "runner"
			require.NoError(t, keyCreateCmd.ParseFlags(args))
			createServer = "http://127.0.0.1:1" // never reached
			err := keyCreateCmd.RunE(keyCreateCmd, nil)
			require.ErrorContains(t, err, "--jobs needs at least one job alias")
		})
	}
}

func TestKeyCreateCommandJobsFlagsAreExclusive(t *testing.T) {
	resetKeyCreateFlags(t)
	require.NoError(t, keyCreateCmd.ParseFlags([]string{"--jobs", "a", "--scope-jobs", "b"}))
	err := keyCreateCmd.ValidateFlagGroups()
	require.Error(t, err)
	require.Contains(t, err.Error(), "jobs")
}
