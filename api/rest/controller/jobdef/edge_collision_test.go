package jobdef

import (
	"encoding/json"
	"net/http"
	"testing"

	contractsvc "github.com/caesium-cloud/caesium/api/rest/service/contract"
	"github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	internalcontract "github.com/caesium-cloud/caesium/internal/contract"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// The contract key only a finance consumer requires.
const collisionFinanceKey = "fin_collide_dependency"

// collisionWorld is B5's colliding-edge world with a real breaking finding
// on the foreign side: marketing's producer P serves D to the marketing
// consumer "C:/z" and "z:/D" to finance's consumer "C", which requires a key P
// does not produce. The edge ids P->"C:/z" on "/D" and P->"C" on "/z:/D" are
// the same string, so derivation merges finance's breaking finding into the
// marketing edge (identity-and-access W7-ε).
func collisionWorld() (producer schema.Definition, all []schema.Definition) {
	producer = scopeDefinition("collide-p", "marketing")
	producer.Steps[0].Datasets = &schema.StepDatasets{Produces: []schema.ProducedDataset{
		{Name: "collide-d"},
		{Name: "z:/collide-d", Schema: map[string]any{"type": "object", "properties": map[string]any{"other": map[string]any{"type": "string"}}}},
	}}
	mkt := scopeDefinition("collide-c:/z", "marketing")
	mkt.Steps[0].Datasets = &schema.StepDatasets{Consumes: []schema.ConsumedDataset{{Name: "collide-d"}}}
	fin := scopeDefinition("collide-c", "finance")
	fin.Steps[0].Datasets = &schema.StepDatasets{Consumes: []schema.ConsumedDataset{{Name: "z:/collide-d", Schema: map[string]any{
		"type":       "object",
		"required":   []any{collisionFinanceKey},
		"properties": map[string]any{collisionFinanceKey: map[string]any{"type": "string"}},
	}}}}
	return producer, []schema.Definition{producer, mkt, fin}
}

func enforceContracts(t *testing.T) {
	t.Helper()
	// Registered before Setenv so it runs after Setenv restores the
	// variable (cleanups run last-in first-out): later tests must not inherit
	// the enforcement mode.
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_CONTRACT_ENFORCEMENT", "fail")
	require.NoError(t, env.Process())
}

func TestConfinedLintDiffApplyWithholdCollidingForeignFinding(t *testing.T) {
	producer, all := collisionWorld()
	conn := freshCatalog(t, all...)
	enforceContracts(t)

	// Precondition: the merged edge shows two marketing endpoints and carries
	// finance's finding, which an unrestricted lint and diff report (their nil
	// scope returns the derived graph unchanged; the handlers derive it from
	// the process database, so it is read here directly).
	derived, err := contractsvc.NewWithDatabase(t.Context(), conn).Graph("", nil)
	require.NoError(t, err)
	graph := *derived
	unrestricted, err := json.Marshal(contractsvc.SummaryFromGraph((*readScope)(nil).filterGraph(t.Context(), graph)))
	require.NoError(t, err)
	require.Contains(t, string(unrestricted), collisionFinanceKey)
	var merged internalcontract.Edge
	for _, edge := range graph.Edges {
		if edge.From == internalcontract.JobNodeID("collide-p") && edge.To == internalcontract.JobNodeID("collide-c:/z") {
			merged = edge
		}
	}
	require.Len(t, merged.Contributors(), 2, "the edge ids collide: %+v", graph.Edges)

	modified := producer
	modified.Steps = append([]schema.Step(nil), producer.Steps...)
	modified.Steps[0].Command = []string{"true", "changed"}

	mktOperator := confined(policy.Grants{"marketing": models.RoleOperator})
	for _, tc := range []struct {
		name      string
		principal *auth.Principal
		visible   bool
	}{
		{"auth off", nil, true},
		{"cluster operator", clusterOperator(), true},
		{"marketing operator", mktOperator, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check := func(surface, body string) {
				t.Helper()
				if tc.visible {
					require.Contains(t, body, collisionFinanceKey, "%s: the unrestricted caller reads the finding", surface)
				} else {
					require.NotContains(t, body, collisionFinanceKey, "%s: a foreign contributor's finding never rides a visible edge", surface)
				}
			}

			status, body, err := callApply(t, tc.principal, applyBody(false, producer))
			require.NoError(t, err)
			require.Equal(t, http.StatusConflict, status, body)
			check("apply", body)
			var refused internalcontract.EnforcementResponse
			require.NoError(t, json.Unmarshal([]byte(body), &refused))
			if tc.visible {
				require.NotEmpty(t, refused.Findings, body)
				require.Zero(t, refused.WithheldFindings, body)
				return
			}
			require.Empty(t, refused.Findings, body)
			require.Positive(t, refused.WithheldFindings, body)
			require.Contains(t, refused.Message, internalcontract.ForeignContractMessage)

			// Lint and diff of a confined caller.
			status, body = callJobdef(t, Lint, tc.principal, LintRequest{Definitions: []schema.Definition{producer}})
			require.Equal(t, http.StatusOK, status, body)
			check("lint", body)
			var lint struct {
				Contracts struct {
					Breaking []json.RawMessage `json:"breaking"`
				} `json:"contracts"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &lint))
			require.Empty(t, lint.Contracts.Breaking, body)

			status, body = callJobdef(t, Diff, tc.principal, DiffRequest{Definitions: []schema.Definition{modified}})
			require.Equal(t, http.StatusOK, status, body)
			var diff DiffResponse
			require.NoError(t, json.Unmarshal([]byte(body), &diff))
			require.Len(t, diff.Modified, 1, body)
			check("diff", body)
		})
	}
}
