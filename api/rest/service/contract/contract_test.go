package contract

import (
	"testing"

	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
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

// TestScopeGraphToDefinitionsDropsUnrelatedBreakingPairs guards the lint gate
// (#362): POST /v1/jobdefs/lint derives its graph over the posted definitions
// unioned with every persisted job, so the summary it reports — and the
// breaking count `caesium job lint --server` exits non-zero on — must only
// cover contracts the linted jobs participate in.
func TestScopeGraphToDefinitionsDropsUnrelatedBreakingPairs(t *testing.T) {
	graph := internalcontract.Graph{
		Nodes: []internalcontract.Node{
			{ID: internalcontract.JobNodeID("producer"), Kind: internalcontract.NodeKindJob, Alias: "producer"},
			{ID: internalcontract.JobNodeID("consumer"), Kind: internalcontract.NodeKindJob, Alias: "consumer"},
			{ID: internalcontract.JobNodeID("unrelated"), Kind: internalcontract.NodeKindJob, Alias: "unrelated"},
		},
		Edges: []internalcontract.Edge{{
			ID:      "edge:inferred:job:producer->job:consumer",
			From:    internalcontract.JobNodeID("producer"),
			To:      internalcontract.JobNodeID("consumer"),
			Class:   internalcontract.EdgeClassInferred,
			Verdict: schemacompat.VerdictBreaking,
			Findings: []schemacompat.Finding{{
				Kind:    schemacompat.FindingKindRequirementUnsatisfied,
				Path:    "trigger.configuration.paramMapping.customer",
				Key:     "customer_id",
				Detail:  "missing customer_id",
				Verdict: schemacompat.VerdictBreaking,
			}},
		}},
	}

	unrelated := SummaryFromGraph(ScopeGraphToDefinitions(graph, []schema.Definition{
		{Metadata: schema.Metadata{Alias: "unrelated"}},
	}))
	require.Empty(t, unrelated.Breaking)
	require.Empty(t, unrelated.Warnings)
	require.Equal(t, 0, unrelated.Edges)

	for _, alias := range []string{"producer", "consumer"} {
		scoped := SummaryFromGraph(ScopeGraphToDefinitions(graph, []schema.Definition{
			{Metadata: schema.Metadata{Alias: alias}},
		}))
		require.Len(t, scoped.Breaking, 1, "linting %s must still see the break it participates in", alias)
		require.Equal(t, "customer_id", scoped.Breaking[0].Key)
		require.Equal(t, 1, scoped.Edges)
	}
}
