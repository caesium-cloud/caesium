// Package schemacompat compares the pragmatic JSON Schema subset Caesium uses
// for cross-job contract enforcement.
package schemacompat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaResourceURL = "https://caesium.internal/schemacompat/schema.json"

// Verdict is the compatibility grade for a schema finding.
type Verdict string

const (
	// VerdictBreaking marks a change that can break an existing consumer.
	VerdictBreaking Verdict = "breaking"
	// VerdictCompatible marks a change the supported subset can prove safe.
	VerdictCompatible Verdict = "compatible"
	// VerdictUnknown marks a change the supported subset cannot prove safe.
	VerdictUnknown Verdict = "unknown"
)

// FindingKind categorizes a schema compatibility finding.
type FindingKind string

const (
	// FindingKindRequiredRemoved means a field that used to be required is no longer guaranteed.
	FindingKindRequiredRemoved FindingKind = "required_removed"
	// FindingKindTypeNarrowed means a type changed in a way that rejects previously valid values.
	FindingKindTypeNarrowed FindingKind = "type_narrowed"
	// FindingKindTypeWidened means a type changed in a way that accepts all old values and more.
	FindingKindTypeWidened FindingKind = "type_widened"
	// FindingKindEnumValuesRemoved means an enum removed one or more previously valid values.
	FindingKindEnumValuesRemoved FindingKind = "enum_values_removed"
	// FindingKindEnumValuesAdded means an enum now allows additional values.
	FindingKindEnumValuesAdded FindingKind = "enum_values_added"
	// FindingKindOptionalPropertyAdded means a new property was added without making it required.
	FindingKindOptionalPropertyAdded FindingKind = "optional_property_added"
	// FindingKindAdditionalPropertiesTightened means additionalProperties=false made a required key unsatisfiable.
	FindingKindAdditionalPropertiesTightened FindingKind = "additional_properties_tightened"
	// FindingKindConstraintRelaxed means a constraint was removed or loosened.
	FindingKindConstraintRelaxed FindingKind = "constraint_relaxed"
	// FindingKindUnknownConstruct means the schema uses a JSON Schema construct outside the supported subset.
	FindingKindUnknownConstruct FindingKind = "unknown_construct"
	// FindingKindInvalidSchema means the schema did not compile as JSON Schema.
	FindingKindInvalidSchema FindingKind = "invalid_schema"
	// FindingKindRequirementUnsatisfied means a producer schema does not guarantee a consumer-required field.
	FindingKindRequirementUnsatisfied FindingKind = "requirement_unsatisfied"
	// FindingKindRequirementTypeMismatch means a producer field type does not satisfy a consumer-required type.
	FindingKindRequirementTypeMismatch FindingKind = "requirement_type_mismatch"
	// FindingKindRequirementUnknown means a consumer requirement could not be proven against the producer schema.
	FindingKindRequirementUnknown FindingKind = "requirement_unknown"
)

// Finding describes one compatibility decision at a concrete schema path.
type Finding struct {
	// Kind categorizes the compatibility issue or note.
	Kind FindingKind `json:"kind"`
	// Path is the dotted JSON path to the relevant schema node.
	Path string `json:"path"`
	// Detail is a human-readable explanation of the finding.
	Detail string `json:"detail"`
	// Verdict is the tri-state compatibility decision for this finding.
	Verdict Verdict `json:"verdict"`
}

// Compare walks oldSchema and newSchema over Caesium's supported compatibility
// subset and returns deterministic findings for breaking, compatible, and
// unknown changes. It uses jsonschema/v6 only to compile-check schemas; all
// compatibility decisions are made over the raw map trees.
func Compare(oldSchema, newSchema map[string]any) []Finding {
	oldSchema = nonNilSchema(oldSchema)
	newSchema = nonNilSchema(newSchema)

	var findings []Finding
	findings = append(findings, scanUnsupported(nil, oldSchema, scanOptions{allowConstraints: true})...)
	findings = append(findings, scanUnsupported(nil, newSchema, scanOptions{allowConstraints: true})...)
	if len(findings) == 0 {
		compileFindings := append(validateSchema("old schema", oldSchema), validateSchema("new schema", newSchema)...)
		if len(compileFindings) > 0 {
			return compileFindings
		}
	}

	compareSchema(&findings, nil, oldSchema, newSchema)
	return findings
}

