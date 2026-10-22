// Package agent implements the service layer behind the scoped /v1/agent/* tool
// surface: the triage bundle, the read-only context passthroughs, timeline
// notes, and the typed-action proposal path that delegates to Stream B's action
// executor. Every method here is reached only through the auth middleware's
// agent-scope switch, which 403s an agent token on any incident but its own.
package agent

import (
	"context"
	"errors"

	iincident "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/db"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ErrIncidentNotFound is returned when the addressed incident does not exist.
var ErrIncidentNotFound = errors.New("agent: incident not found")

// Service wraps the incident package for the REST controllers, mirroring the
// thin service pattern used by lineage/why (a context + a *gorm.DB, with a
// WithDatabase override for tests).
type Service struct {
	ctx    context.Context
	db     *gorm.DB
	reader iincident.ReaderConfinement
	// authorLegacy marks a caller whose agent credential predates namespace
	// confinement: what it writes is withheld from confined readers.
	authorLegacy bool
}

// New creates a Service with the default DB connection.
func New(ctx context.Context) *Service {
	return &Service{ctx: ctx, db: db.Connection()}
}

// WithDatabase returns a copy of the Service using the given connection.
func (s *Service) WithDatabase(conn *gorm.DB) *Service {
	if conn == nil {
		return s
	}
	return &Service{ctx: s.ctx, db: conn, reader: s.reader, authorLegacy: s.authorLegacy}
}

// ForReader returns a copy of the Service serving a reader of the given
// confinement: a namespace-confined principal or an agent session does not
// see what a session from before namespace confinement wrote
// (identity-and-access A8).
func (s *Service) ForReader(reader iincident.ReaderConfinement) *Service {
	return &Service{ctx: s.ctx, db: s.db, reader: reader, authorLegacy: s.authorLegacy}
}

// ForLegacyAuthor returns a copy of the Service whose writes (notes, action
// proposals) are marked as written by a credential that predates namespace
// confinement (identity-and-access A8).
func (s *Service) ForLegacyAuthor(legacy bool) *Service {
	return &Service{ctx: s.ctx, db: s.db, reader: s.reader, authorLegacy: legacy}
}

// Incident loads the addressed incident, translating not-found.
func (s *Service) Incident(id uuid.UUID) (*models.Incident, error) {
	var inc models.Incident
	if err := s.db.WithContext(s.ctx).First(&inc, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIncidentNotFound
		}
		return nil, err
	}
	return &inc, nil
}

// Bundle assembles the triage bundle for an incident.
//
// The playbook it surfaces comes from iincident.ResolvePlaybook — the SAME
// resolver the action executor enforces with. Briefing the agent from the
// bootstrap default profile (what this did before) while judging its proposals
// against the job-scoped policy meant the two disagreed: the agent could plan
// correctly from its brief and still be denied, or believe it was constrained
// when it was not.
func (s *Service) Bundle(id uuid.UUID) (*iincident.Bundle, error) {
	playbook := iincident.ResolvePlaybook(s.ctx, s.db, id, env.Variables().AgentDefaultProfile)
	b, err := iincident.BuildBundleFor(s.ctx, s.db, id, &playbook, s.reader)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIncidentNotFound
		}
		return nil, err
	}
	if s.reader == iincident.ReaderConfined {
		if err := s.withholdQuotedBaselines(b); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// withholdQuotedBaselines removes, for a confined reader, the baseline
// evidence a failure message recorded before baselines were namespace-owned
// (the incident list's last_error rule, identity-and-access B4): such a
// baseline may have come from another namespace's runs. It covers the failure,
// its failed partitions and every run in the run history.
func (s *Service) withholdQuotedBaselines(b *iincident.Bundle) error {
	runIDs := make([]uuid.UUID, 0, len(b.RunHistory)+1)
	if b.Incident.RunID != nil {
		runIDs = append(runIDs, *b.Incident.RunID)
	}
	for i := range b.RunHistory {
		runIDs = append(runIDs, b.RunHistory[i].ID)
	}
	byRun, byTaskRun, err := s.runViolations(runIDs)
	if err != nil {
		return err
	}
	if b.Incident.RunID != nil {
		b.Failure.Error = withholdAll(b.Failure.Error, byRun[*b.Incident.RunID])
	}
	for i := range b.Failure.Partitions {
		p := &b.Failure.Partitions[i]
		p.Error = runstorage.WithholdQuotedBaselineEvidence(p.Error, byTaskRun[p.TaskRunID])
	}
	for i := range b.RunHistory {
		r := &b.RunHistory[i]
		r.Error = withholdAll(r.Error, byRun[r.ID])
	}
	return nil
}

// runViolations loads the data violations of every task run of runIDs, by run
// and by task run.
func (s *Service) runViolations(runIDs []uuid.UUID) (map[uuid.UUID][][]byte, map[uuid.UUID][]byte, error) {
	byRun := map[uuid.UUID][][]byte{}
	byTaskRun := map[uuid.UUID][]byte{}
	if len(runIDs) == 0 {
		return byRun, byTaskRun, nil
	}
	var rows []models.TaskRun
	if err := s.db.WithContext(s.ctx).Select("id", "job_run_id", "data_violations").
		Where("job_run_id IN ?", runIDs).Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	for i := range rows {
		blob := []byte(rows[i].DataViolations)
		byRun[rows[i].JobRunID] = append(byRun[rows[i].JobRunID], blob)
		byTaskRun[rows[i].ID] = blob
	}
	return byRun, byTaskRun, nil
}

// withholdAll applies the baseline rule for every task of one run.
func withholdAll(text string, violations [][]byte) string {
	for _, blob := range violations {
		text = runstorage.WithholdQuotedBaselineEvidence(text, blob)
	}
	return text
}

// AllowedJobs returns the incident's frozen agent read-scope allowlist.
func (s *Service) AllowedJobs(id uuid.UUID) ([]string, error) {
	return iincident.NewStore(s.db).AllowedJobsForIncident(s.ctx, id)
}

// Note appends a free-text finding to the incident timeline.
func (s *Service) Note(inc *models.Incident, text string) (*models.AgentAction, error) {
	return iincident.RecordNoteBy(s.ctx, s.db, inc.ID, nil, &inc.Namespace, text, !s.authorLegacy)
}
