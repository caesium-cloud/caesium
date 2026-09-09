package lineage

import (
	"time"

	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func (s *ImpactSuite) TestMapperPersistsDeclaredIdentityAlongsideArtifacts() {
	job, tr := s.createJobAndRun("declared-consumer", "commit")
	ns := "other-instance"
	declarations := []models.DatasetDeclaration{
		{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: job.Alias + "-task", Name: "warehouse/raw", Direction: models.DatasetDirectionConsumes},
		{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: job.Alias + "-task", Name: "warehouse/clean", Direction: models.DatasetDirectionProduces},
		{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: "different-step", Name: "unrelated", Direction: models.DatasetDirectionProduces},
		{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: job.Alias + "-task", Namespace: &ns, Name: "warehouse/clean", Direction: models.DatasetDirectionProduces},
	}
	s.Require().NoError(s.db.Create(&declarations).Error)
	// Real task instances are admitted after their job definition is applied.
	s.Require().NoError(s.db.Model(tr).Update("created_at", time.Now().UTC()).Error)
	m := newMapper("configured-lineage", s.db)
	payload := taskRunPayload{ID: tr.ID, JobRunID: tr.JobRunID, TaskID: tr.TaskID, Output: map[string]string{"artifact": "s3://bucket/artifact"}}
	evt := event.Event{Type: event.TypeTaskSucceeded, JobID: job.ID, RunID: tr.JobRunID, TaskID: tr.TaskID, Timestamp: time.Now(), Payload: marshalFacet(payload)}
	for range 2 {
		mapped, err := m.mapEvent(evt)
		s.Require().NoError(err)
		s.Require().Len(mapped.Inputs, 1)
		s.Equal("", mapped.Inputs[0].Namespace)
		s.Equal("warehouse/raw", mapped.Inputs[0].Name)
		s.Len(mapped.Outputs, 3, "declared and heuristic identities remain distinct")
	}
	var rows []models.LineageDataset
	s.Require().NoError(s.db.Where("task_run_id = ?", tr.ID).Find(&rows).Error)
	s.Len(rows, 4, "replayed lifecycle events must not duplicate the graph")
	impact, err := QueryImpact(s.ctx, s.db, "", "warehouse/raw", 0)
	s.Require().NoError(err)
	s.Len(impact.Downstream, 3)
	for _, node := range impact.Downstream {
		s.Equal(job.Alias+"-task", node.ProducingStep)
		s.NotEqual("unrelated", node.DatasetName)
	}
	wrongNamespace, err := QueryImpact(s.ctx, s.db, "configured-lineage", "warehouse/raw", 0)
	s.Require().NoError(err)
	s.Empty(wrongNamespace.Downstream, "matching names do not identify matching datasets")

	// Quarantine must never turn a what-if into an authoritative graph edge.
	evt.Quarantine = true
	mapped, err := m.mapEvent(evt)
	s.Require().NoError(err)
	s.Nil(mapped)
}

func (s *ImpactSuite) TestDelayedEventRejectsReplacementDeclarations() {
	job, tr := s.createJobAndRun("delayed-consumer", "commit")
	started := time.Now().UTC().Add(-time.Minute)
	s.Require().NoError(s.db.Model(tr).Update("created_at", started).Error)
	makeDeclarations := func(input, output string, created time.Time) []models.DatasetDeclaration {
		return []models.DatasetDeclaration{
			{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: job.Alias + "-task", Name: input, Direction: models.DatasetDirectionConsumes, CreatedAt: created, UpdatedAt: created},
			{ID: uuid.New(), JobID: job.ID, JobAlias: job.Alias, StepName: job.Alias + "-task", Name: output, Direction: models.DatasetDirectionProduces, CreatedAt: created, UpdatedAt: created},
		}
	}
	old := makeDeclarations("old/input", "old/output", started.Add(-time.Second))
	s.Require().NoError(s.db.Create(&old).Error)
	payload := taskRunPayload{ID: tr.ID, JobRunID: tr.JobRunID, TaskID: tr.TaskID}
	evt := event.Event{Type: event.TypeTaskStarted, JobID: job.ID, RunID: tr.JobRunID, TaskID: tr.TaskID, Timestamp: started, Payload: marshalFacet(payload)}
	m := newMapper("configured", s.db)
	_, err := m.mapEvent(evt)
	s.Require().NoError(err)
	s.Require().NoError(s.db.Where("job_id = ?", job.ID).Delete(&models.DatasetDeclaration{}).Error)
	replacement := makeDeclarations("new/input", "new/output", started.Add(time.Second))
	s.Require().NoError(s.db.Create(&replacement).Error)
	evt.Type = event.TypeTaskSucceeded
	evt.Timestamp = started.Add(2 * time.Second)
	mapped, err := m.mapEvent(evt)
	s.Require().NoError(err)
	s.Empty(mapped.Inputs, "a later event still belongs to its original execution")
	s.Empty(mapped.Outputs)
	oldImpact, err := QueryImpact(s.ctx, s.db, "", "old/input", 0)
	s.Require().NoError(err)
	s.Require().Len(oldImpact.Downstream, 1)
	s.Equal("old/output", oldImpact.Downstream[0].DatasetName)
	newImpact, err := QueryImpact(s.ctx, s.db, "", "new/input", 0)
	s.Require().NoError(err)
	s.Empty(newImpact.Downstream)

	// A retry resets started_at and emits new events on the same instance.
	// It must not combine the replacement definition with the first attempt's
	// already persisted edges.
	s.Require().NoError(s.db.Model(tr).Updates(map[string]any{"started_at": started.Add(3 * time.Second), "attempt": 2}).Error)
	evt.Type = event.TypeTaskStarted
	evt.Timestamp = started.Add(3 * time.Second)
	mapped, err = m.mapEvent(evt)
	s.Require().NoError(err)
	s.Empty(mapped.Inputs)
	s.Empty(mapped.Outputs)
	oldImpact, err = QueryImpact(s.ctx, s.db, "", "old/input", 0)
	s.Require().NoError(err)
	s.Require().Len(oldImpact.Downstream, 1)
	s.Equal("old/output", oldImpact.Downstream[0].DatasetName)
}
