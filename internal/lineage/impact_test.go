package lineage

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/suite"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ImpactSuite tests QueryImpact over an in-memory SQLite graph.
type ImpactSuite struct {
	suite.Suite
	db  *gorm.DB
	ctx context.Context
}

func TestImpactSuite(t *testing.T) {
	suite.Run(t, new(ImpactSuite))
}

func (s *ImpactSuite) SetupTest() {
	dsn := "file:" + uuid.NewString() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	s.Require().NoError(err)
	s.Require().NoError(db.AutoMigrate(models.All...))
	s.db = db
	s.ctx = context.Background()
}

func (s *ImpactSuite) TearDownTest() {
	if s.db != nil {
		sqlDB, _ := s.db.DB()
		if sqlDB != nil {
			_ = sqlDB.Close()
		}
	}
}

// TestNoDownstream: querying a dataset with no consumers returns an empty list.
func (s *ImpactSuite) TestNoDownstream() {
	ns := "test"
	job, run := s.createJobAndRun("job-a", "abc123")
	s.createDataset(run, ns, "raw.table", "output", "extract")
	_ = job

	result, err := QueryImpact(s.ctx, s.db, ns, "raw.table", 0)
	s.Require().NoError(err)
	s.Equal(ns, result.RootNamespace)
	s.Equal("raw.table", result.RootName)
	s.Empty(result.Downstream)
}

// TestDirectConsumer: a single hop — dataset A → step that consumes A and
// produces dataset B.
func (s *ImpactSuite) TestDirectConsumer() {
	ns := "test"

	// Producer job: emits raw.table as output.
	_, runA := s.createJobAndRun("job-producer", "commit-producer")
	s.createDataset(runA, ns, "raw.table", "output", "extract")

	// Consumer job: takes raw.table as input, emits transformed.table as output.
	_, runB := s.createJobAndRun("job-consumer", "commit-consumer")
	s.createDataset(runB, ns, "raw.table", "input", "load")
	s.createDataset(runB, ns, "transformed.table", "output", "load")

	result, err := QueryImpact(s.ctx, s.db, ns, "raw.table", 0)
	s.Require().NoError(err)
	s.Require().Len(result.Downstream, 1)

	node := result.Downstream[0]
	s.Equal("transformed.table", node.DatasetName)
	s.Equal(ns, node.DatasetNamespace)
	s.Equal("output", node.Direction)
	s.Equal("load", node.ProducingStep)
	s.Equal("job-consumer", node.JobAlias)
	s.Equal("commit-consumer", node.ProvenanceCommit)
	s.Equal(0, node.Depth)
}

// TestTransitiveChain: A → B → C — querying A should return both B and C.
func (s *ImpactSuite) TestTransitiveChain() {
	ns := "test"

	// Step 1: emits dataset-a.
	_, runA := s.createJobAndRun("job-a", "")
	s.createDataset(runA, ns, "dataset-a", "output", "step-a")

	// Step 2: consumes dataset-a, emits dataset-b.
	_, runB := s.createJobAndRun("job-b", "")
	s.createDataset(runB, ns, "dataset-a", "input", "step-b")
	s.createDataset(runB, ns, "dataset-b", "output", "step-b")

	// Step 3: consumes dataset-b, emits dataset-c.
	_, runC := s.createJobAndRun("job-c", "")
	s.createDataset(runC, ns, "dataset-b", "input", "step-c")
	s.createDataset(runC, ns, "dataset-c", "output", "step-c")

	result, err := QueryImpact(s.ctx, s.db, ns, "dataset-a", 0)
	s.Require().NoError(err)
	s.Require().Len(result.Downstream, 2)

	byName := make(map[string]ImpactNode, 2)
	for _, n := range result.Downstream {
		byName[n.DatasetName] = n
	}

	b, ok := byName["dataset-b"]
	s.True(ok, "dataset-b should be in downstream")
	s.Equal(0, b.Depth)

	c, ok := byName["dataset-c"]
	s.True(ok, "dataset-c should be in downstream")
	s.Equal(1, c.Depth)
}

// TestCycleGuard: a producer→consumer→producer cycle should not loop forever.
func (s *ImpactSuite) TestCycleGuard() {
	ns := "test"

	// Run 1 produces x and consumes y (simulating a cycle).
	_, run1 := s.createJobAndRun("job-cycle", "")
	s.createDataset(run1, ns, "x", "output", "step")
	s.createDataset(run1, ns, "y", "input", "step")

	// Run 2 consumes x and produces y (completing the cycle).
	_, run2 := s.createJobAndRun("job-cycle-b", "")
	s.createDataset(run2, ns, "x", "input", "step2")
	s.createDataset(run2, ns, "y", "output", "step2")

	// Should not hang; cycle guard keeps result finite.
	result, err := QueryImpact(s.ctx, s.db, ns, "x", 5)
	s.Require().NoError(err)
	s.NotNil(result)
	// y appears exactly once even though the cycle would revisit x.
	count := 0
	for _, n := range result.Downstream {
		if n.DatasetName == "y" {
			count++
		}
	}
	s.Equal(1, count)
}

