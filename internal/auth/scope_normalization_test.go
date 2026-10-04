package auth

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestJobAliasNormalizationPreservesEmptyScopeSemantics(t *testing.T) {
	require.NotNil(t, normalizeJobAliases(nil))
	require.Nil(t, normalizeAllowlist(nil))
	require.Nil(t, normalizeAllowlist([]string{}))
	require.NotNil(t, normalizeAllowlist([]string{" "}))
	require.Equal(t, []string{"a", "z"}, normalizeJobAliases([]string{" z ", "a", "z", " "}))
	for _, jobs := range [][]string{nil, {}, {" "}, {" z ", "a", "z"}} {
		payload, err := json.Marshal(models.KeyScope{Jobs: jobs, Agent: &models.AgentClaim{IncidentID: uuid.New(), Jobs: jobs}})
		require.NoError(t, err)
		scope, err := DecodeScope(payload)
		require.NoError(t, err)
		claim, err := DecodeAgentClaim(payload)
		require.NoError(t, err)
		require.NotNil(t, claim)
		require.NotNil(t, claim.Jobs)
		if len(normalizeJobAliases(jobs)) == 0 {
			require.Nil(t, scope)
			require.Empty(t, claim.Jobs)
		} else {
			require.Equal(t, []string{"a", "z"}, scope.Jobs)
			require.Equal(t, scope.Jobs, claim.Jobs)
		}
	}
}
