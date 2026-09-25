//go:build integration

package lifecycle

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/stretchr/testify/require"
)

func TestReadClusterTaskProofQueriesPublicIdentity(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
	)
	type queryRequest struct {
		SQL   string `json:"sql"`
		Limit int    `json:"limit"`
	}
	requests := make(chan queryRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request queryRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		requests <- request
		_ = json.NewEncoder(w).Encode(cluster.QueryResponse{RowCount: 1, Rows: [][]any{{
			durableID, runID, publicID, "10.244.3.6:9001", 2, 1, "running", nil,
		}}})
	}))
	defer server.Close()

	proof, err := readClusterTaskProof(context.Background(), cluster.NewHTTP(""), server.URL, runID, publicID)
	require.NoError(t, err)
	request := <-requests
	require.Equal(t, durableID, proof.ID)
	require.Equal(t, 2, request.Limit, "two rows must be visible to reject an ambiguous public task")
	require.Contains(t, request.SQL, "WHERE job_run_id = '"+runID+"' AND task_id = '"+publicID+"' LIMIT 2")
	require.NotContains(t, request.SQL, "AND id =")
}

func TestParseClusterTaskProofIdentity(t *testing.T) {
	const (
		runID     = "7c39392d-d1bf-43fc-b803-fce7d606141f"
		publicID  = "148d4542-8df5-4bf2-8ff4-d542af055498"
		durableID = "45b15c10-f7ea-4987-9fcd-d05e0522bbd9"
		otherID   = "6000f2af-5260-4ad2-947c-788e4535b329"
	)
	row := []any{durableID, runID, publicID, "10.244.3.6:9001", float64(2), float64(1), "succeeded", `{"recorder_nonce":"effect-1"}`}
	valid := cluster.QueryResponse{RowCount: 1, Rows: [][]any{row}}
	proof, err := parseClusterTaskProof(valid, runID, publicID)
	require.NoError(t, err)
	require.Equal(t, durableID, proof.ID, "public task identity must resolve to the durable row")
	require.Equal(t, runID, proof.RunID)
	require.Equal(t, publicID, proof.TaskID)
	require.Equal(t, "effect-1", proof.RecorderNonce)
	require.Equal(t, int64(2), proof.OwnerGeneration)
	require.Equal(t, 1, proof.Attempt)

	for _, tc := range []struct {
		name string
		resp cluster.QueryResponse
	}{
		{"missing", cluster.QueryResponse{}},
		{"duplicate public task", cluster.QueryResponse{RowCount: 2, Rows: [][]any{row, {otherID, runID, publicID, "", float64(2), float64(1), "running", nil}}}},
		{"inconsistent row count", cluster.QueryResponse{RowCount: 2, Rows: [][]any{row}}},
		{"wrong run", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, otherID, publicID, "", float64(2), float64(1), "running", nil}}}},
		{"wrong public task", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, runID, otherID, "", float64(2), float64(1), "running", nil}}}},
		{"invalid durable id", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{"not-a-uuid", runID, publicID, "", float64(2), float64(1), "running", nil}}}},
		{"missing column", cluster.QueryResponse{RowCount: 1, Rows: [][]any{row[:7]}}},
		{"invalid output", cluster.QueryResponse{RowCount: 1, Rows: [][]any{{durableID, runID, publicID, "", float64(2), float64(1), "succeeded", `{"recorder_nonce":`}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseClusterTaskProof(tc.resp, runID, publicID)
			require.Error(t, err)
		})
	}
}
