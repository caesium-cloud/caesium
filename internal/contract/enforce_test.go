package contract

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef/schemacompat"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestEvaluateGraphFailModeBlocksAndNamesConsumerTeam(t *testing.T) {
	graph := breakingInferenceGraph()
	err := EvaluateGraph(context.Background(), nil, graph, EnforcementModeFail, time.Now().UTC())

	var enforcementErr *EnforcementError
	require.ErrorAs(t, err, &enforcementErr)
	require.True(t, errors.Is(err, ErrContractBreakBlocked))
	require.Equal(t, "contract_breaking_change", enforcementErr.Response.Error)
	require.Contains(t, enforcementErr.Response.Message, "customer_id")
	require.Contains(t, enforcementErr.Response.Message, "consumer")
	require.Contains(t, enforcementErr.Response.Message, "team: reporting")
	require.Len(t, enforcementErr.Response.Findings, 1)

	finding := enforcementErr.Response.Findings[0]
	require.Equal(t, "producer.output.customer_id", finding.Subject)
	require.Equal(t, "customer_id", finding.OutputKey)
	require.Equal(t, "producer", finding.Producer)
	require.Equal(t, "consumer", finding.Consumer)
	require.Equal(t, "reporting", finding.ConsumerTeam)
	require.Equal(t, "inferred", finding.EdgeClass)
	require.NotEmpty(t, finding.EdgeSetDigest)
}

func TestEvaluateGraphAcknowledgementAllowsMatchingDigest(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	graph := breakingInferenceGraph()

	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ContractAck{}))

	err = EvaluateGraph(context.Background(), db, graph, EnforcementModeFail, now)
	var enforcementErr *EnforcementError
	require.ErrorAs(t, err, &enforcementErr)
	digest := enforcementErr.Response.Findings[0].EdgeSetDigest

	require.NoError(t, db.Create(&models.ContractAck{
		ID:            uuid.New(),
		Dataset:       enforcementErr.Response.Findings[0].Subject,
		EdgeSetDigest: digest,
		Actor:         "integration-test",
		Reason:        "planned migration",
		CreatedAt:     now.Add(-time.Minute),
		ExpiresAt:     now.Add(time.Hour),
	}).Error)

	require.NoError(t, EvaluateGraph(context.Background(), db, graph, EnforcementModeFail, now))
}

func TestAllowBreakingMatchesDeclaredDatasetName(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	graph := breakingDeclaredDatasetGraph()

	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ContractAck{}))

	result, err := EvaluateGraphWithOptions(context.Background(), db, graph, EnforcementModeFail, now, ApplyOptions{
		AllowBreaking:     &AllowBreaking{Dataset: "lake.customers", Actor: "operator@example.com", Reason: "planned migration"},
		DeprecationWindow: time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1)
	require.Equal(t, "lake.customers", result.Warnings[0].Dataset)
	require.Contains(t, result.Warnings[0].Message, "lake.customers")

	var ack models.ContractAck
	require.NoError(t, db.First(&ack).Error)
	require.Equal(t, "lake.customers", ack.Dataset)
	require.Equal(t, "operator@example.com", ack.Actor)
	require.Equal(t, now.Add(time.Hour), ack.ExpiresAt)
}

func TestEdgeSetDigestV2IgnoresConsumerTeam(t *testing.T) {
	findings := []ContractFinding{{
		Subject:      "producer.output.customer_id",
		OutputKey:    "customer_id",
		Producer:     "producer",
		Consumer:     "consumer",
		ConsumerTeam: "reporting",
		EdgeClass:    "inferred",
		Path:         "trigger.configuration.paramMapping.customer",
		Detail:       "missing customer_id",
		Verdict:      "breaking",
		EdgeID:       "edge:1",
	}}
	first, err := edgeSetDigest(findings[0].Subject, findings)
	require.NoError(t, err)

	findings[0].ConsumerTeam = "analytics"
	second, err := edgeSetDigest(findings[0].Subject, findings)
	require.NoError(t, err)

	require.Equal(t, first, second)
}

func TestEvaluateGraphUsesStructuredFindingKey(t *testing.T) {
	graph := breakingInferenceGraph()
	graph.Edges[0].Findings[0].Key = "customer_id"
	graph.Edges[0].Findings[0].Detail = "message without quoted key"

	err := EvaluateGraph(context.Background(), nil, graph, EnforcementModeFail, time.Now().UTC())

	var enforcementErr *EnforcementError
	require.ErrorAs(t, err, &enforcementErr)
	require.Equal(t, "producer.output.customer_id", enforcementErr.Response.Findings[0].Subject)
	require.Equal(t, "customer_id", enforcementErr.Response.Findings[0].OutputKey)
}

func TestEvaluateGraphWarnModeDoesNotBlock(t *testing.T) {
	require.NoError(t, EvaluateGraph(context.Background(), nil, breakingInferenceGraph(), EnforcementModeWarn, time.Now().UTC()))
}

func breakingInferenceGraph() Graph {
	return Graph{
		Nodes: []Node{
			{ID: "job:producer", Kind: NodeKindJob, Alias: "producer"},
			{ID: "job:consumer", Kind: NodeKindJob, Alias: "consumer", Labels: map[string]string{"team": "reporting"}},
		},
		Edges: []Edge{{
			ID:      "edge:inferred:job:producer->job:consumer",
			From:    "job:producer",
			To:      "job:consumer",
			Class:   EdgeClassInferred,
			Verdict: schemacompat.VerdictBreaking,
			Findings: []schemacompat.Finding{{
				Kind:    schemacompat.FindingKindRequirementUnsatisfied,
				Path:    "trigger.configuration.paramMapping.customer",
				Detail:  `paramMapping "customer" references $.tasks[0].output.customer_id output key "customer_id" from producer producer step "export", but that key is missing from the step outputSchema`,
				Verdict: schemacompat.VerdictBreaking,
			}},
		}},
	}
}

func breakingDeclaredDatasetGraph() Graph {
	dataset := &DatasetRef{Name: "lake.customers"}
	return Graph{
		Nodes: []Node{
			{ID: "job:producer", Kind: NodeKindJob, Alias: "producer"},
			{ID: "job:consumer", Kind: NodeKindJob, Alias: "consumer", Labels: map[string]string{"team": "reporting"}},
			{ID: "dataset:/lake.customers", Kind: NodeKindDataset, Dataset: dataset},
		},
		Edges: []Edge{{
			ID:      "edge:declared:job:producer->job:consumer:/lake.customers",
			From:    "job:producer",
			To:      "job:consumer",
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
		}},
	}
}
