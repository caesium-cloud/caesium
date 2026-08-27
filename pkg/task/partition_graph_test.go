package task

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidatePartitionGraph_EmptyDeps(t *testing.T) {
	g, err := ValidatePartitionGraph([]Partition{{Key: "a"}, {Key: "b"}})
	require.NoError(t, err)
	assert.Equal(t, 0, g.Indegree["a"])
	assert.Equal(t, 0, g.Indegree["b"])
	assert.Empty(t, g.Dependents)
}

func TestValidatePartitionGraph_Chain(t *testing.T) {
	g, err := ValidatePartitionGraph([]Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"b"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 0, g.Indegree["a"])
	assert.Equal(t, 1, g.Indegree["b"])
	assert.Equal(t, 1, g.Indegree["c"])
	assert.Equal(t, []string{"b"}, g.Dependents["a"])
	assert.Equal(t, []string{"c"}, g.Dependents["b"])
	assert.Equal(t, []string{"a", "b", "c"}, g.Order)
	assert.Equal(t, 2, g.MaxDepth)
}

func TestValidatePartitionGraph_Diamond(t *testing.T) {
	g, err := ValidatePartitionGraph([]Partition{
		{Key: "a"},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"a"}},
		{Key: "d", DependsOn: []string{"b", "c"}},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, g.Indegree["d"])
	assert.ElementsMatch(t, []string{"b", "c"}, g.Dependents["a"])
	assert.Equal(t, []string{"d"}, g.Dependents["b"])
	assert.Equal(t, []string{"d"}, g.Dependents["c"])
}

func TestValidatePartitionGraph_SelfCycle(t *testing.T) {
	_, err := ValidatePartitionGraph([]Partition{{Key: "a", DependsOn: []string{"a"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), "itself")
}

func TestValidatePartitionGraph_TwoCycle(t *testing.T) {
	_, err := ValidatePartitionGraph([]Partition{
		{Key: "a", DependsOn: []string{"b"}},
		{Key: "b", DependsOn: []string{"a"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), `"b"`)
}

func TestValidatePartitionGraph_LongCycle(t *testing.T) {
	_, err := ValidatePartitionGraph([]Partition{
		{Key: "a", DependsOn: []string{"c"}},
		{Key: "b", DependsOn: []string{"a"}},
		{Key: "c", DependsOn: []string{"b"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cycle")
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), `"b"`)
	assert.Contains(t, err.Error(), `"c"`)
}

func TestValidatePartitionGraph_DanglingKey(t *testing.T) {
	_, err := ValidatePartitionGraph([]Partition{
		{Key: "a", DependsOn: []string{"missing"}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"a"`)
	assert.Contains(t, err.Error(), `"missing"`)
}

func TestValidatePartitionGraph_FanOfDepth1(t *testing.T) {
	parts := make([]Partition, 0, 1024)
	parts = append(parts, Partition{Key: "root"})
	for i := 0; i < 1023; i++ {
		parts = append(parts, Partition{Key: fmt.Sprintf("p%d", i), DependsOn: []string{"root"}})
	}
	g, err := ValidatePartitionGraph(parts)
	require.NoError(t, err)
	assert.Equal(t, 0, g.Indegree["root"])
	assert.Equal(t, 1, g.Indegree["p0"])
	assert.Len(t, g.Dependents["root"], 1023)
	assert.Equal(t, 1, g.MaxDepth)
}

func TestValidatePartitionGraph_DeepChain(t *testing.T) {
	n := 64
	parts := make([]Partition, n)
	parts[0] = Partition{Key: "n0"}
	for i := 1; i < n; i++ {
		parts[i] = Partition{Key: fmt.Sprintf("n%d", i), DependsOn: []string{fmt.Sprintf("n%d", i-1)}}
	}
	g, err := ValidatePartitionGraph(parts)
	require.NoError(t, err)
	assert.Equal(t, n-1, g.MaxDepth)
	assert.Equal(t, 0, g.Indegree["n0"])
	assert.Equal(t, 1, g.Indegree[fmt.Sprintf("n%d", n-1)])
}
