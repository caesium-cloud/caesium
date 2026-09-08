package run

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
)

// DefaultBaselineMinSamples is the cold-start floor when
// CAESIUM_BASELINE_MIN_SAMPLES is unset or nonsensical: below this many clean
// samples a deltaFromBaseline verdict is "seeding" — recorded, never enforced.
const DefaultBaselineMinSamples = 5

// Assertion kinds recorded on a DataViolation. They are the stable enum a UI
// or a backtest groups by, so they are the declared YAML key names rather than
// prose.
const (
	// AssertionMin / AssertionMax are the absolute bounds. They enforce from
	// run one — no baseline, no cold start, no grace.
	AssertionMin = "min"
	AssertionMax = "max"
	// AssertionDeltaFromBaseline compares the observed value against the
	// rolling median of the last N clean samples.
	AssertionDeltaFromBaseline = "deltaFromBaseline"
	// AssertionMaxLag is the freshness assertion: how far the emitted watermark
	// may trail the evaluation time.
	AssertionMaxLag = "maxLag"
	// AssertionMissing is the violation a declared metric that never arrived
	// produces. A step that stops emitting `rowCount` must not silently pass,
	// so "no sample" is a breach of the contract rather than a skipped check.
	AssertionMissing = "missing"
)

// DataViolation is one recorded data-assertion breach — the data-quality
// parallel of pkgtask.SchemaViolation. Instances are persisted verbatim as the
// TaskRun.DataViolations JSON array, so the field names below are a stable API
// shape (Plan 3's assertion backtest replays the same struct):
//
//	{
//	  "dataset": "warehouse/orders",
//	  "metric": "rowCount",
//	  "assertion": "deltaFromBaseline",
//	  "observed": 12,
//	  "bound": 5000,
//	  "delta_from_baseline": "50%",
//	  "baseline_median": 10000,
//	  "baseline_samples": 7,
//	  "seeding": false,
//	  "message": "..."
//	}
//
// Bound is the numeric bound the observed value had to satisfy: the declared
// min or max, the maximum allowed absolute deviation from the median for
// deltaFromBaseline, or the maxLag in seconds for a freshness assertion. It and
// Observed are pointers so "no bound"/"never emitted" is distinguishable from a
// real zero.
type DataViolation struct {
	// Namespace is reserved (empty in v1) and carried so a namespaced dataset
	// needs no shape change later.
	Namespace string `json:"namespace,omitempty"`
	Dataset   string `json:"dataset"`
	Metric    string `json:"metric"`
	// Assertion is one of the Assertion* constants.
	Assertion string `json:"assertion"`
	// Observed is the emitted metric value; nil when the metric never arrived.
	Observed *float64 `json:"observed,omitempty"`
	// Bound is the bound that was breached; nil for a missing metric.
	Bound *float64 `json:"bound,omitempty"`
	// DeltaFromBaseline echoes the declared percentage string ("50%") so the
	// recorded verdict is readable without re-reading the manifest.
	DeltaFromBaseline string `json:"delta_from_baseline,omitempty"`
	// BaselineMedian / BaselineSamples snapshot the rolling baseline the
	// verdict was computed against. Absent for absolute bounds.
	BaselineMedian  *float64 `json:"baseline_median,omitempty"`
	BaselineSamples int      `json:"baseline_samples,omitempty"`
	// Seeding marks a cold-start verdict: the baseline held fewer than the
	// configured minimum samples, so this violation is recorded and surfaced
	// but NEVER enforced, whatever onViolation says.
	Seeding bool `json:"seeding,omitempty"`
	// Message is the human-readable rendering, mirroring how a
	// pkgtask.SchemaViolation carries its own message.
	Message string `json:"message"`
}

// Enforceable reports whether this violation may escalate (fail a task, and —
// once Stream C lands — hold a dataset). A seeding verdict never can.
func (v DataViolation) Enforceable() bool { return !v.Seeding }

// String renders the violation for a task failure message: it names the
// dataset, the metric and the assertion, which is what an operator reading a
// red run needs to find the contract that broke.
func (v DataViolation) String() string {
	return fmt.Sprintf("dataset %q metric %q assertion %q: %s", v.Dataset, v.Metric, v.Assertion, v.Message)
}

// BaselineMinSamples is the configured cold-start floor, falling back to
// DefaultBaselineMinSamples when the environment carries a nonsensical value.
func BaselineMinSamples() int {
	if n := env.Variables().BaselineMinSamples; n > 0 {
		return n
	}
	return DefaultBaselineMinSamples
}

