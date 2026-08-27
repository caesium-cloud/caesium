package run

import (
	"encoding/json"
	"testing"

	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	jobdefschema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// branchFixture builds one branch task with three named successors, in either
// the live-catalog or the quarantined-replay shape. ResolveBranchSkips is the
// seam OwnerManager uses to turn a container's runtime branch decision into
// skips, and it had NO test coverage on either path — so these are
// characterization tests first, written before the two implementations were
// collapsed onto one enumeration, and they must keep passing byte-for-byte
// after.
type branchFixture struct {
	store     *Store
	db        *gorm.DB
	runID     uuid.UUID
	branchID  uuid.UUID
	successor map[string]uuid.UUID
}

func newBranchFixture(t *testing.T, quarantine bool, taskType string, successorNames []string) *branchFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })

	store := NewStore(db)
	jobID := uuid.New()
	require.NoError(t, db.Create(&models.Job{ID: jobID, Alias: "branch-fixture"}).Error)

	runRecord, err := store.Start(jobID, nil)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.JobRun{}).Where("id = ?", runRecord.ID).
		Update("quarantine", quarantine).Error)

	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["echo","hi"]`}
	require.NoError(t, db.Create(atom).Error)

	branch := &models.Task{
		ID: uuid.New(), JobID: jobID, AtomID: atom.ID, Name: "decide",
		Position: 0, Type: taskType, TriggerRule: jobdefschema.TriggerRuleAllSuccess,
	}
	require.NoError(t, db.Create(branch).Error)

	f := &branchFixture{store: store, db: db, runID: runRecord.ID, branchID: branch.ID, successor: map[string]uuid.UUID{}}

	refs := make([]models.TaskExecutionEdgeRef, 0, len(successorNames))
	for i, name := range successorNames {
		id := uuid.New()
		f.successor[name] = id
		require.NoError(t, db.Create(&models.Task{
			ID: id, JobID: jobID, AtomID: atom.ID, Name: name,
			Position: i + 1, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
		}).Error)
		require.NoError(t, db.Create(&models.TaskEdge{
			ID: uuid.New(), JobID: jobID, FromTaskID: branch.ID, ToTaskID: id,
		}).Error)
		refs = append(refs, models.TaskExecutionEdgeRef{TaskID: id, TaskName: name})
	}

	if quarantine {
		desc := models.TaskExecutionDescriptor{
			SchemaVersion: models.TaskExecutionDescriptorSchemaVersion,
			DAG: models.TaskExecutionDAG{
				Successors:     refs,
				TriggerRule:    jobdefschema.TriggerRuleAllSuccess,
				BranchBehavior: taskType,
			},
			Runtime: models.TaskExecutionRuntime{TaskType: taskType},
		}
		encoded, err := json.Marshal(&desc)
		require.NoError(t, err)
		require.NoError(t, db.Create(&models.TaskRun{
			ID: uuid.New(), JobRunID: runRecord.ID, TaskID: branch.ID, AtomID: atom.ID,
			Engine: models.AtomEngineDocker, Image: atom.Image, Command: atom.Command,
			Status: string(TaskStatusPending), Quarantine: true,
			ExecutionDescriptor: datatypes.JSON(encoded),
		}).Error)
	}
	return f
}

func (f *branchFixture) ids(names ...string) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(names))
	for _, n := range names {
		out = append(out, f.successor[n])
	}
	return out
}

func TestResolveBranchSkipsLiveSkipsUnselectedSuccessors(t *testing.T) {
	f := newBranchFixture(t, false, "branch", []string{"a", "b", "c"})

	skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"b"})
	require.NoError(t, err)
	assert.ElementsMatch(t, f.ids("a", "c"), skips,
		"every successor the branch did not select must be skipped")
}

func TestResolveBranchSkipsReplaySkipsUnselectedSuccessors(t *testing.T) {
	f := newBranchFixture(t, true, "branch", []string{"a", "b", "c"})

	skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"b"})
	require.NoError(t, err)
	assert.ElementsMatch(t, f.ids("a", "c"), skips,
		"a quarantined replay resolves the same skips from its FROZEN descriptor")
}

func TestResolveBranchSkipsSelectingEveryTargetSkipsNothing(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		f := newBranchFixture(t, quarantine, "branch", []string{"a", "b"})
		skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"a", "b"})
		require.NoError(t, err, "quarantine=%v", quarantine)
		assert.Empty(t, skips, "quarantine=%v", quarantine)
	}
}

func TestResolveBranchSkipsSelectingNothingSkipsEverything(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		f := newBranchFixture(t, quarantine, "branch", []string{"a", "b"})
		skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, nil)
		require.NoError(t, err, "quarantine=%v", quarantine)
		assert.ElementsMatch(t, f.ids("a", "b"), skips, "quarantine=%v", quarantine)
	}
}

// TestResolveBranchSkipsUnknownSelectionErrors pins the validation both paths
// share: a selection naming a step that is not a successor is a job-definition
// error surfaced to the operator, never a silently-ignored no-op that would
// leave the whole branch skipped.
func TestResolveBranchSkipsUnknownSelectionErrors(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		f := newBranchFixture(t, quarantine, "branch", []string{"a", "b"})
		_, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"nope"})
		require.Error(t, err, "quarantine=%v", quarantine)
		assert.Contains(t, err.Error(), `unknown step "nope"`, "quarantine=%v", quarantine)
		assert.Contains(t, err.Error(), "valid targets", "quarantine=%v", quarantine)
	}
}

// TestResolveBranchSkipsNonBranchTaskSkipsNothing pins that an ordinary task
// resolves no skips on either path — the owner calls this for every completion,
// not only for branch steps.
func TestResolveBranchSkipsNonBranchTaskSkipsNothing(t *testing.T) {
	for _, quarantine := range []bool{false, true} {
		f := newBranchFixture(t, quarantine, "task", []string{"a", "b"})
		skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"a"})
		require.NoError(t, err, "quarantine=%v", quarantine)
		assert.Empty(t, skips, "a non-branch task skips nothing; quarantine=%v", quarantine)
	}
}

// TestResolveBranchSkipsLiveIgnoresUnnamedSuccessor pins a deliberate asymmetry
// the collapse must NOT smooth over: on the live path a successor Task with an
// empty name is unselectable (it is absent from the name map) yet still
// skippable, whereas the replay path falls back to the task ID as its name.
// Preserving it is the difference between a refactor and a behavior change.
func TestResolveBranchSkipsLiveIgnoresUnnamedSuccessor(t *testing.T) {
	f := newBranchFixture(t, false, "branch", []string{"a"})
	unnamed := uuid.New()
	var branch models.Task
	require.NoError(t, f.db.First(&branch, "id = ?", f.branchID).Error)
	var atom models.Atom
	require.NoError(t, f.db.First(&atom).Error)
	require.NoError(t, f.db.Create(&models.Task{
		ID: unnamed, JobID: branch.JobID, AtomID: atom.ID, Name: "",
		Position: 9, Type: "task", TriggerRule: jobdefschema.TriggerRuleAllSuccess,
	}).Error)
	require.NoError(t, f.db.Create(&models.TaskEdge{
		ID: uuid.New(), JobID: branch.JobID, FromTaskID: f.branchID, ToTaskID: unnamed,
	}).Error)

	skips, err := f.store.ResolveBranchSkips(f.runID, f.branchID, []string{"a"})
	require.NoError(t, err)
	assert.Equal(t, []uuid.UUID{unnamed}, skips,
		"an unnamed successor can never be selected, so it is always skipped")
}
