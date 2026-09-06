package worker

import (
	"testing"

	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// Under CAESIUM_TASK_FAILURE_POLICY=continue the worker sweeps a failed task's
// descendants and skips them. It used to skip EVERY descendant, with no regard
// for trigger rules — unlike the local executor's skipDescendantsFiltered,
// which has always left tolerant successors to their own rule evaluation.
//
// That drift defeated the failed-plain-task advancement outright on the
// distributed lane: the store releases an `all_done` consumer inside the
// failure transaction (outstanding_predecessors → 0, task_ready), and this
// sweep then marked the very same row `skipped` before the owner's next
// dispatch tick could claim it. The consumer was released and immediately
// buried.
//
// The DAG below is the smallest shape that separates the two behaviours:
//
//	gate ──┬──▶ strict   (all_success)  ──▶ strict-child
//	       └──▶ tolerant (all_done)     ──▶ beyond
func TestCollectDescendantsFromEdgesSkipsOnlyIntolerantSuccessors(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	jobID := uuid.New()
	atomID := uuid.New()
	mk := func(name, rule string) *models.Task {
		task := &models.Task{ID: uuid.New(), JobID: jobID, AtomID: atomID, Name: name, TriggerRule: rule}
		require.NoError(t, db.Create(task).Error)
		return task
	}

	gate := mk("gate", jobdefschema.TriggerRuleAllSuccess)
	strict := mk("strict", jobdefschema.TriggerRuleAllSuccess)
	strictChild := mk("strict-child", jobdefschema.TriggerRuleAllSuccess)
	tolerant := mk("tolerant", jobdefschema.TriggerRuleAllDone)
	beyond := mk("beyond", jobdefschema.TriggerRuleAllSuccess)

	for _, edge := range [][2]uuid.UUID{
		{gate.ID, strict.ID},
		{strict.ID, strictChild.ID},
		{gate.ID, tolerant.ID},
		{tolerant.ID, beyond.ID},
	} {
		require.NoError(t, db.Create(&models.TaskEdge{
			ID: uuid.New(), JobID: jobID, FromTaskID: edge[0], ToTaskID: edge[1],
		}).Error)
	}

	got, err := collectDescendantsFromEdges(db, gate.ID)
	require.NoError(t, err)

	require.ElementsMatch(t, []uuid.UUID{strict.ID, strictChild.ID}, got,
		"only the intolerant branch is a casualty of gate's failure")
	require.NotContains(t, got, tolerant.ID,
		"an all_done consumer must be left to its own rule — the store just released it")
	require.NotContains(t, got, beyond.ID,
		"the walk stops at a tolerant node: what lies beyond depends on how it resolves, which is not known yet")
}

// TestCollectDescendantsFromEdgesTreatsUnknownRuleAsIntolerant pins the safe
// direction for a successor whose catalog row is missing: the empty rule is
// all_success, so it is swept rather than left dangling.
func TestCollectDescendantsFromEdgesTreatsUnknownRuleAsIntolerant(t *testing.T) {
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })

	jobID := uuid.New()
	start := uuid.New()
	orphan := uuid.New()
	require.NoError(t, db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: jobID, FromTaskID: start, ToTaskID: orphan,
	}).Error)

	got, err := collectDescendantsFromEdges(db, start)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{orphan}, got)
}
