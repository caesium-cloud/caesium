package incident

import (
	"context"
	"errors"
	"fmt"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// An incident and its agent sessions belong to the namespace of the failing
// run, which never changes (identity-and-access design §7.4, plan item A8).
// The helpers here confine what an agent session reads and acts on to that
// namespace: a job that moved out after the incident opened is no longer
// readable or actionable through the incident, and history a job brought in
// from another namespace stays with its old owner.

// ErrNamespaceMoved refuses an action whose target job or run is not in the
// incident's namespace (the job moved after the incident opened, or the run
// was recorded elsewhere).
var ErrNamespaceMoved = errors.New("incident: namespace_moved: the action target is outside the incident's namespace")

// IncidentNamespace is the incident's persisted namespace ("default" when
// legacy-empty).
func IncidentNamespace(inc *models.Incident) string {
	if inc == nil {
		return models.DefaultNamespace
	}
	return models.NamespaceOrDefault(inc.Namespace)
}

// HistoryContext returns ctx restricted to the incident's namespace, so a
// history read through it (run lists, why baselines, cache origins) returns
// only rows persisted in that namespace.
func HistoryContext(ctx context.Context, inc *models.Incident) context.Context {
	return authz.WithAllowedNamespaces(ctx, []string{IncidentNamespace(inc)})
}

// JobsInNamespace returns the aliases whose job currently lives in namespace,
// in input order. Aliases are matched exactly as stored. A lookup error is
// returned, never treated as "all in".
func JobsInNamespace(ctx context.Context, db *gorm.DB, namespace string, aliases []string) ([]string, error) {
	if len(aliases) == 0 {
		return []string{}, nil
	}
	var jobs []models.Job
	if err := db.WithContext(ctx).Select("alias", "namespace").Where("alias IN ?", aliases).Find(&jobs).Error; err != nil {
		return nil, err
	}
	namespace = models.NamespaceOrDefault(namespace)
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

// EffectiveAllowlist is the incident's frozen allowlist minus every job that
// has since moved out of the incident's namespace.
func EffectiveAllowlist(ctx context.Context, db *gorm.DB, inc *models.Incident) ([]string, error) {
	return JobsInNamespace(ctx, db, IncidentNamespace(inc), unmarshalAllowlist(inc.AllowedJobs))
}

// JobInIncidentNamespace reports whether the job currently lives in the
// incident's namespace.
func JobInIncidentNamespace(ctx context.Context, db *gorm.DB, inc *models.Incident, jobID uuid.UUID) (bool, error) {
	var job models.Job
	if err := db.WithContext(ctx).Select("id", "namespace").First(&job, "id = ?", jobID).Error; err != nil {
		return false, err
	}
	return models.NamespaceOrDefault(job.Namespace) == IncidentNamespace(inc), nil
}

// verifyNamespaceBoundary refuses an action unless the incident's job is
// still in the incident's namespace and the run it targets (when any) was
// recorded there. A job or run that no longer exists lives in no other
// namespace, so it is not refused here (the action itself then fails on the
// missing target); any other lookup error refuses.
func verifyNamespaceBoundary(ctx context.Context, db *gorm.DB, inc *models.Incident, runID uuid.UUID) error {
	inside, err := JobInIncidentNamespace(ctx, db, inc, inc.JobID)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		return fmt.Errorf("incident: load incident job namespace: %w", err)
	case !inside:
		return fmt.Errorf("%w: job %s", ErrNamespaceMoved, inc.JobID)
	}
	if runID == uuid.Nil {
		return nil
	}
	var run models.JobRun
	if err := db.WithContext(ctx).Select("id", "namespace").First(&run, "id = ?", runID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return fmt.Errorf("incident: load run namespace: %w", err)
	}
	if models.NamespaceOrDefault(run.Namespace) != IncidentNamespace(inc) {
		return fmt.Errorf("%w: run %s", ErrNamespaceMoved, runID)
	}
	return nil
}

// ReaderConfinement says how much legacy agent-authored content a reader of
// an incident may see: a namespace-confined principal, and any agent session,
// see only content written by namespace-confined sessions.
type ReaderConfinement bool

const (
	// ReaderUnrestricted sees every row as stored.
	ReaderUnrestricted ReaderConfinement = false
	// ReaderConfined does not see legacy agent-authored content.
	ReaderConfined ReaderConfinement = true
)

// WithholdLegacyAgentContent empties the transcript of every session created
// before agent sessions were namespace-confined, and the params and result of
// every agent-authored action such a session could have written. A session
// from that era could read other namespaces' jobs, so what it wrote may quote
// them.
func WithholdLegacyAgentContent(sessions []models.AgentSession, actions []models.AgentAction) {
	for i := range sessions {
		if sessions[i].NamespaceConfined {
			continue
		}
		if sessions[i].SessionLog != "" {
			sessions[i].SessionLog = ""
			sessions[i].SessionLogWithheld = true
		}
	}
	for i := range actions {
		if !legacyAgentAction(&actions[i]) {
			continue
		}
		actions[i].Params = nil
		actions[i].Result = nil
		actions[i].ContentWithheld = true
	}
}

func legacyAgentAction(action *models.AgentAction) bool {
	return action.Actor == models.AgentActionActorAgent && !action.NamespaceConfined
}
