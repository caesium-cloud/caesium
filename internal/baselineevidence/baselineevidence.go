// Package baselineevidence withholds baseline evidence that was not computed
// over a run's own namespace (identity-and-access B4). A data-assertion verdict
// recorded before baselines were namespace-owned may hold another owner's
// samples. It lives below internal/run because internal/incident, which run
// imports, shapes the same verdicts.
package baselineevidence

import "strings"

const (
	// ScopeNamespace is a verdict's baseline_scope for a baseline computed over
	// the evaluated run's own tenancy namespace only.
	ScopeNamespace = "namespace"

	// AssertionDelta is the assertion kind whose bound derives from the baseline.
	AssertionDelta = "deltaFromBaseline"

	// Withheld replaces the message of a verdict whose baseline evidence is not
	// served.
	Withheld = "baseline evidence withheld: recorded before namespace-owned baselines"
)

// Withholds reports whether a verdict carries baseline evidence (the median,
// the sample count, the bound derived from them and the message that renders
// them) that was not computed over the evaluated run's own namespace.
func Withholds(scope, assertion string, hasMedian, hasSamples bool) bool {
	if scope == ScopeNamespace {
		return false
	}
	return hasMedian || hasSamples || assertion == AssertionDelta
}

// WithholdJSON applies the rule to a decoded run or task payload (an event
// payload or the lifecycle bridge's event data): its own data_violations and
// error, and each tasks[] entry's. A data_violations value that is not a list
// is dropped.
func WithholdJSON(payload map[string]any) {
	var withheld []string
	if tasks, ok := payload["tasks"].([]any); ok {
		for _, task := range tasks {
			if fields, ok := task.(map[string]any); ok {
				withheld = append(withheld, withholdViolations(fields)...)
			}
		}
	}
	withheld = append(withheld, withholdViolations(payload)...)
	if raw, ok := payload["error"].(string); ok {
		payload["error"] = ReplaceMessages(raw, withheld)
	}
}

func withholdViolations(fields map[string]any) []string {
	raw, present := fields["data_violations"]
	if !present || raw == nil {
		return nil
	}
	list, ok := raw.([]any)
	if !ok {
		delete(fields, "data_violations")
		return nil
	}
	var withheld []string
	for i, item := range list {
		v, ok := item.(map[string]any)
		if !ok {
			list[i] = nil
			continue
		}
		scope, _ := v["baseline_scope"].(string)
		assertion, _ := v["assertion"].(string)
		_, hasMedian := v["baseline_median"]
		_, hasSamples := v["baseline_samples"]
		if !Withholds(scope, assertion, hasMedian, hasSamples) {
			continue
		}
		if message, ok := v["message"].(string); ok {
			withheld = append(withheld, message)
		}
		delete(v, "baseline_median")
		delete(v, "baseline_samples")
		if assertion == AssertionDelta {
			delete(v, "bound")
		}
		v["message"] = Withheld
	}
	if raw, ok := fields["error"].(string); ok {
		fields["error"] = ReplaceMessages(raw, withheld)
	}
	return withheld
}

// ReplaceMessages replaces every withheld verdict message quoted in text.
func ReplaceMessages(text string, messages []string) string {
	for _, message := range messages {
		if message != "" && message != Withheld {
			text = strings.ReplaceAll(text, message, Withheld)
		}
	}
	return text
}
