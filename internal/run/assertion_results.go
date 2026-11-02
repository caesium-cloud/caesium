package run

import (
	"encoding/json"
	"math"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/baselineevidence"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

// Outcomes recorded on an AssertionResult. They say what the evaluator DECIDED
// at the time, which is not always "passed or broken":
//
//   - passed: the declared check was evaluated and held. A delta check judged
//     against a baseline shorter than the cold-start floor is still `passed`
//     but carries Seeding, because a pass against three samples is not the same
//     evidence as a pass against twenty.
//   - violated: the check was evaluated and broke. Disposition says what the
//     breach did.
//   - unavailable: the metric could not be observed because the marker stream
//     was lost (Reason names how). Nothing was decided.
//   - undecided: a deltaFromBaseline check abstained because there was nothing
//     to compare against (Reason names which). Nothing was decided.
const (
	AssertionOutcomePassed      = "passed"
	AssertionOutcomeViolated    = "violated"
	AssertionOutcomeUnavailable = "unavailable"
	AssertionOutcomeUndecided   = "undecided"
)

// Reasons carried on an `undecided` result.
const (
	// UndecidedNoBaseline: no clean history existed for the metric.
	UndecidedNoBaseline = "no_baseline"
	// UndecidedZeroMedian: the baseline median was exactly zero, where a
	// percentage band has no width.
	UndecidedZeroMedian = "zero_median"
)

// Dispositions carried on a `violated` result: what the breach did. They are
// the same applied-disposition labels caesium_data_assertions_total uses.
const (
	DispositionWarn    = assertionResultWarn
	DispositionHold    = assertionResultHold
	DispositionFail    = assertionResultFail
	DispositionSeeding = assertionResultSeeding
)

// AssertionResult is the evaluator's decision on ONE declared check of one task
// instance, recorded when it was made. Instances are persisted verbatim as the
// TaskRun.DataAssertionResults JSON array, so a read shows what the evaluator
// decided against the baseline and the manifest of that moment, never a
// recomputation from today's.
//
// It is a superset of DataViolation's evidence: a `violated` result carries the
// same observed/bound/baseline fields, and a `passed` one carries the same
// fields for the check that held.
type AssertionResult struct {
	Namespace string `json:"namespace,omitempty"`
	Dataset   string `json:"dataset"`
	Metric    string `json:"metric"`
	// Assertion is min, max, deltaFromBaseline, maxLag, or — for a metric that
	// could not be judged at all — missing / unavailable.
	Assertion string `json:"assertion"`
	Outcome   string `json:"outcome"`
	// Disposition is set only on a violated result.
	Disposition string `json:"disposition,omitempty"`
	// Observed is nil when the metric never arrived.
	Observed *float64 `json:"observed,omitempty"`
	// Bound is the declared min / max, the maximum allowed deviation from the
	// median for deltaFromBaseline, or the maxLag in seconds.
	Bound *float64 `json:"bound,omitempty"`
	// BoundWithheld replaces Bound on a read that may not receive a bound
	// derived from another owner's baseline. It is set at read time only.
	BoundWithheld     bool     `json:"bound_withheld,omitempty"`
	DeltaFromBaseline string   `json:"delta_from_baseline,omitempty"`
	BaselineMedian    *float64 `json:"baseline_median,omitempty"`
	BaselineSamples   int      `json:"baseline_samples,omitempty"`
	BaselineScope     string   `json:"baseline_scope,omitempty"`
	// Seeding marks a delta check judged against fewer clean samples than the
	// cold-start floor: recorded, never enforced.
	Seeding bool `json:"seeding,omitempty"`
	// Reason names why an unavailable or undecided result decided nothing.
	Reason string `json:"reason,omitempty"`
	// Message is the violation's rendering; empty on every other outcome.
	Message string `json:"message,omitempty"`
}

// Clean reports whether this result is the unqualified good outcome: decided,
// held, and not merely held against a baseline too short to trust.
func (r AssertionResult) Clean() bool {
	return r.Outcome == AssertionOutcomePassed && !r.Seeding
}

// violationDisposition is the applied disposition of one violation under a
// contract's onViolation mode — the label caesium_data_assertions_total counts
// and the Disposition a violated result records, from one definition.
func violationDisposition(v DataViolation, onViolation string) string {
	switch {
	case v.Assertion == AssertionUnavailable:
		return assertionResultUnavailable
	case !v.Enforceable():
		return assertionResultSeeding
	case onViolation == jobdef.DatasetOnViolationFail:
		return assertionResultFail
	case onViolation == jobdef.DatasetOnViolationHold:
		return assertionResultHold
	default:
		return assertionResultWarn
	}
}

// DescribeAssertions is the pure companion of EvaluateAssertions: given the
// SAME inputs and the verdicts EvaluateAssertions returned for them, it lists
// every declared check with its outcome — the ones that held as well as the
// ones that broke. It decides nothing itself: a check is `violated` exactly
// when EvaluateAssertions returned a violation for it, and it reads the
// baseline only to describe it (it never feeds one), so recording passes cannot
// change a verdict, an evaluation order or a baseline.
//
// Results come back in the evaluator's order (rowCount, nullRate, freshness,
// then custom in declaration order), one per declared check.
func DescribeAssertions(
	dataset, namespace string,
	assertions *jobdef.DatasetAssertions,
	observed map[string]float64,
	baselines map[string]*BaselineStats,
	minSamples int,
	violations []DataViolation,
	onViolation string,
) []AssertionResult {
	if assertions.IsEmpty() {
		return nil
	}
	broke := make(map[AssertionRef]DataViolation, len(violations))
	for _, v := range violations {
		broke[AssertionRef{Metric: v.Metric, Assertion: v.Assertion}] = v
	}

	var results []AssertionResult
	base := func(metric, assertion string) AssertionResult {
		return AssertionResult{Namespace: namespace, Dataset: dataset, Metric: metric, Assertion: assertion}
	}
	// violated turns a recorded violation into its result.
	violated := func(v DataViolation) AssertionResult {
		r := base(v.Metric, v.Assertion)
		r.Observed, r.Bound = v.Observed, v.Bound
		r.DeltaFromBaseline = v.DeltaFromBaseline
		r.BaselineMedian, r.BaselineSamples, r.BaselineScope = v.BaselineMedian, v.BaselineSamples, v.BaselineScope
		r.Seeding, r.Reason, r.Message = v.Seeding, v.Reason, v.Message
		if v.Assertion == AssertionUnavailable {
			r.Outcome = AssertionOutcomeUnavailable
			return r
		}
		r.Outcome = AssertionOutcomeViolated
		r.Disposition = violationDisposition(v, onViolation)
		return r
	}
	// metricGone reports a metric the run never delivered: the evaluator
	// returned one missing/unavailable violation for it and judged nothing else.
	metricGone := func(metric string) (DataViolation, bool) {
		if v, ok := broke[AssertionRef{Metric: metric, Assertion: AssertionUnavailable}]; ok {
			return v, true
		}
		v, ok := broke[AssertionRef{Metric: metric, Assertion: AssertionMissing}]
		return v, ok
	}
	// bounded describes a min / max / maxLag check: violated when the
	// evaluator said so, passed otherwise.
	bounded := func(metric, assertion string, value float64, bound float64) AssertionResult {
		if v, ok := broke[AssertionRef{Metric: metric, Assertion: assertion}]; ok {
			return violated(v)
		}
		r := base(metric, assertion)
		r.Outcome, r.Observed, r.Bound = AssertionOutcomePassed, floatPtr(value), floatPtr(bound)
		return r
	}

	spec := func(s *jobdef.AssertionSpec, metric string) {
		if s == nil || metric == "" {
			return
		}
		value, emitted := observed[metric]
		if !emitted {
			if v, ok := metricGone(metric); ok {
				results = append(results, violated(v))
			}
			return
		}
		if s.Min != nil {
			results = append(results, bounded(metric, AssertionMin, value, *s.Min))
		}
		if s.Max != nil {
			results = append(results, bounded(metric, AssertionMax, value, *s.Max))
		}
		raw := strings.TrimSpace(s.DeltaFromBaseline)
		if raw == "" {
			return
		}
		pct, err := jobdef.ParseDeltaFromBaseline(raw)
		if err != nil {
			return
		}
		if v, ok := broke[AssertionRef{Metric: metric, Assertion: AssertionDeltaFromBaseline}]; ok {
			results = append(results, violated(v))
			return
		}
		r := base(metric, AssertionDeltaFromBaseline)
		r.DeltaFromBaseline, r.Observed = raw, floatPtr(value)
		baseline := baselines[metric]
		switch {
		case baseline == nil || baseline.Samples == 0:
			r.Outcome, r.Reason = AssertionOutcomeUndecided, UndecidedNoBaseline
		case baseline.Median == 0:
			r.Outcome, r.Reason = AssertionOutcomeUndecided, UndecidedZeroMedian
			r.BaselineMedian, r.BaselineSamples, r.BaselineScope = floatPtr(0), baseline.Samples, baseline.Scope
		default:
			r.Outcome = AssertionOutcomePassed
			r.Bound = floatPtr(pct * math.Abs(baseline.Median))
			r.BaselineMedian, r.BaselineSamples, r.BaselineScope = floatPtr(baseline.Median), baseline.Samples, baseline.Scope
			r.Seeding = minSamples > 0 && baseline.Samples < minSamples
		}
		results = append(results, r)
	}

	if assertions.RowCount != nil {
		spec(assertions.RowCount, jobdef.AssertionMetricName(assertions.RowCount, jobdef.DefaultRowCountMetric))
	}
	if assertions.NullRate != nil {
		spec(assertions.NullRate, jobdef.AssertionMetricName(assertions.NullRate, jobdef.DefaultNullRateMetric))
	}
	if f := assertions.Freshness; f != nil {
		watermark := strings.TrimSpace(f.Watermark)
		maxLag, err := time.ParseDuration(strings.TrimSpace(f.MaxLag))
		if watermark != "" {
			if value, emitted := observed[watermark]; !emitted {
				if v, ok := metricGone(watermark); ok {
					results = append(results, violated(v))
				}
			} else if err == nil && maxLag > 0 {
				results = append(results, bounded(watermark, AssertionMaxLag, value, maxLag.Seconds()))
			}
		}
	}
	for i := range assertions.Custom {
		c := &assertions.Custom[i]
		spec(c, strings.TrimSpace(c.Metric))
	}
	return results
}

// AssertionVerdict is the single word a task instance's results reduce to, so
// a group of N instances can be counted without pretending one answer covers
// them all.
const (
	// VerdictPassed: every recorded check passed and none was cold-start.
	VerdictPassed = "passed"
	// VerdictSeeding: nothing broke, but at least one check was judged against
	// a baseline too short to trust.
	VerdictSeeding = "seeding"
	// VerdictUndecided: nothing broke, but at least one check had nothing to
	// compare against or could not be observed.
	VerdictUndecided = "undecided"
	// VerdictViolated: at least one check broke.
	VerdictViolated = "violated"
	// VerdictNotEvaluated: the instance was served from cache; no assertion ran.
	VerdictNotEvaluated = "not_evaluated"
	// VerdictNotRecorded: the instance carries no result record (it predates
	// recording, or its evaluation never reached the seam). It says nothing
	// about whether anything passed.
	VerdictNotRecorded = "not_recorded"
)

// VerdictOf reduces one instance's results to its worst outcome.
func VerdictOf(results []AssertionResult) string {
	if len(results) == 0 {
		return VerdictNotRecorded
	}
	verdict := VerdictPassed
	for _, r := range results {
		switch {
		case r.Outcome == AssertionOutcomeViolated:
			return VerdictViolated
		case r.Outcome == AssertionOutcomeUnavailable, r.Outcome == AssertionOutcomeUndecided:
			verdict = VerdictUndecided
		case r.Seeding && verdict == VerdictPassed:
			verdict = VerdictSeeding
		}
	}
	return verdict
}

// WithholdUnownedBaselineEvidenceFromResults is what a namespace-confined
// reader receives of recorded results: a result whose baseline evidence was not
// computed over the evaluated run's own namespace loses its median, sample
// count, the bound derived from them and its message — the rule
// WithholdUnownedBaselineEvidence applies to violations, from the same
// predicate. A withheld delta result says so with BoundWithheld rather than
// showing a bound that looks like "no bound". It modifies results in place and
// returns whether anything was withheld.
func WithholdUnownedBaselineEvidenceFromResults(results []AssertionResult) bool {
	withheld := false
	for i := range results {
		r := &results[i]
		if r.BaselineMedian == nil && r.BaselineSamples == 0 && r.Outcome != AssertionOutcomeViolated {
			// An abstention or a bounded check carries no baseline evidence.
			continue
		}
		if !baselineevidence.Withholds(r.BaselineScope, r.Assertion, r.BaselineMedian != nil, r.BaselineSamples != 0) {
			continue
		}
		withheld = true
		r.BaselineMedian, r.BaselineSamples = nil, 0
		if r.Message != "" {
			r.Message = BaselineEvidenceWithheld
		}
		if r.Assertion == AssertionDeltaFromBaseline {
			r.Bound, r.BoundWithheld = nil, true
		}
	}
	return withheld
}

// decodeAssertionResults reads the persisted array. A blob that does not decode
// is treated as no record at all rather than guessed at.
func decodeAssertionResults(raw []byte) []AssertionResult {
	if len(raw) == 0 {
		return nil
	}
	var results []AssertionResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return nil
	}
	return results
}
