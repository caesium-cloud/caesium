package auth

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type aliasNormalizationCase struct {
	name         string
	input        []string
	want         []string
	allowlistNil bool
}

func aliasNormalizationCases() []aliasNormalizationCase {
	return []aliasNormalizationCase{
		{name: "nil", input: nil, want: []string{}, allowlistNil: true},
		{name: "empty", input: []string{}, want: []string{}, allowlistNil: true},
		{name: "blank", input: []string{" ", "\t\n"}, want: []string{}},
		{name: "trim_deduplicate_sort", input: []string{" z ", "a", "z", " ", " a "}, want: []string{"a", "z"}},
		{name: "case_sensitive", input: []string{" b ", "A", "a", "B", " A "}, want: []string{"A", "B", "a", "b"}},
	}
}

func TestCanonicalJobAliasNormalizationHasIndependentResults(t *testing.T) {
	for _, tc := range aliasNormalizationCases() {
		t.Run(tc.name, func(t *testing.T) {
			var original []string
			if tc.input != nil {
				original = make([]string, len(tc.input))
				copy(original, tc.input)
			}
			allowlist := normalizeAllowlist(tc.input)
			if tc.allowlistNil {
				require.Nil(t, allowlist)
			} else {
				require.NotNil(t, allowlist)
				require.Equal(t, tc.want, allowlist)
			}
			require.Equal(t, original, tc.input, "canonical normalization must not mutate input")

			jobs := normalizeJobAliases(tc.input)
			require.NotNil(t, jobs)
			require.Equal(t, tc.want, jobs)
			require.Equal(t, original, tc.input, "scope normalization must not mutate input")
			if len(jobs) > 0 {
				jobs[0] = "changed scope result"
				allowlist[0] = "changed allowlist result"
				require.Equal(t, original, tc.input, "normalized results must not alias input")
			}
		})
	}
}

func TestJobAliasNormalizationPreservesEmptyScopeSemantics(t *testing.T) {
	for _, tc := range aliasNormalizationCases() {
		t.Run(tc.name, func(t *testing.T) {
			incidentID := uuid.New()
			payload, err := json.Marshal(models.KeyScope{Jobs: tc.input, Agent: &models.AgentClaim{IncidentID: incidentID, Jobs: tc.input}})
			require.NoError(t, err)
			scope, err := DecodeScope(payload)
			require.NoError(t, err)
			claim, err := DecodeAgentClaim(payload)
			require.NoError(t, err)
			require.NotNil(t, claim)
			require.Equal(t, incidentID, claim.IncidentID)
			require.NotNil(t, claim.Jobs)
			require.Equal(t, tc.want, claim.Jobs)
			if len(tc.want) == 0 {
				require.Nil(t, scope)
			} else {
				require.NotNil(t, scope)
				require.Equal(t, tc.want, scope.Jobs)
			}
		})
	}
}
