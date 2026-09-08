package jobdef

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	if err := compileJSONSchema(schema); err != nil {
		return fmt.Errorf("step %q: invalid outputSchema: %w", stepName, err)
	}
	return nil
}

// validateDatasetSchema compiles an inline dataset schema using the jsonschema
// library to ensure it is a syntactically valid JSON Schema.
func validateDatasetSchema(field string, schema map[string]any) error {
	if err := compileJSONSchema(schema); err != nil {
		return fmt.Errorf("%s: invalid schema: %w", field, err)
	}
	return nil
}

func compileJSONSchema(schema map[string]any) error {
	doc, err := marshalForCompiler(schema)
	if err != nil {
		return err
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaResourceURL, doc); err != nil {
		return err
	}
	if _, err := c.Compile(schemaResourceURL); err != nil {
		return err
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

// validateDatasetAssertionSurface validates the data-circuit-breaker fields on
// one produces[] entry: the assertion block's syntax, the onViolation
// disposition and the release mode.
//
// Per arc convention 1 the whole surface is inert with a clear message when
// CAESIUM_DATA_ASSERTIONS_ENABLED is unset, mirroring the freshness trigger
// gate in validateTrigger: declaring assertions against a server that will
// never evaluate them is a silent no-op, and silent no-ops are how data
// contracts rot.
func validateDatasetAssertionSurface(stepIdx, produceIdx int, p *ProducedDataset) error {
	field := fmt.Sprintf("steps[%d].datasets.produces[%d]", stepIdx, produceIdx)

	declared := !p.Assertions.IsEmpty() ||
		strings.TrimSpace(p.OnViolation) != "" ||
		strings.TrimSpace(p.Release) != ""
	if declared && !dataAssertionsFeatureEnabled() {
		return fmt.Errorf("%s declares assertions/onViolation/release, which require CAESIUM_DATA_ASSERTIONS_ENABLED=true", field)
	}

	switch strings.TrimSpace(p.OnViolation) {
	case "", DatasetOnViolationWarn, DatasetOnViolationFail, DatasetOnViolationHold:
	default:
		return fmt.Errorf("%s.onViolation %q must be one of [%q,%q,%q]",
			field, p.OnViolation, DatasetOnViolationWarn, DatasetOnViolationFail, DatasetOnViolationHold)
	}

	switch strings.TrimSpace(p.Release) {
	case "", DatasetReleaseAuto, DatasetReleaseManual:
	default:
		return fmt.Errorf("%s.release %q must be one of [%q,%q]",
			field, p.Release, DatasetReleaseAuto, DatasetReleaseManual)
	}

	if p.Assertions.IsEmpty() {
		// onViolation without assertions has nothing to dispatch. Reject it
		// rather than accept a knob that can never fire.
		if strings.TrimSpace(p.OnViolation) != "" {
			return fmt.Errorf("%s.onViolation is set but no assertions are declared", field)
		}
		return nil
	}

	// Every assertion reads exactly one emitted metric, and two assertions on
	// the same metric would produce two verdicts on one sample with no defined
	// precedence — so the metric names must be unique within the block.
	seen := make(map[string]string, 4)
	claim := func(assertion, metric string) error {
		if prior, dup := seen[metric]; dup {
			return fmt.Errorf("%s.assertions.%s and %s both assert on metric %q; give one an explicit distinct metric",
				field, prior, assertion, metric)
		}
		seen[metric] = assertion
		return nil
	}

	if p.Assertions.RowCount != nil {
		metric := assertionMetricName(p.Assertions.RowCount, DefaultRowCountMetric)
		if err := validateAssertionSpec(field+".assertions.rowCount", p.Assertions.RowCount, metric); err != nil {
			return err
		}
		if err := claim("rowCount", metric); err != nil {
			return err
		}
	}
	if p.Assertions.NullRate != nil {
		metric := assertionMetricName(p.Assertions.NullRate, DefaultNullRateMetric)
		if err := validateAssertionSpec(field+".assertions.nullRate", p.Assertions.NullRate, metric); err != nil {
			return err
		}
		if err := claim("nullRate", metric); err != nil {
			return err
		}
	}
	if fa := p.Assertions.Freshness; fa != nil {
		watermark := strings.TrimSpace(fa.Watermark)
		if watermark == "" {
			return fmt.Errorf("%s.assertions.freshness.watermark is required: it names the emitted RFC3339 metric to measure lag from", field)
		}
		maxLag := strings.TrimSpace(fa.MaxLag)
		if maxLag == "" {
			return fmt.Errorf("%s.assertions.freshness.maxLag is required", field)
		}
		dur, err := time.ParseDuration(maxLag)
		if err != nil {
			return fmt.Errorf("%s.assertions.freshness.maxLag %q must be a valid duration: %w", field, fa.MaxLag, err)
		}
		if dur <= 0 {
			return fmt.Errorf("%s.assertions.freshness.maxLag %q must be a positive duration", field, fa.MaxLag)
		}
		if err := claim("freshness", watermark); err != nil {
			return err
		}
	}
	for k := range p.Assertions.Custom {
		custom := &p.Assertions.Custom[k]
		customField := fmt.Sprintf("%s.assertions.custom[%d]", field, k)
		metric := strings.TrimSpace(custom.Metric)
		if metric == "" {
			return fmt.Errorf("%s.metric is required: a custom assertion names the emitted metric it reads", customField)
		}
		if err := validateAssertionSpec(customField, custom, metric); err != nil {
			return err
		}
		if err := claim(fmt.Sprintf("custom[%d]", k), metric); err != nil {
			return err
		}
	}

	return nil
}

// AssertionMetricName resolves the emitted metric key an assertion reads: the
// explicit `metric:` when the author gave one, else the shorthand's default
// (DefaultRowCountMetric / DefaultNullRateMetric). It is exported because the
// post-task evaluator (internal/run) must resolve the metric key exactly as
// apply-time validation did — one rule, one implementation — or a declared
// assertion would silently read a different metric than the lint checked.
func AssertionMetricName(spec *AssertionSpec, fallback string) string {
	return assertionMetricName(spec, fallback)
}

// assertionMetricName resolves the emitted metric key an assertion reads: the
// explicit `metric:` when the author gave one, else the shorthand's default.
func assertionMetricName(spec *AssertionSpec, fallback string) string {
	if spec == nil {
		return fallback
	}
	if metric := strings.TrimSpace(spec.Metric); metric != "" {
		return metric
	}
	return fallback
}

// validateAssertionSpec checks one bound triple. An assertion with no bound at
// all is rejected: it would record a metric and assert nothing, which reads as
// enforcement but is not.
func validateAssertionSpec(field string, spec *AssertionSpec, metric string) error {
	if spec.Min == nil && spec.Max == nil && strings.TrimSpace(spec.DeltaFromBaseline) == "" {
		return fmt.Errorf("%s must declare at least one of min, max or deltaFromBaseline", field)
	}
	if spec.Min != nil && spec.Max != nil && *spec.Min > *spec.Max {
		return fmt.Errorf("%s.min (%v) must not exceed max (%v)", field, *spec.Min, *spec.Max)
	}
	if raw := strings.TrimSpace(spec.DeltaFromBaseline); raw != "" {
		if _, err := ParseDeltaFromBaseline(raw); err != nil {
			return fmt.Errorf("%s.deltaFromBaseline: %w", field, err)
		}
	}
	if strings.ContainsAny(metric, " \t") {
		return fmt.Errorf("%s.metric %q must not contain whitespace", field, metric)
	}
	return nil
}

// ParseDeltaFromBaseline parses the `deltaFromBaseline: 50%` grammar into a
// fraction of the baseline median (0.5). It is exported because the evaluator
// (internal/run) reads the same persisted spec and must interpret it
// identically — one grammar, one parser.
func ParseDeltaFromBaseline(raw string) (float64, error) {
	trimmed := strings.TrimSpace(raw)
	if !strings.HasSuffix(trimmed, "%") {
		return 0, fmt.Errorf("%q must be a percentage of the baseline median, e.g. \"50%%\"", raw)
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(trimmed, "%")), 64)
	if err != nil {
		return 0, fmt.Errorf("%q must be a percentage of the baseline median, e.g. \"50%%\"", raw)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%q must be a positive percentage", raw)
	}
	return value / 100, nil
}