// Satisfies checks whether a producer schema still satisfies a consumer's
// required/type subset. It returns breaking findings for requirements the
// producer no longer guarantees and unknown findings when the subset cannot
// prove compatibility.
func Satisfies(schema, requirement map[string]any) []Finding {
	schema = nonNilSchema(schema)
	requirement = nonNilSchema(requirement)

	var findings []Finding
	findings = append(findings, scanUnsupported(nil, schema, scanOptions{allowConstraints: true})...)
	findings = append(findings, scanUnsupported(nil, requirement, scanOptions{})...)
	if len(findings) == 0 {
		compileFindings := append(validateSchema("producer schema", schema), validateSchema("consumer requirement", requirement)...)
		if len(compileFindings) > 0 {
			return compileFindings
		}
	}

	satisfySchema(&findings, nil, schema, requirement)
	return findings
}

func nonNilSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	return schema
}

func validateSchema(label string, schema map[string]any) []Finding {
	data, err := json.Marshal(schema)
	if err != nil {
		return []Finding{finding(FindingKindInvalidSchema, nil, VerdictUnknown, "%s cannot be marshaled: %v", label, err)}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return []Finding{finding(FindingKindInvalidSchema, nil, VerdictUnknown, "%s cannot be decoded as JSON Schema: %v", label, err)}
	}

	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaResourceURL, doc); err != nil {
		return []Finding{finding(FindingKindInvalidSchema, nil, VerdictUnknown, "%s cannot be added to JSON Schema compiler: %v", label, err)}
	}
	if _, err := compiler.Compile(schemaResourceURL); err != nil {
		return []Finding{finding(FindingKindInvalidSchema, nil, VerdictUnknown, "%s does not compile as JSON Schema: %v", label, err)}
	}
	return nil
}

var unsupportedKeywords = map[string]struct{}{
	"$ref":              {},
	"allOf":             {},
	"anyOf":             {},
	"oneOf":             {},
	"not":               {},
	"if":                {},
	"then":              {},
	"else":              {},
	"patternProperties": {},
	"dependentSchemas":  {},
}

var supportedKeywords = map[string]struct{}{
	"$comment":             {},
	"$id":                  {},
	"$schema":              {},
	"additionalProperties": {},
	"default":              {},
	"deprecated":           {},
	"description":          {},
	"enum":                 {},
	"examples":             {},
	"properties":           {},
	"readOnly":             {},
	"required":             {},
	"title":                {},
	"type":                 {},
	"writeOnly":            {},
}

var relaxableConstraintKeywords = map[string]constraintDirection{
	"exclusiveMaximum": upperBoundConstraint,
	"exclusiveMinimum": lowerBoundConstraint,
	"format":           removalOnlyConstraint,
	"maxItems":         upperBoundConstraint,
	"maxLength":        upperBoundConstraint,
	"maxProperties":    upperBoundConstraint,
	"maximum":          upperBoundConstraint,
	"minItems":         lowerBoundConstraint,
	"minLength":        lowerBoundConstraint,
	"minProperties":    lowerBoundConstraint,
	"minimum":          lowerBoundConstraint,
	"multipleOf":       removalOnlyConstraint,
	"pattern":          removalOnlyConstraint,
	"uniqueItems":      boolFalseRelaxesConstraint,
}

type scanOptions struct {
	allowConstraints bool
}

func scanUnsupported(path schemaPath, schema map[string]any, opts scanOptions) []Finding {
	var findings []Finding
	for _, key := range sortedKeys(schema) {
		value := schema[key]
		keyPath := path.append(key)

		if _, unsupported := unsupportedKeywords[key]; unsupported {
			findings = append(findings, finding(
				FindingKindUnknownConstruct,
				keyPath,
				VerdictUnknown,
				"keyword %q is outside Caesium's compatibility subset; cannot prove compatibility",
				key,
			))
			continue
		}

		if _, supported := supportedKeywords[key]; !supported {
			if _, constraint := relaxableConstraintKeywords[key]; constraint && opts.allowConstraints {
				continue
			}
			findings = append(findings, finding(
				FindingKindUnknownConstruct,
				keyPath,
				VerdictUnknown,
				"keyword %q is outside Caesium's compatibility subset; cannot prove compatibility",
				key,
			))
			continue
		}

		switch key {
		case "additionalProperties":
			if value == nil {
				continue
			}
			if _, ok := value.(bool); !ok {
				findings = append(findings, finding(
					FindingKindUnknownConstruct,
					keyPath,
					VerdictUnknown,
					"schema-valued additionalProperties is outside Caesium's compatibility subset; cannot prove compatibility",
				))
			}
		case "properties":
			props, ok := value.(map[string]any)
			if !ok {
				findings = append(findings, finding(
					FindingKindUnknownConstruct,
					keyPath,
					VerdictUnknown,
					"properties must be an object for Caesium's compatibility subset; cannot prove compatibility",
				))
				continue
			}
			for _, propName := range sortedKeys(props) {
				propPath := path.append("properties", propName)
				propSchema, ok := props[propName].(map[string]any)
				if !ok {
					findings = append(findings, finding(
						FindingKindUnknownConstruct,
						propPath,
						VerdictUnknown,
						"property schema is outside Caesium's compatibility subset; cannot prove compatibility",
					))
					continue
				}
				findings = append(findings, scanUnsupported(propPath, propSchema, opts)...)
			}
		}
	}
	return findings
}

