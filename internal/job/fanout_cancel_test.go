package job

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/stretchr/testify/require"
)

// fanout_cancel_test.go pins that a CANCELLED local run still resolves every
// instance row of a fan-out group.
//
// The straggler sweep at the end of runFannedGroup exists precisely because
// local mode has no recovery owner: an instance row left non-terminal is
// stranded for good, and it hangs the run's accounting and every downstream
// group-status read. But the sweep used to read through the RUN's context, so
// the moment the run was cancelled its very first query returned
// context.Canceled and it returned before resolving anything — the cleanup was
// unavailable in exactly the situation that needs it most. It now runs on
// context.WithoutCancel bounded by fanOutSweepTimeout.

// TestFanOutLocalCancelledRunStillResolvesEveryInstance drives the sweep's
// cancellation path through the real scheduler.
//
// The setup is a rate-limited group because that is the deterministic way to
// hold instances PENDING across a cancellation: a 2-per-minute rule over four
// partitions parks two of them behind a window that cannot roll for up to a
// minute, so when the run is cancelled they are still pending and only the sweep
// can resolve them. Cancelling a group whose instances are merely in flight does
// NOT exercise this — those rows are resolved by their own dispatch goroutines.
func TestFanOutLocalCancelledRunStillResolvesEveryInstance(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	seedRateLimitedFanOutStep(t, f, "warehouse-cancel-"+f.jobID.String()[:8], 2)

	f.engine.logsByPartition["a"] = "##caesium::output {\"rows\":\"1\"}\n"
	f.engine.logsByPartition["b"] = "##caesium::output {\"rows\":\"2\"}\n"
	f.engine.logsByPartition["c"] = "##caesium::output {\"rows\":\"3\"}\n"
	f.engine.logsByPartition["d"] = "##caesium::output {\"rows\":\"4\"}\n"

	cancel, done := runFanOutInBackground(t, f)
	defer cancel()

	// Two instances have run to completion; two are parked behind the window and
	// nothing will dispatch them before it rolls.
	require.Eventually(t, func() bool {
		terminal, parked := countParkedAndTerminal(f)
		return terminal == 2 && parked == 2
	}, 10*time.Second, 10*time.Millisecond,
		"the group must settle with two instances parked before the run is cancelled")

	cancel()
	select {
	case err := <-done:
		require.Error(t, err, "a cancelled run reports the cancellation")
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	// The contract: no instance row is left behind by a cancelled run.
	rows := f.instanceRows(t)
	require.Len(t, rows, 4)
	var unresolved []string
	for _, row := range rows {
		if !run.IsTerminal(run.TaskStatus(row.Status)) {
			unresolved = append(unresolved, row.PartitionValue+"="+row.Status)
		}
	}
	require.Empty(t, unresolved,
		"a cancelled run must leave no instance pending or running; local mode has no recovery owner: %v", unresolved)

	// The rows it resolved say WHY, mirroring the unfanned local cancel path's
	// wording instead of blaming a dependency that does not exist. Status stays
	// SKIPPED rather than the unfanned path's `failed`: these never ran, so
	// `failed` would be a lie about work that never started.
	cancelledRows := 0
	for _, row := range rows {
		if row.Status != string(run.TaskStatusSkipped) {
			require.Equal(t, string(run.TaskStatusSucceeded), row.Status, "partition %s", row.PartitionValue)
			continue
		}
		cancelledRows++
		require.Contains(t, row.Error, "cancelled",
			"a partition resolved by the cancellation must say so, not report a phantom dependency")
		require.NotNil(t, row.CompletedAt, "a resolved instance must carry a completion timestamp")
	}
	require.Equal(t, 2, cancelledRows)

	// The instances that DID run keep their identity and output, so a later
	// `run retry --partition` can still rebuild the group's full aggregate from
	// the rows (the finding-3 contract, across a cancellation).
	var jobRun models.JobRun
	require.NoError(t, f.db.Where("job_id = ?", f.jobID).Order("created_at DESC").First(&jobRun).Error)
	identities, err := f.store.FanOutInstanceIdentities(t.Context(), jobRun.ID, f.fanned)
	require.NoError(t, err)
	require.Len(t, identities, 4)

	succeeded := 0
	for _, inst := range identities {
		if inst.Status != run.TaskStatusSucceeded {
			continue
		}
		succeeded++
		require.NotEmpty(t, inst.IdentityHash,
			"partition %s must keep its identity across the cancellation", inst.PartitionValue)
		require.NotEmpty(t, inst.Output,
			"partition %s must keep the output it recorded", inst.PartitionValue)
	}
	require.Equal(t, 2, succeeded)
}

// TestFanOutLocalCancelledMidFlightStillResolvesPendingSiblings covers the OTHER
// route out of the group loop, which the test above cannot reach.
//
// Detaching the sweep is not sufficient on its own: the loop's own
// `TaskRunInstances` read at the top of each pass still carries the run's
// context. When the cancellation lands while instances are IN FLIGHT, those
// instances report, the loop absorbs them and comes back around — and that read
// fails with context.Canceled before a single row is examined, returning from
// runFannedGroup without ever reaching the sweep. The pending siblings were
// stranded exactly as before. (This showed up as a ~75%-failure "flake" in the
// package: which of the two routes a cancellation takes depends on timing.)
//
// Slow containers plus a rate limit make the shape deterministic: two instances
// are still running and two are parked at the moment of cancellation.
func TestFanOutLocalCancelledMidFlightStillResolvesPendingSiblings(t *testing.T) {
	f := newFanOutFixture(t, `["a","b","c","d"]`, &schema.FanOut{
		From:          "list",
		MaxPartitions: 16,
		FailurePolicy: schema.FanOutFailureContinue,
	}, 0)
	seedRateLimitedFanOutStep(t, f, "warehouse-inflight-"+f.jobID.String()[:8], 2)
	for _, p := range []string{"a", "b", "c", "d"} {
		f.engine.runDurationByPartition[p] = 5 * time.Second
	}

	cancel, done := runFanOutInBackground(t, f)
	defer cancel()

	// Two containers started and are still running; two are parked. Nothing is
	// terminal yet — that is what forces the loop back through its head read.
	require.Eventually(t, func() bool {
		terminal, parked := countParkedAndTerminal(f)
		started := 0
		for _, p := range []string{"a", "b", "c", "d"} {
			started += f.engine.createCount(p)
		}
		return terminal == 0 && parked == 2 && started == 2
	}, 10*time.Second, 10*time.Millisecond,
		"two instances must be in flight and two parked when the run is cancelled")

	cancel()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("run did not return after cancellation")
	}

	rows := f.instanceRows(t)
	require.Len(t, rows, 4)
	var unresolved []string
	for _, row := range rows {
		if !run.IsTerminal(run.TaskStatus(row.Status)) {
			unresolved = append(unresolved, row.PartitionValue+"="+row.Status)
		}
	}
	require.Empty(t, unresolved,
		"cancelling mid-flight must still resolve the siblings that were never dispatched: %v", unresolved)
}