// TestMaxDepthRespected: a chain longer than maxDepth is truncated.
func (s *ImpactSuite) TestMaxDepthRespected() {
	ns := "test"

	// Build a chain of 5 hops.
	datasets := []string{"d0", "d1", "d2", "d3", "d4", "d5"}
	for i := 0; i < len(datasets)-1; i++ {
		_, run := s.createJobAndRun("hop-job-"+datasets[i], "")
		s.createDataset(run, ns, datasets[i], "input", "step")
		s.createDataset(run, ns, datasets[i+1], "output", "step")
	}

	// maxDepth=2 should return d1 (depth 0) and d2 (depth 1) only.
	result, err := QueryImpact(s.ctx, s.db, ns, "d0", 2)
	s.Require().NoError(err)

	seen := make(map[string]bool)
	for _, n := range result.Downstream {
		seen[n.DatasetName] = true
	}
	s.True(seen["d1"])
	s.True(seen["d2"])
	s.False(seen["d3"], "depth 3 should be truncated at maxDepth=2")
}

// TestCrossJobBoundary: consumers in a different job are returned with their
// job alias and provenance.
func (s *ImpactSuite) TestCrossJobBoundary() {
	ns := "test"

	jobA, runA := s.createJobAndRun("job-source", "sha-source")
	s.createDataset(runA, ns, "events.raw", "output", "ingest")
	_ = jobA

	jobB, runB := s.createJobAndRunWithRepo("job-sink", "sha-sink", "https://github.com/org/repo")
	s.createDataset(runB, ns, "events.raw", "input", "transform")
	s.createDataset(runB, ns, "events.cleaned", "output", "transform")
	_ = jobB

	result, err := QueryImpact(s.ctx, s.db, ns, "events.raw", 0)
	s.Require().NoError(err)
	s.Require().Len(result.Downstream, 1)

	node := result.Downstream[0]
	s.Equal("events.cleaned", node.DatasetName)
	s.Equal("job-sink", node.JobAlias)
	s.Equal("sha-sink", node.ProvenanceCommit)
	s.Equal("https://github.com/org/repo", node.ProvenanceRepo)
}

// TestStepNameFromFacet: helper correctly extracts step names.
func (s *ImpactSuite) TestStepNameFromFacet() {
	raw := marshalFacet(map[string]interface{}{
		"caesium_dataset": map[string]interface{}{
			"step_name": "my-step",
		},
	})
	s.Equal("my-step", stepNameFromFacet(raw))
	s.Equal("", stepNameFromFacet(nil))
	s.Equal("", stepNameFromFacet([]byte(`{}`)))
	s.Equal("", stepNameFromFacet([]byte(`not-json`)))
}

// --- helpers ---

func (s *ImpactSuite) createJobAndRun(alias, commit string) (*models.Job, *models.TaskRun) {
	return s.createJobAndRunWithRepo(alias, commit, "")
}

func (s *ImpactSuite) createJobAndRunWithRepo(alias, commit, repo string) (*models.Job, *models.TaskRun) {
	triggerID := uuid.New()
	s.Require().NoError(s.db.Create(&models.Trigger{
		ID:   triggerID,
		Type: models.TriggerTypeCron,
	}).Error)

	job := &models.Job{
		ID:               uuid.New(),
		Alias:            alias,
		TriggerID:        triggerID,
		ProvenanceCommit: commit,
		ProvenanceRepo:   repo,
	}
	s.Require().NoError(s.db.Create(job).Error)

	atomID := uuid.New()
	s.Require().NoError(s.db.Create(&models.Atom{
		ID:     atomID,
		Engine: models.AtomEngineDocker,
		Image:  "alpine:3.23",
	}).Error)

	task := &models.Task{
		ID:     uuid.New(),
		JobID:  job.ID,
		AtomID: atomID,
		Name:   alias + "-task",
	}
	s.Require().NoError(s.db.Create(task).Error)

	jobRun := &models.JobRun{
		ID:          uuid.New(),
		JobID:       job.ID,
		TriggerID:   triggerID,
		Status:      "succeeded",
		StartedAt:   time.Now().UTC(),
		CompletedAt: ptrTime(time.Now().UTC()),
	}
	s.Require().NoError(s.db.Create(jobRun).Error)

	taskRun := &models.TaskRun{
		ID:        uuid.New(),
		JobRunID:  jobRun.ID,
		TaskID:    task.ID,
		AtomID:    atomID,
		Engine:    models.AtomEngineDocker,
		Image:     "alpine:3.23",
		Command:   "[]",
		Status:    "succeeded",
		ClaimedBy: "",
	}
	s.Require().NoError(s.db.Create(taskRun).Error)

	return job, taskRun
}

func (s *ImpactSuite) createDataset(run *models.TaskRun, ns, name, direction, stepName string) {
	facet := marshalFacet(map[string]interface{}{
		"caesium_dataset": map[string]interface{}{
			"step_name": stepName,
			"direction": direction,
		},
	})
	ds := &models.LineageDataset{
		ID:           uuid.New(),
		TaskRunID:    run.ID,
		Namespace:    ns,
		Name:         name,
		Direction:    direction,
		FacetSummary: datatypes.JSON(facet),
		CreatedAt:    time.Now().UTC(),
	}
	s.Require().NoError(s.db.Create(ds).Error)
}

func ptrTime(t time.Time) *time.Time { return &t }

func marshalFacet(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