// AssertionMetrics returns the sorted, de-duplicated set of emitted metric keys
// a declared assertion block reads — the keys whose absence is a violation.
// Apply-time validation already guarantees one assertion per metric.
//
// It has no in-tree production caller today (the evaluator asks the pure core
// directly, and baseline loading uses the narrower BaselineMetrics); it is kept
// exported as the companion primitive to EvaluateAssertions, which Plan 3's
// backtest needs to know which metrics of a recorded history a candidate
// contract reads.
func AssertionMetrics(assertions *jobdef.DatasetAssertions) []string {
	if assertions.IsEmpty() {
		return nil
	}
	seen := make(map[string]struct{}, 4)
	add := func(metric string) {
		if metric = strings.TrimSpace(metric); metric != "" {
			seen[metric] = struct{}{}
		}
	}
	if assertions.RowCount != nil {
		add(jobdef.AssertionMetricName(assertions.RowCount, jobdef.DefaultRowCountMetric))
	}
	if assertions.NullRate != nil {
		add(jobdef.AssertionMetricName(assertions.NullRate, jobdef.DefaultNullRateMetric))
	}
	if assertions.Freshness != nil {
		add(assertions.Freshness.Watermark)
	}
	for i := range assertions.Custom {
		add(strings.TrimSpace(assertions.Custom[i].Metric))
	}
	metrics := make([]string, 0, len(seen))
	for metric := range seen {
		metrics = append(metrics, metric)
	}
	sort.Strings(metrics)
	return metrics
}

// BaselineMetrics returns the subset of AssertionMetrics whose evaluation
// actually needs history: the metrics a `deltaFromBaseline` bound reads.
// Absolute min/max bounds enforce from run one and the freshness assertion
// measures lag from the evaluation instant, so neither needs a baseline — and
// loading one for them would be an indexed read per succeeded task that nothing
// consumes.
func BaselineMetrics(assertions *jobdef.DatasetAssertions) []string {
	if assertions.IsEmpty() {
		return nil
	}
	metrics := make([]string, 0, 4)
	add := func(spec *jobdef.AssertionSpec, metric string) {
		if spec == nil || strings.TrimSpace(spec.DeltaFromBaseline) == "" {
			return
		}
		if metric = strings.TrimSpace(metric); metric != "" {
			metrics = append(metrics, metric)
		}
	}
	add(assertions.RowCount, jobdef.AssertionMetricName(assertions.RowCount, jobdef.DefaultRowCountMetric))
	add(assertions.NullRate, jobdef.AssertionMetricName(assertions.NullRate, jobdef.DefaultNullRateMetric))
	for i := range assertions.Custom {
		add(&assertions.Custom[i], assertions.Custom[i].Metric)
	}
	sort.Strings(metrics)
	return metrics
}

// EvaluateAssertions is the PURE CORE of the data circuit breaker: it turns a
// declared contract plus one run's observations into verdicts, and does
// nothing else. It opens no transaction, writes no row, touches no hold, needs
// no executor and no live run — so Plan 3's assertion backtest
// (backtesting.md F3) can replay it over recorded DatasetMetric history exactly
// as the live evaluator runs it, and any change to the semantics is a change to
// one function that a table test pins.
//
// Inputs:
//   - dataset: the declared dataset identity the verdicts are attributed to.
//   - assertions: the declared spec (as persisted on
//     models.DatasetDeclaration.AssertionsJSON). Nil/empty yields no verdicts.
//   - observed: the metric values this run emitted for this dataset. A declared
//     metric MISSING from this map is itself a violation.
//   - baselines: the rolling baseline per metric, keyed exactly as `observed`
//     is. A nil or absent entry means "no history"; a deltaFromBaseline
//     assertion then yields no verdict at all rather than a fake breach against
//     a median of zero.
//   - minSamples: the cold-start floor (see BaselineMinSamples). A baseline
//     below it makes deltaFromBaseline verdicts Seeding — recorded, never
//     enforced. Absolute bounds ignore it entirely. A value <= 0 disables the
//     grace period.
//   - at: the evaluation instant, used only by the freshness assertion so a
//     backtest can ask "was this fresh AT that run?" rather than "is it fresh
//     now?".
//
// Verdicts come back in a deterministic order (rowCount, nullRate, freshness,
// then custom in declaration order) so a persisted array is stable across runs.
func EvaluateAssertions(
	dataset string,
	assertions *jobdef.DatasetAssertions,
	observed map[string]float64,
	baselines map[string]*BaselineStats,
	minSamples int,
	at time.Time,
) []DataViolation {
	if assertions.IsEmpty() {
		return nil
	}

	var violations []DataViolation
	evaluate := func(spec *jobdef.AssertionSpec, metric string) {
		if spec == nil {
			return
		}
		violations = append(violations,
			EvaluateAssertion(dataset, metric, *spec, lookup(observed, metric), baselines[metric], minSamples)...)
	}

	if assertions.RowCount != nil {
		evaluate(assertions.RowCount, jobdef.AssertionMetricName(assertions.RowCount, jobdef.DefaultRowCountMetric))
	}
	if assertions.NullRate != nil {
		evaluate(assertions.NullRate, jobdef.AssertionMetricName(assertions.NullRate, jobdef.DefaultNullRateMetric))
	}
	if assertions.Freshness != nil {
		watermark := strings.TrimSpace(assertions.Freshness.Watermark)
		violations = append(violations,
			EvaluateFreshnessAssertion(dataset, *assertions.Freshness, lookup(observed, watermark), at)...)
	}
	for i := range assertions.Custom {
		spec := &assertions.Custom[i]
		evaluate(spec, strings.TrimSpace(spec.Metric))
	}
	return violations
}

