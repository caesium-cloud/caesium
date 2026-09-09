package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	metricstestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// data_assertions_unavailable_test.go pins issue #437: a marker stream that was
// TRUNCATED or UNREADABLE is not evidence that a step stopped emitting a
// declared metric, and must never redden a run or break a circuit.
//
// The distinction is only knowable at the executor seam, so these scenarios
// drive it the way the executors do — through MetricsCapture — and assert on
// the recorded verdict, the dispatch outcome and the counter label.

// TestMarkUnavailable_RewritesOnlyTheMissingVerdicts is the pure core: a lost
// stream downgrades `missing`, and NOTHING else. A metric that arrived was
// judged against its real value, so a genuine breach must survive.
func TestMarkUnavailable_RewritesOnlyTheMissingVerdicts(t *testing.T) {
	violations := []DataViolation{
		{Dataset: "d", Metric: "rowCount", Assertion: AssertionMissing, Message: "never emitted"},
		{Dataset: "d", Metric: "dedup_ratio", Assertion: AssertionMax, Observed: ptrOf(0.31), Message: "over max"},
		{Dataset: "d", Metric: "watermark", Assertion: AssertionMaxLag, Message: "stale"},
	}

	out := MarkUnavailable(violations, UnavailableStreamTruncated)
	require.Len(t, out, 3)

	assert.Equal(t, AssertionUnavailable, out[0].Assertion)
	assert.Equal(t, UnavailableStreamTruncated, out[0].Reason)
	assert.Contains(t, out[0].Message, "rowCount")
	assert.Contains(t, out[0].Message, "##caesium::metrics")
	assert.Contains(t, out[0].Message, "never enforced")
	assert.False(t, out[0].Enforceable(), "an unavailable verdict never escalates")

	assert.Equal(t, AssertionMax, out[1].Assertion, "a metric that ARRIVED is judged normally")
	assert.Empty(t, out[1].Reason)
	assert.True(t, out[1].Enforceable(),
		"a truncated stream must not launder a real breach into an infrastructure excuse")

	assert.Equal(t, AssertionMaxLag, out[2].Assertion)
	assert.True(t, out[2].Enforceable())
}

// TestMarkUnavailable_LogUnreadableCarriesItsOwnReason pins the second reason
// and its distinct operator-facing wording.
func TestMarkUnavailable_LogUnreadableCarriesItsOwnReason(t *testing.T) {
	out := MarkUnavailable([]DataViolation{
		{Dataset: "d", Metric: "rowCount", Assertion: AssertionMissing},
	}, UnavailableLogUnreadable)

	require.Len(t, out, 1)
	assert.Equal(t, AssertionUnavailable, out[0].Assertion)
	assert.Equal(t, UnavailableLogUnreadable, out[0].Reason)
	assert.Contains(t, out[0].Message, "log could not be read")
}

// TestMarkUnavailable_NoReasonIsANoOp: a COMPLETE capture leaves `missing`
// exactly as B1 intended — a step that stops reporting a metric breaks its
// contract, and that must stay red under onViolation: fail.
func TestMarkUnavailable_NoReasonIsANoOp(t *testing.T) {
	out := MarkUnavailable([]DataViolation{
		{Dataset: "d", Metric: "rowCount", Assertion: AssertionMissing, Message: "never emitted"},
	}, "")

	require.Len(t, out, 1)
	assert.Equal(t, AssertionMissing, out[0].Assertion)
	assert.Empty(t, out[0].Reason)
	assert.True(t, out[0].Enforceable())
}

// TestMetricsCaptureUnavailableReason pins the bounded reason enum and the
// precedence between the two flags.
func TestMetricsCaptureUnavailableReason(t *testing.T) {
	assert.Empty(t, MetricsCapture{}.UnavailableReason(),
		"the zero capture is 'nothing emitted, nothing lost'")
	assert.Empty(t, CapturedMetrics([]pkgtask.DatasetMetricSample{
		{Metric: "rowCount", Value: 1},
	}).UnavailableReason())
	assert.Equal(t, UnavailableStreamTruncated, MetricsCapture{Truncated: true}.UnavailableReason())
	assert.Equal(t, UnavailableLogUnreadable, MetricsCapture{Unreadable: true}.UnavailableReason())
	assert.Equal(t, UnavailableLogUnreadable,
		MetricsCapture{Unreadable: true, Truncated: true}.UnavailableReason(),
		"a log that could not be read at all is the stronger statement")
}

