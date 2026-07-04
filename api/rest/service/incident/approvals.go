package incident

import (
	"errors"
	"time"

	incidentcore "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Approval-flow errors distinguishable by the controller for HTTP status mapping.
var (
	// ErrApprovalIncidentMismatch is returned when the approval exists but does
	// not belong to the incident named in the route path.
	ErrApprovalIncidentMismatch = errors.New("approval does not belong to incident")
	// ErrApprovalNotPending is returned when an already-decided (or expired)
	// approval is decided again — decisions are idempotent-once, not re-writable.
	ErrApprovalNotPending = errors.New("approval is not pending")
)

// DecideResult carries the resolved approval plus the incident it belongs to,
// so the controller can emit the incident_status_changed / approval SSE events.
type DecideResult struct {
	Approval models.ApprovalRequest
	Incident models.Incident
	// StatusChanged reports whether the incident advanced state as part of the
	// decision (awaiting_approval → triaging). Used to decide SSE emission.
	StatusChanged bool
}

// Approve resolves a pending tier-3 approval as approved. decider is the operator
// identity from the authenticated principal.
func (s *Service) Approve(incidentID, approvalID uuid.UUID, decider, reason string) (*DecideResult, error) {
	return s.decide(incidentID, approvalID, models.ApprovalDecisionApproved, decider, reason)
}

// Reject resolves a pending tier-3 approval as rejected.
func (s *Service) Reject(incidentID, approvalID uuid.UUID, decider, reason string) (*DecideResult, error) {
	return s.decide(incidentID, approvalID, models.ApprovalDecisionRejected, decider, reason)
}

// decide records the human decision on a tier-3 approval and mirrors it onto the
// audit-spine AgentAction, then resumes the incident. The whole mutation runs in
// one transaction so the approval, the action, and the incident state never drift.
//
// This is the human-decision boundary the design's "tier 3 always terminates at a
// human" invariant rests on. The route-level auth (operator role) plus the
// authorizeScope agent-token rejection guarantee the caller is a human operator,
// not the agent; this method records WHO decided (decider) on the approval row.
func (s *Service) decide(incidentID, approvalID uuid.UUID, decision models.ApprovalDecision, decider, reason string) (*DecideResult, error) {
	var (
		approval      models.ApprovalRequest
		incident      models.Incident
		statusChanged bool
	)

	err := s.db.WithContext(s.ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&approval, "id = ?", approvalID).Error; err != nil {
			return err
		}
		if approval.IncidentID != incidentID {
			return ErrApprovalIncidentMismatch
		}
		if approval.Decision != models.ApprovalDecisionPending {
			return ErrApprovalNotPending
		}

		now := time.Now().UTC()
		approval.Decision = decision
		approval.Decider = decider
		approval.Reason = reason
		approval.DecidedAt = &now
		approval.UpdatedAt = now
		if err := tx.Model(&models.ApprovalRequest{}).
			Where("id = ?", approvalID).
			Updates(map[string]any{
				"decision":   decision,
				"decider":    decider,
				"reason":     reason,
				"decided_at": now,
				"updated_at": now,
			}).Error; err != nil {
			return err
		}

		// Mirror the decision onto the audit-spine action row.
		actionStatus := models.AgentActionStatusApproved
		if decision == models.ApprovalDecisionRejected {
			actionStatus = models.AgentActionStatusRejected
		}
		if err := tx.Model(&models.AgentAction{}).
			Where("id = ?", approval.ActionID).
			Updates(map[string]any{
				"status":     actionStatus,
				"updated_at": now,
			}).Error; err != nil {
			return err
		}

		if err := tx.First(&incident, "id = ?", incidentID).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Resume the incident on decision: while an approval was pending the incident
	// parked in awaiting_approval and the agent session ended. A decision returns
	// it to triaging so a fresh session (or the executor, for an approved action)
	// can resume. Done OUTSIDE the decision transaction and best-effort: an
	// incident already advanced past awaiting_approval (take-over, terminal) must
	// not fail the decision that was legitimately recorded.
	if incident.Status == models.IncidentStatusAwaitingApproval {
		store := incidentcore.NewStore(s.db)
		if resumed, terr := store.Transition(s.ctx, incidentID, models.IncidentStatusTriaging, ""); terr == nil {
			incident = *resumed
			statusChanged = true
		}
	}

	return &DecideResult{
		Approval:      approval,
		Incident:      incident,
		StatusChanged: statusChanged,
	}, nil
}
