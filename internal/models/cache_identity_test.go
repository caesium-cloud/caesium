package models

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestFrozenImageIdentityChecksCompleteClosure(t *testing.T) {
	root := TaskExecutionDescriptor{Baseline: TaskExecutionBaseline{TaskID: uuid.New()}}
	leaf := TaskExecutionDescriptor{Baseline: TaskExecutionBaseline{TaskID: uuid.New()}, DAG: TaskExecutionDAG{Predecessors: []TaskExecutionEdgeRef{{TaskID: root.Baseline.TaskID}}}}
	flag := FrozenImageIdentityChecks([]TaskExecutionDescriptor{root, leaf})
	require.NotNil(t, flag)
	require.False(t, *flag)
	require.Nil(t, FrozenImageIdentityChecks([]TaskExecutionDescriptor{leaf}))
	incomplete := root
	incomplete.DAG.OutstandingPredecessors = 1
	require.Nil(t, FrozenImageIdentityChecks([]TaskExecutionDescriptor{incomplete}))
	root.Cache.PinDigests = true
	flag = FrozenImageIdentityChecks([]TaskExecutionDescriptor{root, leaf})
	require.False(t, *flag, "disabled pinning cannot introduce uncertainty")
	root.Cache.Enabled = true
	flag = FrozenImageIdentityChecks([]TaskExecutionDescriptor{root, leaf})
	require.True(t, *flag)
}