func compareSchema(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	compareRequired(findings, path, oldSchema, newSchema)
	compareTypes(findings, path, oldSchema, newSchema)
	compareEnums(findings, path, oldSchema, newSchema)
	compareAdditionalProperties(findings, path, oldSchema, newSchema)
	compareRelaxableConstraints(findings, path, oldSchema, newSchema)
	compareAddedOptionalProperties(findings, path, oldSchema, newSchema)

	oldProps := schemaProperties(oldSchema)
	newProps := schemaProperties(newSchema)
	for _, key := range sortedKeys(oldProps) {
		oldProp, oldOK := oldProps[key].(map[string]any)
		newProp, newOK := newProps[key].(map[string]any)
		if oldOK && newOK {
			compareSchema(findings, path.append("properties", key), oldProp, newProp)
		}
	}
}

func compareRequired(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	oldRequired := requiredSet(oldSchema)
	newRequired := requiredSet(newSchema)
	oldProps := schemaProperties(oldSchema)
	newProps := schemaProperties(newSchema)

	for _, key := range sortedSetKeys(oldRequired) {
		propPath := path.append("properties", key)
		_, oldHadProperty := oldProps[key]
		_, newHasProperty := newProps[key]
		if oldHadProperty && !newHasProperty {
			*findings = append(*findings, finding(
				FindingKindRequiredRemoved,
				propPath,
				VerdictBreaking,
				"required field %q was removed from properties",
				key,
			))
			continue
		}

		if _, stillRequired := newRequired[key]; !stillRequired {
			*findings = append(*findings, finding(
				FindingKindRequiredRemoved,
				propPath,
				VerdictBreaking,
				"required field %q is no longer required",
				key,
			))
		}
	}
}

func compareTypes(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	oldTypes, oldHasTypes := schemaTypes(oldSchema["type"])
	newTypes, newHasTypes := schemaTypes(newSchema["type"])
	typePath := path.append("type")

	switch {
	case !oldHasTypes && !newHasTypes:
		return
	case oldHasTypes && !newHasTypes:
		*findings = append(*findings, finding(
			FindingKindConstraintRelaxed,
			typePath,
			VerdictCompatible,
			"type constraint removed; new schema accepts all previously valid types",
		))
	case !oldHasTypes && newHasTypes:
		*findings = append(*findings, finding(
			FindingKindTypeNarrowed,
			typePath,
			VerdictBreaking,
			"type constraint added: %s",
			formatTypes(newTypes),
		))
	case typeSetsEqual(oldTypes, newTypes):
		return
	case typeSetAllowsAll(newTypes, oldTypes):
		*findings = append(*findings, finding(
			FindingKindTypeWidened,
			typePath,
			VerdictCompatible,
			"type widened from %s to %s",
			formatTypes(oldTypes),
			formatTypes(newTypes),
		))
	default:
		*findings = append(*findings, finding(
			FindingKindTypeNarrowed,
			typePath,
			VerdictBreaking,
			"type narrowed or changed from %s to %s",
			formatTypes(oldTypes),
			formatTypes(newTypes),
		))
	}
}

