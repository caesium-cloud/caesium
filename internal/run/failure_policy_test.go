package run

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestNormalizeTaskFailurePolicy(t *testing.T) {
	for _, tc := range []struct{ value, want string }{{"", "halt"}, {"unknown", "halt"}, {"halt", "halt"}, {" HALT ", "halt"}, {"continue", "continue"}, {" Continue ", "continue"}} {
		require.Equal(t, tc.want, NormalizeTaskFailurePolicy(tc.value))
	}
}
