package freshness

import (
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Declarations as persisted rows carry their job's id; definitions being
// validated carry uuid.Nil.
func persisted(id uuid.UUID, decl models.DatasetDeclaration) models.DatasetDeclaration {
	decl.JobID = id
	return decl
}

// A stored declaration's alias is trimmed (BuildDeclarations), so a legacy
// "payroll " job reads "payroll" exactly like its canonical twin. Two
// producers that only look alike are still two producers (identity-and-access
// W7-ε): persisted twins, and a persisted twin plus the incoming canonical
// definition.
func TestValidateGraphTreatsSameAliasJobsAsDistinctProducers(t *testing.T) {
	legacy, canonical := uuid.New(), uuid.New()
	for name, decls := range map[string][]models.DatasetDeclaration{
		"persisted twins": {
			persisted(legacy, produces("payroll", "ledger")),
			persisted(canonical, produces("payroll", "ledger")),
		},
		"incoming canonical beside a persisted twin": {
			persisted(legacy, produces("payroll", "ledger")),
			produces("payroll", "ledger"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateGraph(decls)
			require.ErrorIs(t, err, ErrDatasetMultipleProducers)
			var graphErr *GraphError
			require.True(t, errors.As(err, &graphErr))
			require.Equal(t, []string{"payroll", "payroll"}, graphErr.Jobs)
			require.Equal(t, `dataset produced by multiple jobs: "ledger" produced by payroll, payroll`, err.Error())
		})
	}
}

// One job's own rows (one id) stay one producer, and a cycle between two
// same-alias jobs is a cycle between two jobs, not a self-loop.
func TestValidateGraphJobIdentityKeepsOneJobOneNode(t *testing.T) {
	job := uuid.New()
	require.NoError(t, ValidateGraph([]models.DatasetDeclaration{
		persisted(job, produces("payroll", "ledger")),
		persisted(job, produces("payroll", "ledger")),
		persisted(job, consumes("payroll", "ledger")),
	}))

	legacy, canonical := uuid.New(), uuid.New()
	err := ValidateGraph([]models.DatasetDeclaration{
		persisted(legacy, produces("payroll", "x")),
		persisted(legacy, consumes("payroll", "y")),
		persisted(canonical, produces("payroll", "y")),
		persisted(canonical, consumes("payroll", "x")),
	})
	require.ErrorIs(t, err, ErrDatasetGraphCycle)
	require.Equal(t, "dataset dependency cycle detected: payroll -> payroll -> payroll", err.Error())
}

// Distinct aliases keep the diagnostics they always had: alias order, whatever
// the ids, for persisted, incoming and mixed declarations.
func TestValidateGraphDiagnosticsKeepAliasOrder(t *testing.T) {
	high := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	low := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	for name, decls := range map[string][]models.DatasetDeclaration{
		"incoming":  {produces("job_b", "d"), produces("job_a", "d")},
		"persisted": {persisted(low, produces("job_b", "d")), persisted(high, produces("job_a", "d"))},
		"mixed":     {persisted(low, produces("job_b", "d")), produces("job_a", "d")},
	} {
		t.Run(name, func(t *testing.T) {
			err := ValidateGraph(decls)
			require.Equal(t, `dataset produced by multiple jobs: "d" produced by job_a, job_b`, err.Error())
		})
	}
	for name, decls := range map[string][]models.DatasetDeclaration{
		"incoming": {
			produces("job_a", "X"), consumes("job_a", "Y"),
			produces("job_b", "Y"), consumes("job_b", "X"),
		},
		"mixed": {
			persisted(high, produces("job_a", "X")), persisted(high, consumes("job_a", "Y")),
			produces("job_b", "Y"), consumes("job_b", "X"),
		},
	} {
		t.Run("cycle "+name, func(t *testing.T) {
			err := ValidateGraph(decls)
			require.Equal(t, "dataset dependency cycle detected: job_a -> job_b -> job_a", err.Error())
		})
	}
}