func compareEnums(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	oldEnum, oldHasEnum := enumValues(oldSchema["enum"])
	newEnum, newHasEnum := enumValues(newSchema["enum"])
	enumPath := path.append("enum")

	switch {
	case oldHasEnum && newHasEnum:
		removed := enumDifference(oldEnum, newEnum)
		added := enumDifference(newEnum, oldEnum)
		if len(removed) > 0 {
			*findings = append(*findings, finding(
				FindingKindEnumValuesRemoved,
				enumPath,
				VerdictBreaking,
				"enum values removed: %s",
				formatEnumValues(removed),
			))
		}
		if len(added) > 0 {
			*findings = append(*findings, finding(
				FindingKindEnumValuesAdded,
				enumPath,
				VerdictCompatible,
				"enum values added: %s",
				formatEnumValues(added),
			))
		}
	case oldHasEnum && !newHasEnum:
		*findings = append(*findings, finding(
			FindingKindConstraintRelaxed,
			enumPath,
			VerdictCompatible,
			"enum constraint removed",
		))
	case !oldHasEnum && newHasEnum:
		*findings = append(*findings, finding(
			FindingKindEnumValuesRemoved,
			enumPath,
			VerdictBreaking,
			"enum constraint added; values outside %s are no longer allowed",
			formatEnumValues(newEnum),
		))
	}
}

func compareAdditionalProperties(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	oldFalse := additionalPropertiesFalse(oldSchema)
	newFalse := additionalPropertiesFalse(newSchema)
	if oldFalse && !newFalse {
		*findings = append(*findings, finding(
			FindingKindConstraintRelaxed,
			path.append("additionalProperties"),
			VerdictCompatible,
			"additionalProperties relaxed from false",
		))
		return
	}
	if !newFalse {
		return
	}

	newProps := schemaProperties(newSchema)
	oldProps := schemaProperties(oldSchema)
	oldRequired := requiredSet(oldSchema)
	// Only the NEW schema's required set matters here: a key required by the
	// old schema but dropped by the new one is compareRequired's finding
	// (required-removed), not an additionalProperties tightening.
	required := requiredSet(newSchema)
	for _, key := range sortedSetKeys(required) {
		if _, ok := newProps[key]; ok {
			continue
		}
		if oldFalse {
			// The old schema was already additionalProperties: false. Flag only
			// NEWLY unsatisfiable keys — a key that was already
			// required-outside-properties is a pre-existing invariant, not a
			// change introduced by this revision.
			_, wasRequired := oldRequired[key]
			_, hadProp := oldProps[key]
			if wasRequired && !hadProp {
				continue
			}
		}
		*findings = append(*findings, finding(
			FindingKindAdditionalPropertiesTightened,
			path.append("properties", key),
			VerdictBreaking,
			"additionalProperties tightened to false while required field %q is outside properties",
			key,
		))
	}
}

type constraintDirection int

const (
	lowerBoundConstraint constraintDirection = iota
	upperBoundConstraint
	removalOnlyConstraint
	boolFalseRelaxesConstraint
)

func compareRelaxableConstraints(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	for _, key := range sortedConstraintKeys() {
		oldValue, oldOK := oldSchema[key]
		newValue, newOK := newSchema[key]
		if !oldOK && !newOK {
			continue
		}

		constraintPath := path.append(key)
		switch {
		case oldOK && !newOK:
			*findings = append(*findings, finding(
				FindingKindConstraintRelaxed,
				constraintPath,
				VerdictCompatible,
				"constraint %q removed",
				key,
			))
		case !oldOK && newOK:
			*findings = append(*findings, finding(
				FindingKindUnknownConstruct,
				constraintPath,
				VerdictUnknown,
				"constraint %q added; cannot prove compatibility",
				key,
			))
		case constraintValuesEqual(oldValue, newValue):
			continue
		case constraintRelaxed(relaxableConstraintKeywords[key], oldValue, newValue):
			*findings = append(*findings, finding(
				FindingKindConstraintRelaxed,
				constraintPath,
				VerdictCompatible,
				"constraint %q relaxed from %s to %s",
				key,
				canonicalJSON(oldValue),
				canonicalJSON(newValue),
			))
		default:
			*findings = append(*findings, finding(
				FindingKindUnknownConstruct,
				constraintPath,
				VerdictUnknown,
				"constraint %q changed from %s to %s; cannot prove compatibility",
				key,
				canonicalJSON(oldValue),
				canonicalJSON(newValue),
			))
		}
	}
}

