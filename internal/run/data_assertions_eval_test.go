package run

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptrOf(v float64) *float64 { return &v }

// baselineOf builds a BaselineStats with a chosen median and sample count, the
// only two fields the evaluator reads.
func baselineOf(median float64, samples int) *BaselineStats {
	return &BaselineStats{Median: median, Samples: samples}
}

// TestEvaluateAssertions_PureCore is the table test that pins the whole
// assertion semantics. It calls the pure core directly — no store, no DB, no
// executor, no clock — which is exactly the property Plan 3's assertion
// backtest depends on: if this test needs a database to run, the core is not
// pure any more.
func TestEvaluateAssertions_PureCore(t *testing.T) {
	const dataset = "warehouse/orders"
	at := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		assertions *jobdef.DatasetAssertions
		observed   map[string]float64
		baselines  map[string]*BaselineStats
		minSamples int
		want       []DataViolation // compared on the identifying fields only
	}{
		{
			name:       "min holds from run one",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
			observed:   map[string]float64{"rowCount": 1000},
		},
		{
			name:       "min breaches from run one with no baseline at all",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
			observed:   map[string]float64{"rowCount": 999},
			want:       []DataViolation{{Metric: "rowCount", Assertion: AssertionMin, Observed: ptrOf(999), Bound: ptrOf(1000)}},
		},
		{
			name:       "max breaches on a custom metric",
			assertions: &jobdef.DatasetAssertions{Custom: []jobdef.AssertionSpec{{Metric: "dedup_ratio", Max: ptrOf(0.05)}}},
			observed:   map[string]float64{"dedup_ratio": 0.31},
			want:       []DataViolation{{Metric: "dedup_ratio", Assertion: AssertionMax, Observed: ptrOf(0.31), Bound: ptrOf(0.05)}},
		},
		{
			name:       "nullRate resolves the shorthand's default metric",
			assertions: &jobdef.DatasetAssertions{NullRate: &jobdef.AssertionSpec{Max: ptrOf(0.02)}},
			observed:   map[string]float64{"nullRate": 0.5},
			want:       []DataViolation{{Metric: "nullRate", Assertion: AssertionMax, Observed: ptrOf(0.5), Bound: ptrOf(0.02)}},
		},
		{
			name:       "nullRate honours an explicit metric override",
			assertions: &jobdef.DatasetAssertions{NullRate: &jobdef.AssertionSpec{Metric: "null_rate_customer_id", Max: ptrOf(0.02)}},
			observed:   map[string]float64{"null_rate_customer_id": 0.5, "nullRate": 0},
			want:       []DataViolation{{Metric: "null_rate_customer_id", Assertion: AssertionMax, Observed: ptrOf(0.5), Bound: ptrOf(0.02)}},
		},
		{
			name:       "a declared metric that never arrived is itself a violation",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
			observed:   map[string]float64{"dedup_ratio": 0.01},
			want:       []DataViolation{{Metric: "rowCount", Assertion: AssertionMissing}},
		},
		{
			name:       "a step that emits nothing at all violates every declared assertion",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}, NullRate: &jobdef.AssertionSpec{Max: ptrOf(0.02)}},
			observed:   nil,
			want: []DataViolation{
				{Metric: "rowCount", Assertion: AssertionMissing},
				{Metric: "nullRate", Assertion: AssertionMissing},
			},
		},
		{
			name:       "deltaFromBaseline passes inside the band",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 14000},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(10000, 20)},
			minSamples: 5,
		},
		{
			name:       "deltaFromBaseline passes exactly on the band edge",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 15000},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(10000, 20)},
			minSamples: 5,
		},
		{
			name:       "deltaFromBaseline breaches outside the band once seeded",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(10000, 20)},
			minSamples: 5,
			want: []DataViolation{{
				Metric: "rowCount", Assertion: AssertionDeltaFromBaseline,
				Observed: ptrOf(100), Bound: ptrOf(5000), BaselineMedian: ptrOf(10000), BaselineSamples: 20,
			}},
		},
		{
			name:       "cold start records the delta breach as seeding",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(10000, 2)},
			minSamples: 5,
			want: []DataViolation{{
				Metric: "rowCount", Assertion: AssertionDeltaFromBaseline,
				Observed: ptrOf(100), Bound: ptrOf(5000), BaselineMedian: ptrOf(10000), BaselineSamples: 2, Seeding: true,
			}},
		},
		{
			name: "cold start never softens the absolute bounds",
			assertions: &jobdef.DatasetAssertions{
				RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000), DeltaFromBaseline: "50%"},
			},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(10000, 2)},
			minSamples: 5,
			want: []DataViolation{
				{Metric: "rowCount", Assertion: AssertionMin, Observed: ptrOf(100), Bound: ptrOf(1000)},
				{Metric: "rowCount", Assertion: AssertionDeltaFromBaseline, Observed: ptrOf(100), Seeding: true},
			},
		},
		{
			name:       "a nil baseline yields no delta verdict",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": nil},
			minSamples: 5,
		},
		{
			name:       "an empty baseline yields no delta verdict",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(0, 0)},
			minSamples: 5,
		},
		{
			name:       "a zero median yields no delta verdict rather than a zero-width band",
			assertions: &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{DeltaFromBaseline: "50%"}},
			observed:   map[string]float64{"rowCount": 100},
			baselines:  map[string]*BaselineStats{"rowCount": baselineOf(0, 20)},
			minSamples: 5,
		},
		{
			name: "freshness passes inside maxLag",
			assertions: &jobdef.DatasetAssertions{
				Freshness: &jobdef.FreshnessAssertion{Watermark: "max_event_time", MaxLag: "26h"},
			},
			observed: map[string]float64{"max_event_time": float64(at.Add(-time.Hour).Unix())},
		},
		{
			name: "freshness breaches beyond maxLag",
			assertions: &jobdef.DatasetAssertions{
				Freshness: &jobdef.FreshnessAssertion{Watermark: "max_event_time", MaxLag: "1h"},
			},
			observed: map[string]float64{"max_event_time": float64(at.Add(-48 * time.Hour).Unix())},
			want:     []DataViolation{{Metric: "max_event_time", Assertion: AssertionMaxLag}},
		},
		{
			name: "a missing watermark is a violation too",
			assertions: &jobdef.DatasetAssertions{
				Freshness: &jobdef.FreshnessAssertion{Watermark: "max_event_time", MaxLag: "1h"},
			},
			observed: map[string]float64{"rowCount": 1},
			want:     []DataViolation{{Metric: "max_event_time", Assertion: AssertionMissing}},
		},
		{
			name:       "no assertions declared yields no verdicts",
			assertions: nil,
			observed:   map[string]float64{"rowCount": 1},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateAssertions(dataset, tc.assertions, tc.observed, tc.baselines, tc.minSamples, at)
			require.Len(t, got, len(tc.want), "verdicts: %+v", got)

			for i, want := range tc.want {
				actual := got[i]
				assert.Equal(t, dataset, actual.Dataset)
				assert.Equal(t, want.Metric, actual.Metric)
				assert.Equal(t, want.Assertion, actual.Assertion)
				assert.Equal(t, want.Seeding, actual.Seeding)
				assert.NotEmpty(t, actual.Message, "every violation carries a human-readable message")
				if want.Observed != nil {
					require.NotNil(t, actual.Observed)
					assert.InDelta(t, *want.Observed, *actual.Observed, 1e-9)
				}
				if want.Bound != nil {
					require.NotNil(t, actual.Bound)
					assert.InDelta(t, *want.Bound, *actual.Bound, 1e-9)
				}
				if want.BaselineMedian != nil {
					require.NotNil(t, actual.BaselineMedian)
					assert.InDelta(t, *want.BaselineMedian, *actual.BaselineMedian, 1e-9)
					assert.Equal(t, want.BaselineSamples, actual.BaselineSamples)
				}
				// A missing metric reports no observed value at all: claiming
				// one would invent data the step never emitted.
				if want.Assertion == AssertionMissing {
					assert.Nil(t, actual.Observed)
				}
			}
		})
	}
}

