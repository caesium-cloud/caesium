package dataset

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestListCommandShowsPagingWhenHeldDatasetIsOffPage(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	rows := make([]datasetState, 55)
	for i := range rows {
		rows[i] = datasetState{Name: fmt.Sprintf("dataset-%02d", i), Status: "healthy"}
	}
	rows[54].HoldStatus = "active"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/datasets", r.URL.Path)
		require.Equal(t, "healthy", r.URL.Query().Get("status"))
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		require.NoError(t, err)
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		require.NoError(t, err)
		end := min(offset+limit, len(rows))
		require.NoError(t, json.NewEncoder(w).Encode(listResponse{Datasets: rows[offset:end], Total: int64(len(rows)), Limit: limit, Offset: offset}))
	}))
	defer server.Close()

	first, err := executeDatasetCommand(t, server.URL, newListCommand(), "--status", "healthy")
	require.NoError(t, err)
	require.Contains(t, first, "dataset-00")
	require.NotContains(t, first, "dataset-54")
	require.Contains(t, first, "Showing 50 of 55 datasets (limit 50, offset 0).")
	require.Contains(t, first, "use --offset 50 for the next page")

	older, err := executeDatasetCommand(t, server.URL, newListCommand(), "--status", "healthy", "--limit", "10", "--offset", "50")
	require.NoError(t, err)
	require.Contains(t, older, "dataset-54")
	require.Contains(t, older, "HOLD")
	require.Contains(t, older, "active")
	require.Contains(t, older, "Showing 5 of 55 datasets (limit 10, offset 50).")
	require.NotContains(t, older, "More datasets available")

	stdout, err := executeDatasetCommand(t, server.URL, newListCommand(), "--status", "healthy", "--limit", "10", "--offset", "50", "--json")
	require.NoError(t, err)
	var page listResponse
	require.NoError(t, json.Unmarshal([]byte(stdout), &page))
	require.EqualValues(t, 55, page.Total)
	require.Equal(t, 10, page.Limit)
	require.Equal(t, 50, page.Offset)
	require.Len(t, page.Datasets, 5)
	require.Equal(t, "active", page.Datasets[4].HoldStatus)
}

func TestListCommandRejectsInvalidPaginationBeforeRequest(t *testing.T) {
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--limit", "-1"}, {"--offset", "-1"}} {
		stdout, err := executeDatasetCommand(t, "invalid-server", newListCommand(), args...)
		require.ErrorContains(t, err, args[0])
		require.Empty(t, stdout)
	}
}
