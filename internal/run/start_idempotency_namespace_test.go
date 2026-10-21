package run

import (
	"context"
	"errors"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var errReplayDenied = errors.New("replay guard: denied")

func moveJob(t *testing.T, db *gorm.DB, jobID uuid.UUID, namespace string) {
	t.Helper()
	require.NoError(t, db.Model(&models.Job{}).Where("id = ?", jobID).Update("namespace", namespace).Error)
}

// recordingGuard returns a replay guard that records every namespace set it
// is called with and answers with result.
func recordingGuard(result error) (StartOption, *[][]string) {
	var seen [][]string
	return WithStartReplayGuard(func(namespaces []string) error {
		seen = append(seen, append([]string(nil), namespaces...))
		return result
	}), &seen
}

// TestReplayGuardRunsBeforeRequestComparison: after the job moved, a retry
// under a guard that refuses the record's namespace gets the guard's error
// whether its request matches the recorded one or not, on the up-front lookup
// and on FindIdempotentStart (the paused-job path) alike. A guard that allows
// still gets the comparison, and a fresh key never consults the guard.
func TestReplayGuardRunsBeforeRequestComparison(t *testing.T) {
	ctx := context.Background()
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-guard", jobdef.ConcurrencyStrategyFail, 5)
	moveJob(t, db, job.ID, "marketing")
	params := map[string]string{"region": "eu"}

	first, err := store.StartWithResult(ctx, job.ID, nil, WithStartParams(params), WithStartIdempotencyKey("k"))
	require.NoError(t, err)
	require.Equal(t, "marketing", first.Run.Namespace)
	record, err := findStartIdempotency(db, job.ID, "k")
	require.NoError(t, err)
	require.Equal(t, "marketing", record.Namespace, "the record carries the namespace admission froze")
	moveJob(t, db, job.ID, "finance")

	deny, seen := recordingGuard(errReplayDenied)
	for _, request := range []map[string]string{params, {"region": "us"}} {
		_, err = store.StartWithResult(ctx, job.ID, nil, WithStartParams(request), WithStartIdempotencyKey("k"), deny)
		require.ErrorIs(t, err, errReplayDenied, "request %v", request)
		_, _, err = store.FindIdempotentStart(ctx, job.ID, WithStartParams(request), WithStartIdempotencyKey("k"), deny)
		require.ErrorIs(t, err, errReplayDenied, "request %v", request)
	}
	require.Equal(t, [][]string{{"marketing"}, {"marketing"}, {"marketing"}, {"marketing"}}, *seen)

	allow, _ := recordingGuard(nil)
	_, err = store.StartWithResult(ctx, job.ID, nil, WithStartParams(map[string]string{"region": "us"}), WithStartIdempotencyKey("k"), allow)
	require.ErrorIs(t, err, ErrIdempotencyKeyReused, "an authorised caller still has its request compared")
	got, err := store.StartWithResult(ctx, job.ID, nil, WithStartParams(params), WithStartIdempotencyKey("k"), allow)
	require.NoError(t, err)
	require.True(t, got.Replayed)
	require.Equal(t, first.Run.ID, got.Run.ID)

	fresh, freshSeen := recordingGuard(errReplayDenied)
	created, err := store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("fresh"), fresh)
	require.NoError(t, err)
	require.False(t, created.Replayed)
	require.Empty(t, *freshSeen, "a new key has no record to authorise")
	require.Equal(t, int64(2), countJobRuns(t, db, job.ID))
}

// TestReplayGuardOnConcurrentWinnerFallbacks forces the two paths where a
// concurrent start with the same key commits after this start's up-front
// lookup — the unique-index conflict and the refused admission — and checks
// that each authorises the winner's record before comparing requests. The
// enricher seam runs in exactly that window, so it stands in for the winner.
func TestReplayGuardOnConcurrentWinnerFallbacks(t *testing.T) {
	for _, tc := range []struct {
		name     string
		strategy string
		maxRuns  int
	}{
		{"unique conflict", jobdef.ConcurrencyStrategyFail, 5},
		{"refused admission", jobdef.ConcurrencyStrategyFail, 1},
	} {
		for _, allowed := range []bool{false, true} {
			name := tc.name + " denied"
			if allowed {
				name = tc.name + " allowed"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				db, store := openIdempotencyStore(t)
				job := createConcurrencyJob(t, db, "idem-fallback", tc.strategy, tc.maxRuns)
				moveJob(t, db, job.ID, "marketing")

				raced := false
				SetStartParamsEnricher(func(_ context.Context, conn *gorm.DB, jobID uuid.UUID, params map[string]string, _ bool) (map[string]string, error) {
					if raced {
						return params, nil
					}
					raced = true
					// The winner was admitted in marketing, under a different
					// request; the job has since moved to finance.
					winner, err := store.StartWithResult(context.Background(), jobID, nil)
					require.NoError(t, err)
					runID := winner.Run.ID
					require.NoError(t, conn.Create(&models.RunStartIdempotency{
						ID:             uuid.New(),
						JobID:          jobID,
						IdempotencyKey: "raced",
						RequestHash:    startRequestHash(map[string]string{"region": "us"}, ""),
						Outcome:        string(StartOutcomeCreated),
						RunID:          &runID,
						Namespace:      "marketing",
					}).Error)
					moveJob(t, conn, jobID, "finance")
					return params, nil
				})
				t.Cleanup(func() { SetStartParamsEnricher(nil) })

				var guardErr error
				if !allowed {
					guardErr = errReplayDenied
				}
				guard, seen := recordingGuard(guardErr)
				_, err := store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("raced"), guard)
				require.True(t, raced)
				if allowed {
					require.ErrorIs(t, err, ErrIdempotencyKeyReused)
				} else {
					require.ErrorIs(t, err, errReplayDenied, "the guard must refuse before the requests are compared")
				}
				require.Equal(t, [][]string{{"marketing"}}, *seen)
				require.Equal(t, int64(1), countJobRuns(t, db, job.ID), "the losing admission wrote nothing")
			})
		}
	}
}