// EvaluateAssertion evaluates ONE declared bound triple against one observation.
// It is the smallest pure unit — side-effect free, no I/O — and is exported
// alongside EvaluateAssertions so a caller holding a single spec (a backtest
// sweeping candidate thresholds) need not synthesise a whole assertion block.
//
// observed is nil when the step did not emit the metric, which yields exactly
// one AssertionMissing violation and no bound checks: the contract is already
// broken and reporting "min breached" against a value nobody emitted would be a
// lie.
func EvaluateAssertion(
	dataset, metric string,
	spec jobdef.AssertionSpec,
	observed *float64,
	baseline *BaselineStats,
	minSamples int,
) []DataViolation {
	if observed == nil {
		return []DataViolation{{
			Dataset:           dataset,
			Metric:            metric,
			Assertion:         AssertionMissing,
			DeltaFromBaseline: strings.TrimSpace(spec.DeltaFromBaseline),
			Message: fmt.Sprintf(
				"declared metric %q was never emitted by this run; a step that stops reporting a metric does not pass its assertion",
				metric),
		}}
	}

	value := *observed
	var violations []DataViolation

	if spec.Min != nil && value < *spec.Min {
		violations = append(violations, DataViolation{
			Dataset:   dataset,
			Metric:    metric,
			Assertion: AssertionMin,
			Observed:  floatPtr(value),
			Bound:     floatPtr(*spec.Min),
			Message:   fmt.Sprintf("observed %s is below the declared min %s", formatValue(value), formatValue(*spec.Min)),
		})
	}
	if spec.Max != nil && value > *spec.Max {
		violations = append(violations, DataViolation{
			Dataset:   dataset,
			Metric:    metric,
			Assertion: AssertionMax,
			Observed:  floatPtr(value),
			Bound:     floatPtr(*spec.Max),
			Message:   fmt.Sprintf("observed %s exceeds the declared max %s", formatValue(value), formatValue(*spec.Max)),
		})
	}

	if delta := evaluateDelta(dataset, metric, spec, value, baseline, minSamples); delta != nil {
		violations = append(violations, *delta)
	}
	return violations
}

