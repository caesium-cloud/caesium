package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// describe runs the pure evaluator and its describer over the same inputs, the
// way evaluateContracts does.
func describe(
	assertions *jobdef.DatasetAssertions,
	observed map[string]float64,
	baselines map[string]*BaselineStats,
	minSamples int,
	onViolation, unavailableReason string,
	at time.Time,
) ([]DataViolation, []AssertionResult) {
	violations := MarkUnavailable(EvaluateAssertions("warehouse/orders", assertions, observed, baselines, minSamples, at), unavailableReason)
	return violations, DescribeAssertions("warehouse/orders", "", assertions, observed, baselines, minSamples, violations, onViolation)
}

func resultFor(t *testing.T, results []AssertionResult, metric, assertion string) AssertionResult {
	t.Helper()
	for _, r := range results {
		if r.Metric == metric && r.Assertion == assertion {
			return r
		}
	}
	require.Failf(t, "no result", "no %s.%s in %+v", metric, assertion, results)
	return AssertionResult{}
}

func TestDescribeAssertions_PassesAreRecordedWithTheirBounds(t *testing.T) {
	at := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	assertions := &jobdef.DatasetAssertions{
		RowCount:  &jobdef.AssertionSpec{Min: ptrOf(1000), Max: ptrOf(5000)},
		Freshness: &jobdef.FreshnessAssertion{Watermark: "updated_at", MaxLag: "1h"},
	}
	violations, results := describe(assertions,
		map[string]float64{"rowCount": 1200, "updated_at": float64(at.Add(-10 * time.Minute).Unix())},
		nil, 5, jobdef.DatasetOnViolationFail, "", at)

	require.Empty(t, violations)
	require.Len(t, results, 3)
	for _, r := range results {
		assert.Equal(t, AssertionOutcomePassed, r.Outcome)
		assert.Empty(t, r.Disposition, "a pass has no disposition")
		assert.True(t, r.Clean())
	}
	min := resultFor(t, results, "rowCount", AssertionMin)
	require.NotNil(t, min.Observed)
	require.NotNil(t, min.Bound)
	assert.InDelta(t, 1200, *min.Observed, 0.001)
	assert.InDelta(t, 1000, *min.Bound, 0.001)
	assert.InDelta(t, 3600, *resultFor(t, results, "updated_at", AssertionMaxLag).Bound, 0.001, "maxLag is recorded in seconds")
}

func TestDescribeAssertions_ViolatedResultsAgreeWithTheEvaluatorAndCarryTheDisposition(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000), Max: ptrOf(5000)}}
	for onViolation, want := range map[string]string{
		jobdef.DatasetOnViolationWarn: DispositionWarn,
		jobdef.DatasetOnViolationFail: DispositionFail,
		jobdef.DatasetOnViolationHold: DispositionHold,
	} {
		violations, results := describe(assertions, map[string]float64{"rowCount": 12}, nil, 5, onViolation, "", time.Now())
		require.Len(t, violations, 1)
		require.Len(t, results, 2)

		broken := resultFor(t, results, "rowCount", AssertionMin)
		assert.Equal(t, AssertionOutcomeViolated, broken.Outcome)
		assert.Equal(t, want, broken.Disposition, onViolation)
		assert.Equal(t, violations[0].Message, broken.Message)
		assert.Equal(t, AssertionOutcomePassed, resultFor(t, results, "rowCount", AssertionMax).Outcome,
			"the sibling bound that held is still reported as passed")
	}
}

func TestDescribeAssertions_ColdStartIsNotACleanPass(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}}

	_, seeded := describe(assertions, map[string]float64{"rowCount": 105},
		map[string]*BaselineStats{"rowCount": {Median: 100, Samples: 3, Scope: BaselineScopeNamespace}}, 5, jobdef.DatasetOnViolationFail, "", time.Now())
	r := resultFor(t, seeded, "rowCount", AssertionDeltaFromBaseline)
	assert.Equal(t, AssertionOutcomePassed, r.Outcome)
	assert.True(t, r.Seeding, "a pass against 3 of 5 samples says so")
	assert.False(t, r.Clean())
	assert.Equal(t, 3, r.BaselineSamples)
	assert.InDelta(t, 50, *r.Bound, 0.001, "the bound is the allowed deviation, 50%% of the median")

	_, mature := describe(assertions, map[string]float64{"rowCount": 105},
		map[string]*BaselineStats{"rowCount": {Median: 100, Samples: 9, Scope: BaselineScopeNamespace}}, 5, jobdef.DatasetOnViolationFail, "", time.Now())
	assert.True(t, resultFor(t, mature, "rowCount", AssertionDeltaFromBaseline).Clean())
}

