package job

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// fanout_ratelimit_test.go pins that a fan-out group consumes ONE rate-limit
// token PER INSTANCE.
//
// The local scheduler acquired a single token for the CATALOG task before
// entering the group and then launched every instance without acquiring again,
// so a `2 per minute` rule admitted two partitions under a worker (which
// acquires per claimed row) and all 1000 locally. The rejection path was worse
// than the over-admission: it parked the row by catalog task id, which names N
// rows, so RateLimitTask returned ErrAmbiguousTaskRun and dispatch — which
// treats any error as fatal — halted the entire run.

// seedRateLimitedFanOutStep gives the fixture's job a rate-limit declaration and
// points the fanned step at it. Both live in the DATABASE because
// ratelimit.RuleForTask resolves the rule by joining task_runs -> tasks -> jobs,
// not from the in-memory job definition.
func seedRateLimitedFanOutStep(t *testing.T, f *fanOutFixture, resource string, limit int) {
	t.Helper()

	now := time.Now().UTC()
	trigger := &models.Trigger{
		ID: uuid.New(), Alias: "fanout-rl-trigger-" + f.jobID.String()[:8],
		Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, f.db.Create(trigger).Error)

	rateLimits := datatypes.JSON(fmt.Sprintf(
		`[{"resource":%q,"limit":%d,"window":"1m"}]`, resource, limit))
	require.NoError(t, f.db.Create(&models.Job{
		ID: f.jobID, Alias: "fanout-rl-" + f.jobID.String()[:8], TriggerID: trigger.ID,
		RateLimits: rateLimits, CreatedAt: now, UpdatedAt: now,
	}).Error)

	require.NoError(t, f.db.Model(&models.Task{}).
		Where("id = ?", f.fanned).
		Updates(map[string]any{"rate_limit_resource": resource, "rate_limit_units": 1}).Error)
}

// runFanOutInBackground starts the fixture's job on a cancellable context and
// returns the cancel func plus the channel its error lands on. The parked
// instances of a rate-limited group cannot become dispatchable until the fixed
// window rolls (the limiter floors windows at one minute), so these tests
// observe the admission decision and then cancel rather than waiting it out.
func runFanOutInBackground(t *testing.T, f *fanOutFixture) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		opts := withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine)
		done <- New(&models.Job{ID: f.jobID}, opts...).Run(ctx)
	}()
	return cancel, done
}

// countParkedAndTerminal reports how many instances of the fanned group are
// terminal and how many are parked behind a live rate-limit window.
//
// It deliberately uses no require/assert: it is called from inside an Eventually
// condition, which runs off the test goroutine where t.FailNow is illegal, and
// it is polled before the run has even created its JobRun.
func countParkedAndTerminal(f *fanOutFixture) (terminal, parked int) {
	var rows []models.TaskRun
	if err := f.db.
		Where("task_id = ? AND partition_count > 0", f.fanned).
		Find(&rows).Error; err != nil {
		return 0, 0
	}
	now := time.Now().UTC()
	for _, row := range rows {
		if run.IsTerminal(run.TaskStatus(row.Status)) {
			terminal++
			continue
		}
		if row.RateLimitRetryAfter != nil && row.RateLimitRetryAfter.After(now) {
			parked++
		}
	}
	return terminal, parked
}

// TestFanOutLocalAcquiresRateLimitPerInstance: a 2-per-minute rule over a
// 4-partition group must admit exactly two partitions and park the other two on
// THEIR OWN rows.
func TestFanOutLocalAcquiresRateLimitPerInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	seedRateLimitedFanOutStep(t, f, "warehouse-"+f.jobID.String()[:8], 2)

	cancel, done := runFanOutInBackground(t, f)
	defer cancel()

	// Polling the rows (rather than sleeping) is what keeps this deterministic.
	require.Eventually(t, func() bool {
		terminal, parked := countParkedAndTerminal(f)
		return terminal == 2 && parked == 2
	}, 10*time.Second, 10*time.Millisecond,
		"exactly two partitions may hold the two available tokens; the rest must park on their own rows")

	// Snapshot WHILE the run is live: a parked instance is pending with its own
	// retry-after deadline, and the park is instance-addressed — the two that
	// hold tokens are untouched by it. This has to be read before the
	// cancellation, because cancelling hands the parked rows to the straggler
	// sweep, which resolves them (see fanout_cancel_test.go).
	var parkedValues, ranValues []string
	for _, row := range f.instanceRows(t) {
		if row.RateLimitRetryAfter != nil {
			parkedValues = append(parkedValues, row.PartitionValue)
			require.Equal(t, string(run.TaskStatusPending), row.Status,
				"a parked partition waits pending for its window, it is not failed")
			continue
		}
		ranValues = append(ranValues, row.PartitionValue)
		require.Equal(t, string(run.TaskStatusSucceeded), row.Status)
	}
	require.Len(t, parkedValues, 2, "parked: %v, ran: %v", parkedValues, ranValues)
	require.Len(t, ranValues, 2, "parked: %v, ran: %v", parkedValues, ranValues)

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	created := 0
	for _, p := range []string{"a", "b", "c", "d"} {
		created += f.engine.createCount(p)
	}
	require.Equal(t, 2, created,
		"one token per instance: a 2-per-minute rule must not admit all four partitions")
}

// TestFanOutLocalRateLimitDoesNotHaltTheRun pins the second half: parking a
// fanned step must never surface as a dispatch error, which is fatal to the run.
func TestFanOutLocalRateLimitDoesNotHaltTheRun(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	seedRateLimitedFanOutStep(t, f, "warehouse-halt-"+f.jobID.String()[:8], 2)

	cancel, done := runFanOutInBackground(t, f)
	defer cancel()

	require.Eventually(t, func() bool {
		_, parked := countParkedAndTerminal(f)
		return parked == 2
	}, 10*time.Second, 10*time.Millisecond)

	// The producer resolved and two partitions executed: the DAG kept moving
	// rather than aborting the moment the group hit its limit.
	var producer models.TaskRun
	require.NoError(t, f.db.
		Where("task_id = ?", f.producer).
		First(&producer).Error)
	require.Equal(t, string(run.TaskStatusSucceeded), producer.Status,
		"an unrelated step must not be halted by a rate-limited fan-out group")

	cancel()
	select {
	case err := <-done:
		require.Error(t, err, "the cancelled run reports the cancellation")
		require.NotContains(t, err.Error(), "multiple task instances match",
			"parking a fanned step must address instances, never the ambiguous catalog id")
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}
