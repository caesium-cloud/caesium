package contract

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
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

func TestDeriveGraphUnscopedLifecyclePatternFansOutToKnownJobs(t *testing.T) {
	producerA := Job{
		Alias: "producer-a",
		Steps: []Step{{
			Name:         "extract",
			OutputSchema: outputSchemaWithProperties("row_count"),
		}},
	}
	producerB := Job{
		Alias: "producer-b",
		Steps: []Step{{
			Name:         "extract",
			OutputSchema: outputSchemaWithProperties("row_count"),
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTriggerWithFilter(map[string]any{}, map[string]string{
			"rows": "$.tasks[0].output.row_count",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producerA, producerB, consumer}})
	require.NoError(t, err)

	edgeA := requireEdge(t, graph, EdgeClassInferred, "job:producer-a", "job:consumer", "")
	require.Equal(t, schemacompat.VerdictCompatible, edgeA.Verdict)
	require.Empty(t, edgeA.Findings)

	edgeB := requireEdge(t, graph, EdgeClassInferred, "job:producer-b", "job:consumer", "")
	require.Equal(t, schemacompat.VerdictCompatible, edgeB.Verdict)
	require.Empty(t, edgeB.Findings)
	require.Len(t, graph.Edges, 2)
}

func TestDeriveGraphUnknownJobIDFilterWarns(t *testing.T) {
	missingJobID := uuid.New()
	consumer := Job{
		Alias: "consumer",
		Trigger: eventTriggerWithFilter(map[string]any{"job_id": missingJobID.String()}, map[string]string{
			"rows": "$.tasks[0].output.row_count",
		}),
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassInferred, "job:"+missingJobID.String(), "job:consumer", "")
	require.Equal(t, schemacompat.VerdictUnknown, edge.Verdict)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequirementUnknown, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Detail, missingJobID.String())
	require.Contains(t, edge.Findings[0].Detail, "not present in the merged job set")
	requireJobNode(t, graph, missingJobID.String())
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

func TestGORMStoreListContractEvidenceAggregatesBeforeJoining(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	createContractEvidenceTables(t, db)

	producerJobID := uuid.New()
	consumerJobID := uuid.New()
	insertEvidenceJob(t, db, producerJobID, "producer")
	insertEvidenceJob(t, db, consumerJobID, "consumer")

	olderConsumerSeen := time.Date(2026, 7, 7, 10, 0, 0, 0, time.UTC)
	newerConsumerSeen := olderConsumerSeen.Add(2 * time.Hour)
	insertEvidenceDataset(t, db, producerJobID, "lake", "customers", "output", olderConsumerSeen.Add(-2*time.Hour))
	insertEvidenceDataset(t, db, producerJobID, "lake", "customers", "output", olderConsumerSeen.Add(-time.Hour))
	insertEvidenceDataset(t, db, consumerJobID, "lake", "customers", "input", olderConsumerSeen)
	insertEvidenceDataset(t, db, consumerJobID, "lake", "customers", "input", newerConsumerSeen)

	records, err := (GORMStore{DB: db}).ListContractEvidence(context.Background())
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, producerJobID, records[0].ProducerJobID)
	require.Equal(t, "producer", records[0].ProducerJobAlias)
	require.Equal(t, consumerJobID, records[0].ConsumerJobID)
	require.Equal(t, "consumer", records[0].ConsumerJobAlias)
	require.Equal(t, DatasetRef{Namespace: "lake", Name: "customers"}, records[0].Dataset)
	require.WithinDuration(t, newerConsumerSeen, records[0].LastSeen, time.Second)
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

func TestDeriveGraphDeclaredEdgeResolvesSchemaFromOutput(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name:         "export",
			OutputSchema: objectSchemaWithRequired("customer_id", "row_count"),
			Produces: []ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
			}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name: "load",
			Consumes: []ConsumedDataset{{
				Name:   "lake.customers",
				Schema: objectSchemaWithRequired("customer_id"),
			}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictCompatible, edge.Verdict)
	require.Empty(t, edge.Findings)
	require.Contains(t, edge.ProducerSchema["properties"], "customer_id")
	require.Contains(t, edge.ConsumerSchema["properties"], "customer_id")
	requireDatasetNode(t, graph, "/lake.customers")
}

func TestDeriveGraphDeclaredEdgeUsesInlineProducerSchema(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name: "export",
			Produces: []ProducedDataset{{
				Name:   "lake.customers",
				Schema: objectSchemaWithRequired("customer_id"),
			}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name: "load",
			Consumes: []ConsumedDataset{{
				Name:   "lake.customers",
				Schema: objectSchemaWithRequired("customer_id"),
			}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictCompatible, edge.Verdict)
	require.Empty(t, edge.Findings)
	require.Contains(t, edge.ProducerSchema["properties"], "customer_id")
}

func TestDeriveGraphDeclaredNameLevelEdgeWithoutSchemas(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name:     "export",
			Produces: []ProducedDataset{{Name: "lake.customers"}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name:     "load",
			Consumes: []ConsumedDataset{{Name: "lake.customers"}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictCompatible, edge.Verdict)
	require.Empty(t, edge.Findings)
	require.Empty(t, edge.ProducerSchema)
	require.Empty(t, edge.ConsumerSchema)
}

func TestDeriveGraphDeclaredConsumerRequirementReportsBreaking(t *testing.T) {
	producer := Job{
		Alias: "producer",
		Steps: []Step{{
			Name:         "export",
			OutputSchema: objectSchemaWithRequired("row_count"),
			Produces: []ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
			}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name: "load",
			Consumes: []ConsumedDataset{{
				Name:   "lake.customers",
				Schema: objectSchemaWithRequired("customer_id"),
			}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{Jobs: []Job{producer, consumer}})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictBreaking, edge.Verdict)
	require.Len(t, edge.Findings, 2)
	require.Equal(t, schemacompat.FindingKindRequirementUnsatisfied, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Path, "datasets.consumes.lake.customers.schema")
	require.Contains(t, edge.Findings[0].Detail, "customer_id")
}

func TestDeriveGraphDeclaredSchemaLessConsumerUsesProducerComparison(t *testing.T) {
	producer := Job{
		Alias:    "producer",
		Incoming: true,
		Steps: []Step{{
			Name:         "export",
			OutputSchema: objectSchemaWithRequired("row_count"),
			Produces: []ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
			}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name:     "load",
			Consumes: []ConsumedDataset{{Name: "lake.customers"}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{
		Jobs: []Job{producer, consumer},
		PreviousProducerSchemas: []ProducerSchemaRecord{{
			JobAlias:    "producer",
			StepName:    "export",
			Dataset:     DatasetRef{Name: "lake.customers"},
			Schema:      objectSchemaWithRequired("customer_id", "row_count"),
			SchemaKnown: true,
		}},
	})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictBreaking, edge.Verdict)
	require.NotEmpty(t, edge.PreviousProducerSchema)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequiredRemoved, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Detail, "customer_id")
}

func TestDeriveGraphDeclaredBatchConsumerRequirementCanMigrate(t *testing.T) {
	producer := Job{
		Alias:    "producer",
		Incoming: true,
		Steps: []Step{{
			Name:         "export",
			OutputSchema: objectSchemaWithRequired("row_count"),
			Produces: []ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
			}},
		}},
	}
	consumer := Job{
		Alias:    "consumer",
		Incoming: true,
		Steps: []Step{{
			Name: "load",
			Consumes: []ConsumedDataset{{
				Name:   "lake.customers",
				Schema: objectSchemaWithRequired("row_count"),
			}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{
		Jobs: []Job{producer, consumer},
		PreviousProducerSchemas: []ProducerSchemaRecord{{
			JobAlias:    "producer",
			StepName:    "export",
			Dataset:     DatasetRef{Name: "lake.customers"},
			Schema:      objectSchemaWithRequired("customer_id", "row_count"),
			SchemaKnown: true,
		}},
	})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictCompatible, edge.Verdict)
	require.Empty(t, edge.Findings)
}

func TestDeriveGraphDeclaredUnknownPreviousProducerSchemaWarns(t *testing.T) {
	producer := Job{
		Alias:    "producer",
		Incoming: true,
		Steps: []Step{{
			Name:         "export",
			OutputSchema: objectSchemaWithRequired("customer_id"),
			Produces: []ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
			}},
		}},
	}
	consumer := Job{
		Alias: "consumer",
		Steps: []Step{{
			Name:     "load",
			Consumes: []ConsumedDataset{{Name: "lake.customers"}},
		}},
	}

	graph, err := DeriveGraph(DeriveInput{
		Jobs: []Job{producer, consumer},
		PreviousProducerSchemas: []ProducerSchemaRecord{{
			JobAlias: "producer",
			StepName: "export",
			Dataset:  DatasetRef{Name: "lake.customers"},
		}},
	})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictUnknown, edge.Verdict)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequirementUnknown, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Detail, "persisted producer schema")
}

