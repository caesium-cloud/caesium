package jobdef

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// schemaResourceURL is the pseudo-URL used for in-memory schema resources.
const schemaResourceURL = "https://caesium.internal/schema.json"

// validateSchemas checks outputSchema/inputSchema fields on all steps.
// It is called after the DAG structure has been validated (no cycles, no unknown refs).
func validateSchemas(steps []Step, names map[string]int, predecessors map[string]map[string]struct{}) error {
	for i := range steps {
		step := &steps[i]

		if step.OutputSchema != nil {
			if err := validateOutputSchema(step.Name, step.OutputSchema); err != nil {
				return err
			}
		}

		if step.InputSchema != nil {
			if err := validateInputSchema(i, step.Name, step.InputSchema, predecessors[step.Name], names, steps); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateOutputSchema compiles the schema using the jsonschema library to ensure
// it is a syntactically valid JSON Schema.
func validateOutputSchema(stepName string, schema map[string]any) error {
	doc, err := marshalForCompiler(schema)
	if err != nil {
		return fmt.Errorf("step %q: invalid outputSchema: %w", stepName, err)
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaResourceURL, doc); err != nil {
		return fmt.Errorf("step %q: invalid outputSchema: %w", stepName, err)
	}
	if _, err := c.Compile(schemaResourceURL); err != nil {
		return fmt.Errorf("step %q: invalid outputSchema: %w", stepName, err)
	}
	return nil
}

// validateInputSchema checks that each key in inputSchema references a valid predecessor step,
// and that the required keys are declared in the producer's outputSchema (if present).
func validateInputSchema(stepIdx int, stepName string, inputSchema map[string]map[string]any, preds map[string]struct{}, names map[string]int, steps []Step) error {
	for producerName, consumerSchema := range inputSchema {
		// Must reference an existing step.
		if _, exists := names[producerName]; !exists {
			return fmt.Errorf("steps[%d].inputSchema: references unknown step %q", stepIdx, producerName)
		}
		// Must be an actual predecessor (via DAG edges).
		if _, isPred := preds[producerName]; !isPred {
			return fmt.Errorf("steps[%d].inputSchema: step %q is not a predecessor of %q", stepIdx, producerName, stepName)
		}
		// If the producer has an outputSchema, check compatibility.
		producerIdx := names[producerName]
		producer := &steps[producerIdx]
		if producer.OutputSchema != nil {
			if err := checkSchemaCompatibility(stepIdx, producerName, producer.OutputSchema, consumerSchema); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkSchemaCompatibility verifies that all keys declared as required in consumerSchema
// exist in the producer's outputSchema.properties, and that declared types match.
func checkSchemaCompatibility(consumerIdx int, producerName string, outputSchema map[string]any, consumerSchema map[string]any) error {
	// Extract producer's declared properties.
	producerProps, _ := outputSchema["properties"].(map[string]any)

	// Extract consumer's required keys.
	required, ok := consumerSchema["required"]
	if !ok {
		return nil // No required keys declared — nothing to check.
	}

	requiredList, ok := required.([]any)
	if !ok {
		return fmt.Errorf("steps[%d].inputSchema[%q]: \"required\" must be an array", consumerIdx, producerName)
	}

	for _, item := range requiredList {
		key, ok := item.(string)
		if !ok {
			return fmt.Errorf("steps[%d].inputSchema[%q]: \"required\" entries must be strings", consumerIdx, producerName)
		}

		if producerProps == nil {
			return fmt.Errorf("steps[%d].inputSchema[%q]: requires key %q but step %q declares no outputSchema properties",
				consumerIdx, producerName, key, producerName)
		}

		producerPropRaw, exists := producerProps[key]
		if !exists {
			return fmt.Errorf("steps[%d].inputSchema[%q]: requires key %q which is not declared in step %q outputSchema",
				consumerIdx, producerName, key, producerName)
		}

		// If consumer also declares a type for this key, verify it matches the producer's type.
		consumerProps, hasConsumerProps := consumerSchema["properties"].(map[string]any)
		if !hasConsumerProps {
			continue
		}
		consumerPropRaw, hasConsumerProp := consumerProps[key]
		if !hasConsumerProp {
			continue
		}
		producerProp, _ := producerPropRaw.(map[string]any)
		consumerProp, _ := consumerPropRaw.(map[string]any)
		if producerProp == nil || consumerProp == nil {
			continue
		}
		producerType, _ := producerProp["type"].(string)
		consumerType, _ := consumerProp["type"].(string)
		if producerType != "" && consumerType != "" && producerType != consumerType {
			return fmt.Errorf("steps[%d].inputSchema[%q]: key %q type mismatch: producer declares %q, consumer expects %q",
				consumerIdx, producerName, key, producerType, consumerType)
		}
	}

	return nil
}

// marshalForCompiler round-trips a map through JSON to produce a value suitable
// for jsonschema.Compiler.AddResource (which expects the same format as jsonschema.UnmarshalJSON).
func marshalForCompiler(v map[string]any) (any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(data))
}
