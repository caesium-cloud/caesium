package start

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// agentActionExecutor adapts the incident action executor onto the shape the
// /v1/agent/incidents/:id/actions surface expects
// (agentsvc.ActionExecutor). Without it, ProposeAction takes its
// "no executor registered" fallback and records a bare `proposed` AgentAction
// with no tier evaluation, no playbook enforcement, and no ApprovalRequest — the
// agent proposes into a void nobody can approve (trust-the-substrate ledger L9).
//
// It resolves the two things the HTTP surface cannot know on its own:
//
//   - the EFFECTIVE PLAYBOOK the action is validated against, and
//   - the AGENT SESSION the proposal belongs to, so the audit spine links the
//     action to the container that made it and the approval flow can end that
//     session while a human decides.
type agentActionExecutor struct {
	db   *gorm.DB
	exec *incident.Executor
}

func newAgentActionExecutor(conn *gorm.DB, exec *incident.Executor) *agentActionExecutor {
	return &agentActionExecutor{db: conn, exec: exec}
}

// ExecuteAgentAction validates, records, and routes one agent-proposed action.
func (a *agentActionExecutor) ExecuteAgentAction(ctx context.Context, req agentsvc.ActionRequest) (*agentsvc.ActionResult, error) {
	var params incident.ActionParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return nil, fmt.Errorf("agent: decode action params: %w", err)
		}
	}

	action, err := a.exec.Execute(ctx, incident.ActionRequest{
		IncidentID: req.IncidentID,
		SessionID:  a.activeSessionID(ctx, req.IncidentID),
		Actor:      models.AgentActionActorAgent,
		Type:       req.Type,
		Params:     params,
		Playbook:   a.playbook(ctx, req.IncidentID),
	})
	if action == nil {
		return nil, err
	}
	// A recorded-but-refused action is not a transport error: the row IS the
	// answer (rejected by playbook, or failed on dispatch), and the agent must be
	// able to read it. Only a failure to record at all returns nil above.
	if err != nil && !errors.Is(err, incident.ErrActionNotPermitted) {
		log.Warn("agent: proposed action did not execute",
			"incident_id", req.IncidentID, "type", req.Type, "error", err)
	}
	return &agentsvc.ActionResult{
		Action:      action,
		Disposition: disposition(action),
	}, nil
}

// disposition maps the recorded row onto the coarse label the endpoint returns.
// A tier-3 row left `proposed` is reported as awaiting_approval, which is what
// actually happened: an ApprovalRequest now exists and a human owns the next
// move.
func disposition(action *models.AgentAction) string {
	if action.Status == models.AgentActionStatusProposed && action.Tier >= incident.TierApproval {
		return "awaiting_approval"
	}
	return string(action.Status)
}