// TestEvaluateDataAssertions_TruncatedStreamIsUnavailableNotMissing is the
// headline: the declared metric never reached the evaluator because the
// ##caesium::metrics scan overflowed its cap, and the contract says
// onViolation: fail — yet the task stays green and the verdict names the
// infrastructure reason.
func TestEvaluateDataAssertions_TruncatedStreamIsUnavailableNotMissing(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	before := metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultUnavailable)
	beforeFail := metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultFail)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	// The scan dropped rowCount but kept an unrelated sample, exactly as the
	// accumulator behaves once it hits MaxMetricsBytes mid-stream.
	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, MetricsCapture{
		Samples:   []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "filler", Value: 1}},
		Truncated: true,
	})
	require.NoError(t, err,
		"a lost observation is an infrastructure fault; it must not redden a run declared onViolation: fail")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, UnavailableStreamTruncated, violations[0].Reason)
	assert.Equal(t, "rowCount", violations[0].Metric)
	assert.False(t, violations[0].Enforceable())

	assert.Equal(t, before+1,
		metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultUnavailable),
		"the verdict is counted under its own disposition, not as a pass, a seed or a failure")
	assert.Equal(t, beforeFail,
		metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultFail))
}

// TestEvaluateDataAssertions_UnreadableLogIsUnavailableNotMissing is the other
// reason: no marker of any kind could be read, so every declared metric is
// unavailable rather than missing.
func TestEvaluateDataAssertions_UnreadableLogIsUnavailableNotMissing(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{
			RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)},
			Freshness: &jobdef.FreshnessAssertion{
				Watermark: "updated_at",
				MaxLag:    "1h",
			},
		},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID,
		MetricsCapture{Unreadable: true})
	require.NoError(t, err)

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 2, "both the bound and the freshness watermark lost their observation")
	for _, violation := range violations {
		assert.Equal(t, AssertionUnavailable, violation.Assertion)
		assert.Equal(t, UnavailableLogUnreadable, violation.Reason)
		assert.False(t, violation.Enforceable())
	}
}

// TestEvaluateDataAssertions_CleanEmptyCaptureStaysMissing is the guard against
// over-reach: with no read error and no truncation, a step that emits nothing
// still breaks its contract. This is what B1 intended and what #437 must not
// weaken.
func TestEvaluateDataAssertions_CleanEmptyCaptureStaysMissing(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, MetricsCapture{})
	require.Error(t, err, "a step that legitimately stops emitting a declared metric still fails its contract")
	assert.Contains(t, err.Error(), "rowCount")

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionMissing, violations[0].Assertion)
	assert.Empty(t, violations[0].Reason)
	assert.True(t, violations[0].Enforceable())
}

// TestEvaluateDataAssertions_LostStreamNeverOpensAHold is the circuit-breaker
// half of the contract (#439 wired holds to Enforceable): the breaker must not
// trip on an observation nobody has.
func TestEvaluateDataAssertions_LostStreamNeverOpensAHold(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	before := metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultHold)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	require.NoError(t, EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID,
		MetricsCapture{Truncated: true}))

	assert.Empty(t, activeHolds(t, db, "warehouse/orders"),
		"a lost marker stream is not evidence against a dataset; holding it would skip every consumer for an infrastructure fault")
	assert.Equal(t, before, metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultHold))

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionUnavailable, violations[0].Assertion)
}

// TestEvaluateDataAssertions_TruncationDoesNotLaunderARealBreach: the metric
// that DID survive the truncation is judged on its real value, so an
// onViolation: fail contract it breaches still reddens the run.
func TestEvaluateDataAssertions_TruncationDoesNotLaunderARealBreach(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesWithAssertions(t, db, jobID, stepName, "warehouse/orders",
		jobdef.DatasetAssertions{RowCount: &jobdef.AssertionSpec{Min: ptrOf(1000)}},
		jobdef.DatasetOnViolationFail)

	var taskRun models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&taskRun).Error)

	err := EvaluateDataAssertions(store, taskRun.JobRunID, taskID, taskRunID, MetricsCapture{
		Samples:   []pkgtask.DatasetMetricSample{{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}},
		Truncated: true,
	})
	require.Error(t, err, "rowCount arrived and is below its declared min; the truncation is irrelevant to it")
	assert.Contains(t, err.Error(), AssertionMin)

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionMin, violations[0].Assertion)
}

