package schemacompat

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareVerdictMatrix(t *testing.T) {
	cases := []struct {
		name        string
		oldSchema   map[string]any
		newSchema   map[string]any
		wantKind    FindingKind
		wantPath    string
		wantVerdict Verdict
	}{
		{
			name: "removed required entry is breaking",
			oldSchema: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			newSchema: objectSchema(
				nil,
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			wantKind:    FindingKindRequiredRemoved,
			wantPath:    "properties.customer_id",
			wantVerdict: VerdictBreaking,
		},
		{
			name: "removed required property schema is breaking",
			oldSchema: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			newSchema: objectSchema(
				[]any{"customer_id"},
				map[string]any{},
			),
			wantKind:    FindingKindRequiredRemoved,
			wantPath:    "properties.customer_id",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "string to integer type change is breaking",
			oldSchema:   map[string]any{"type": "string"},
			newSchema:   map[string]any{"type": "integer"},
			wantKind:    FindingKindTypeNarrowed,
			wantPath:    "type",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "number to integer type narrowing is breaking",
			oldSchema:   map[string]any{"type": "number"},
			newSchema:   map[string]any{"type": "integer"},
			wantKind:    FindingKindTypeNarrowed,
			wantPath:    "type",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "integer to number type widening is compatible",
			oldSchema:   map[string]any{"type": "integer"},
			newSchema:   map[string]any{"type": "number"},
			wantKind:    FindingKindTypeWidened,
			wantPath:    "type",
			wantVerdict: VerdictCompatible,
		},
		{
			name:        "type list losing a member narrows",
			oldSchema:   map[string]any{"type": []any{"string", "null"}},
			newSchema:   map[string]any{"type": []any{"string"}},
			wantKind:    FindingKindTypeNarrowed,
			wantPath:    "type",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "type list gaining a member widens",
			oldSchema:   map[string]any{"type": []any{"string"}},
			newSchema:   map[string]any{"type": []any{"string", "null"}},
			wantKind:    FindingKindTypeWidened,
			wantPath:    "type",
			wantVerdict: VerdictCompatible,
		},
		{
			name:        "enum value removal is breaking",
			oldSchema:   map[string]any{"type": "string", "enum": []any{"blue", "red"}},
			newSchema:   map[string]any{"type": "string", "enum": []any{"red"}},
			wantKind:    FindingKindEnumValuesRemoved,
			wantPath:    "enum",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "enum value addition is compatible",
			oldSchema:   map[string]any{"type": "string", "enum": []any{"red"}},
			newSchema:   map[string]any{"type": "string", "enum": []any{"blue", "red"}},
			wantKind:    FindingKindEnumValuesAdded,
			wantPath:    "enum",
			wantVerdict: VerdictCompatible,
		},
		{
			name: "additive optional property is compatible",
			oldSchema: objectSchema(
				[]any{"id"},
				map[string]any{"id": map[string]any{"type": "string"}},
			),
			newSchema: objectSchema(
				[]any{"id"},
				map[string]any{
					"id":       map[string]any{"type": "string"},
					"nickname": map[string]any{"type": "string"},
				},
			),
			wantKind:    FindingKindOptionalPropertyAdded,
			wantPath:    "properties.nickname",
			wantVerdict: VerdictCompatible,
		},
		{
			name: "additionalProperties false with required key outside properties is breaking",
			oldSchema: objectSchema(
				[]any{"tenant"},
				map[string]any{},
			),
			newSchema: map[string]any{
				"type":                 "object",
				"required":             []any{"tenant"},
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			wantKind:    FindingKindAdditionalPropertiesTightened,
			wantPath:    "properties.tenant",
			wantVerdict: VerdictBreaking,
		},
		{
			name:        "relaxed numeric constraint is compatible",
			oldSchema:   map[string]any{"type": "string", "minLength": 5},
			newSchema:   map[string]any{"type": "string", "minLength": 3},
			wantKind:    FindingKindConstraintRelaxed,
			wantPath:    "minLength",
			wantVerdict: VerdictCompatible,
		},
		{
			name:        "removed constraint is compatible",
			oldSchema:   map[string]any{"type": "string", "maxLength": 10},
			newSchema:   map[string]any{"type": "string"},
			wantKind:    FindingKindConstraintRelaxed,
			wantPath:    "maxLength",
			wantVerdict: VerdictCompatible,
		},
		{
			name:        "new constraint is unknown",
			oldSchema:   map[string]any{"type": "string"},
			newSchema:   map[string]any{"type": "string", "minLength": 3},
			wantKind:    FindingKindUnknownConstruct,
			wantPath:    "minLength",
			wantVerdict: VerdictUnknown,
		},
		{
			name: "nested required removal is breaking",
			oldSchema: objectSchema(
				[]any{"customer"},
				map[string]any{
					"customer": objectSchema(
						[]any{"id"},
						map[string]any{"id": map[string]any{"type": "string"}},
					),
				},
			),
			newSchema: objectSchema(
				[]any{"customer"},
				map[string]any{
					"customer": objectSchema(
						nil,
						map[string]any{"id": map[string]any{"type": "string"}},
					),
				},
			),
			wantKind:    FindingKindRequiredRemoved,
			wantPath:    "properties.customer.properties.id",
			wantVerdict: VerdictBreaking,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Compare(tc.oldSchema, tc.newSchema)
			requireContainsFinding(t, findings, tc.wantKind, tc.wantPath, tc.wantVerdict)
		})
	}
}

func TestCompareUnknownConstructs(t *testing.T) {
	unknowns := []struct {
		keyword string
		value   any
	}{
		{keyword: "$ref", value: "#/$defs/customer"},
		{keyword: "allOf", value: []any{map[string]any{"type": "string"}}},
		{keyword: "anyOf", value: []any{map[string]any{"type": "string"}}},
		{keyword: "oneOf", value: []any{map[string]any{"type": "string"}}},
		{keyword: "not", value: map[string]any{"type": "null"}},
		{keyword: "if", value: map[string]any{"required": []any{"kind"}}},
		{keyword: "then", value: map[string]any{"required": []any{"id"}}},
		{keyword: "else", value: map[string]any{"required": []any{"fallback"}}},
		{keyword: "patternProperties", value: map[string]any{"^x-": map[string]any{"type": "string"}}},
		{keyword: "dependentSchemas", value: map[string]any{"card": map[string]any{"required": []any{"billing_address"}}}},
	}

	for _, tc := range unknowns {
		t.Run(tc.keyword, func(t *testing.T) {
			findings := Compare(map[string]any{"type": "object"}, map[string]any{
				"type":     "object",
				tc.keyword: tc.value,
			})
			requireContainsFinding(t, findings, FindingKindUnknownConstruct, tc.keyword, VerdictUnknown)
		})
	}
}

func TestCompareDocOnlyEditHasNoFindings(t *testing.T) {
	findings := Compare(
		map[string]any{"type": "object", "title": "Old title", "description": "old"},
		map[string]any{"type": "object", "title": "New title", "description": "new"},
	)
	require.Empty(t, findings)
}

// A key that was required by the OLD schema but dropped by the new one is
// compareRequired's required-removed finding — it must NOT also surface as an
// additionalProperties tightening (the tightening guard reads only the NEW
// schema's required set). Regression for the review-flagged union false
// positive.
func TestCompareDroppedRequiredDoesNotDoubleAsTightening(t *testing.T) {
	findings := Compare(
		map[string]any{
			"type":     "object",
			"required": []any{"customer_id"},
			"properties": map[string]any{
				"customer_id": map[string]any{"type": "string"},
			},
		},
		map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"row_count": map[string]any{"type": "integer"},
			},
		},
	)

	requireContainsFinding(t, findings, FindingKindRequiredRemoved, "properties.customer_id", VerdictBreaking)
	for _, f := range findings {
		require.NotEqual(t, FindingKindAdditionalPropertiesTightened, f.Kind,
			"dropped-required key must not double as a tightening finding: %#v", f)
	}
}

