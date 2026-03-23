package task

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidateOutput(t *testing.T) {
	cases := []struct {
		name          string
		output        map[string]string
		schema        map[string]any
		wantViolation bool
		wantErr       bool
	}{
		{
			name:   "nil schema - no-op",
			output: map[string]string{"key": "value"},
			schema: nil,
		},
		{
			name:   "empty schema - no-op",
			output: map[string]string{"key": "value"},
			schema: map[string]any{},
		},
		{
			name:   "output matches schema",
			output: map[string]string{"row_count": "42", "file_path": "/out/data.parquet"},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"row_count": map[string]any{"type": "integer"},
					"file_path": map[string]any{"type": "string"},
				},
				"required": []any{"row_count", "file_path"},
			},
		},
		{
			name:          "missing required key",
			output:        map[string]string{"file_path": "/out/data.parquet"},
			wantViolation: true,
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"row_count": map[string]any{"type": "integer"},
					"file_path": map[string]any{"type": "string"},
				},
				"required": []any{"row_count", "file_path"},
			},
		},
		{
			name:          "integer type coercion failure - string abc vs integer",
			output:        map[string]string{"row_count": "abc", "file_path": "/out"},
			wantViolation: true,
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"row_count": map[string]any{"type": "integer"},
					"file_path": map[string]any{"type": "string"},
				},
				"required": []any{"row_count", "file_path"},
			},
		},
		{
			name:   "integer coercion succeeds - string 42 validates as integer",
			output: map[string]string{"count": "42"},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{"type": "integer"},
				},
				"required": []any{"count"},
			},
		},
		{
			name:   "number coercion succeeds",
			output: map[string]string{"ratio": "3.14"},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ratio": map[string]any{"type": "number"},
				},
				"required": []any{"ratio"},
			},
		},
		{
			name:   "boolean coercion succeeds",
			output: map[string]string{"success": "true"},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"success": map[string]any{"type": "boolean"},
				},
				"required": []any{"success"},
			},
		},
		{
			name:          "empty output against required fields",
			output:        map[string]string{},
			wantViolation: true,
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
				},
				"required": []any{"file_path"},
			},
		},
		{
			name:   "extra output keys not in schema are allowed",
			output: map[string]string{"file_path": "/out", "extra": "data"},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path": map[string]any{"type": "string"},
				},
				"required": []any{"file_path"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			violations, err := ValidateOutput(tc.output, tc.schema)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			if tc.wantViolation {
				require.NotEmpty(t, violations, "expected violations but got none")
			} else {
				require.Empty(t, violations, "expected no violations but got: %v", violations)
			}
		})
	}
}

func TestInstanceLocationStrEscapesJSONPointerTokens(t *testing.T) {
	t.Parallel()

	got := instanceLocationStr([]string{"root", "foo/bar", "tilde~key"})

	require.Equal(t, "/root/foo~1bar/tilde~0key", got)
}
