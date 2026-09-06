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

// ActionExecutor is the server-side action executor: it validates a typed action
// against the effective playbook, executes tier-1/2 actions, routes tier-3
// through the approval gate (creating the ApprovalRequest a human decides on),
// and records the AgentAction audit row with the correct actor/tier/status.
//
// It is registered at startup by cmd/start (an adapter over
// internal/incident.Executor), inside the CAESIUM_AGENT_REMEDIATION_ENABLED
// master gate.
type ActionExecutor interface {
	ExecuteAgentAction(ctx context.Context, req ActionRequest) (*ActionResult, error)
}

// executor is the process-wide registered action executor.
var executor ActionExecutor

// SetActionExecutor registers the server-side action executor. Wired once at
// startup (cmd/start/start.go, behind the remediation master gate).
func SetActionExecutor(e ActionExecutor) { executor = e }

// ProposeAction records/executes a typed action for an incident. It delegates
// entirely to the registered executor (validation, tier routing, approval-gate
// creation, execution, audit).
//
// The executor-nil fallback below records a bare `proposed` AgentAction with NO
// tier evaluation and NO ApprovalRequest. It exists only so this package's unit
// tests can exercise the surface without constructing the whole incident
// executor, and for a server that never enabled remediation. Do not rely on it
// as a product path: a tier-3 action recorded through it is unapprovable,
// because nothing created the approval row.
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
