package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
)

// ActionRequest is a typed remediation action proposed/executed by the agent
// through POST /v1/agent/incidents/:id/actions. The incident id comes from the
// route (and is scope-checked by the middleware); the agent supplies the action
// type and its params.
type ActionRequest struct {
	IncidentID uuid.UUID       `json:"-"`
	Type       string          `json:"type"`
	Params     json.RawMessage `json:"params,omitempty"`
}

// ActionResult is what the actions endpoint returns: the recorded AgentAction
// row and a coarse disposition (proposed | executed | failed | awaiting_approval).
type ActionResult struct {
	Action      *models.AgentAction `json:"action"`
	Disposition string              `json:"disposition"`
}

// ErrUnknownActionType is returned when the action type is empty/unrecognized at
// the surface level (deep validation is the executor's job).
var ErrUnknownActionType = errors.New("agent: action type is required")

// ActionExecutor is Stream B's server-side action executor. It validates a typed
// action against the effective playbook, executes tier-1/2 actions, routes
// tier-3 through the approval gate, and records the AgentAction audit row with
// the correct actor/tier/status.
//
// CROSS-PR SEAM: Stream B (internal/incident/executor.go) ships this in a
// sibling PR; the orchestrator sequences B before C at merge. Until an executor
// is registered via SetActionExecutor, ProposeAction degrades to recording a
// `proposed` AgentAction row (the audit spine) and returning it — the tool
// surface is live and auditable, and B's executor takes over the execution
// semantics when wired.
type ActionExecutor interface {
	ExecuteAgentAction(ctx context.Context, req ActionRequest) (*ActionResult, error)
}

// executor is the process-wide registered action executor (nil until Stream B
// wires one at startup).
var executor ActionExecutor

// SetActionExecutor registers Stream B's action executor. Wired once at startup.
func SetActionExecutor(e ActionExecutor) { executor = e }

// ProposeAction records/executes a typed action for an incident. When Stream B's
// executor is registered it delegates entirely (execution + audit). Otherwise it
// records a `proposed` AgentAction row so the timeline and audit spine work and
// the endpoint is exercisable end-to-end; the executor supersedes this.
func (s *Service) ProposeAction(inc *models.Incident, req ActionRequest) (*ActionResult, error) {
	req.Type = strings.TrimSpace(req.Type)
	if req.Type == "" {
		return nil, ErrUnknownActionType
	}
	req.IncidentID = inc.ID

	if executor != nil {
		return executor.ExecuteAgentAction(s.ctx, req)
	}

	now := time.Now().UTC()
	var params datatypes.JSON
	if len(req.Params) > 0 {
		params = datatypes.JSON(req.Params)
	}
	action := &models.AgentAction{
		ID:         uuid.New(),
		Namespace:  inc.Namespace,
		IncidentID: inc.ID,
		Type:       req.Type,
		Params:     params,
		Status:     models.AgentActionStatusProposed,
		Actor:      models.AgentActionActorAgent,
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := s.db.WithContext(s.ctx).Create(action).Error; err != nil {
		return nil, err
	}
	return &ActionResult{Action: action, Disposition: string(models.AgentActionStatusProposed)}, nil
}
