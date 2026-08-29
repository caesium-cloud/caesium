package runtime

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterpolateParamRefs_SubstitutesBracedForm(t *testing.T) {
	got, err := InterpolateParamRefs(map[string]string{
		"GIT_REF": "${CAESIUM_PARAM_SHA}",
		"PATH":    "refs/heads/${CAESIUM_PARAM_BRANCH}",
		"MULTI":   "${CAESIUM_PARAM_A}-${CAESIUM_PARAM_B}",
		"LITERAL": "keep-me",
	}, map[string]string{
		"sha":    "abc123",
		"BRANCH": "main",
		"a":      "one",
		"b":      "two",
	})
	require.NoError(t, err)
	assert.Equal(t, "abc123", got["GIT_REF"])
	assert.Equal(t, "refs/heads/main", got["PATH"])
	assert.Equal(t, "one-two", got["MULTI"])
	assert.Equal(t, "keep-me", got["LITERAL"])
}

func TestInterpolateParamRefs_DoesNotMutateInput(t *testing.T) {
	env := map[string]string{"GIT_REF": "${CAESIUM_PARAM_SHA}"}
	got, err := InterpolateParamRefs(env, map[string]string{"SHA": "deadbeef"})
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", got["GIT_REF"])
	assert.Equal(t, "${CAESIUM_PARAM_SHA}", env["GIT_REF"])
}

func TestInterpolateParamRefs_EmptyAndNil(t *testing.T) {
	got, err := InterpolateParamRefs(nil, map[string]string{"SHA": "x"})
	require.NoError(t, err)
	assert.Nil(t, got)

	got, err = InterpolateParamRefs(map[string]string{}, nil)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestInterpolateParamRefs_EmptyParamValueIsPresent(t *testing.T) {
	got, err := InterpolateParamRefs(map[string]string{
		"GIT_REF": "${CAESIUM_PARAM_SHA}",
	}, map[string]string{"SHA": ""})
	require.NoError(t, err)
	assert.Equal(t, "", got["GIT_REF"])
}

func TestInterpolateParamRefs_MissingParamFailsClosed(t *testing.T) {
	got, err := InterpolateParamRefs(map[string]string{
		"GIT_REF": "${CAESIUM_PARAM_SHA}",
		"OK":      "literal",
	}, map[string]string{"OTHER": "x"})
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "GIT_REF")
	assert.Contains(t, err.Error(), "${CAESIUM_PARAM_SHA}")
	assert.NotContains(t, err.Error(), "OK")
}

func TestInterpolateParamRefs_NilParamsWithTokenFails(t *testing.T) {
	got, err := InterpolateParamRefs(map[string]string{
		"GIT_REF": "${CAESIUM_PARAM_SHA}",
	}, nil)
	require.Error(t, err)
	assert.Nil(t, got)
}

func TestInterpolateParamRefs_LeavesNonParamTokens(t *testing.T) {
	env := map[string]string{
		"BARE":    "$CAESIUM_PARAM_SHA",
		"OUTPUT":  "${CAESIUM_OUTPUT_CHECKOUT_COMMIT}",
		"HOME":    "${HOME}",
		"DEFAULT": "${CAESIUM_PARAM_SHA:-main}",
	}
	got, err := InterpolateParamRefs(env, map[string]string{"SHA": "abc"})
	require.NoError(t, err)
	assert.Equal(t, env, got)
}

func TestInterpolateParamRefs_MixedMissingAndPresent(t *testing.T) {
	got, err := InterpolateParamRefs(map[string]string{
		"REF": "${CAESIUM_PARAM_SHA}@${CAESIUM_PARAM_MISSING}",
	}, map[string]string{"SHA": "abc"})
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Contains(t, err.Error(), "MISSING")
	assert.True(t, strings.Contains(err.Error(), "unresolved"), err.Error())
}
