package contract

import (
	"testing"

	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/stretchr/testify/require"
)

func TestFindingsFromGraphPreservesConcreteKey(t *testing.T) {
	graph := internalcontract.Graph{
		Edges: []internalcontract.Edge{{
			ID:      "inferred:producer:consumer",
			From:    internalcontract.JobNodeID("producer"),
			To:      internalcontract.JobNodeID("consumer"),
			Class:   internalcontract.EdgeClassInferred,
			Verdict: schemacompat.VerdictBreaking,
			Findings: []schemacompat.Finding{{
				Kind:    schemacompat.FindingKindRequirementUnsatisfied,
				Path:    "trigger.configuration.paramMapping.upstream_rows",
				Key:     "row_count",
				Detail:  "missing row_count",
				Verdict: schemacompat.VerdictBreaking,
			}},
		}},
	}

	findings := FindingsFromGraph(graph)

	require.Len(t, findings, 1)
	require.Equal(t, "row_count", findings[0].Key)
	require.Equal(t, "job:producer", findings[0].From)
	require.Equal(t, "job:consumer", findings[0].To)
}
