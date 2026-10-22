package auth

import (
	"context"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

// AgentAllowedJobs returns the aliases of an agent claim's frozen allowlist
// whose job currently lives in the incident's namespace, in claim order
// (identity-and-access A8). A job that moved out since the incident opened is
// dropped, so an agent session can never read it. Aliases match exactly as
// stored. Any lookup error is returned; the caller denies.
func (s *Service) AgentAllowedJobs(ctx context.Context, incidentID uuid.UUID, aliases []string) ([]string, error) {
	namespace, err := s.IncidentNamespace(ctx, incidentID)
	if err != nil {
		return nil, err
	}
	if len(aliases) == 0 {
		return []string{}, nil
	}
	conn, err := s.readDB(ctx)
	if err != nil {
		return nil, err
	}
	var jobs []models.Job
	err = s.withReadRetry(func() error {
		jobs = nil
		return conn.Select("alias", "namespace").Where("alias IN ?", aliases).Find(&jobs).Error
	})
	if err != nil {
		return nil, err
	}
	inside := make(map[string]bool, len(jobs))
	for i := range jobs {
		if models.NamespaceOrDefault(jobs[i].Namespace) == namespace {
			inside[jobs[i].Alias] = true
		}
	}
	out := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if inside[alias] {
			out = append(out, alias)
		}
	}
	return out, nil
}
