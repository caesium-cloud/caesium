package jobdef

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// minimalDef is a template for building valid job definitions in tests.
const minimalDef = `
apiVersion: v1
kind: Job
metadata:
  alias: test-job
trigger:
  type: http
  configuration: {}
steps:
`

func TestValidateOutputSchema(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid output schema",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        file_path: { type: string }
      required: [row_count, file_path]
`,
		},
		{
			name: "no schema fields - backwards compat",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
`,
		},
		{
			name: "empty output schema (no properties)",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
    outputSchema:
      type: object
`,
		},
		{
			name: "invalid output schema - bad type value",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
    outputSchema:
      type: not-a-valid-type
`,
			wantErr: `step "extract": invalid outputSchema`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestValidateInputSchema(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "valid input schema referencing predecessor",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        file_path: { type: string }
      required: [row_count, file_path]
  - name: transform
    image: etl:latest
    dependsOn: [extract]
    inputSchema:
      extract:
        required: [file_path]
`,
		},
		{
			name: "input schema references unknown step",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
  - name: transform
    image: etl:latest
    dependsOn: [extract]
    inputSchema:
      nonexistent:
        required: [file_path]
`,
			wantErr: `inputSchema: references unknown step "nonexistent"`,
		},
		{
			name: "input schema references non-predecessor step",
			yaml: minimalDef + `
  - name: extract
    image: etl:latest
  - name: other
    image: etl:latest
    dependsOn: [extract]
  - name: transform
    image: etl:latest
    dependsOn: [extract]
    inputSchema:
      other:
        required: [file_path]
`,
			wantErr: `inputSchema: step "other" is not a predecessor of "transform"`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSchemaCompatibility(t *testing.T) {
	base := minimalDef + `
  - name: extract
    image: etl:latest
    outputSchema:
      type: object
      properties:
        row_count: { type: integer }
        file_path: { type: string }
      required: [row_count, file_path]
  - name: transform
    image: etl:latest
    dependsOn: [extract]
    inputSchema:
      extract:
`

	cases := []struct {
		name        string
		inputSchema string
		wantErr     string
	}{
		{
			name:        "compatible - required key exists",
			inputSchema: `        required: [file_path]`,
		},
		{
			name:        "compatible - required key with matching type",
			inputSchema: `        required: [row_count]` + "\n" + `        properties:` + "\n" + `          row_count: { type: integer }`,
		},
		{
			name:        "incompatible - required key missing from producer",
			inputSchema: `        required: [missing_key]`,
			wantErr:     `requires key "missing_key" which is not declared`,
		},
		{
			name: "incompatible - type mismatch",
			inputSchema: "        required: [row_count]\n" +
				"        properties:\n" +
				"          row_count: { type: string }",
			wantErr: `key "row_count" type mismatch`,
		},
		{
			name:        "no required keys - passes",
			inputSchema: `        type: object`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := base + tc.inputSchema + "\n"
			_, err := Parse([]byte(yaml))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestSchemaValidationMetadata(t *testing.T) {
	cases := []struct {
		name    string
		sv      string
		wantErr string
	}{
		{name: "empty - disabled", sv: ""},
		{name: "warn", sv: "warn"},
		{name: "fail", sv: "fail"},
		{name: "invalid value", sv: "invalid", wantErr: "schemaValidation"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			yaml := minimalDef
			if tc.sv != "" {
				yaml = strings.Replace(yaml,
					"metadata:\n  alias: test-job",
					"metadata:\n  alias: test-job\n  schemaValidation: "+tc.sv,
					1)
			}
			yaml += "  - name: step1\n    image: app:latest\n"

			_, err := Parse([]byte(yaml))
			if tc.wantErr != "" {
				require.Error(t, err)
				require.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
