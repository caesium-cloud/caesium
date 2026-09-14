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
    image: alpine:3.23
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
    image: alpine:3.23
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
    image: alpine:3.23
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

func TestCollectDefinitions_SurfacesKnownJobTypedDecodeErrors(t *testing.T) {
	dir := writeTestFile(t, "bad.job.yaml", `
apiVersion: v1
kind: Job
metadata:
  alias: bad-duration
  taskTimeout: banana
trigger: {type: http, configuration: {path: bad-duration}}
steps:
  - name: extract
    image: alpine:3.23
    command: echo hello
`)
	_, err := CollectDefinitions([]string{dir}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad.job.yaml")
	require.Contains(t, err.Error(), "cannot unmarshal")
}

func TestCollectDefinitions_MixedDirectoryFailsOnInvalidKnownJob(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a-valid.job.yaml", `
apiVersion: v1
kind: Job
metadata: {alias: valid}
trigger: {type: http, configuration: {path: valid}}
steps: [{name: run, image: alpine:3.23}]
`)
	writeFile(t, dir, "b-invalid.job.yaml", `
apiVersion: v1
kind: Job
metadata: {alias: invalid, taskTimeout: banana}
trigger: {type: http, configuration: {path: invalid}}
steps: [{name: run, image: alpine:3.23}]
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.Error(t, err)
	require.Nil(t, defs)
	require.Contains(t, err.Error(), "b-invalid.job.yaml")
}

func TestCollectDefinitions_MultiDocumentFailsOnInvalidKnownJob(t *testing.T) {
	dir := writeTestFile(t, "mixed.job.yaml", `
apiVersion: v1
kind: Job
metadata: {alias: valid}
trigger: {type: http, configuration: {path: valid}}
steps: [{name: run, image: alpine:3.23}]
---
apiVersion: v1
kind: Job
metadata: {alias: invalid}
trigger: {type: http, configuration: {path: invalid}}
steps: [{name: run, image: alpine:3.23, command: echo hello}]
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.Error(t, err)
	require.Nil(t, defs)
	require.Contains(t, err.Error(), "mixed.job.yaml")
}

func TestCollectDefinitions_SkipsKubernetesJob(t *testing.T) {
	dir := writeTestFile(t, "kubernetes.yaml", `
apiVersion: batch/v1
kind: Job
metadata:
  name: kubernetes-job
spec:
  template: {}
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.NoError(t, err)
	require.Empty(t, defs)
}

func TestCollectDefinitions_RecognizesAliasedAndMergedJobHeaders(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "aliases.yaml", `
apiVersion: &version v1
kind: &kind Job
metadata: {alias: aliased-header}
trigger: {type: http, configuration: {path: aliased-header}}
steps: [{name: run, image: alpine:3.23}]
`)
	writeFile(t, dir, "merged.yaml", `
<<: &job-header
  apiVersion: v1
  kind: Job
metadata: {alias: merged-header}
trigger: {type: http, configuration: {path: merged-header}}
steps: [{name: run, image: alpine:3.23}]
`)
	defs, err := CollectDefinitions([]string{dir}, true)
	require.NoError(t, err)
	require.Len(t, defs, 2)
	require.Equal(t, "aliased-header", defs[0].Metadata.Alias)
	require.Equal(t, "merged-header", defs[1].Metadata.Alias)
}

func TestCollectDefinitions_JobSuffixFailsClosedOnMissingOrMisspelledKind(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "a-valid.yaml", `
apiVersion: v1
kind: Job
metadata: {alias: valid}
trigger: {type: http, configuration: {path: valid}}
steps: [{name: run, image: alpine:3.23}]
`)
	writeFile(t, dir, "b-invalid.job.yaml", `
apiVersion: v1
knd: Job
metadata: {alias: invalid}
trigger: {type: http, configuration: {path: invalid}}
steps: [{name: run, image: alpine:3.23}]
`)
	defs, err := CollectDefinitions([]string{dir}, false)
	require.Error(t, err)
	require.Nil(t, defs)
	require.Contains(t, err.Error(), "b-invalid.job.yaml")
	require.Contains(t, err.Error(), "YAML path knd")
}

func TestCollectDefinitions_DuplicateKindKeysFail(t *testing.T) {
	dir := writeTestFile(t, "duplicate.yaml", `
apiVersion: v1
kind: ConfigMap
kind: Job
metadata: {alias: duplicate-kind}
`)
	_, err := CollectDefinitions([]string{dir}, false)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mapping key \"kind\" already defined")
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
