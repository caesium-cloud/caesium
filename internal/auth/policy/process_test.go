package policy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessHolderDefaultsToEmptyPolicy(t *testing.T) {
	t.Cleanup(func() { SetProcessHolder(nil) })
	SetProcessHolder(nil)

	h := ProcessHolder()
	require.Nil(t, h)
	require.False(t, h.FileBacked())
	_, declared := h.Namespace(DefaultNamespace)
	require.True(t, declared, "the default namespace is always declared")
	_, declared = h.Namespace("marketing")
	require.False(t, declared)
}

func TestProcessHolderReturnsInstalledHolder(t *testing.T) {
	t.Cleanup(func() { SetProcessHolder(nil) })
	view := EmptyPolicy()
	view.Namespaces["marketing"] = Namespace{Quotas: &Quotas{MaxConcurrentRuns: 2}}
	installed := NewEnvHolder(view, "", nil)

	SetProcessHolder(installed)

	require.Same(t, installed, ProcessHolder())
	ns, declared := ProcessHolder().Namespace("marketing")
	require.True(t, declared)
	require.Equal(t, 2, ns.Quotas.MaxConcurrentRuns)
}