// sortedConstraintKeysList is computed once — relaxableConstraintKeywords is
// static, and this list is consulted on every schema-node comparison.
var sortedConstraintKeysList = func() []string {
	keys := make([]string, 0, len(relaxableConstraintKeywords))
	for key := range relaxableConstraintKeywords {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}()

func sortedConstraintKeys() []string {
	return sortedConstraintKeysList
}

func constraintValuesEqual(a, b any) bool {
	return canonicalJSON(a) == canonicalJSON(b)
}

func constraintRelaxed(direction constraintDirection, oldValue, newValue any) bool {
	switch direction {
	case lowerBoundConstraint:
		oldNumber, oldOK := numericValue(oldValue)
		newNumber, newOK := numericValue(newValue)
		return oldOK && newOK && newNumber <= oldNumber
	case upperBoundConstraint:
		oldNumber, oldOK := numericValue(oldValue)
		newNumber, newOK := numericValue(newValue)
		return oldOK && newOK && newNumber >= oldNumber
	case boolFalseRelaxesConstraint:
		oldBool, oldOK := oldValue.(bool)
		newBool, newOK := newValue.(bool)
		return oldOK && newOK && oldBool && !newBool
	default:
		return false
	}
}

func numericValue(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int8:
		return float64(v), true
	case int16:
		return float64(v), true
	case int32:
		return float64(v), true
	case int64:
		return float64(v), true
	case uint:
		return float64(v), true
	case uint8:
		return float64(v), true
	case uint16:
		return float64(v), true
	case uint32:
		return float64(v), true
	case uint64:
		return float64(v), true
	case json.Number:
		n, err := v.Float64()
		return n, err == nil
	default:
		return 0, false
	}
}

func compareAddedOptionalProperties(findings *[]Finding, path schemaPath, oldSchema, newSchema map[string]any) {
	oldProps := schemaProperties(oldSchema)
	newProps := schemaProperties(newSchema)
	newRequired := requiredSet(newSchema)

	for _, key := range sortedKeys(newProps) {
		if _, existed := oldProps[key]; existed {
			continue
		}
		if _, required := newRequired[key]; required {
			// A property added simultaneously to properties AND required
			// strengthens the producer's guarantee — deliberately no finding:
			// consumers can only gain from the stronger contract.
			continue
		}
		*findings = append(*findings, finding(
			FindingKindOptionalPropertyAdded,
			path.append("properties", key),
			VerdictCompatible,
			"optional property %q added",
			key,
		))
	}
}

func satisfySchema(findings *[]Finding, path schemaPath, schema, requirement map[string]any) {
	satisfyType(findings, path, schema, requirement)

	requirementRequired := requiredSet(requirement)
	schemaRequired := requiredSet(schema)
	schemaProps := schemaProperties(schema)
	requirementProps := schemaProperties(requirement)
	additionalFalse := additionalPropertiesFalse(schema)

	for _, key := range sortedSetKeys(requirementRequired) {
		propPath := path.append("properties", key)
		_, schemaHasProperty := schemaProps[key]
		if additionalFalse && !schemaHasProperty {
			*findings = append(*findings, finding(
				FindingKindRequirementUnsatisfied,
				propPath,
				VerdictBreaking,
				"required field %q is not satisfiable because producer additionalProperties is false and the field is outside properties",
				key,
			))
			continue
		}
		if _, schemaRequires := schemaRequired[key]; !schemaRequires {
			*findings = append(*findings, finding(
				FindingKindRequirementUnsatisfied,
				propPath,
				VerdictBreaking,
				"consumer requires field %q but producer schema does not require it",
				key,
			))
		}
	}

	for _, key := range sortedKeys(requirementProps) {
		requirementProp, requirementOK := requirementProps[key].(map[string]any)
		if !requirementOK {
			continue
		}
		propPath := path.append("properties", key)
		schemaProp, schemaOK := schemaProps[key].(map[string]any)
		if !schemaOK {
			// With additionalProperties: false the producer can never emit this
			// field at all: if the consumer REQUIRES it, the required-key loop
			// above already emitted a breaking RequirementUnsatisfied; if it is
			// optional, a field that never arrives satisfies vacuously. Either
			// way an unknown here would be noise.
			if _, requiresType := schemaTypes(requirementProp["type"]); requiresType && !additionalFalse {
				*findings = append(*findings, finding(
					FindingKindRequirementUnknown,
					propPath.append("type"),
					VerdictUnknown,
					"consumer requires a type for field %q but producer has no property schema to prove it",
					key,
				))
			}
			continue
		}
		satisfySchema(findings, propPath, schemaProp, requirementProp)
	}
}

