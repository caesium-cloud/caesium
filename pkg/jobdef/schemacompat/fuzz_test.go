package schemacompat

import (
	"encoding/json"
	"testing"
)

func FuzzCompare(f *testing.F) {
	seeds := []struct {
		oldSchema string
		newSchema string
	}{
		{
			oldSchema: `{}`,
			newSchema: `{}`,
		},
		{
			oldSchema: `{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}`,
			newSchema: `{"type":"object","properties":{"id":{"type":"string"}}}`,
		},
		{
			oldSchema: `{"type":"integer"}`,
			newSchema: `{"type":"number"}`,
		},
		{
			oldSchema: `{"type":"string","enum":["red","blue"]}`,
			newSchema: `{"type":"string","enum":["red"]}`,
		},
		{
			oldSchema: `{"type":"object","properties":{"customer":{"type":"object","required":["id"],"properties":{"id":{"type":"string"}}}}}`,
			newSchema: `{"type":"object","properties":{"customer":{"type":"object","properties":{"id":{"type":"integer"}}}}}`,
		},
		{
			oldSchema: `{"type":"object"}`,
			newSchema: `{"type":"object","oneOf":[{"required":["id"]}]}`,
		},
	}

	for _, seed := range seeds {
		f.Add(seed.oldSchema, seed.newSchema)
	}

	f.Fuzz(func(t *testing.T, oldRaw string, newRaw string) {
		oldSchema := fuzzSchema(oldRaw)
		newSchema := fuzzSchema(newRaw)

		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("schema compatibility walker panicked: %v", r)
			}
		}()

		_ = Compare(oldSchema, newSchema)
		_ = Satisfies(newSchema, oldSchema)

		// Reflexivity: a schema is always compatible with itself under
		// Compare. Every one of compareSchema's six sub-comparisons
		// (compareRequired, compareTypes, compareEnums,
		// compareAdditionalProperties, compareRelaxableConstraints,
		// compareAddedOptionalProperties) computes both sides from the SAME
		// map when old==new, so each one's own equality/subset check is
		// satisfied trivially and never reaches a Breaking branch; the
		// walker's "cannot prove this construct" escape hatch
		// (scanUnsupported, validateSchema) always reports VerdictUnknown,
		// never VerdictBreaking, regardless. So Compare(s, s) must never
		// contain a breaking finding for ANY schema this fuzzer can produce,
		// valid or not — an invariant of the walker itself, not a property
		// that requires well-formed input, so no seed is narrowed to reach
		// it.
		//
		// The same claim does NOT hold for Satisfies(s, s): a schema that
		// requires a field outside "properties" while also declaring
		// additionalProperties: false is self-contradictory (nothing can ever
		// satisfy it, including itself), and Satisfies correctly reports that
		// as breaking even reflexively — so no Satisfies(s, s) property is
		// asserted here.
		for _, s := range []map[string]any{oldSchema, newSchema} {
			for _, finding := range Compare(s, s) {
				if finding.Verdict == VerdictBreaking {
					t.Fatalf("Compare(s, s) reported a schema as breaking against itself: %+v\nschema=%v", finding, s)
				}
			}
		}
	})
}

func fuzzSchema(raw string) map[string]any {
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err == nil && schema != nil {
		return schema
	}
	return map[string]any{"description": raw}
}
