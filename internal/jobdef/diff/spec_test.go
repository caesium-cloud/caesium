package diff

import (
	"os"
	"path/filepath"
	"testing"

	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

func TestLoadDefinitionsFromFile(t *testing.T) {
	content := `apiVersion: v1
kind: Job
metadata:
  alias: a
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
- name: step
  image: alpine
`

	dir := t.TempDir()
	path := filepath.Join(dir, "job.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))

	specs, err := LoadDefinitions([]string{path})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	spec := specs["a"]
	require.Equal(t, "a", spec.Alias)
	require.Equal(t, "cron", spec.Trigger.Type)
	require.Len(t, spec.Steps, 1)
}

func TestLoadDefinitionsRejectsDuplicates(t *testing.T) {
	content := `apiVersion: v1
kind: Job
metadata:
  alias: a
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
- name: step
  image: alpine
`
	dir := t.TempDir()
	p1 := filepath.Join(dir, "one.yaml")
	p2 := filepath.Join(dir, "two.yaml")
	require.NoError(t, os.WriteFile(p1, []byte(content), 0o644))
	require.NoError(t, os.WriteFile(p2, []byte(content), 0o644))

	_, err := LoadDefinitions([]string{p1, p2})
	require.Error(t, err)
}

func TestIsBlankDefinition(t *testing.T) {
	blank := &schema.Definition{}
	require.True(t, isBlankDefinition(blank))

	def := &schema.Definition{Metadata: schema.Metadata{Alias: "job"}}
	require.False(t, isBlankDefinition(def))
}
