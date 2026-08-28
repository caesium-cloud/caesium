package run

import (
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestCheckpointWriter_CadenceAdvancesPerFanOutInstance closes G3's stated
// checkpoint-cadence concern deliberately, rather than leaving it closed by
// accident.
//
// CheckpointWriter.due keys off RunState.seq, and G3 flagged that a fanned group
// could starve checkpointing "exactly when the run holds the most state": if a
// whole group's completion advanced the cursor once, an events-based cadence
// would see one event for N partitions and a large group would cross no
// threshold at all. Under D4's expansion the cursor advances per INSTANCE — each
// sibling is a distinct terminal transition with its own sequence — so the
// cadence scales with the group instead of ignoring it.
//
// The assertion is the arithmetic that only holds if that is true: eight
// partitions completing against Events=4 must write two checkpoints, at
// sequences 4 and 8. Were the group one event, the same eight completions would
// write NONE.
func TestCheckpointWriter_CadenceAdvancesPerFanOutInstance(t *testing.T) {
	const (
		partitions      = 8
		eventsThreshold = 4
	)

	b := newTopoBuilder()
	producer, fanned, publish := b.task(""), b.task(""), b.task("")
	b.edge(producer, fanned)
	b.edge(fanned, publish)
	rs := NewRunState(b.build(), 0)

	parts := make([]pkgtask.Partition, 0, partitions)
	for i := 0; i < partitions; i++ {
		parts = append(parts, pkgtask.Partition{Key: string(rune('a' + i))})
	}
	insts := make([]ExpandedInstance, 0, len(parts))
	ids := make([]uuid.UUID, 0, len(parts))
	base := rs.indegree[fanned]
	for i, p := range parts {
		id := uuid.New()
		ids = append(ids, id)
		insts = append(insts, ExpandedInstance{
			TaskRunID: id, TaskID: fanned, PartitionIndex: i, Partition: p,
			OutstandingPredecessors: base,
		})
	}
	rs.ApplyExpansion(&FanOutExpansion{
		ProducerTaskID: producer,
		Partitions:     parts,
		Groups:         []ExpandedGroup{{TaskID: fanned, TaskName: "process", Instances: insts}},
	})

	// Interval is an hour, so every write below is due on the EVENT count alone
	// — the cadence this test is about, with no wall-clock escape hatch.
	f := &fakePersister{}
	w := NewCheckpointWriter(f, uuid.New(), CheckpointConfig{
		Events: eventsThreshold, Interval: time.Hour, KeepFulls: 3,
	})

	rs.ApplyCompletion(producer, TaskStatusSucceeded, nil) // sequence 1
	require.NoError(t, w.Maybe(rs, 1))
	require.Equal(t, 0, f.writes, "one terminal transition is below the threshold")

	writesAt := make(map[int]int64)
	for i, id := range ids {
		res := rs.ApplyCompletion(id, TaskStatusSucceeded, nil)
		require.True(t, res.Applied)
		require.Equalf(t, int64(i+2), res.TerminalSequence,
			"instance %d must be stamped with its OWN sequence; a group that advances the cursor once starves the cadence", i)

		before := f.writes
		require.NoError(t, w.Maybe(rs, 1))
		if f.writes > before {
			writesAt[i] = f.lastSeq
		}
	}

	require.Equal(t, 2, f.writes,
		"%d instances at a %d-event cadence must checkpoint twice; one event per GROUP would checkpoint zero times",
		partitions, eventsThreshold)
	require.Equal(t, f.writes, f.prunes, "every checkpoint prunes")
	require.Equal(t, map[int]int64{2: 4, 6: 8}, writesAt,
		"checkpoints must land every %d instance completions, not once at the end", eventsThreshold)
	// The last instance's transition is one event past the second threshold, so
	// it stays in the replay tail rather than in a checkpoint — which is the
	// cadence working, not a gap: recovery reads checkpoint + tail.
	require.Equal(t, int64(partitions), f.lastSeq)

	// And the cadence keeps working past the group: the fan-in successor's own
	// completion is one more event on the same cursor.
	res := rs.ApplyCompletion(publish, TaskStatusSucceeded, nil)
	require.Equal(t, int64(partitions+2), res.TerminalSequence)
	require.True(t, rs.IsComplete())
}

// TestOwnerManager_CheckpointsOnceEveryNInstanceCompletions closes the other
// half of G3's starvation concern through the PRODUCTION wiring.
//
// The test above pins that the sequence cursor advances per instance, but it
// calls CheckpointWriter.Maybe itself, once per instance — so it assumes by
// construction the thing that would also cause starvation: Maybe being *called*
// once per group rather than once per completion. The only production call site
// is OwnerManager.CompleteInstance's res.Durable() branch
// (internal/run/owner_manager.go), so this drives real completions through the
// manager and reads the durable run_checkpoints rows instead of a fake
// persister. A group that checkpointed once per group would leave NO rows here:
// six instance completions would be one event against a three-event threshold.
func TestOwnerManager_CheckpointsOnceEveryNInstanceCompletions(t *testing.T) {
	const eventsThreshold = 3

	// No maxParallel: every partition is offered at once, so the completions
	// below are the only thing pacing the cadence.
	f := newFanOutFailoverFixture(t, `{"from":"list"}`)
	// KeepFulls is deliberately large: pruning would delete the very rows this
	// test counts.
	mgr := NewOwnerManager(f.store, CheckpointConfig{
		Events: eventsThreshold, Interval: time.Hour, KeepFulls: 100,
	})
	require.NoError(t, mgr.Adopt(f.runID, 1))

	// seed (sequence 1) and the producer (sequence 2) are two events — below the
	// threshold, so nothing is written yet.
	mgr.MarkDispatched(f.runID, f.seed, "node-1", 1, 0)
	_, err := mgr.Complete(f.runID, f.seed, TaskStatusSucceeded, "success", "", "", nil, nil)
	require.NoError(t, err)

	parts := []pkgtask.Partition{
		{Key: "a"}, {Key: "b"}, {Key: "c"}, {Key: "d"}, {Key: "e"}, {Key: "f"},
	}
	mgr.MarkDispatched(f.runID, f.list, "node-1", 1, 0)
	res, err := mgr.CompleteInstance(f.runID, f.list, uuid.Nil, TaskStatusSucceeded,
		"success", "", "", nil, nil, parts)
	require.NoError(t, err)
	require.Len(t, res.Ready, len(parts))
	require.Empty(t, checkpointSequences(t, f), "two events is below the three-event threshold")

	ready := mgr.ReadyForDispatch(f.runID)
	require.Len(t, ready, len(parts))
	for _, dt := range ready {
		mgr.MarkDispatched(f.runID, dt.ExecutionRef(), "node-1", dt.Attempt, 0)
	}
	// Instances take sequences 3..8, so the writer is due at 3 and again at 6.
	for _, dt := range ready {
		_, err := mgr.CompleteInstance(f.runID, dt.TaskID, dt.TaskRunID,
			TaskStatusSucceeded, "success", "", "", nil, nil, nil)
		require.NoError(t, err)
	}

	require.Equal(t, []int64{3, 6}, checkpointSequences(t, f),
		"CompleteInstance must run the checkpoint cadence per INSTANCE; once per group "+
			"would leave no checkpoint at all for a six-partition group")
}

// checkpointSequences returns the run's persisted checkpoint sequence_high
// values in ascending order.
func checkpointSequences(t *testing.T, f *fanOutFailoverFixture) []int64 {
	t.Helper()
	var rows []models.RunCheckpoint
	require.NoError(t, f.db.Where("run_id = ?", f.runID.String()).
		Order("sequence_high ASC").Find(&rows).Error)
	out := make([]int64, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.SequenceHigh)
	}
	return out
}
