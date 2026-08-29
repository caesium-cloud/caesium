package contract

import (
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/stretchr/testify/require"
)

func TestAliasSetCollectsTrimmedAliases(t *testing.T) {
	aliases := AliasSet([]schema.Definition{
		{Metadata: schema.Metadata{Alias: " producer "}},
		{Metadata: schema.Metadata{Alias: "consumer"}},
		{Metadata: schema.Metadata{Alias: "   "}},
	})

	require.Equal(t, map[string]struct{}{"producer": {}, "consumer": {}}, aliases)
}

func TestAliasSetOnEmptyBatchIsNonNilAndEmpty(t *testing.T) {
	aliases := AliasSet(nil)
	require.NotNil(t, aliases)
	require.Empty(t, aliases)
}

func TestEdgeInAliasScopeMatchesEitherEndpoint(t *testing.T) {
	edge := Edge{From: JobNodeID("producer"), To: JobNodeID("consumer")}

	require.True(t, EdgeInAliasScope(edge, map[string]struct{}{"producer": {}}))
	require.True(t, EdgeInAliasScope(edge, map[string]struct{}{"consumer": {}}))
	require.False(t, EdgeInAliasScope(edge, map[string]struct{}{"unrelated": {}}))
	require.False(t, EdgeInAliasScope(edge, map[string]struct{}{}))
	require.True(t, EdgeInAliasScope(edge, nil), "a nil alias set means no scope was requested")
}

func TestEdgeInAliasScopeIgnoresDatasetNodes(t *testing.T) {
	dataset := DatasetRef{Namespace: "lake", Name: "customers"}
	edge := Edge{From: datasetNodeID(dataset), To: datasetNodeID(dataset)}

	require.False(t, EdgeInAliasScope(edge, map[string]struct{}{"customers": {}}))
	require.False(t, EdgeInAliasScope(edge, map[string]struct{}{"lake/customers": {}}))
}

// TestScopeGraphToAliasesDropsUnrelatedBreakingPairs is the regression this
// scope exists for (#362): `caesium job lint --server` derives its graph over
// the linted definitions unioned with every persisted job, so an unrelated
// breaking pair on a shared server used to fail every later lint.
func TestScopeGraphToAliasesDropsUnrelatedBreakingPairs(t *testing.T) {
	graph := scopeFixtureGraph()

	scoped := ScopeGraphToAliases(graph, map[string]struct{}{"unrelated": {}})

	require.Empty(t, scoped.Edges, "no edge touches the linted job")
	require.Len(t, scoped.Nodes, 1)
	require.Equal(t, JobNodeID("unrelated"), scoped.Nodes[0].ID,
		"the linted job's own node is retained even with no edges")
}

func TestScopeGraphToAliasesKeepsEdgesTouchingTheLintedProducer(t *testing.T) {
	graph := scopeFixtureGraph()

	scoped := ScopeGraphToAliases(graph, map[string]struct{}{"producer": {}})

	require.Len(t, scoped.Edges, 1)
	require.Equal(t, JobNodeID("producer"), scoped.Edges[0].From)
	require.Equal(t, JobNodeID("consumer"), scoped.Edges[0].To)
	require.Equal(t, schemacompat.VerdictBreaking, scoped.Edges[0].Verdict)
	require.ElementsMatch(t,
		[]string{JobNodeID("producer"), JobNodeID("consumer"), "dataset:lake/customers"},
		scopeNodeIDs(scoped),
		"nodes referenced by a kept edge, including its dataset, are retained")
}

// TestScopeGraphToAliasesKeepsEdgesTouchingTheLintedConsumer is the direct
// consumer half of the scope: linting only the consumer must still surface a
// break introduced by a producer that lives on the server.
func TestScopeGraphToAliasesKeepsEdgesTouchingTheLintedConsumer(t *testing.T) {
	graph := scopeFixtureGraph()

	scoped := ScopeGraphToAliases(graph, map[string]struct{}{"consumer": {}})

	require.Len(t, scoped.Edges, 1)
	require.Equal(t, JobNodeID("consumer"), scoped.Edges[0].To)
}

func TestScopeGraphToAliasesWithNilSetIsUnscoped(t *testing.T) {
	graph := scopeFixtureGraph()

	scoped := ScopeGraphToAliases(graph, nil)

	require.Equal(t, graph, scoped)
}

// scopeFixtureGraph mirrors a shared server: one breaking declared pair
// (producer -> consumer over lake.customers) plus an entirely separate healthy
// pair, and a linted-but-unconnected job node.
func scopeFixtureGraph() Graph {
	dataset := &DatasetRef{Namespace: "lake", Name: "customers"}
	return Graph{
		Nodes: []Node{
			{ID: JobNodeID("producer"), Kind: NodeKindJob, Alias: "producer"},
			{ID: JobNodeID("consumer"), Kind: NodeKindJob, Alias: "consumer"},
			{ID: JobNodeID("other-producer"), Kind: NodeKindJob, Alias: "other-producer"},
			{ID: JobNodeID("other-consumer"), Kind: NodeKindJob, Alias: "other-consumer"},
			{ID: JobNodeID("unrelated"), Kind: NodeKindJob, Alias: "unrelated"},
			{ID: "dataset:lake/customers", Kind: NodeKindDataset, Dataset: dataset},
		},
		Edges: []Edge{
			{
				ID:      "edge:declared:job:producer->job:consumer:lake/customers",
				From:    JobNodeID("producer"),
				To:      JobNodeID("consumer"),
				Class:   EdgeClassDeclared,
				Verdict: schemacompat.VerdictBreaking,
				Dataset: dataset,
				Findings: []schemacompat.Finding{{
					Kind:    schemacompat.FindingKindRequiredRemoved,
					Path:    "datasets.produces.lake.customers.schema.properties.customer_id",
					Key:     "customer_id",
					Detail:  "required property removed",
					Verdict: schemacompat.VerdictBreaking,
				}},
			},
			{
				ID:      "edge:inferred:job:other-producer->job:other-consumer",
				From:    JobNodeID("other-producer"),
				To:      JobNodeID("other-consumer"),
				Class:   EdgeClassInferred,
				Verdict: schemacompat.VerdictCompatible,
			},
		},
	}
}

func scopeNodeIDs(graph Graph) []string {
	ids := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		ids = append(ids, node.ID)
	}
	return ids
}
