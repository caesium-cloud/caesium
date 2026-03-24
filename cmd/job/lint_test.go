package job

import (
	"testing"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestContractSummarySortsContractsDeterministically(t *testing.T) {
	t.Parallel()

	steps := []jobdef.Step{
		{
			Name: "consume-b",
			InputSchema: map[string]map[string]any{
				"produce-z": {
					"required": []any{"beta", "alpha"},
				},
			},
		},
		{
			Name: "consume-a",
			InputSchema: map[string]map[string]any{
				"produce-a": {
					"required": []any{"delta", "charlie"},
				},
			},
		},
	}

	require.Equal(
		t,
		"2 data contracts (produce-a → consume-a: charlie, delta; produce-z → consume-b: alpha, beta)",
		contractSummary(steps),
	)
}