// playbook resolves the EFFECTIVE policy for an incident from the job the
// incident belongs to.
//
// This is an authorization decision, not a lookup convenience: the returned
// Playbook is what incident.Executor.Execute consults to decide whether a
// proposed action runs autonomously. Resolving it from the deployment-wide
// CAESIUM_AGENT_DEFAULT_PROFILE — as this did before — evaluates a job that
// narrowed its own allowlist under the default's wider one, so an action the
// job forbids executes without a human ever seeing it.
//
// Resolution, in order:
//
//  1. incident → job. The job's persisted `metadata.remediation` block
//     (models.Job.Remediation) is the job's policy.
//  2. That block names the AgentProfile whose playbook is the base; a job that
//     declares a block but no profile uses the deployment default. The block's
//     `autonomy` sub-block then NARROWS that base (Playbook.Narrow — neither
//     document can grant what the other withholds).
//  3. A job with no remediation block at all falls back to the deployment
//     default profile: that is what "default profile" means, and the job has
//     expressed no policy to override it.
//
// Every failure fails CLOSED, and the severity matches what was lost:
//
//   - the incident or job cannot be read → the zero Playbook (tier 3 to a
//     human, tier 2 needs an explicit allow, tier 0/1 default autonomous);
//   - the job DECLARED a policy whose profile cannot be loaded → DenyAllPlaybook,
//     which permits no autonomous action at all. Falling back to the deployment
//     default here would substitute a policy the job explicitly replaced, which
//     is the exact widening this function exists to prevent.
func (a *agentActionExecutor) playbook(ctx context.Context, incidentID uuid.UUID) incident.Playbook {
	var inc models.Incident
	if err := a.db.WithContext(ctx).Select("id", "job_id").First(&inc, "id = ?", incidentID).Error; err != nil {
		log.Warn("agent: could not load incident for playbook; failing closed",
			"incident_id", incidentID, "error", err)
		return incident.Playbook{}
	}

	var job models.Job
	if err := a.db.WithContext(ctx).Select("id", "alias", "remediation").First(&job, "id = ?", inc.JobID).Error; err != nil {
		log.Warn("agent: could not load job for playbook; failing closed",
			"incident_id", incidentID, "job_id", inc.JobID, "error", err)
		return incident.Playbook{}
	}

	if len(job.Remediation) == 0 {
		// No job-level policy: the deployment default governs.
		return a.defaultProfilePlaybook(ctx, incidentID)
	}

	var block schema.MetadataRemediation
	if err := json.Unmarshal(job.Remediation, &block); err != nil {
		log.Warn("agent: could not decode job remediation policy; denying autonomous actions",
			"incident_id", incidentID, "job_id", inc.JobID, "error", err)
		return incident.DenyAllPlaybook()
	}

	base, ok := a.profilePlaybook(ctx, incidentID, block.Profile)
	if !ok {
		// The job named a policy we cannot resolve. Denying is the only answer
		// that does not silently substitute a different one.
		return incident.DenyAllPlaybook()
	}

	// The job's own autonomy block is a valid playbook document (both are
	// pkg/jobdef.RemediationAutonomy's shape), so the shared decoder reads it.
	return base.Narrow(incident.DecodePlaybook(job.Remediation))
}

// profilePlaybook loads a named AgentProfile's playbook. An empty name means the
// job deferred to the deployment default. The bool reports whether resolution
// SUCCEEDED — false is a hard failure the caller must fail closed on, and is
// deliberately distinct from "resolved to an unconfigured (zero) playbook".
func (a *agentActionExecutor) profilePlaybook(ctx context.Context, incidentID uuid.UUID, name string) (incident.Playbook, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return a.defaultProfilePlaybook(ctx, incidentID), true
	}
	var profile models.AgentProfile
	if err := a.db.WithContext(ctx).First(&profile, "name = ?", name).Error; err != nil {
		log.Warn("agent: could not load job-declared agent profile for playbook",
			"incident_id", incidentID, "profile", name, "error", err)
		return incident.Playbook{}, false
	}
	return incident.DecodePlaybook(profile.Playbook), true
}

// defaultProfilePlaybook resolves CAESIUM_AGENT_DEFAULT_PROFILE's playbook. An
// unset or unreadable default yields the zero Playbook — never a permissive one.
func (a *agentActionExecutor) defaultProfilePlaybook(ctx context.Context, incidentID uuid.UUID) incident.Playbook {
	name := strings.TrimSpace(env.Variables().AgentDefaultProfile)
	if name == "" {
		return incident.Playbook{}
	}
	var profile models.AgentProfile
	if err := a.db.WithContext(ctx).First(&profile, "name = ?", name).Error; err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warn("agent: could not load default profile for playbook",
				"incident_id", incidentID, "profile", name, "error", err)
		}
		return incident.Playbook{}
	}
	return incident.DecodePlaybook(profile.Playbook)
}

// activeSessionID returns the newest non-terminal agent session for the
// incident, or nil. The agent's scoped token is minted per session and carries
// only the incident id, so the session is resolved from the incident rather than
// from the request — which is also why it is best-effort: a proposal made
// outside a live session (a test harness, a replayed token) still records.
func (a *agentActionExecutor) activeSessionID(ctx context.Context, incidentID uuid.UUID) *uuid.UUID {
	var session models.AgentSession
	err := a.db.WithContext(ctx).
		Where("incident_id = ? AND state IN ?", incidentID, []models.AgentSessionState{
			models.AgentSessionStatePending,
			models.AgentSessionStateRunning,
		}).
		Order("created_at DESC").
		First(&session).Error
	if err != nil {
		return nil
	}
	id := session.ID
	return &id
}
