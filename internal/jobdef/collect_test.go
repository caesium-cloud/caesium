package jobdef

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCollectDefinitions_ValidCaesiumJob(t *testing.T) {
	dir := writeTestFile(t, "job.yaml", `
apiVersion: v1
kind: Job
metadata:
  alias: test-job
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: step-one
    image: alpine
    command: ["echo", "hello"]
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.NoError(t, err)
	assert.Len(t, defs, 1)
	assert.Equal(t, "test-job", defs[0].Metadata.Alias)
}

func TestCollectDefinitions_SkipsNonCaesiumYAML(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "Chart.yaml", `
apiVersion: v2
name: my-chart
description: A Helm chart
version: 0.1.0
`)
	writeFile(t, dir, "job.yaml", `
apiVersion: v1
kind: Job
metadata:
  alias: real-job
trigger:
  type: cron
  configuration:
    expression: "*/5 * * * *"
steps:
  - name: greet
    image: alpine
    command: ["echo", "hi"]
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.NoError(t, err)
	assert.Len(t, defs, 1)
	assert.Equal(t, "real-job", defs[0].Metadata.Alias)
}

func TestCollectDefinitions_SurfacesMalformedYAML(t *testing.T) {
	dir := writeTestFile(t, "broken.yaml", `
apiVersion: v1
kind: Job
metadata:
  alias: broken-job
steps:
  - name: bad
    image: alpine
    command: [unclosed bracket
`)
	_, err := CollectDefinitions([]string{dir}, false)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "broken.yaml")
}

func TestCollectDefinitions_SurfacesBadIndentation(t *testing.T) {
	// YAML where 'alias' is a sibling of 'metadata' rather than nested
	// under it. This is valid YAML but not a valid Caesium definition, so
	// it should either error (validate=true) or be skipped — never produce
	// a definition with the wrong alias.
	dir := writeTestFile(t, "indent.yaml", `
apiVersion: v1
kind: Job
metadata:
alias: no-indent
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	if err != nil {
		return // surfaced as error — acceptable
	}
	// If no error, the definition should not have captured "no-indent" as the alias.
	for _, d := range defs {
		assert.NotEqual(t, "no-indent", d.Metadata.Alias,
			"bad indentation should not produce a definition with the wrong alias")
	}
}

func TestCollectDefinitions_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	defs, err := CollectDefinitions([]string{dir}, false)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestCollectDefinitions_BlankYAMLSkipped(t *testing.T) {
	dir := writeTestFile(t, "empty.yaml", `
---
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.NoError(t, err)
	assert.Empty(t, defs)
}

func TestIsYAML(t *testing.T) {
	assert.True(t, IsYAML("foo.yaml"))
	assert.True(t, IsYAML("bar.yml"))
	assert.True(t, IsYAML("BAZ.YAML"))
	assert.False(t, IsYAML("foo.json"))
	assert.False(t, IsYAML("foo.go"))
}

// writeTestFile creates a temp directory with a single file and returns the dir path.
func writeTestFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, dir, name, content)
	return dir
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}
