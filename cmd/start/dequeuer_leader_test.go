package start

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/internal/runqueue"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestDequeuerLeaderCheckIsBackendAware: on the internal dqlite database the
// dequeuer drains only on the leader; on PostgreSQL, which has no dqlite app,
// it carries no leadership gate (as the cron scheduler) instead of failing
// every tick and stranding quota-held runs.
func TestDequeuerLeaderCheckIsBackendAware(t *testing.T) {
	for _, databaseType := range []string{"", "internal", "dqlite", " DQLITE "} {
		require.NotNil(t, dequeuerLeaderCheck(databaseType), "%q", databaseType)
	}
	require.Nil(t, dequeuerLeaderCheck("postgres"))
	require.Nil(t, dequeuerLeaderCheck("Postgres"))
}

// TestRunQueueDrainMode: the queue flags run the full dequeuer and a policy
// file the quota-only one, without a query; with neither, quota-held rows
// decide, and a failed check starts the (read-only when idle) quota-only
// dequeuer rather than strand them.
func TestRunQueueDrainMode(t *testing.T) {
	never := func(context.Context) (bool, error) {
		t.Fatal("a flag or a policy file decides without a query")
		return false, nil
	}
	answer := func(held bool, err error) func(context.Context) (bool, error) {
		return func(context.Context) (bool, error) { return held, err }
	}
	cases := []struct {
		name             string
		vars             env.Environment
		check            func(context.Context) (bool, error)
		start, quotaOnly bool
	}{
		{"queue enabled", env.Environment{RunQueueEnabled: true, AuthPolicyFile: "/p.yaml"}, never, true, false},
		{"dequeuer enabled", env.Environment{RunQueueDequeuerEnabled: true}, never, true, false},
		{"policy file", env.Environment{AuthPolicyFile: "/p.yaml"}, never, true, true},
		{"never had a policy", env.Environment{}, answer(false, nil), false, true},
		{"policy removed, rows held", env.Environment{}, answer(true, nil), true, true},
		{"check failed", env.Environment{}, answer(false, errors.New("boom")), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, quotaOnly := runQueueDrainMode(t.Context(), tc.vars, tc.check)
			require.Equal(t, tc.start, start)
			if start {
				require.Equal(t, tc.quotaOnly, quotaOnly)
			}
		})
	}
}

// TestRestartAfterPolicyRemovalDrainsQuotaHeldRuns is the restart-shaped
// regression: a policy file parked a policy-free request (held_reason
// namespace_quota) with both queue flags off; the operator removed the file
// and restarted. The startup decision now starts the quota-only dequeuer, and
// it promotes the request under the empty policy. Once drained, a further
// restart starts nothing.
func TestRestartAfterPolicyRemovalDrainsQuotaHeldRuns(t *testing.T) {
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	previous := policy.ProcessHolder()
	t.Cleanup(func() {
		run.ExpectNamespacePolicy(false)
		policy.SetProcessHolder(previous)
	})
	policy.SetProcessHolder(nil)

	job := &models.Job{ID: uuid.New(), Alias: "restart-drain", Namespace: "marketing"}
	require.NoError(t, db.Create(job).Error)
	reason, namespace := models.RunQueueHeldReasonNamespaceQuota, "marketing"
	require.NoError(t, db.Create(&models.RunQueue{
		ID: uuid.New(), JobID: job.ID, Priority: run.PriorityNormalValue,
		CreatedAt: time.Now().UTC(), Namespace: &namespace, HeldReason: &reason,
	}).Error)

	vars := env.Environment{} // no queue flags, no policy file
	store := run.NewStore(db)
	run.ExpectNamespacePolicy(vars.AuthPolicyFile != "")
	start, quotaOnly := runQueueDrainMode(t.Context(), vars, store.HasQuotaHeldRuns)
	require.True(t, start, "held rows start the dequeuer although no policy is configured")
	require.True(t, quotaOnly)

	var launched []uuid.UUID
	d := runqueue.NewDequeuer(runqueue.Config{
		DB: db, Store: store, NodeID: "node-restart", QuotaOnly: quotaOnly,
		LaunchRun: func(_ context.Context, j *models.Job, _ *run.JobRun) { launched = append(launched, j.ID) },
	})
	require.NoError(t, d.DrainOnce(t.Context()))
	require.Equal(t, []uuid.UUID{job.ID}, launched, "the parked request runs: its namespace is unlimited now")
	var left int64
	require.NoError(t, db.Model(&models.RunQueue{}).Count(&left).Error)
	require.Zero(t, left)

	start, _ = runQueueDrainMode(t.Context(), vars, store.HasQuotaHeldRuns)
	require.False(t, start, "nothing held: a deployment without a policy starts no dequeuer")
}
