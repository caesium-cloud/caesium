package job

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseBranchSelection_ValidNames(t *testing.T) {
	logs := strings.NewReader("##caesium::branch fast-path\n##caesium::branch slow-path\n")
	selected, err := parseBranchSelection(logs, []string{"fast-path", "slow-path", "skip-path"})
	require.NoError(t, err)
	assert.True(t, selected["fast-path"])
	assert.True(t, selected["slow-path"])
	assert.False(t, selected["skip-path"])
}

func TestParseBranchSelection_InvalidName(t *testing.T) {
	logs := strings.NewReader("##caesium::branch nonexistent\n")
	_, err := parseBranchSelection(logs, []string{"fast-path", "slow-path"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown step")
	assert.Contains(t, err.Error(), "nonexistent")
}

func TestParseBranchSelection_EmptyOutput(t *testing.T) {
	logs := strings.NewReader("some log output\n")
	selected, err := parseBranchSelection(logs, []string{"fast-path", "slow-path"})
	require.NoError(t, err)
	assert.Empty(t, selected)
}

func TestParseBranchSelection_SingleBranch(t *testing.T) {
	logs := strings.NewReader("##caesium::branch fast-path\n")
	selected, err := parseBranchSelection(logs, []string{"fast-path", "slow-path"})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"fast-path": true}, selected)
}

func TestValidateBranchSelection_Valid(t *testing.T) {
	selected, err := validateBranchSelection([]string{"a", "b"}, []string{"a", "b", "c"})
	require.NoError(t, err)
	assert.True(t, selected["a"])
	assert.True(t, selected["b"])
	assert.False(t, selected["c"])
}

func TestValidateBranchSelection_InvalidName(t *testing.T) {
	_, err := validateBranchSelection([]string{"bogus"}, []string{"a", "b"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown step")
	assert.Contains(t, err.Error(), "bogus")
}

func TestValidateBranchSelection_Empty(t *testing.T) {
	selected, err := validateBranchSelection(nil, []string{"a", "b"})
	require.NoError(t, err)
	assert.Empty(t, selected)
}
