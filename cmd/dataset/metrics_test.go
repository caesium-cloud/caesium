package dataset

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMetricsCommandShowsRawPagingAndBaselineMembership(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	response := `{"namespace":"tenant/ns","name":"warehouse/orders","metric":"dedup_ratio","series":[{"value":0.1,"violated":false,"in_baseline":false,"created_at":"2026-09-01T10:00:00Z"},{"value":0.2,"violated":false,"in_baseline":true,"created_at":"2026-09-01T11:00:00Z"}],"baseline":{"samples":1,"median":0.2,"p10":0.2,"p90":0.2},"window":20,"min_samples":3,"seeding":true,"total":57,"limit":2,"offset":50}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodGet, r.Method)
		require.Equal(t, "/v1/datasets/tenant%2Fns/warehouse%2Forders/metrics", r.URL.EscapedPath())
		require.Equal(t, "dedup_ratio", r.URL.Query().Get("metric"))
		require.Equal(t, "2", r.URL.Query().Get("limit"))
		require.Equal(t, "50", r.URL.Query().Get("offset"))
		_, err := w.Write([]byte(response))
		require.NoError(t, err)
	}))
	defer server.Close()
	args := []string{"warehouse/orders", "--namespace", "tenant/ns", "--metric", "dedup_ratio", "--limit", "2", "--offset", "50"}
	stdout, err := executeDatasetCommand(t, server.URL, newMetricsCommand(), args...)
	require.NoError(t, err)
	require.Regexp(t, `BASELINE WINDOW\s+20`, stdout)
	require.Contains(t, stdout, "IN BASELINE")
	require.Regexp(t, `0.1\s+false\s+false`, stdout)
	require.Regexp(t, `0.2\s+false\s+true`, stdout)
	require.Contains(t, stdout, "Showing 2 of 57 samples (limit 2, offset 50).")
	require.Contains(t, stdout, "use --offset 52 for the next page")

	stdout, err = executeDatasetCommand(t, server.URL, newMetricsCommand(), append(args, "--json")...)
	require.NoError(t, err)
	require.JSONEq(t, response, stdout, "JSON must retain every server field without table text")
}

func TestMetricsCommandDefaultPageAndInvalidPagination(t *testing.T) {
	t.Setenv(apiKeyEnvVar, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "50", r.URL.Query().Get("limit"))
		require.Equal(t, "0", r.URL.Query().Get("offset"))
		require.NoError(t, json.NewEncoder(w).Encode(metricsResponse{Window: 20, Limit: 50}))
	}))
	defer server.Close()
	stdout, err := executeDatasetCommand(t, server.URL, newMetricsCommand(), "orders", "--metric", "rowCount")
	require.NoError(t, err)
	require.Contains(t, stdout, "Showing 0 of 0 samples (limit 50, offset 0).")
	for _, args := range [][]string{{"--limit", "0"}, {"--limit", "201"}, {"--limit", "-1"}, {"--offset", "-1"}} {
		stdout, err := executeDatasetCommand(t, "invalid-server", newMetricsCommand(), append([]string{"orders", "--metric", "rowCount"}, args...)...)
		require.ErrorContains(t, err, args[0])
		require.Empty(t, stdout)
	}
}