func TestGORMStoreListContractProducerSchemasUsesTaskOutputSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	createContractJobTables(t, db)

	jobID := uuid.New()
	triggerID := uuid.New()
	insertContractTrigger(t, db, triggerID)
	insertContractJob(t, db, jobID, triggerID, "producer")
	insertContractTask(t, db, jobID, "export", objectSchemaWithRequired("customer_id"))
	insertDatasetDeclarationWithSchema(t, db, jobID, "producer", "export", "lake.customers", "produces", "", schema.DatasetSchemaFromOutput, 2)

	records, err := (GORMStore{DB: db}).ListContractProducerSchemas(context.Background(), []schema.Definition{{
		Metadata: schema.Metadata{Alias: "producer"},
	}})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "producer", records[0].JobAlias)
	require.Equal(t, "export", records[0].StepName)
	require.Equal(t, DatasetRef{Name: "lake.customers"}, records[0].Dataset)
	require.True(t, records[0].SchemaKnown)
	require.Contains(t, records[0].Schema["properties"], "customer_id")
}

func TestGORMStoreListContractProducerSchemasUsesDeclarationInlineSchema(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	createContractJobTables(t, db)

	jobID := uuid.New()
	triggerID := uuid.New()
	insertContractTrigger(t, db, triggerID)
	insertContractJob(t, db, jobID, triggerID, "producer")
	insertDatasetDeclarationWithSchema(t, db, jobID, "producer", "export", "lake.customers", "produces", mustJSON(t, objectSchemaWithRequired("customer_id")), "", 3)

	records, err := (GORMStore{DB: db}).ListContractProducerSchemas(context.Background(), []schema.Definition{{
		Metadata: schema.Metadata{Alias: "producer"},
	}})
	require.NoError(t, err)
	require.Len(t, records, 1)
	require.Equal(t, "producer", records[0].JobAlias)
	require.Equal(t, "export", records[0].StepName)
	require.Equal(t, DatasetRef{Name: "lake.customers"}, records[0].Dataset)
	require.True(t, records[0].SchemaKnown)
	require.Contains(t, records[0].Schema["properties"], "customer_id")
}

