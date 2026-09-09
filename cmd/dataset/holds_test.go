package dataset

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func executeHoldsPage(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	priorServer, priorNamespace, priorKey := serverFlag, namespaceFlag, apiKeyFlag
	defer func() { serverFlag, namespaceFlag, apiKeyFlag = priorServer, priorNamespace, priorKey }()
	var stdout, stderr bytes.Buffer
	root := &cobra.Command{Use: "dataset", SilenceErrors: true, SilenceUsage: true}
	root.PersistentFlags().StringVar(&serverFlag, "server", server, "Server")
	root.PersistentFlags().StringVar(&namespaceFlag, "namespace", "", "Namespace")
	root.PersistentFlags().StringVar(&apiKeyFlag, "api-key", "", "API key")
	root.AddCommand(newHoldsCommand())
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetArgs(append([]string{"holds"}, args...))
	err := root.ExecuteContext(t.Context())
	return stdout.String(), err
}

// A 55-row HTTP feed makes the older holds unreachable under the previous CLI.
// Drive flag parsing, query construction, HTTP, JSON, and table rendering together.
func TestHoldsCommandSelectsOlderPagesAndShowsTruncation(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	rows := make([]datasetHold, 55)
	for i := range rows {
		rows[i] = datasetHold{ID: fmt.Sprintf("hold-%02d", i), Namespace: "tenant", Name: fmt.Sprintf("dataset-%02d", i), Status: "active"}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/datasets/holds", r.URL.Path)
		require.Equal(t, "active", r.URL.Query().Get("status"))
		require.Equal(t, "tenant", r.URL.Query().Get("namespace"))
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		require.NoError(t, err)
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		require.NoError(t, err)
		end := min(offset+limit, len(rows))
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(holdsResponse{Holds: rows[offset:end], Total: int64(len(rows)), Limit: limit, Offset: offset}))
	}))
	defer server.Close()

	first, err := executeHoldsPage(t, server.URL, "--namespace", "tenant")
	require.NoError(t, err)
	require.Contains(t, first, "dataset-00")
	require.NotContains(t, first, "dataset-54")
	require.Contains(t, first, "Showing 50 of 55 holds (limit 50, offset 0).")
	require.Contains(t, first, "use --offset 50 for the next page")

	older, err := executeHoldsPage(t, server.URL, "--namespace", "tenant", "--limit", "10", "--offset", "50")
	require.NoError(t, err)
	require.Contains(t, older, "dataset-54")
	require.NotContains(t, older, "dataset-00")
	require.Contains(t, older, "Showing 5 of 55 holds (limit 10, offset 50).")
	require.NotContains(t, older, "More holds available")

	stdout, err := executeHoldsPage(t, server.URL, "--namespace", "tenant", "--limit", "10", "--offset", "50", "--json")
	require.NoError(t, err)
	var page holdsResponse
	require.NoError(t, json.Unmarshal([]byte(stdout), &page), "stdout must contain only the unchanged JSON envelope")
	require.EqualValues(t, 55, page.Total)
	require.Equal(t, 10, page.Limit)
	require.Equal(t, 50, page.Offset)
	require.Len(t, page.Holds, 5)
	require.Equal(t, "dataset-54", page.Holds[4].Name)
}

func TestHoldsCommandRejectsInvalidPaginationBeforeRequest(t *testing.T) {
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--limit", "-1"}, {"--offset", "-1"}} {
		stdout, err := executeHoldsPage(t, "invalid-server", args...)
		require.Error(t, err)
		require.Contains(t, err.Error(), args[0])
		require.Empty(t, stdout)
	}
}
