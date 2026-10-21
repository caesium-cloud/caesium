package job

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A confined writer's legacy create may only wire dependencies between the
// tasks it creates (PR #668 review r9): any other task, existing or not, or
// an unparsable reference, is foreign.
func TestRequireOwnDependencies(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	created := []string{a.String(), b.String()}

	require.NoError(t, requireOwnDependencies(created, nil))
	require.NoError(t, requireOwnDependencies(created, []edgeSpec{{from: a.String(), to: b.String()}}))
	require.NoError(t, requireOwnDependencies(created, []edgeSpec{{from: "{" + a.String() + "}", to: b.String()}}), "compared as UUIDs")

	for _, from := range []string{uuid.NewString(), "not-a-uuid", ""} {
		err := requireOwnDependencies(created, []edgeSpec{{from: a.String(), to: b.String()}, {from: from, to: b.String()}})
		require.ErrorIs(t, err, errForeignDependency, "from %q", from)
		require.Equal(t, errForeignDependency.Error(), err.Error(), "the refusal names no task")
	}
	require.ErrorIs(t, requireOwnDependencies(nil, []edgeSpec{{from: a.String(), to: b.String()}}), errForeignDependency)
}