// TestEvaluateDataAssertions_LostStreamNeverReleasesAHold: the clean-run
// release must not fire on a run whose evidence never arrived, or a hold would
// clear because the marker stream broke rather than because the data recovered.
func TestEvaluateDataAssertions_LostStreamNeverReleasesAHold(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	// A real breach opens the hold.
	require.NoError(t, emitSamples(t, store, db, taskID, taskRunID,
		pkgtask.DatasetMetricSample{Dataset: "warehouse/orders", Metric: "rowCount", Value: 12}))
	require.Len(t, activeHolds(t, db, "warehouse/orders"), 1)

	// A later run of the same job whose marker stream was lost.
	time.Sleep(time.Millisecond)
	_, taskID2, taskRunID2, _ := seedTaskRunForJob(t, db, jobID, stepName, string(TaskStatusRunning))
	var second models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID2).First(&second).Error)
	require.NoError(t, EvaluateDataAssertions(store, second.JobRunID, taskID2, taskRunID2,
		MetricsCapture{Unreadable: true}))

	assert.Len(t, activeHolds(t, db, "warehouse/orders"), 1,
		"a run that observed nothing has not disproved the assertion that opened the hold")
}

// TestEvaluateDataAssertionsClaimed_StaleClaimRecordsNoUnavailableVerdict is
// the composition of this file's rule with the claim fence (#453): the two
// answer different questions, and the claim's answer comes first.
//
// A superseded worker whose marker stream was ALSO lost must record nothing at
// all — no sample, no hold, no violation on the replacement attempt's row, and
// no disposition counter. `unavailable` being warn-only is not a licence to
// write it anyway: the verdict describes an attempt that no longer owns the
// row, so an operator reading the replacement's violations would be reading
// somebody else's failed observation.
func TestEvaluateDataAssertionsClaimed_StaleClaimRecordsNoUnavailableVerdict(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	before := metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultUnavailable)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	stale := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, stale)
	claimTaskRun(t, db, taskRunID, TaskClaim{ClaimedBy: "worker-b", ClaimAttempt: 2})

	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &stale,
		MetricsCapture{Unreadable: true}))

	assert.Empty(t, metricRows(t, db))
	assert.Empty(t, activeHolds(t, db, "warehouse/orders"),
		"a lost stream opens no hold, and a stale claim may not write one either")
	assert.Nil(t, dataViolationsOf(t, db, taskRunID),
		"the superseded attempt must not stamp its unavailable verdict on the replacement's row")
	assert.Equal(t, before,
		metricstestutil.CounterValue(t, metrics.DataAssertionsTotal, assertionResultUnavailable),
		"a verdict nothing acted on must not move the disposition counter either")
}

// TestEvaluateDataAssertionsClaimed_LiveClaimStillRecordsUnavailable is the
// other half, and the one that would catch the fence turned into a kill switch
// for this file's new kind: the SAME lost stream, submitted by the worker that
// actually holds the row, still records the `unavailable` verdict — and still
// opens no hold, because that is a property of the kind, not of the claim.
func TestEvaluateDataAssertionsClaimed_LiveClaimStillRecordsUnavailable(t *testing.T) {
	setDataAssertions(t, true)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	store := NewStore(db)

	jobID, taskID, taskRunID, stepName := seedTaskRun(t, db, string(TaskStatusRunning), false)
	declareProducesHold(t, db, jobID, stepName, "warehouse/orders", jobdef.DatasetReleaseAuto)

	var row models.TaskRun
	require.NoError(t, db.Where("id = ?", taskRunID).First(&row).Error)

	live := TaskClaim{ClaimedBy: "worker-a", ClaimAttempt: 1}
	claimTaskRun(t, db, taskRunID, live)

	require.NoError(t, EvaluateDataAssertionsClaimed(store, row.JobRunID, taskID, taskRunID, &live,
		MetricsCapture{Truncated: true}))

	violations := dataViolationsOf(t, db, taskRunID)
	require.Len(t, violations, 1)
	assert.Equal(t, AssertionUnavailable, violations[0].Assertion)
	assert.Equal(t, UnavailableStreamTruncated, violations[0].Reason)
	assert.Empty(t, activeHolds(t, db, "warehouse/orders"),
		"the claim is live, so the verdict is recorded — but an unavailable verdict still never breaks the circuit")
}
