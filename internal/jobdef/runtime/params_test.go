package runtime

import (
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestBuildRunParamEnvBaseAndNormalization(t *testing.T) {
	id := uuid.New()
	for _, params := range []map[string]string{nil, {}, {"branch": "main", "Mixed_Key": "value"}} {
		got := BuildRunParamEnv(id, "job", params)
		require.NotNil(t, got)
		require.Equal(t, id.String(), got["CAESIUM_RUN_ID"])
		require.Equal(t, "job", got["CAESIUM_JOB_ALIAS"])
		if len(params) > 0 {
			require.Equal(t, "main", got["CAESIUM_PARAM_BRANCH"])
			require.Equal(t, "value", got["CAESIUM_PARAM_MIXED_KEY"])
		} else {
			require.Len(t, got, 2)
		}
	}
	got := BuildRunParamEnv(id, "job", map[string]string{"key": "lower", "KEY": "upper"})
	require.Contains(t, []string{"lower", "upper"}, got["CAESIUM_PARAM_KEY"])
	require.Len(t, got, 3)
}
