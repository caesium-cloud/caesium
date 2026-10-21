package contract

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// hideConsumer is a namespace-confined caller's view that cannot read the
// graph fixtures' consumer job.
func hideConsumer(f ContractFinding) bool { return f.Consumer != "consumer" }

// requireNoForeignText asserts the serialised body names nothing of the
// foreign side: alias, team, dataset or schema key.
func requireNoForeignText(t *testing.T, body any, foreign ...string) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	for _, s := range foreign {
		require.NotContains(t, string(encoded), s, "the body must not name %q: %s", s, encoded)
	}
}

// B9 (design §8.3 boundary): a 409 finding that touches a job the caller
// cannot read is withheld whole, and only counted.
func TestEvaluateGraphWithheldFindingsDiscloseNothing(t *testing.T) {
	for name, graph := range map[string]Graph{"inferred": breakingInferenceGraph(), "declared": breakingDeclaredDatasetGraph()} {
		t.Run(name, func(t *testing.T) {
			_, err := EvaluateGraphWithOptions(context.Background(), nil, graph, EnforcementModeFail, time.Now().UTC(), ApplyOptions{Visible: hideConsumer})
			var enforcementErr *EnforcementError
			require.ErrorAs(t, err, &enforcementErr)
			response := enforcementErr.Response
			require.Equal(t, "contract_breaking_change", response.Error)
			require.Empty(t, response.Findings)
			require.NotNil(t, response.Findings, "the list is empty, not null")
			require.Equal(t, 1, response.WithheldFindings)
			require.Equal(t, "contract enforcement blocked 1 breaking finding(s): "+ForeignContractMessage+" (1 finding(s))", response.Message)
			requireNoForeignText(t, response, "consumer", "reporting", "customer_id", "lake.customers")
		})
	}
}

// Without a view nothing changes (the unrestricted response).
func TestEvaluateGraphWithoutViewIsUnchanged(t *testing.T) {
	graph := breakingInferenceGraph()
	_, plain := EvaluateGraphWithOptions(context.Background(), nil, graph, EnforcementModeFail, time.Now().UTC(), ApplyOptions{})
	_, everything := EvaluateGraphWithOptions(context.Background(), nil, graph, EnforcementModeFail, time.Now().UTC(),
		ApplyOptions{Visible: func(ContractFinding) bool { return true }})
	var a, b *EnforcementError
	require.ErrorAs(t, plain, &a)
	require.ErrorAs(t, everything, &b)
	require.Equal(t, a.Response, b.Response)
	encoded, err := json.Marshal(a.Response)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "withheld_findings", "the field is omitted for an unrestricted caller")
}

// Warnings of an acknowledged or declared break that concern a foreign job
// are withheld and counted too.
func TestEvaluateGraphWithheldWarnings(t *testing.T) {
	now := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	graph := breakingDeclaredDatasetGraph()
	db, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&models.ContractAck{}))

	result, err := EvaluateGraphWithOptions(context.Background(), db, graph, EnforcementModeFail, now, ApplyOptions{
		AllowBreaking:     &AllowBreaking{Dataset: "lake.customers", Actor: "operator@example.com"},
		DeprecationWindow: time.Hour,
		Visible:           hideConsumer,
	})
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	require.Equal(t, 1, result.WithheldWarnings)

	result, err = EvaluateGraphWithOptions(context.Background(), db, graph, EnforcementModeFail, now.Add(time.Minute), ApplyOptions{Visible: hideConsumer})
	require.NoError(t, err)
	require.Empty(t, result.Warnings)
	require.Equal(t, 1, result.WithheldWarnings)
	requireNoForeignText(t, result.Warnings, "consumer", "reporting")

	result, err = EvaluateGraphWithOptions(context.Background(), db, graph, EnforcementModeFail, now.Add(time.Minute), ApplyOptions{})
	require.NoError(t, err)
	require.Len(t, result.Warnings, 1, "an unrestricted caller still gets it")
	require.Zero(t, result.WithheldWarnings)
}