// A property added simultaneously to properties AND required strengthens the
// producer's guarantee — deliberately no finding of any verdict.
func TestCompareAddedRequiredPropertyIsSilentlyCompatible(t *testing.T) {
	findings := Compare(
		map[string]any{
			"type": "object",
			"properties": map[string]any{
				"customer_id": map[string]any{"type": "string"},
			},
		},
		map[string]any{
			"type":     "object",
			"required": []any{"row_count"},
			"properties": map[string]any{
				"customer_id": map[string]any{"type": "string"},
				"row_count":   map[string]any{"type": "integer"},
			},
		},
	)
	require.Empty(t, findings)
}

func TestSatisfies(t *testing.T) {
	cases := []struct {
		name        string
		schema      map[string]any
		requirement map[string]any
		wantKind    FindingKind
		wantPath    string
		wantVerdict Verdict
		wantClean   bool
	}{
		{
			name: "required string field satisfies consumer requirement",
			schema: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			requirement: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			wantClean: true,
		},
		{
			name: "consumer required field must still be required by producer",
			schema: objectSchema(
				nil,
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			requirement: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			wantKind:    FindingKindRequirementUnsatisfied,
			wantPath:    "properties.customer_id",
			wantVerdict: VerdictBreaking,
		},
		{
			name: "additionalProperties false makes missing required property unsatisfiable",
			schema: map[string]any{
				"type":                 "object",
				"required":             []any{"customer_id"},
				"properties":           map[string]any{},
				"additionalProperties": false,
			},
			requirement: objectSchema(
				[]any{"customer_id"},
				map[string]any{"customer_id": map[string]any{"type": "string"}},
			),
			wantKind:    FindingKindRequirementUnsatisfied,
			wantPath:    "properties.customer_id",
			wantVerdict: VerdictBreaking,
		},
		{
			name: "producer number does not satisfy consumer integer",
			schema: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "number"}},
			),
			requirement: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "integer"}},
			),
			wantKind:    FindingKindRequirementTypeMismatch,
			wantPath:    "properties.count.type",
			wantVerdict: VerdictBreaking,
		},
		{
			name: "producer integer satisfies consumer number",
			schema: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "integer"}},
			),
			requirement: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "number"}},
			),
			wantClean: true,
		},
		{
			name: "consumer constraint is unknown",
			schema: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "integer"}},
			),
			requirement: objectSchema(
				[]any{"count"},
				map[string]any{"count": map[string]any{"type": "integer", "minimum": 0}},
			),
			wantKind:    FindingKindUnknownConstruct,
			wantPath:    "properties.count.minimum",
			wantVerdict: VerdictUnknown,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			findings := Satisfies(tc.schema, tc.requirement)
			if tc.wantClean {
				require.Empty(t, findings)
				return
			}
			requireContainsFinding(t, findings, tc.wantKind, tc.wantPath, tc.wantVerdict)
		})
	}
}

func objectSchema(required []any, properties map[string]any) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": properties,
	}
	if required != nil {
		schema["required"] = required
	}
	return schema
}

func requireContainsFinding(t *testing.T, findings []Finding, kind FindingKind, path string, verdict Verdict) {
	t.Helper()

	for _, finding := range findings {
		if finding.Kind == kind && finding.Path == path && finding.Verdict == verdict {
			return
		}
	}
	require.Failf(t, "missing finding", "kind=%s path=%s verdict=%s findings=%#v", kind, path, verdict, findings)
}
