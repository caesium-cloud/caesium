package incident

import (
	"encoding/json"
	"errors"
	"net/http"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	svc "github.com/caesium-cloud/caesium/api/rest/service/incident"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// decisionBody is the optional JSON body for approve/reject: a free-text reason
// recorded on the approval and, for rejections, surfaced to the agent/operator.
type decisionBody struct {
	Reason string `json:"reason"`
}

// Approve handles POST /v1/incidents/:id/approvals/:approval_id/approve.
//
// SECURITY: this route is operator-gated by RBAC and — critically — agent session
// tokens are rejected OUTRIGHT in authorizeScope before reaching this handler, so
// the caller is always a human operator. That is the load-bearing half of the
// design's "tier 3 always terminates at a human" invariant.
func (ctrl *Controller) Approve(c *echo.Context) error {
	return ctrl.decide(c, true)
}

// Reject handles POST /v1/incidents/:id/approvals/:approval_id/reject.
func (ctrl *Controller) Reject(c *echo.Context) error {
	return ctrl.decide(c, false)
}

func (ctrl *Controller) decide(c *echo.Context, approve bool) error {
	incidentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}
	approvalID, err := uuid.Parse(c.Param("approval_id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	reason := parseDecisionReason(c)
	decider := deciderIdentity(c)

	service := svc.New(c.Request().Context())
	var result *svc.DecideResult
	if approve {
		result, err = service.Approve(incidentID, approvalID, decider, reason)
	} else {
		result, err = service.Reject(incidentID, approvalID, decider, reason)
	}
	if err != nil {
		return mapApprovalError(err)
	}

	ctrl.emitDecisionEvents(result)
	return c.JSON(http.StatusOK, result.Approval)
}

// mapApprovalError translates service errors into HTTP status codes.
func mapApprovalError(err error) error {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, svc.ErrApprovalIncidentMismatch):
		// Mismatch is treated as not-found: the approval is not addressable under
		// this incident's path, and we do not leak that it exists elsewhere.
		return echo.ErrNotFound
	case errors.Is(err, svc.ErrApprovalNotPending):
		return echo.NewHTTPError(http.StatusConflict, "approval is not pending")
	default:
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}
}

// emitDecisionEvents publishes best-effort incident lifecycle events so the
// Console can live-update. Fire-and-forget: a full bus never blocks the response.
func (ctrl *Controller) emitDecisionEvents(result *svc.DecideResult) {
	if ctrl.bus == nil || result == nil {
		return
	}

	actionPayload, _ := json.Marshal(map[string]any{
		"incident_id": result.Incident.ID,
		"approval_id": result.Approval.ID,
		"action_id":   result.Approval.ActionID,
		"decision":    result.Approval.Decision,
	})
	ctrl.bus.Publish(event.Event{
		Type:    event.TypeAgentActionRecorded,
		JobID:   result.Incident.JobID,
		Payload: actionPayload,
	})

	if result.StatusChanged {
		statusPayload, _ := json.Marshal(map[string]any{
			"incident_id": result.Incident.ID,
			"status":      result.Incident.Status,
		})
		ctrl.bus.Publish(event.Event{
			Type:    event.TypeIncidentStatusChanged,
			JobID:   result.Incident.JobID,
			Payload: statusPayload,
		})
	}
}

// parseDecisionReason reads the optional {"reason": "..."} body without failing
// the request when the body is empty or not JSON.
func parseDecisionReason(c *echo.Context) string {
	var body decisionBody
	if err := json.NewDecoder(c.Request().Body).Decode(&body); err != nil {
		return ""
	}
	return body.Reason
}

// deciderIdentity resolves the operator identity recorded on the approval. The
// auth-mode precondition guarantees a principal is present when the feature is
// enabled; the fallback keeps the audit row non-empty defensively.
func deciderIdentity(c *echo.Context) string {
	if p := authmw.GetPrincipal(c); p != nil && p.Subject != "" {
		return p.Subject
	}
	return "operator"
}
