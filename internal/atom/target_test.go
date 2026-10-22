package atom

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKubeTarget(t *testing.T) {
	require.True(t, KubeTarget{}.IsZero())
	require.Nil(t, KubeTarget{}.Ptr())
	target := KubeTarget{ServiceAccountName: "jobs"}
	require.False(t, target.IsZero())
	ptr := target.Ptr()
	require.Equal(t, &target, ptr)
	ptr.Namespace = "changed"
	require.Empty(t, target.Namespace, "Ptr returns a copy")
}
