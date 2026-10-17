package freshness

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

// TestGraphErrorKeepsTheHistoricalMessage pins the declared-graph error text
// (lint and apply return it verbatim to unrestricted callers) and the fields
// a response boundary redacts from (identity-and-access B2).
func TestGraphErrorKeepsTheHistoricalMessage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decls   []models.DatasetDeclaration
		want    error
		message string
		dataset string
		jobs    []string
	}{
		{
			name:    "multiple producers",
			decls:   []models.DatasetDeclaration{produces("job_b", "d"), produces("job_a", "d")},
			want:    ErrDatasetMultipleProducers,
			message: `dataset produced by multiple jobs: "d" produced by job_a, job_b`,
			dataset: "d",
			jobs:    []string{"job_a", "job_b"},
		},
		{
			name:    "unresolved consume",
			decls:   []models.DatasetDeclaration{consumes("rollup", "nowhere")},
			want:    ErrDatasetUnresolvedConsumes,
			message: `consumed dataset is not produced or declared: job "rollup" consumes "nowhere" which is not produced by any job or declared as a source`,
			dataset: "nowhere",
			jobs:    []string{"rollup"},
		},
		{
			name: "cycle",
			decls: []models.DatasetDeclaration{
				produces("job_a", "X"), consumes("job_a", "Y"),
				produces("job_b", "Y"), consumes("job_b", "X"),
			},
			want: ErrDatasetGraphCycle,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGraph(tc.decls)
			require.ErrorIs(t, err, tc.want)
			var graphErr *GraphError
			require.True(t, errors.As(err, &graphErr))
			require.Equal(t, tc.want, graphErr.Err)
			if tc.message != "" {
				require.EqualError(t, err, tc.message)
				require.Equal(t, tc.dataset, graphErr.Dataset)
				require.Equal(t, tc.jobs, graphErr.Jobs)
				return
			}
			require.Contains(t, err.Error(), "dataset dependency cycle detected: ")
			require.Contains(t, graphErr.Jobs, "job_a")
			require.Contains(t, graphErr.Jobs, "job_b")
		})
	}
}