func TestDescribeAssertions_SeedingBreachIsAViolationWithTheSeedingDisposition(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "10%"}}
	violations, results := describe(assertions, map[string]float64{"rowCount": 500},
		map[string]*BaselineStats{"rowCount": {Median: 100, Samples: 2, Scope: BaselineScopeNamespace}}, 5, jobdef.DatasetOnViolationFail, "", time.Now())
	require.Len(t, violations, 1)
	require.True(t, violations[0].Seeding)

	r := resultFor(t, results, "rowCount", AssertionDeltaFromBaseline)
	assert.Equal(t, AssertionOutcomeViolated, r.Outcome)
	assert.Equal(t, DispositionSeeding, r.Disposition, "not enforced, whatever onViolation says")
	assert.True(t, r.Seeding)
}

func TestDescribeAssertions_AbstentionsAreUndecidedNotPassed(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}}

	_, none := describe(assertions, map[string]float64{"rowCount": 100}, nil, 5, jobdef.DatasetOnViolationFail, "", time.Now())
	r := resultFor(t, none, "rowCount", AssertionDeltaFromBaseline)
	assert.Equal(t, AssertionOutcomeUndecided, r.Outcome)
	assert.Equal(t, UndecidedNoBaseline, r.Reason)
	assert.Nil(t, r.Bound, "no baseline, no band")

	_, zero := describe(assertions, map[string]float64{"rowCount": 100},
		map[string]*BaselineStats{"rowCount": {Median: 0, Samples: 6, Scope: BaselineScopeNamespace}}, 5, jobdef.DatasetOnViolationFail, "", time.Now())
	assert.Equal(t, UndecidedZeroMedian, resultFor(t, zero, "rowCount", AssertionDeltaFromBaseline).Reason)
}

func TestDescribeAssertions_MissingAndLostMetrics(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(10), Max: ptrOf(20)}}

	_, missing := describe(assertions, nil, nil, 5, jobdef.DatasetOnViolationHold, "", time.Now())
	require.Len(t, missing, 1, "one result for the metric, not one per bound that could not be judged")
	assert.Equal(t, AssertionMissing, missing[0].Assertion)
	assert.Equal(t, AssertionOutcomeViolated, missing[0].Outcome)
	assert.Equal(t, DispositionHold, missing[0].Disposition)
	assert.Nil(t, missing[0].Observed)

	for _, reason := range []string{UnavailableLogUnreadable, UnavailableStreamTruncated} {
		_, lost := describe(assertions, nil, nil, 5, jobdef.DatasetOnViolationFail, reason, time.Now())
		require.Len(t, lost, 1)
		assert.Equal(t, AssertionOutcomeUnavailable, lost[0].Outcome, "a lost stream is not a broken contract")
		assert.Equal(t, reason, lost[0].Reason)
		assert.Empty(t, lost[0].Disposition)
	}

	// A truncated stream does not launder a metric that DID arrive: it is
	// judged on its value, so a real pass stays a pass.
	_, arrived := describe(assertions, map[string]float64{"rowCount": 15}, nil, 5, jobdef.DatasetOnViolationFail, UnavailableStreamTruncated, time.Now())
	assert.True(t, resultFor(t, arrived, "rowCount", AssertionMin).Clean())
}

// TestDescribeAssertions_NeverDisagreesWithTheEvaluator pins the contract that
// recording passes decides nothing: over a sweep of inputs, a result is violated
// exactly when EvaluateAssertions produced a violation for that check.
func TestDescribeAssertions_NeverDisagreesWithTheEvaluator(t *testing.T) {
	at := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	assertions := &jobdef.DatasetAssertions{
		RowCount:  &jobdef.AssertionSpec{Min: ptrOf(50), Max: ptrOf(150), DeltaFromBaseline: "20%"},
		NullRate:  &jobdef.AssertionSpec{Max: ptrOf(0.1)},
		Freshness: &jobdef.FreshnessAssertion{Watermark: "wm", MaxLag: "1h"},
		Custom:    []jobdef.AssertionSpec{{Metric: "dedup", Max: ptrOf(0.5)}},
	}
	baselines := map[string]*BaselineStats{"rowCount": {Median: 100, Samples: 8, Scope: BaselineScopeNamespace}}
	for _, rows := range []float64{0, 49, 50, 100, 119, 121, 150, 151} {
		for _, nulls := range []float64{0, 0.1, 0.11} {
			for _, wm := range []time.Duration{time.Minute, 2 * time.Hour} {
				observed := map[string]float64{"rowCount": rows, "nullRate": nulls, "wm": float64(at.Add(-wm).Unix()), "dedup": 0.2}
				violations, results := describe(assertions, observed, baselines, 5, jobdef.DatasetOnViolationWarn, "", at)

				broke := map[AssertionRef]bool{}
				for _, v := range violations {
					broke[AssertionRef{Metric: v.Metric, Assertion: v.Assertion}] = true
				}
				violated := 0
				for _, r := range results {
					isBroken := broke[AssertionRef{Metric: r.Metric, Assertion: r.Assertion}]
					assert.Equal(t, isBroken, r.Outcome == AssertionOutcomeViolated, "%+v", r)
					if isBroken {
						violated++
					}
				}
				assert.Len(t, violations, violated, "every violation has exactly one violated result")
			}
		}
	}
}

