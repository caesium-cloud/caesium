package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// SchemaViolation describes a single output schema validation failure.
type SchemaViolation struct {
	Key     string `json:"key"`
	Message string `json:"message"`
}

// ValidateOutput validates a task's string-valued output map against a JSON Schema.
// XCom values are all strings; this function coerces them to typed values based on
// the schema's declared property types before validation.
//
// Returns nil violations (not an error) when the output is valid.
// Returns a non-empty violations slice when validation fails.
// Returns an error only when the schema itself cannot be compiled.
func ValidateOutput(output map[string]string, schemaRaw map[string]any) ([]SchemaViolation, error) {
	if len(schemaRaw) == 0 {
		return nil, nil
	}

	// Compile the JSON Schema.
	schemaBytes, err := json.Marshal(schemaRaw)
	if err != nil {
		return nil, fmt.Errorf("marshal schema: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	const schemaURL = "https://caesium.internal/output-schema.json"
	if err := c.AddResource(schemaURL, doc); err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}
	sch, err := c.Compile(schemaURL)
	if err != nil {
		return nil, fmt.Errorf("compile schema: %w", err)
	}

	// Extract declared property types from the schema for coercion hints.
	props, _ := schemaRaw["properties"].(map[string]any)

	// Build a typed instance from the string output map.
	instance := make(map[string]any, len(output))
	for k, v := range output {
		instance[k] = coerceValue(v, declaredType(props, k))
	}

	// Validate and collect violations.
	if err := sch.Validate(instance); err != nil {
		return collectViolations(err), nil
	}
	return nil, nil
}

// ValidateOutputSchemaBytes validates a task output map against a schema encoded as JSON bytes.
func ValidateOutputSchemaBytes(output map[string]string, schemaBytes []byte) ([]SchemaViolation, error) {
	if len(schemaBytes) == 0 {
		return nil, nil
	}

	var schemaRaw map[string]any
	if err := json.Unmarshal(schemaBytes, &schemaRaw); err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}

	return ValidateOutput(output, schemaRaw)
}

// coerceValue attempts to convert a string value to the declared schema type.
// Falls back to the original string if conversion fails.
func coerceValue(s string, typeName string) any {
	switch typeName {
	case "integer":
		if i, err := strconv.Atoi(s); err == nil {
			return i
		}
	case "number":
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(s); err == nil {
			return b
		}
	case "object", "array":
		var v any
		if err := json.Unmarshal([]byte(s), &v); err == nil {
			return v
		}
	}
	return s
}

// declaredType returns the "type" field from the schema property named key, or "".
func declaredType(props map[string]any, key string) string {
	if props == nil {
		return ""
	}
	propRaw, ok := props[key]
	if !ok {
		return ""
	}
	prop, _ := propRaw.(map[string]any)
	if prop == nil {
		return ""
	}
	t, _ := prop["type"].(string)
	return t
}

// collectViolations converts a jsonschema validation error into a flat list of SchemaViolation.
func collectViolations(err error) []SchemaViolation {
	if err == nil {
		return nil
	}
	var violations []SchemaViolation
	if ve, ok := err.(*jsonschema.ValidationError); ok {
		appendLeafViolations(&violations, ve)
		if len(violations) == 0 {
			// Top-level error with no sub-causes.
			violations = append(violations, SchemaViolation{
				Key:     instanceLocationStr(ve.InstanceLocation),
				Message: ve.Error(),
			})
		}
	} else {
		violations = append(violations, SchemaViolation{
			Key:     "",
			Message: err.Error(),
		})
	}
	return violations
}

func appendLeafViolations(dst *[]SchemaViolation, err *jsonschema.ValidationError) {
	if err == nil {
		return
	}
	if len(err.Causes) == 0 {
		*dst = append(*dst, SchemaViolation{
			Key:     instanceLocationStr(err.InstanceLocation),
			Message: err.Error(),
		})
		return
	}
	for _, cause := range err.Causes {
		appendLeafViolations(dst, cause)
	}
}

// instanceLocationStr converts a JSON Pointer path ([]string) to a slash-joined string.
func instanceLocationStr(parts []string) string {
	if len(parts) == 0 {
		return ""
	}

	var b strings.Builder
	for _, p := range parts {
		b.WriteByte('/')
		p = strings.ReplaceAll(p, "~", "~0")
		p = strings.ReplaceAll(p, "/", "~1")
		b.WriteString(p)
	}
	return b.String()
}
