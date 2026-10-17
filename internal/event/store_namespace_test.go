package event

import (
	"context"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestListRangeBoundsAndRunScope: ListRange never returns an event above its
// upper bound, and with runScoped under a namespace restriction returns only
// events of runs persisted in an allowed namespace (never runless ones);
// unrestricted, runScoped changes nothing (identity-and-access B4).
func TestListRangeBoundsAndRunScope(t *testing.T) {
	s := openStore(t)
	now := time.Now().UTC()
	jobID := uuid.New()
	run := func(namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, s.db.Create(&models.JobRun{ID: id, JobID: jobID, Namespace: namespace, Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	mkt, fin := run("marketing"), run("finance")
	appendEvent := func(evt *Event) uint64 {
		tx := s.db.Begin()
		require.NoError(t, s.AppendTx(tx, evt))
		require.NoError(t, tx.Commit().Error)
		return evt.Sequence
	}
	one := appendEvent(&Event{Type: TypeRunStarted, JobID: jobID, RunID: mkt})
	two := appendEvent(&Event{Type: TypeRunStarted, JobID: jobID, RunID: fin})
	appendEvent(&Event{Type: TypeJobPaused, JobID: jobID})
	four := appendEvent(&Event{Type: TypeRunCompleted, JobID: jobID, RunID: mkt})

	seqs := func(events []Event) []uint64 {
		out := make([]uint64, 0, len(events))
		for _, e := range events {
			out = append(out, e.Sequence)
		}
		return out
	}
	ctx := context.Background()
	all, err := s.ListSince(ctx, 0, 100, Filter{})
	require.NoError(t, err)
	require.Len(t, all, 4)
	bounded, err := s.ListRange(ctx, 0, two, 100, Filter{}, true)
	require.NoError(t, err)
	require.Equal(t, []uint64{one, two}, seqs(bounded), "the bound is inclusive and unrestricted callers are unscoped")

	marketing := authz.WithAllowedNamespaces(ctx, []string{"marketing"})
	scoped, err := s.ListRange(marketing, 0, 0, 100, Filter{}, true)
	require.NoError(t, err)
	require.Equal(t, []uint64{one, four}, seqs(scoped), "only marketing runs' events; the runless event is cluster-only")
	require.True(t, authz.Filtered(marketing))
	scoped, err = s.ListRange(marketing, one, four-1, 100, Filter{}, true)
	require.NoError(t, err)
	require.Empty(t, scoped, "a hidden tail before the bound returns nothing, so a catch-up ends")
	unscoped, err := s.ListRange(marketing, 0, 0, 100, Filter{}, false)
	require.NoError(t, err)
	require.Len(t, unscoped, 4, "the restriction applies only when the caller asks for it")
}

// TestListRangeHidesAdmissionRefusedRuns: a run admission refused (a consumed
// dataset is held) carries the upstream hold in its task rows, so its events
// never reach a confined subscriber, even one that may read its namespace.
func TestListRangeHidesAdmissionRefusedRuns(t *testing.T) {
	s := openStore(t)
	now := time.Now().UTC()
	held := uuid.New()
	require.NoError(t, s.db.Create(&models.JobRun{ID: held, JobID: uuid.New(), Namespace: "marketing", Status: "skipped",
		SkipReason: "dataset_hold:/upstream", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	tx := s.db.Begin()
	require.NoError(t, s.AppendTx(tx, &Event{Type: TypeTaskSkipped, RunID: held}))
	require.NoError(t, tx.Commit().Error)

	marketing := authz.WithAllowedNamespaces(context.Background(), []string{"marketing"})
	scoped, err := s.ListRange(marketing, 0, 0, 100, Filter{}, true)
	require.NoError(t, err)
	require.Empty(t, scoped)
	all, err := s.ListRange(context.Background(), 0, 0, 100, Filter{}, true)
	require.NoError(t, err)
	require.Len(t, all, 1, "an unrestricted subscriber still sees it")

	ownership, err := s.RunOwnership(context.Background(), held)
	require.NoError(t, err)
	require.Equal(t, "marketing", ownership.Namespace)
	require.False(t, ownership.Streamable())
}