// TestEvaluateAssertions_IsSideEffectFree pins the shape requirement itself:
// the core is callable with nothing but values, and it does not mutate the
// inputs it is handed.
func TestEvaluateAssertions_IsSideEffectFree(t *testing.T) {
	assertions := &jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000), DeltaFromBaseline: "50%"}}
	before, err := json.Marshal(assertions)
	require.NoError(t, err)

	observed := map[string]float64{"rowCount": 10}
	baseline := baselineOf(10000, 20)
	baselines := map[string]*BaselineStats{"rowCount": baseline}

	first := EvaluateAssertions("orders", assertions, observed, baselines, 5, time.Now())
	second := EvaluateAssertions("orders", assertions, observed, baselines, 5, time.Now())
	assert.Equal(t, first, second, "the core is deterministic on identical inputs")

	after, err := json.Marshal(assertions)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "the declared spec is not mutated")
	assert.Equal(t, map[string]float64{"rowCount": 10}, observed, "the observations are not mutated")
	assert.Equal(t, 20, baseline.Samples, "the baseline is not mutated")
}

// TestDataViolation_Enforceable pins the one rule that decides whether a
// verdict can turn a run red: a seeding verdict never can.
func TestDataViolation_Enforceable(t *testing.T) {
	assert.True(t, DataViolation{Assertion: AssertionMin}.Enforceable())
	assert.True(t, DataViolation{Assertion: AssertionMissing}.Enforceable())
	assert.False(t, DataViolation{Assertion: AssertionDeltaFromBaseline, Seeding: true}.Enforceable())
}

