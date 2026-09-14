package jobdef

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseRejectsUnknownStructuralFieldsWithPathAndLine(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: strict-fields
  schemaValidaton: fail
trigger:
  type: http
  configuration: {path: strict-fields}
steps:
  - name: extract
    image: alpine:3.23
    dependsON: load
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path metadata.schemaValidaton (line 5): unknown field "schemaValidaton"`)
	require.Contains(t, err.Error(), `YAML path steps[0].dependsON (line 12): unknown field "dependsON"`)
}

func TestParseKeepsFreeFormMapsAndCustomDatasetShapes(t *testing.T) {
	def, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: strict-free-maps
  annotations:
    arbitrary.example/key: value
trigger:
  type: http
  configuration:
    path: strict-free-maps
    providerSpecific:
      nested: true
steps:
  - name: extract
    image: alpine:3.23
    env:
      ARBITRARY_NAME: value
    outputSchema:
      type: object
      properties:
        arbitrary:
          type: string
    datasets:
      consumes:
        - source.dataset
        - name: object.dataset
          schema:
            type: object
            properties:
              arbitrary:
                vendorExtension: true
`))
	require.NoError(t, err)
	require.Equal(t, "value", def.Steps[0].Env["ARBITRARY_NAME"])
	require.Len(t, def.Steps[0].Datasets.Consumes, 2)
}

func TestParseRejectsUnknownFieldInCustomDatasetMapping(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata: {alias: strict-custom}
trigger: {type: http, configuration: {path: strict-custom}}
steps:
  - name: extract
    image: alpine:3.23
    datasets:
      consumes:
        - name: source.dataset
          schemas: {type: object}
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path steps[0].datasets.consumes[0].schemas (line 11)`)
}

func TestParseRejectsUnknownMergedField(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  <<: &defaults
    schemaValidaton: fail
  alias: strict-merge
trigger: {type: http, configuration: {path: strict-merge}}
steps: [{name: extract, image: alpine:3.23}]
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path metadata.schemaValidaton (line 5)`)
}

func TestParseTreatsQuotedMergeKeyAsStructuralField(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: quoted-merge
  "<<": {schemaValidation: fail}
trigger: {type: http, configuration: {path: quoted-merge}}
steps: [{name: extract, image: alpine:3.23}]
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path metadata.<< (line 5): unknown field "<<"`)
}

func TestParseRejectsUnknownCacheFieldsAtMetadataAndStep(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: strict-cache
  cache:
    enabeld: false
trigger: {type: http, configuration: {path: strict-cache}}
steps:
  - name: extract
    image: alpine:3.23
    cache:
      pinDigest: true
`))
	require.Error(t, err)
	require.Contains(t, err.Error(), `YAML path metadata.cache.enabeld (line 6)`)
	require.Contains(t, err.Error(), `YAML path steps[0].cache.pinDigest (line 12)`)
}

func TestParseAcceptsBooleanAndKnownMapCacheForms(t *testing.T) {
	_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: strict-cache-valid
  cache: true
trigger: {type: http, configuration: {path: strict-cache-valid}}
steps:
  - name: extract
    image: alpine:3.23
    cache: {ttl: never, chain: values, version: 2, pinDigests: true, digestTTL: 0}
`))
	require.NoError(t, err)
}

func TestParseRejectsRecursiveCacheAliasesWithoutRecursingForever(t *testing.T) {
	for _, merge := range []string{"*cache", "[*cache]"} {
		_, err := Parse([]byte(`apiVersion: v1
kind: Job
metadata:
  alias: strict-cache-cycle
  cache: &cache
    <<: ` + merge + `
trigger: {type: http, configuration: {path: strict-cache-cycle}}
steps: [{name: extract, image: alpine:3.23}]
`))
		require.Error(t, err)
		require.Contains(t, err.Error(), "contains itself")
	}
}