func TestGORMStoreDeriveGraphUsesPersistedConsumerSchemaRequirement(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	createContractJobTables(t, db)

	consumerID := uuid.New()
	triggerID := uuid.New()
	insertContractTrigger(t, db, triggerID)
	insertContractJob(t, db, consumerID, triggerID, "consumer")
	insertContractTask(t, db, consumerID, "load", nil)
	insertDatasetDeclarationWithSchema(t, db, consumerID, "consumer", "load", "lake.customers", "consumes", mustJSON(t, objectSchemaWithRequired("customer_id")), "", 0)

	incomingProducer := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: "producer"},
		Trigger: schema.Trigger{
			Type:          schema.TriggerCron,
			Configuration: map[string]any{"cron": "0 0 * * *"},
		},
		Steps: []schema.Step{{
			Name:         "export",
			Image:        "alpine:3.23",
			OutputSchema: objectSchemaWithRequired("row_count"),
			Datasets: &schema.StepDatasets{Produces: []schema.ProducedDataset{{
				Name:       "lake.customers",
				SchemaFrom: schema.DatasetSchemaFromOutput,
				Version:    2,
			}}},
		}},
	}

	store := GORMStore{DB: db}
	graph, err := (Deriver{Jobs: store, ProducerSchemas: store}).DeriveGraph(context.Background(), []schema.Definition{incomingProducer})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassDeclared, "job:producer", "job:consumer", "/lake.customers")
	require.Equal(t, schemacompat.VerdictBreaking, edge.Verdict)
	require.Contains(t, edge.ProducerSchema["properties"], "row_count")
	require.Contains(t, edge.ConsumerSchema["properties"], "customer_id")
	require.Len(t, edge.Findings, 2)
	require.Equal(t, schemacompat.FindingKindRequirementUnsatisfied, edge.Findings[0].Kind)
	require.Contains(t, edge.Findings[0].Path, "datasets.consumes.lake.customers.schema")
	require.Contains(t, edge.Findings[0].Detail, "customer_id")
}