// evaluateDelta implements the deltaFromBaseline rule documented on
// jobdef.AssertionSpec: |value − median| must be ≤ pct × median.
//
// It deliberately returns NO verdict in three cases, because each of them would
// otherwise manufacture a breach out of missing information:
//   - no declared deltaFromBaseline, or an unparseable one (apply-time
//     validation rejects those, so a bad value here is a corrupt registry row,
//     not an author's intent to enforce something);
//   - no baseline at all (nil, or zero samples) — the first run of a dataset
//     has nothing to deviate from;
//   - a baseline median of exactly zero, where "±50% of the median" is a
//     zero-width band that every nonzero value breaches.
//
// A verdict computed against a baseline SHORTER than minSamples is returned
// with Seeding set: recorded and visible, never enforced.
//
// One consequence of the second case is worth stating plainly, because it is
// the intended fallback rather than an oversight: samples rejected by an
// enforced violation are excluded from the baseline (models.DatasetMetric
// Violated), so a dataset that breaches an ABSOLUTE bound on every single run
// eventually has no clean history at all — and its deltaFromBaseline assertion
// then falls back to "no verdict" rather than reporting a breach. Nothing goes
// silently green: the min/max bound that rejected every sample is still firing
// on every run, which is the signal an operator acts on. A dataset whose delta
// assertion has gone quiet is one whose absolute bounds are already screaming.
func evaluateDelta(dataset, metric string, spec jobdef.AssertionSpec, value float64, baseline *BaselineStats, minSamples int) *DataViolation {
	raw := strings.TrimSpace(spec.DeltaFromBaseline)
	if raw == "" {
		return nil
	}
	pct, err := jobdef.ParseDeltaFromBaseline(raw)
	if err != nil {
		return nil
	}
	if baseline == nil || baseline.Samples == 0 {
		return nil
	}
	median := baseline.Median
	if median == 0 {
		return nil
	}

	allowed := pct * math.Abs(median)
	deviation := math.Abs(value - median)
	if deviation <= allowed {
		return nil
	}

	seeding := minSamples > 0 && baseline.Samples < minSamples
	message := fmt.Sprintf("observed %s deviates %s from the baseline median %s (%d sample(s)), more than the declared %s",
		formatValue(value), formatValue(deviation), formatValue(median), baseline.Samples, raw)
	if seeding {
		message += fmt.Sprintf("; seeding: fewer than %d clean samples, so this verdict is recorded but not enforced", minSamples)
	}
	return &DataViolation{
		Dataset:           dataset,
		Metric:            metric,
		Assertion:         AssertionDeltaFromBaseline,
		Observed:          floatPtr(value),
		Bound:             floatPtr(allowed),
		DeltaFromBaseline: raw,
		BaselineMedian:    floatPtr(median),
		BaselineSamples:   baseline.Samples,
		Seeding:           seeding,
		Message:           message,
	}
}

// EvaluateFreshnessAssertion checks a declared watermark against its maxLag.
// The watermark arrives as the emitted metric's value — epoch seconds, which is
// how pkg/task normalises an RFC3339 metric — and the lag is measured from
// `at`, not from time.Now(), so a replay reproduces the verdict of the moment.
// A watermark that never arrived is an AssertionMissing violation, exactly like
// any other declared metric.
func EvaluateFreshnessAssertion(dataset string, spec jobdef.FreshnessAssertion, observed *float64, at time.Time) []DataViolation {
	metric := strings.TrimSpace(spec.Watermark)
	if metric == "" {
		return nil
	}
	if observed == nil {
		return []DataViolation{{
			Dataset:   dataset,
			Metric:    metric,
			Assertion: AssertionMissing,
			Message: fmt.Sprintf(
				"declared watermark metric %q was never emitted by this run; a step that stops reporting a watermark does not pass its freshness assertion",
				metric),
		}}
	}
	maxLag, err := time.ParseDuration(strings.TrimSpace(spec.MaxLag))
	if err != nil || maxLag <= 0 {
		return nil
	}

	// Epoch seconds arrive as a float64 and may carry a fraction (pkg/task
	// preserves it), so convert through nanoseconds rather than truncating.
	watermark := time.Unix(0, int64(*observed*float64(time.Second))).UTC()
	lag := at.UTC().Sub(watermark)
	if lag <= maxLag {
		return nil
	}
	return []DataViolation{{
		Dataset:   dataset,
		Metric:    metric,
		Assertion: AssertionMaxLag,
		Observed:  floatPtr(*observed),
		Bound:     floatPtr(maxLag.Seconds()),
		Message: fmt.Sprintf("watermark %s lags %s behind %s, more than the declared maxLag %s",
			watermark.Format(time.RFC3339), lag.Round(time.Second), at.UTC().Format(time.RFC3339), spec.MaxLag),
	}}
}

// lookup returns a pointer to the observed value, or nil when the metric was
// never emitted — the distinction the whole "missing metric is a violation"
// rule rests on.
func lookup(observed map[string]float64, metric string) *float64 {
	if metric == "" {
		return nil
	}
	value, ok := observed[metric]
	if !ok {
		return nil
	}
	return &value
}

func floatPtr(v float64) *float64 { return &v }

// formatValue renders a metric value without a trailing ".000000" on the
// integers most of these metrics are (row counts), while keeping precision on
// the ratios (null rates).
func formatValue(v float64) string {
	return strconv.FormatFloat(v, 'g', -1, 64)
}
