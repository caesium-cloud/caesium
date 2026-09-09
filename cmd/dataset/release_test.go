package dataset

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	runstore "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestResolveActiveHoldRequiresExactUniqueIdentity(t *testing.T) {
	id := uuid.NewString()
	valid := datasetHold{ID: id, Namespace: "", Name: "warehouse/orders", Status: "active"}
	cases := []struct {
		name      string
		response  holdsResponse
		wantError string
	}{
		{"exact slash name", holdsResponse{Holds: []datasetHold{valid}, Total: 1}, ""},
		{"none", holdsResponse{Holds: []datasetHold{}}, "no active hold exists for dataset _/warehouse/orders"},
		{"truncated feed", holdsResponse{Holds: []datasetHold{valid}, Total: 2}, "unique active hold"},
		{"another namespace", holdsResponse{Holds: []datasetHold{{ID: id, Namespace: "warehouse", Name: "orders", Status: "active"}}, Total: 1}, "did not match"},
		{"released", holdsResponse{Holds: []datasetHold{{ID: id, Name: valid.Name, Status: "released"}}, Total: 1}, "did not match"},
		{"invalid id", holdsResponse{Holds: []datasetHold{{ID: "bad", Name: valid.Name, Status: "active"}}, Total: 1}, "invalid id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(tc.response)
			require.NoError(t, err)
			got, err := resolveActiveHold(body, "", valid.Name)
			if tc.wantError != "" {
				require.ErrorContains(t, err, tc.wantError)
				require.Empty(t, got)
				return
			}
			require.NoError(t, err)
			require.Equal(t, id, got)
		})
	}
}

func TestDatasetRefPreservesSlashNames(t *testing.T) {
	prior := namespaceFlag
	t.Cleanup(func() { namespaceFlag = prior })
	for _, ns := range []string{"", "_", "another"} {
		namespaceFlag = ns
		namespace, name, err := splitDatasetRef("warehouse/orders")
		require.NoError(t, err)
		require.Equal(t, "warehouse/orders", name)
		if ns == "_" {
			ns = ""
		}
		require.Equal(t, ns, namespace)
	}
}

func TestReleaseTolerancesRejectMalformedOrDuplicateWindows(t *testing.T) {
	for _, input := range [][]string{{"min"}, {"min=0s"}, {"min=1 day"}, {"rowCount=24h"}, {"min=1h", "min=2h"}} {
		_, err := parseReleaseTolerances(input)
		require.Error(t, err, "%v", input)
	}
	got, err := parseReleaseTolerances([]string{"min=24h", "deltaFromBaseline=30m"})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"min": "24h", "deltaFromBaseline": "30m"}, got)
	for _, assertion := range []string{runstore.AssertionMin, runstore.AssertionMax, runstore.AssertionDeltaFromBaseline, runstore.AssertionMaxLag, runstore.AssertionMissing} {
		got, err := parseReleaseTolerances([]string{assertion + "=1h"})
		require.NoError(t, err)
		require.Equal(t, map[string]string{assertion: "1h"}, got)
	}
}

