package start

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
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
		SessionID:  a.callerSessionID(ctx, req.IncidentID, req.TokenID),
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

// playbook resolves the EFFECTIVE policy for an incident.
//
// This is an authorization decision, not a lookup convenience: the returned
// Playbook is what incident.Executor.Execute consults to decide whether a
// proposed action runs autonomously. Resolving it from the deployment-wide
// CAESIUM_AGENT_DEFAULT_PROFILE — as this did before — evaluates a job that
// narrowed its own allowlist under the default's wider one, so an action the
// job forbids executes without a human ever seeing it.
//
// The resolution itself lives in incident.ResolvePlaybook because the triage
// BUNDLE must brief the agent with the same policy this ENFORCES; two copies
// drifted apart once already.
func (a *agentActionExecutor) playbook(ctx context.Context, incidentID uuid.UUID) incident.Playbook {
	return incident.ResolvePlaybook(ctx, a.db, incidentID, env.Variables().AgentDefaultProfile)
}

// callerSessionID resolves the session that MADE this proposal, from the API key
// that authenticated the request.
//
// The credential is minted per session and recorded on AgentSession.TokenID, so
// the token is an exact identifier. Matching on "the newest non-terminal session
// for the incident" — what this did before — is wrong the moment two sessions
// overlap on one incident: the proposal is attributed to the wrong container in
// the audit spine, and the approval flow then ends THAT session, leaving the
// actual proposer running while killing a bystander.
//
// It remains best-effort. A proposal made outside a live session (a test
// harness, a replayed token) still records, with a nil session id — the row is
// the audit spine and must never be lost because its session cannot be named.
// The incident scope is still applied, so a token cannot claim a session
// belonging to a different incident.
func (a *agentActionExecutor) callerSessionID(ctx context.Context, incidentID uuid.UUID, tokenID *uuid.UUID) *uuid.UUID {
	if tokenID == nil {
		return nil
	}
	var session models.AgentSession
	err := a.db.WithContext(ctx).
		Where("incident_id = ? AND token_id = ?", incidentID, *tokenID).
		Order("created_at DESC").
		First(&session).Error
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			log.Warn("agent: could not resolve the proposing session",
				"incident_id", incidentID, "token_id", *tokenID, "error", err)
		}
		return nil
	}
	id := session.ID
	return &id
}