// TestReplayGuardCoversRecordsWithoutRun: a skipped start names no run, but
// its record still carries the namespace it was admitted in, so a retry from
// a namespace that cannot read it is refused before comparison.
func TestReplayGuardCoversRecordsWithoutRun(t *testing.T) {
	ctx := context.Background()
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-guard-skip", jobdef.ConcurrencyStrategySkip, 1)
	moveJob(t, db, job.ID, "marketing")
	_, err := store.StartWithResult(ctx, job.ID, nil)
	require.NoError(t, err)
	skipped, err := store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("s"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeSkipped, skipped.Outcome)
	require.Nil(t, skipped.RunID)
	moveJob(t, db, job.ID, "finance")

	deny, seen := recordingGuard(errReplayDenied)
	_, err = store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("s"), deny)
	require.ErrorIs(t, err, errReplayDenied)
	_, err = store.StartWithResult(ctx, job.ID, nil, WithStartPriority("high"), WithStartIdempotencyKey("s"), deny)
	require.ErrorIs(t, err, errReplayDenied)
	require.Equal(t, [][]string{{"marketing"}, {"marketing"}}, *seen)
}

// TestReplayGuardSeesPromotedRunNamespace: a start queued in marketing and
// promoted after the job moved to finance is attributed to both namespaces.
// Since identity-and-access D1 a queued row records its namespace and is
// never promoted across a move (TestQueuedRunNeverPromotedAcrossANamespaceMove),
// so the only row that can still be promoted this way is one queued before
// the column existed: its namespace is NULL and promotion keeps the old rule.
func TestReplayGuardSeesPromotedRunNamespace(t *testing.T) {
	ctx := context.Background()
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-guard-queue", jobdef.ConcurrencyStrategyQueue, 1)
	moveJob(t, db, job.ID, "marketing")
	holder, err := store.StartWithResult(ctx, job.ID, nil)
	require.NoError(t, err)
	queued, err := store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("q"))
	require.NoError(t, err)
	require.Equal(t, StartOutcomeQueued, queued.Outcome)
	require.NoError(t, db.Model(&models.RunQueue{}).Where("job_id = ?", job.ID).Update("namespace", nil).Error,
		"simulate a row queued before run_queue.namespace existed")

	moveJob(t, db, job.ID, "finance")
	finishRun(t, db, holder.Run.ID)
	promoted := promoteQueued(t, store, job.ID)
	require.Equal(t, "finance", promoted.Namespace)

	allow, seen := recordingGuard(nil)
	got, err := store.StartWithResult(ctx, job.ID, nil, WithStartIdempotencyKey("q"), allow)
	require.NoError(t, err)
	require.Equal(t, promoted.ID, got.Run.ID)
	require.Equal(t, [][]string{{"finance", "marketing"}}, *seen)
}

// TestIdempotentStartNamespaces pins the attribution rules, including the
// fail-closed cases: a record that names a run which no longer exists, and a
// record written before the namespace column that names no run.
func TestIdempotentStartNamespaces(t *testing.T) {
	ctx := context.Background()
	db, store := openIdempotencyStore(t)
	job := createConcurrencyJob(t, db, "idem-attribution", jobdef.ConcurrencyStrategyFail, 5)
	moveJob(t, db, job.ID, "finance")
	started, err := store.StartWithResult(ctx, job.ID, nil)
	require.NoError(t, err)
	runID, gone := started.Run.ID, uuid.New()

	for _, tc := range []struct {
		name   string
		record models.RunStartIdempotency
		want   []string
	}{
		{"recorded, no run", models.RunStartIdempotency{Namespace: "marketing"}, []string{"marketing"}},
		{"recorded, run elsewhere", models.RunStartIdempotency{Namespace: "marketing", RunID: &runID}, []string{"finance", "marketing"}},
		{"recorded, same run namespace", models.RunStartIdempotency{Namespace: "finance", RunID: &runID}, []string{"finance"}},
		{"legacy with run", models.RunStartIdempotency{RunID: &runID}, []string{"finance"}},
		{"legacy without run", models.RunStartIdempotency{}, nil},
		{"run gone", models.RunStartIdempotency{Namespace: "marketing", RunID: &gone}, nil},
	} {
		got, err := idempotentStartNamespaces(db, &tc.record)
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.want, got, tc.name)
	}
}
