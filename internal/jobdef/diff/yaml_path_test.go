package diff

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadDefinitionsAcceptsUppercaseYMLAndIgnoresOtherExtensions(t *testing.T) {
	dir := t.TempDir()
	valid := `apiVersion: v1
kind: Job
metadata:
  alias: uppercase
trigger:
  type: cron
  configuration:
    cron: "* * * * *"
steps:
- name: step
  image: alpine:3.23
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "job.YML"), []byte(valid), 0o600))
	for _, name := range []string{"broken.yaml.json", "broken.txt", "broken"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("not a job definition"), 0o600))
	}
	specs, err := LoadDefinitions([]string{dir})
	require.NoError(t, err)
	require.Len(t, specs, 1)
	require.Equal(t, "uppercase", specs["uppercase"].Alias)
}
