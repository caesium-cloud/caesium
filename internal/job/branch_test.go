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
