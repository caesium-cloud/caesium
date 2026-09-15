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

func TestLintServerFlagValueTracksBareNormalization(t *testing.T) {
	flag := lintCmd.Flags().Lookup("server")
	if flag == nil {
		t.Fatal("--server flag is not registered")
	}

	original := lintServer
	t.Cleanup(func() { lintServer = original })

	lintServer = lintBareServerMarker
	if got := flag.Value.String(); got != lintBareServerMarker {
		t.Fatalf("bare --server Value.String() = %q, want marker %q", got, lintBareServerMarker)
	}
	lintServer = defaultLintServer
	if got := flag.Value.String(); got != defaultLintServer {
		t.Fatalf("normalized bare --server Value.String() = %q, want %q", got, defaultLintServer)
	}
}
