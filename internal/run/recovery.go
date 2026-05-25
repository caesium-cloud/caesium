package run

import (
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// RecoveryResult summarizes what a recovering owner must do after reconstructing
// a run's state from a checkpoint plus the post-checkpoint terminal rows.
type RecoveryResult struct {
	// Ready are pending tasks whose predecessors are satisfied — dispatch them.
	Ready []uuid.UUID
	// ReDispatch are tasks left running by the previous owner with no terminal
	// row, i.e. in-flight work whose worker outcome never reached the owner.
	// They are re-dispatched with attempt+1 and a fresh claim.
	ReDispatch []uuid.UUID
	// MaxSequence is the highest terminal_sequence observed (checkpoint or row);
	// the recovered owner continues allocating from here.
	MaxSequence int64
	// SequenceGaps are missing terminal_sequence values between the checkpoint
	// and the highest observed row.  A gap means the previous owner allocated a
	// sequence but crashed before persisting the row; the affected task is
	// treated as never-completed and recovered via ReDispatch.  Reported for
	// observability; not an error.
	SequenceGaps []int64
	// Complete is true when every task is already terminal (nothing to do).
	Complete bool
}

// RecoverRunState reconstructs a RunState for an owned run after an ownership
// change.  topo is reloaded from the catalog; checkpoint is the latest full
// snapshot (nil to replay from scratch); terminalRows are the post-checkpoint
// terminal task_runs rows ordered by terminal_sequence ascending (from
// Store.TerminalTaskRunsSince).
//
// It restores the snapshot (or a fresh state), replays each terminal row to
// advance the DAG, then classifies the leftover non-terminal tasks: running
// tasks become ReDispatch (their worker outcome was lost), and ready tasks
// become Ready.  Wall-clock time is never consulted; ordering is by
// terminal_sequence so recovery is deterministic and clock-skew-safe.
func RecoverRunState(topo RunTopology, checkpoint *models.RunCheckpoint, terminalRows []models.TaskRun) (*RunState, RecoveryResult, error) {
	var (
		rs    *RunState
		start int64
		err   error
	)
	if checkpoint != nil {
		start = checkpoint.SequenceHigh
		rs, err = Restore(topo, checkpoint.StateBlob)
		if err != nil {
			// Corrupt checkpoint: fall back to a from-scratch replay over all
			// terminal rows (the design's "rebuild from terminal rows only").
			rs = NewRunState(topo, 0)
			start = 0
		}
	} else {
		rs = NewRunState(topo, 0)
	}

	var res RecoveryResult
	expected := start + 1
	for i := range terminalRows {
		row := &terminalRows[i]
		rs.ApplyTerminalRow(row.TaskID, TaskStatus(row.Status), row.TerminalSequence)

		// Dense-sequence gap check: every persisted terminal_sequence should be
		// the next after the previous. A hole means a sequence was allocated but
		// its row never landed (owner crashed mid-write).
		for expected < row.TerminalSequence {
			res.SequenceGaps = append(res.SequenceGaps, expected)
			expected++
		}
		if row.TerminalSequence >= expected {
			expected = row.TerminalSequence + 1
		}
	}

	res.MaxSequence = rs.Sequence()
	res.ReDispatch = rs.RunningTasks()
	// The reconstructed ready queue is authoritative: it holds every pending task
	// whose predecessors are satisfied (roots of a from-scratch replay plus
	// successors freed by the replayed tail).
	res.Ready = rs.ReadyTasks()
	res.Complete = rs.IsComplete()
	return rs, res, nil
}
