package ptr

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOf(t *testing.T) {
	p := Of(7)
	require.NotNil(t, p)
	require.Equal(t, 7, *p)
	*p = 8
	require.Equal(t, 8, *Of(8))
}

func TestClone(t *testing.T) {
	require.Nil(t, Clone[int](nil))

	src := Of(3)
	got := Clone(src)
	require.NotNil(t, got)
	require.Equal(t, 3, *got)
	require.NotSame(t, src, got)
	*got = 4
	require.Equal(t, 3, *src)
}

func TestDeref(t *testing.T) {
	require.Equal(t, 0, Deref[int](nil))
	require.Equal(t, "", Deref[string](nil))
	require.Equal(t, 9, Deref(Of(9)))
}
