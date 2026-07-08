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
	})
}

func fuzzSchema(raw string) map[string]any {
	var schema map[string]any
	if err := json.Unmarshal([]byte(raw), &schema); err == nil && schema != nil {
		return schema
	}
	return map[string]any{"description": raw}
}
