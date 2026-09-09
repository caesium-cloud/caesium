package dataset

import (
	"encoding/json"
	"testing"

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
}
