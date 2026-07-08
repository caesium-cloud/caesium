package contract

import (
	"testing"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDeriveGraphInferredEdgeReportsMissingMappedOutput(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name:         "extract",
			OutputSchema: outputSchemaWithProperties("row_count"),
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTrigger("producer", map[string]string{
			"rows":     "$.tasks[0].output.row_count",
			"customer": "$.tasks[0].output.customer_id",
			"run_id":   "$.run_id",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassInferred, "job:producer", "job:consumer", "")
	require.Equal(t, schemacompat.VerdictBreaking, edge.Verdict)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequirementUnsatisfied, edge.Findings[0].Kind)
	require.Equal(t, schemacompat.VerdictBreaking, edge.Findings[0].Verdict)
	require.Equal(t, "trigger.configuration.paramMapping.customer", edge.Findings[0].Path)
	require.Contains(t, edge.Findings[0].Detail, "customer_id")
}

func TestDeriveGraphInferredUnresolvedIndexDegradesToUnknown(t *testing.T) {
	tests := []struct {
		name         string
		outputKey    string
		producerKeys []string
		wantKind     schemacompat.FindingKind
	}{
		{
			name:         "key found in producer union",
			outputKey:    "customer_id",
			producerKeys: []string{"customer_id"},
			wantKind:     schemacompat.FindingKindRequirementUnknown,
		},
		{
			name:         "key missing from producer union",
			outputKey:    "customer_id",
			producerKeys: []string{"row_count"},
			wantKind:     schemacompat.FindingKindRequirementUnsatisfied,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			producer := Job{
				Alias: "producer",
				Steps: []Step{{
					Name:         "extract",
					OutputSchema: outputSchemaWithProperties(tt.producerKeys...),
				}},
			}
			consumer := Job{
				Alias: "consumer",
				Trigger: eventTrigger("producer", map[string]string{
					"customer": "$.tasks[3].output." + tt.outputKey,
				}),
			}

			graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
			require.NoError(t, err)

			edge := requireEdge(t, graph, EdgeClassInferred, "job:producer", "job:consumer", "")
			require.Equal(t, schemacompat.VerdictUnknown, edge.Verdict)
			require.Len(t, edge.Findings, 1)
			require.Equal(t, tt.wantKind, edge.Findings[0].Kind)
			require.Equal(t, schemacompat.VerdictUnknown, edge.Findings[0].Verdict)
			require.Contains(t, edge.Findings[0].Detail, "cannot be resolved")
		})
	}
}

func TestDeriveGraphIgnoresParamMappingsOutsideTaskOutputPaths(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name:         "extract",
			OutputSchema: outputSchemaWithProperties("row_count"),
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTrigger("producer", map[string]string{
			"run_id": "$.run_id",
			"root":   "$",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)
	require.Empty(t, graph.Edges)
}

func TestDeriveGraphInferredUnknownProducerAliasWarns(t *testing.T) {
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTrigger("missing-producer", map[string]string{
			"rows": "$.tasks[0].output.row_count",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassInferred, "job:missing-producer", "job:consumer", "")
	require.Equal(t, schemacompat.VerdictUnknown, edge.Verdict)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequirementUnknown, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Detail, "not present in the merged job set")
}

func TestDeriveGraphEvidenceEdgesAggregateLastSeen(t *testing.T) {
	producerID := uuid.New()
	consumerID := uuid.New()
	older := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	newer := older.Add(2 * time.Hour)

	graph, err := DeriveGraph(DeriveInput{
		Jobs: []Job{
			{ID: producerID, Alias: "producer"},
			{ID: consumerID, Alias: "consumer"},
		},
		Evidence: []EvidenceRecord{
			{
				ProducerJobID:    producerID,
				ProducerJobAlias: "producer",
				ConsumerJobID:    consumerID,
				ConsumerJobAlias: "consumer",
				Dataset:          DatasetRef{Namespace: "lake", Name: "customers"},
				LastSeen:         older,
			},
			{
				ProducerJobID:    producerID,
				ProducerJobAlias: "producer",
				ConsumerJobID:    consumerID,
				ConsumerJobAlias: "consumer",
				Dataset:          DatasetRef{Namespace: "lake", Name: "customers"},
				LastSeen:         newer,
			},
		},
	})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassEvidence, "job:producer", "job:consumer", "lake/customers")
	require.Equal(t, schemacompat.VerdictUnknown, edge.Verdict)
	require.Empty(t, edge.Findings)
	require.NotNil(t, edge.LastSeen)
	require.Equal(t, newer, *edge.LastSeen)
	requireDatasetNode(t, graph, "lake/customers")
}

func TestDeriveGraphInferredEdgeResolvesJobIDFilter(t *testing.T) {
	producerID := uuid.New()
	producer := Job{
		ID:    producerID,
		Alias: "producer",
		Steps: []Step{{
			Name:         "extract",
			OutputSchema: outputSchemaWithProperties("row_count"),
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTriggerWithFilter(map[string]any{"job_id": producerID.String()}, map[string]string{
			"rows": "$.tasks[0].output.row_count",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassInferred, "job:producer", "job:consumer", "")
	require.Equal(t, schemacompat.VerdictCompatible, edge.Verdict)
	require.Empty(t, edge.Findings)
}

func outputSchemaWithProperties(keys ...string) map[string]any {
	properties := make(map[string]any, len(keys))
	for _, key := range keys {
		properties[key] = map[string]any{"type": "string"}
	}
	return map[string]any{
		"type":       "object",
		"properties": properties,
	}
}

func eventTrigger(sourceAlias string, mapping map[string]string) Trigger {
	filter := map[string]any{}
	if sourceAlias != "" {
		filter["job_alias"] = sourceAlias
	}
	return eventTriggerWithFilter(filter, mapping)
}

func eventTriggerWithFilter(filter map[string]any, mapping map[string]string) Trigger {
	cfg := map[string]any{
		"events": []any{
			map[string]any{
				"type":   "run_completed",
				"source": "caesium",
				"filter": filter,
			},
		},
		"paramMapping": map[string]any{},
	}
	for key, value := range mapping {
		cfg["paramMapping"].(map[string]any)[key] = value
	}
	return Trigger{
		Type:          schema.TriggerEvent,
		Configuration: cfg,
	}
}

func requireEdge(t *testing.T, graph Graph, class EdgeClass, from, to, dataset string) Edge {
	t.Helper()
	for _, edge := range graph.Edges {
		if edge.Class != class || edge.From != from || edge.To != to {
			continue
		}
		if datasetKey(edge.Dataset) != dataset {
			continue
		}
		return edge
	}
	t.Fatalf("edge %s %s -> %s dataset %q not found in %#v", class, from, to, dataset, graph.Edges)
	return Edge{}
}

func requireDatasetNode(t *testing.T, graph Graph, dataset string) {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.Kind == NodeKindDataset && datasetKey(node.Dataset) == dataset {
			return
		}
	}
	t.Fatalf("dataset node %q not found in %#v", dataset, graph.Nodes)
}