func satisfyType(findings *[]Finding, path schemaPath, schema, requirement map[string]any) {
	requirementTypes, requirementHasTypes := schemaTypes(requirement["type"])
	if !requirementHasTypes {
		return
	}

	producerTypes, schemaHasTypes := schemaTypes(schema["type"])
	typePath := path.append("type")
	if !schemaHasTypes {
		*findings = append(*findings, finding(
			FindingKindRequirementUnknown,
			typePath,
			VerdictUnknown,
			"consumer requires type %s but producer type is unspecified",
			formatTypes(requirementTypes),
		))
		return
	}

	if !typeSetAllowsAll(requirementTypes, producerTypes) {
		*findings = append(*findings, finding(
			FindingKindRequirementTypeMismatch,
			typePath,
			VerdictBreaking,
			"producer type %s does not satisfy consumer type %s",
			formatTypes(producerTypes),
			formatTypes(requirementTypes),
		))
	}
}

func schemaProperties(schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return map[string]any{}
	}
	return props
}

func requiredSet(schema map[string]any) map[string]struct{} {
	values, ok := stringSlice(schema["required"])
	if !ok {
		return map[string]struct{}{}
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		set[value] = struct{}{}
	}
	return set
}

func stringSlice(raw any) ([]string, bool) {
	switch v := raw.(type) {
	case []any:
		values := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			values = append(values, s)
		}
		return values, true
	case []string:
		return append([]string(nil), v...), true
	default:
		return nil, false
	}
}

type typeSet map[string]struct{}

func schemaTypes(raw any) (typeSet, bool) {
	switch v := raw.(type) {
	case string:
		return typeSet{v: {}}, true
	case []any:
		values := make(typeSet, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			values[s] = struct{}{}
		}
		return values, true
	case []string:
		values := make(typeSet, len(v))
		for _, item := range v {
			values[item] = struct{}{}
		}
		return values, true
	default:
		return nil, false
	}
}

func typeSetsEqual(a, b typeSet) bool {
	if len(a) != len(b) {
		return false
	}
	for item := range a {
		if _, ok := b[item]; !ok {
			return false
		}
	}
	return true
}

func typeSetAllowsAll(allowed, required typeSet) bool {
	for item := range required {
		if typeSetAllows(allowed, item) {
			continue
		}
		return false
	}
	return true
}

func typeSetAllows(allowed typeSet, item string) bool {
	if _, ok := allowed[item]; ok {
		return true
	}
	return item == "integer" && hasType(allowed, "number")
}

func hasType(types typeSet, item string) bool {
	_, ok := types[item]
	return ok
}

func formatTypes(types typeSet) string {
	values := make([]string, 0, len(types))
	for item := range types {
		values = append(values, item)
	}
	sort.Strings(values)
	return strings.Join(values, "|")
}

func enumValues(raw any) ([]string, bool) {
	var result []string
	switch values := raw.(type) {
	case []any:
		result = make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, canonicalJSON(value))
		}
	case []string:
		result = make([]string, 0, len(values))
		for _, value := range values {
			result = append(result, canonicalJSON(value))
		}
	default:
		return nil, false
	}
	sort.Strings(result)
	return result, true
}

func enumDifference(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, item := range b {
		bSet[item] = struct{}{}
	}

	var diff []string
	for _, item := range a {
		if _, ok := bSet[item]; !ok {
			diff = append(diff, item)
		}
	}
	sort.Strings(diff)
	return diff
}

func formatEnumValues(values []string) string {
	if len(values) == 0 {
		return ""
	}
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return strings.Join(sorted, ", ")
}

func canonicalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%#v", value)
	}
	return string(data)
}

func additionalPropertiesFalse(schema map[string]any) bool {
	value, ok := schema["additionalProperties"].(bool)
	return ok && !value
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedSetKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

type schemaPath []string

func (p schemaPath) append(parts ...string) schemaPath {
	next := make(schemaPath, 0, len(p)+len(parts))
	next = append(next, p...)
	next = append(next, parts...)
	return next
}

func (p schemaPath) string() string {
	return strings.Join(p, ".")
}

func finding(kind FindingKind, path schemaPath, verdict Verdict, format string, args ...any) Finding {
	return Finding{
		Kind:    kind,
		Path:    path.string(),
		Detail:  fmt.Sprintf(format, args...),
		Verdict: verdict,
	}
}