func TestGORMStoreDeriveGraphReportsIncomingProducerRemovedMappedOutput(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	createContractJobTables(t, db)

	producerAlias := "contract-diff-producer"
	consumerAlias := "contract-diff-consumer"

	producerID := uuid.New()
	producerTriggerID := uuid.New()
	insertContractTriggerWithConfig(t, db, producerTriggerID, schema.TriggerCron, map[string]any{
		"cron":     "0 2 * * *",
		"timezone": "UTC",
	})
	insertContractJob(t, db, producerID, producerTriggerID, producerAlias)
	insertContractTask(t, db, producerID, "export", integerOutputSchemaWithRequired("row_count"))

	consumerID := uuid.New()
	consumerTriggerID := uuid.New()
	insertContractTriggerWithConfig(t, db, consumerTriggerID, schema.TriggerEvent, map[string]any{
		"events": []any{
			map[string]any{
				"type":   "run_completed",
				"source": "caesium",
				"filter": map[string]any{
					"job_alias": producerAlias,
				},
			},
		},
		"paramMapping": map[string]any{
			"upstream_rows": "$.tasks[0].output.row_count",
		},
	})
	insertContractJob(t, db, consumerID, consumerTriggerID, consumerAlias)
	insertContractTask(t, db, consumerID, "load", nil)

	incomingProducer := schema.Definition{
		APIVersion: schema.APIVersionV1,
		Kind:       "Job",
		Metadata:   schema.Metadata{Alias: producerAlias},
		Trigger: schema.Trigger{
			Type: schema.TriggerCron,
			Configuration: map[string]any{
				"cron":     "0 2 * * *",
				"timezone": "UTC",
			},
		},
		Steps: []schema.Step{{
			Name:         "export",
			Engine:       "docker",
			Image:        "alpine:3.23",
			Command:      []string{"sh", "-c", "echo export"},
			OutputSchema: integerOutputSchemaWithRequired(),
		}},
	}

	graph, err := (Deriver{Jobs: GORMStore{DB: db}}).DeriveGraph(context.Background(), []schema.Definition{incomingProducer})
	require.NoError(t, err)

	edge := requireEdge(t, graph, EdgeClassInferred, JobNodeID(producerAlias), JobNodeID(consumerAlias), "")
	require.Equal(t, JobNodeID(producerAlias), edge.From)
	require.Equal(t, schemacompat.VerdictBreaking, edge.Verdict)
	require.Len(t, edge.Findings, 1)
	require.Equal(t, schemacompat.FindingKindRequirementUnsatisfied, edge.Findings[0].Kind)
	require.Equal(t, schemacompat.VerdictBreaking, edge.Findings[0].Verdict)
	require.Equal(t, "row_count", edge.Findings[0].Key)
	require.Equal(t, "trigger.configuration.paramMapping.upstream_rows", edge.Findings[0].Path)
	require.Contains(t, edge.Findings[0].Detail, "row_count")
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

func objectSchemaWithRequired(keys ...string) map[string]any {
	schema := outputSchemaWithProperties(keys...)
	schema["required"] = append([]string(nil), keys...)
	return schema
}

func integerOutputSchemaWithRequired(keys ...string) map[string]any {
	properties := make(map[string]any, len(keys))
	for _, key := range keys {
		properties[key] = map[string]any{"type": "integer"}
	}
	return map[string]any{
		"type":       "object",
		"required":   append([]string(nil), keys...),
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

func requireJobNode(t *testing.T, graph Graph, alias string) {
	t.Helper()
	for _, node := range graph.Nodes {
		if node.Kind == NodeKindJob && node.Alias == alias {
			return
		}
	}
	t.Fatalf("job node %q not found in %#v", alias, graph.Nodes)
}

func createContractEvidenceTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("CREATE TABLE jobs (id text PRIMARY KEY, alias text NOT NULL, deleted_at datetime)").Error)
	require.NoError(t, db.Exec("CREATE TABLE job_runs (id text PRIMARY KEY, job_id text NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE TABLE task_runs (id text PRIMARY KEY, job_run_id text NOT NULL)").Error)
	require.NoError(t, db.Exec("CREATE TABLE lineage_datasets (id text PRIMARY KEY, task_run_id text NOT NULL, namespace text NOT NULL, name text NOT NULL, direction text NOT NULL, created_at datetime NOT NULL)").Error)
}

func insertEvidenceJob(t *testing.T, db *gorm.DB, jobID uuid.UUID, alias string) {
	t.Helper()
	require.NoError(t, db.Exec("INSERT INTO jobs (id, alias, deleted_at) VALUES (?, ?, NULL)", jobID, alias).Error)
}

func insertEvidenceDataset(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace, name, direction string, createdAt time.Time) {
	t.Helper()
	jobRunID := uuid.New()
	taskRunID := uuid.New()
	require.NoError(t, db.Exec("INSERT INTO job_runs (id, job_id) VALUES (?, ?)", jobRunID, jobID).Error)
	require.NoError(t, db.Exec("INSERT INTO task_runs (id, job_run_id) VALUES (?, ?)", taskRunID, jobRunID).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO lineage_datasets (id, task_run_id, namespace, name, direction, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		uuid.New(),
		taskRunID,
		namespace,
		name,
		direction,
		createdAt,
	).Error)
}

func createContractJobTables(t *testing.T, db *gorm.DB) {
	t.Helper()
	require.NoError(t, db.Exec("CREATE TABLE jobs (id text PRIMARY KEY, alias text NOT NULL, trigger_id text NOT NULL, labels json, deleted_at datetime)").Error)
	require.NoError(t, db.Exec("CREATE TABLE triggers (id text PRIMARY KEY, type text NOT NULL, configuration text NOT NULL, deleted_at datetime)").Error)
	require.NoError(t, db.Exec("CREATE TABLE tasks (id text PRIMARY KEY, job_id text NOT NULL, name text NOT NULL, position integer NOT NULL, output_schema json, deleted_at datetime, created_at datetime)").Error)
	require.NoError(t, db.Exec("CREATE TABLE dataset_declarations (id text PRIMARY KEY, job_id text NOT NULL, job_alias text NOT NULL, step_name text NOT NULL, name text NOT NULL, direction text NOT NULL, schema_json text, schema_from text, schema_version integer)").Error)
}

func insertContractTrigger(t *testing.T, db *gorm.DB, triggerID uuid.UUID) {
	t.Helper()
	insertContractTriggerWithConfig(t, db, triggerID, schema.TriggerCron, map[string]any{})
}

func insertContractTriggerWithConfig(t *testing.T, db *gorm.DB, triggerID uuid.UUID, triggerType string, configuration map[string]any) {
	t.Helper()
	require.NoError(t, db.Exec("INSERT INTO triggers (id, type, configuration, deleted_at) VALUES (?, ?, ?, NULL)", triggerID, triggerType, mustJSON(t, configuration)).Error)
}

func insertContractJob(t *testing.T, db *gorm.DB, jobID, triggerID uuid.UUID, alias string) {
	t.Helper()
	require.NoError(t, db.Exec("INSERT INTO jobs (id, alias, trigger_id, labels, deleted_at) VALUES (?, ?, ?, '{}', NULL)", jobID, alias, triggerID).Error)
}

func insertContractTask(t *testing.T, db *gorm.DB, jobID uuid.UUID, name string, outputSchema map[string]any) {
	t.Helper()
	require.NoError(t, db.Exec(
		"INSERT INTO tasks (id, job_id, name, position, output_schema, deleted_at, created_at) VALUES (?, ?, ?, ?, ?, NULL, ?)",
		uuid.New(),
		jobID,
		name,
		0,
		mustJSON(t, outputSchema),
		time.Now().UTC(),
	).Error)
}

func insertDatasetDeclarationWithSchema(t *testing.T, db *gorm.DB, jobID uuid.UUID, jobAlias, stepName, name, direction, schemaJSON, schemaFrom string, schemaVersion int) {
	t.Helper()
	require.NoError(t, db.Exec(
		"INSERT INTO dataset_declarations (id, job_id, job_alias, step_name, name, direction, schema_json, schema_from, schema_version) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		uuid.New(),
		jobID,
		jobAlias,
		stepName,
		name,
		direction,
		schemaJSON,
		schemaFrom,
		schemaVersion,
	).Error)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return string(data)
}