func TestDescribeAssertions_EmptyContractDescribesNothing(t *testing.T) {
	assert.Nil(t, DescribeAssertions("d", "", nil, nil, nil, 5, nil, jobdef.DatasetOnViolationWarn))
	assert.Nil(t, DescribeAssertions("d", "", &jobdef.DatasetAssertions{}, nil, nil, 5, nil, jobdef.DatasetOnViolationWarn))
}

func TestVerdictOf(t *testing.T) {
	passed := AssertionResult{Outcome: AssertionOutcomePassed}
	seeding := AssertionResult{Outcome: AssertionOutcomePassed, Seeding: true}
	undecided := AssertionResult{Outcome: AssertionOutcomeUndecided}
	unavailable := AssertionResult{Outcome: AssertionOutcomeUnavailable}
	violated := AssertionResult{Outcome: AssertionOutcomeViolated}

	assert.Equal(t, VerdictNotRecorded, VerdictOf(nil), "no record is not a pass")
	assert.Equal(t, VerdictPassed, VerdictOf([]AssertionResult{passed, passed}))
	assert.Equal(t, VerdictSeeding, VerdictOf([]AssertionResult{passed, seeding}))
	assert.Equal(t, VerdictUndecided, VerdictOf([]AssertionResult{seeding, undecided}))
	assert.Equal(t, VerdictUndecided, VerdictOf([]AssertionResult{passed, unavailable}))
	assert.Equal(t, VerdictViolated, VerdictOf([]AssertionResult{passed, undecided, violated}))
}

func TestWithholdUnownedBaselineEvidenceFromResults(t *testing.T) {
	median := 100.0
	owned := AssertionResult{Assertion: AssertionDeltaFromBaseline, Outcome: AssertionOutcomePassed,
		BaselineMedian: &median, BaselineSamples: 9, BaselineScope: BaselineScopeNamespace, Bound: ptrOf(20)}
	legacy := AssertionResult{Assertion: AssertionDeltaFromBaseline, Outcome: AssertionOutcomeViolated,
		BaselineMedian: &median, BaselineSamples: 9, Bound: ptrOf(20), Message: "median 100 over 9 samples"}
	unowned := AssertionResult{Assertion: AssertionDeltaFromBaseline, Outcome: AssertionOutcomePassed,
		BaselineMedian: &median, BaselineSamples: 9, Bound: ptrOf(20)}
	bounded := AssertionResult{Assertion: AssertionMin, Outcome: AssertionOutcomeViolated, Bound: ptrOf(1000), Message: "below min"}
	abstained := AssertionResult{Assertion: AssertionDeltaFromBaseline, Outcome: AssertionOutcomeUndecided, Reason: UndecidedNoBaseline}

	results := []AssertionResult{owned, legacy, unowned, bounded, abstained}
	require.True(t, WithholdUnownedBaselineEvidenceFromResults(results))

	assert.Equal(t, owned, results[0], "a baseline computed over the run's own namespace is served unchanged")
	for _, i := range []int{1, 2} {
		assert.Nil(t, results[i].BaselineMedian, i)
		assert.Zero(t, results[i].BaselineSamples, i)
		assert.Nil(t, results[i].Bound, "a bound derived from another owner's baseline is not served")
		assert.True(t, results[i].BoundWithheld, "and the reader is told it was withheld rather than absent")
	}
	assert.Equal(t, BaselineEvidenceWithheld, results[1].Message)
	assert.Empty(t, results[2].Message, "a pass has no message to replace")
	assert.Equal(t, bounded, results[3], "an absolute bound derives from the manifest, not a baseline")
	assert.Equal(t, abstained, results[4], "an abstention carries no baseline evidence to withhold")
}