func TestReleaseCommandRefreshesConflictsWithoutReleasingAnotherHold(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	oldID, newerID := uuid.NewString(), uuid.NewString()
	for _, tc := range []struct {
		name          string
		currentID     string
		refreshStatus int
		wantError     string
	}{
		{name: "new breach remains held", currentID: newerID, wantError: "is still held by newer active hold " + newerID},
		{name: "no current hold", wantError: "no active hold is currently recorded for dataset tenant/warehouse/orders"},
		{name: "same hold remains active", currentID: oldID, wantError: "is still held by active hold " + oldID},
		{name: "refresh authorization expired", refreshStatus: http.StatusUnauthorized, wantError: "current hold state for dataset tenant/warehouse/orders could not be verified: dataset request failed (401): expired credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gets, posts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer test-operator", r.Header.Get("Authorization"))
				if r.Method == http.MethodGet {
					n := gets.Add(1)
					require.Equal(t, "/v1/datasets/holds", r.URL.Path)
					require.Equal(t, "tenant", r.URL.Query().Get("namespace"))
					require.Equal(t, "warehouse/orders", r.URL.Query().Get("name"))
					require.Equal(t, "active", r.URL.Query().Get("status"))
					if n > 1 && tc.refreshStatus != 0 {
						http.Error(w, "expired credential", tc.refreshStatus)
						return
					}
					id := oldID
					if n > 1 {
						id = tc.currentID
					}
					page := holdsResponse{Holds: []datasetHold{}}
					if id != "" {
						page.Holds = []datasetHold{{ID: id, Namespace: "tenant", Name: "warehouse/orders", Status: "active"}}
						page.Total = 1
					}
					require.NoError(t, json.NewEncoder(w).Encode(page))
					return
				}
				posts.Add(1)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/datasets/holds/"+oldID+"/release", r.URL.Path, "must never release the newer hold automatically")
				var payload struct {
					Reason string `json:"reason"`
				}
				require.NoError(t, json.NewDecoder(r.Body).Decode(&payload))
				require.Equal(t, "reviewed breach", payload.Reason)
				http.Error(w, "dataset hold is already released", http.StatusConflict)
			}))
			defer server.Close()
			stdout, err := executeDatasetCommand(t, server.URL, newReleaseCommand(), "warehouse/orders", "--namespace", "tenant", "--reason", "reviewed breach", "--api-key", "test-operator", "--json")
			require.ErrorContains(t, err, "dataset request failed (409): dataset hold is already released")
			require.ErrorContains(t, err, tc.wantError)
			var statusErr *httpStatusError
			require.ErrorAs(t, err, &statusErr, "original conflict status must remain available")
			require.Equal(t, http.StatusConflict, statusErr.StatusCode)
			require.Empty(t, stdout, "an unsuccessful release must not emit a success or JSON payload")
			require.EqualValues(t, 2, gets.Load())
			require.EqualValues(t, 1, posts.Load())
		})
	}
}

func TestReleaseCommandDoesNotRefreshAuthorizationFailures(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		var gets, posts atomic.Int32
		id := uuid.NewString()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				gets.Add(1)
				if status == http.StatusUnauthorized {
					http.Error(w, "unauthorized", status)
					return
				}
				require.NoError(t, json.NewEncoder(w).Encode(holdsResponse{Holds: []datasetHold{{ID: id, Name: "orders", Status: "active"}}, Total: 1}))
				return
			}
			posts.Add(1)
			http.Error(w, "viewer cannot release", status)
		}))
		stdout, err := executeDatasetCommand(t, server.URL, newReleaseCommand(), "orders", "--reason", "reviewed")
		server.Close()
		var statusErr *httpStatusError
		require.ErrorAs(t, err, &statusErr)
		require.Equal(t, status, statusErr.StatusCode)
		require.Empty(t, stdout)
		require.EqualValues(t, 1, gets.Load())
		if status == http.StatusUnauthorized {
			require.Zero(t, posts.Load())
		} else {
			require.EqualValues(t, 1, posts.Load())
		}
	}
}

func TestReleaseCommandNoHoldExplainsNamespaceSelection(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	for _, namespace := range []string{"", "tenant"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, http.MethodGet, r.Method)
			require.True(t, r.URL.Query().Has("namespace"))
			require.Equal(t, namespace, r.URL.Query().Get("namespace"))
			require.NoError(t, json.NewEncoder(w).Encode(holdsResponse{Holds: []datasetHold{}}))
		}))
		stdout, err := executeDatasetCommand(t, server.URL, newReleaseCommand(), "orders", "--namespace", namespace, "--reason", "reviewed")
		server.Close()
		require.ErrorIs(t, err, errNoActiveHold)
		require.ErrorContains(t, err, "namespace \""+displayNamespace(namespace)+"\"")
		require.ErrorContains(t, err, "use --namespace to target another namespace")
		require.Empty(t, stdout)
	}
}
