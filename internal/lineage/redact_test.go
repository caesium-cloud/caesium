package lineage

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func confinedRedactor(t *testing.T, namespaces ...string) *authz.Redactor {
	t.Helper()
	r := authz.NewRedactor(authz.WithAllowedNamespaces(context.Background(), namespaces))
	require.NotNil(t, r)
	return r
}

func TestStoredImpactKeepsOwnersForRedaction(t *testing.T) {
	raw, err := EncodeStoredImpact(&ImpactResult{RootName: "orders", Downstream: []ImpactNode{
		{DatasetName: "own", JobID: uuid.New(), JobNamespace: "finance", RunNamespace: "finance"},
		{DatasetName: "foreign", JobID: uuid.New(), JobNamespace: "marketing", RunNamespace: "marketing"},
		{DatasetName: "moved", JobID: uuid.New(), JobNamespace: "finance", RunNamespace: "marketing", Depth: 1},
	}})
	require.NoError(t, err)

	got, err := RedactStoredImpact(confinedRedactor(t, "finance"), raw)
	require.NoError(t, err)
	require.Equal(t, "orders", got.RootName)
	require.Len(t, got.Downstream, 3)
	require.Equal(t, "own", got.Downstream[0].(ImpactNode).DatasetName)
	require.IsType(t, ExternalImpactNode{}, got.Downstream[1])
	require.IsType(t, ExternalImpactNode{}, got.Downstream[2])

	body, err := json.Marshal(got)
	require.NoError(t, err)
	require.NotContains(t, string(body), "foreign")
	require.NotContains(t, string(body), "moved")
	require.NotContains(t, string(body), "marketing")
}

// The stored form must stay readable as the ImpactResult wire shape: the hold
// routes serve it verbatim.
func TestStoredImpactIsAnImpactResult(t *testing.T) {
	raw, err := EncodeStoredImpact(&ImpactResult{RootNamespace: "ol", RootName: "orders", Downstream: []ImpactNode{
		{DatasetName: "own", JobAlias: "rollup", JobNamespace: "finance", RunNamespace: "finance", Depth: 2},
	}})
	require.NoError(t, err)

	var wire ImpactResult
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Equal(t, "ol", wire.RootNamespace)
	require.Len(t, wire.Downstream, 1)
	require.Equal(t, "rollup", wire.Downstream[0].JobAlias)
	require.Equal(t, 2, wire.Downstream[0].Depth)
}

func TestStoredImpactNormalisesLegacyOwners(t *testing.T) {
	raw, err := EncodeStoredImpact(&ImpactResult{Downstream: []ImpactNode{{DatasetName: "legacy-run"}}})
	require.NoError(t, err)

	got, err := RedactStoredImpact(confinedRedactor(t, "default"), raw)
	require.NoError(t, err)
	require.IsType(t, ImpactNode{}, got.Downstream[0], "a blank owner the query returned is the default namespace")
}

func TestRedactStoredImpactTreatsMissingOwnersAsForeign(t *testing.T) {
	raw, err := json.Marshal(ImpactResult{Downstream: []ImpactNode{{DatasetName: "unknown-owner"}}})
	require.NoError(t, err)

	got, err := RedactStoredImpact(confinedRedactor(t, "default"), raw)
	require.NoError(t, err)
	require.IsType(t, ExternalImpactNode{}, got.Downstream[0])

	unrestricted, err := RedactStoredImpact(nil, raw)
	require.NoError(t, err)
	require.IsType(t, ImpactNode{}, unrestricted.Downstream[0])
}

func TestRedactStoredImpactRefusesACorruptCone(t *testing.T) {
	_, err := RedactStoredImpact(confinedRedactor(t, "default"), []byte("{"))
	require.Error(t, err)
}