// TestDataViolation_JSONShape pins the persisted contract. Plan 3's backtest
// and the UI both read these exact keys, so a rename is a breaking change and
// has to fail here first.
func TestDataViolation_JSONShape(t *testing.T) {
	violation := DataViolation{
		Dataset:           "warehouse/orders",
		Metric:            "rowCount",
		Assertion:         AssertionDeltaFromBaseline,
		Observed:          ptrOf(100),
		Bound:             ptrOf(5000),
		DeltaFromBaseline: "50%",
		BaselineMedian:    ptrOf(10000),
		BaselineSamples:   7,
		Seeding:           true,
		Message:           "observed 100 deviates",
	}
	b, err := json.Marshal(violation)
	require.NoError(t, err)
	assert.JSONEq(t, `{
		"dataset": "warehouse/orders",
		"metric": "rowCount",
		"assertion": "deltaFromBaseline",
		"observed": 100,
		"bound": 5000,
		"delta_from_baseline": "50%",
		"baseline_median": 10000,
		"baseline_samples": 7,
		"seeding": true,
		"message": "observed 100 deviates"
	}`, string(b))

	// A clean absolute-bound violation omits every baseline field rather than
	// recording a zero median nobody computed.
	b, err = json.Marshal(DataViolation{Dataset: "d", Metric: "m", Assertion: AssertionMin, Message: "x"})
	require.NoError(t, err)
	assert.JSONEq(t, `{"dataset":"d","metric":"m","assertion":"min","message":"x"}`, string(b))
}

func TestAssertionMetrics(t *testing.T) {
	assert.Nil(t, AssertionMetrics(nil))
	assert.Nil(t, AssertionMetrics(&jobdef.DatasetAssertions{}))

	metrics := AssertionMetrics(&jobdef.DatasetAssertions{
		RowCount:  &jobdef.AssertionSpec{Min: ptrOf(1)},
		NullRate:  &jobdef.AssertionSpec{Metric: "null_rate_customer_id", Max: ptrOf(0.02)},
		Freshness: &jobdef.FreshnessAssertion{Watermark: "max_event_time", MaxLag: "26h"},
		Custom:    []jobdef.AssertionSpec{{Metric: "dedup_ratio", Max: ptrOf(0.05)}},
	})
	assert.Equal(t, []string{"dedup_ratio", "max_event_time", "null_rate_customer_id", "rowCount"}, metrics)
}

func TestBaselineMinSamplesFallsBackToTheDefault(t *testing.T) {
	assert.Equal(t, DefaultBaselineMinSamples, BaselineMinSamples(),
		"an unset CAESIUM_BASELINE_MIN_SAMPLES must read as the documented default")
}
