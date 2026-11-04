package lineage

import (
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TestImpactNodeCarriesProducingRunID: a node names the job run that wrote its
// dataset row, so the console can link to that run.
func (s *ImpactSuite) TestImpactNodeCarriesProducingRunID() {
	const ns = "test"
	_, runA := s.createJobAndRun("job-producer", "c1")
	s.createDataset(runA, ns, "raw.table", "output", "extract")
	_, runB := s.createJobAndRun("job-consumer", "c2")
	s.createDataset(runB, ns, "raw.table", "input", "load")
	s.createDataset(runB, ns, "out.table", "output", "load")

	result, err := QueryImpact(s.ctx, s.db, ns, "raw.table", 0)
	s.Require().NoError(err)
	s.Require().Len(result.Downstream, 1)
	s.Equal(runB.JobRunID.String(), result.Downstream[0].RunID)

	raw, err := json.Marshal(result)
	s.Require().NoError(err)
	s.Contains(string(raw), `"run_id":"`+runB.JobRunID.String()+`"`)
}

// TestQueryRunDatasets lists a run's persisted dataset identities and rejects
// a run that does not belong to the job.
func (s *ImpactSuite) TestQueryRunDatasets() {
	const ns = "test"
	jobB, runB := s.createJobAndRun("job-consumer", "c2")
	s.createDataset(runB, ns, "out.table", "output", "load")
	s.createDataset(runB, ns, "raw.table", "input", "load")
	_, other := s.createJobAndRun("job-other", "c3")
	s.createDataset(other, ns, "unrelated", "output", "x")

	got, err := QueryRunDatasets(s.ctx, s.db, jobB.ID, runB.JobRunID)
	s.Require().NoError(err)
	s.Equal(runB.JobRunID, got.RunID)
	s.Require().Len(got.Datasets, 2)
	s.Equal("input", got.Datasets[0].Direction)
	s.Equal("raw.table", got.Datasets[0].Name)
	s.Equal("job-consumer-task", got.Datasets[0].TaskName)
	s.Equal("out.table", got.Datasets[1].Name)

	_, err = QueryRunDatasets(s.ctx, s.db, uuid.New(), runB.JobRunID)
	s.ErrorIs(err, gorm.ErrRecordNotFound)
}

// TestRepeatedRunsCollapseToLatestProducer: two runs of one job that consume
// and produce the same datasets yield ONE node whose run_id is the later run.
// Before the collapse every historical run's row was its own node.
func (s *ImpactSuite) TestRepeatedRunsCollapseToLatestProducer() {
	const ns = "test"
	_, prod := s.createJobAndRun("job-producer", "c1")
	s.createDataset(prod, ns, "raw.table", "output", "extract")

	job, first := s.createJobAndRun("job-consumer", "c2")
	s.createDataset(first, ns, "raw.table", "input", "load")
	s.createDataset(first, ns, "out.table", "output", "load")
	// Backdate the first run's rows so the ordering is unambiguous.
	s.Require().NoError(s.db.Model(&models.LineageDataset{}).Where("task_run_id = ?", first.ID).
		Update("created_at", time.Now().UTC().Add(-time.Hour)).Error)

	second := s.createTaskRunInNewJobRun(job, "job-consumer-task")
	s.createDataset(second, ns, "raw.table", "input", "load")
	s.createDataset(second, ns, "out.table", "output", "load")

	result, err := QueryImpact(s.ctx, s.db, ns, "raw.table", 0)
	s.Require().NoError(err)
	s.Require().Len(result.Downstream, 1)
	s.Equal(second.JobRunID.String(), result.Downstream[0].RunID)
	s.NotEqual(first.JobRunID, second.JobRunID)
}

// createTaskRunInNewJobRun adds a later run of an existing job reusing its task.
func (s *ImpactSuite) createTaskRunInNewJobRun(job *models.Job, taskName string) *models.TaskRun {
	var task models.Task
	s.Require().NoError(s.db.Where("job_id = ? AND name = ?", job.ID, taskName).First(&task).Error)
	jr := &models.JobRun{ID: uuid.New(), JobID: job.ID, TriggerID: job.TriggerID, Status: "succeeded", StartedAt: time.Now().UTC()}
	s.Require().NoError(s.db.Create(jr).Error)
	tr := &models.TaskRun{
		ID: uuid.New(), JobRunID: jr.ID, TaskID: task.ID, AtomID: task.AtomID,
		Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: "[]", Status: "succeeded",
	}
	s.Require().NoError(s.db.Create(tr).Error)
	return tr
}
