package incident

import (
	"errors"
	"fmt"
	"time"

	incidentcore "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Approval-flow errors distinguishable by the controller for HTTP status mapping.
var (
	// ErrApprovalIncidentMismatch is returned when the approval exists but does
	// not belong to the incident named in the route path.
	ErrApprovalIncidentMismatch = errors.New("approval does not belong to incident")
	// ErrApprovalNotPending is returned when an already-decided (or expired)
	// approval is decided again — decisions are once-only, and a concurrent
	// decision that lost the race sees this too.
	ErrApprovalNotPending = errors.New("approval is not pending")
)

// DecideResult carries the resolved approval plus the incident it belongs to,
// so the controller can emit the incident_status_changed / approval SSE events.
type DecideResult struct {
	Approval models.ApprovalRequest
	Incident models.Incident
	// StatusChanged reports whether the incident advanced state as part of the
	// decision. Used to decide SSE emission.
	StatusChanged bool
}

// Approve resolves a pending tier-3 approval as approved. decider is the operator
// identity from the authenticated principal. An approve resumes the incident to
// triaging so the executor can run the approved action.
func (s *Service) Approve(incidentID, approvalID uuid.UUID, decider, reason string) (*DecideResult, error) {
	return s.decide(incidentID, approvalID, models.ApprovalDecisionApproved, decider, reason)
}

// Reject resolves a pending tier-3 approval as rejected. A rejection is a human's
// decision AGAINST the action, so — unlike approve — it does NOT return the
// incident to triaging (which would let the agent re-propose the identical
// action). It escalates the incident: a human now owns it, and the rejected
// action row is stamped rejected so it is excluded from any re-proposal.
func (s *Service) Reject(incidentID, approvalID uuid.UUID, decider, reason string) (*DecideResult, error) {
	return s.decide(incidentID, approvalID, models.ApprovalDecisionRejected, decider, reason)
}

// decide records the human decision on a tier-3 approval, mirrors it onto the
// audit-spine AgentAction, and advances the incident — ALL in one transaction so
// the decision, the action, and the incident status can never drift. The decision
// write is a conditional update guarded on decision = pending, so two operators
// racing to decide the same approval cannot both "succeed": the loser matches zero
// rows and is refused with ErrApprovalNotPending (→ 409).
//
// This is the human-decision boundary the design's "tier 3 always terminates at a
// human" invariant rests on. The route-level auth (operator role) plus the
// authorizeScope agent-token rejection guarantee the caller is a human operator;
// this method records WHO decided (decider) on the approval row.
func (s *Service) decide(incidentID, approvalID uuid.UUID, decision models.ApprovalDecision, decider, reason string) (*DecideResult, error) {
	// Approve resumes triaging (the executor runs the approved action); reject
	// escalates (a human owns it) — reject must never re-enter the triage loop.
	target := models.IncidentStatusTriaging
	actionStatus := models.AgentActionStatusApproved
	if decision == models.ApprovalDecisionRejected {
		target = models.IncidentStatusEscalated
		actionStatus = models.AgentActionStatusRejected
	}

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

		now := time.Now().UTC()

		// Conditional decision write — only a still-pending approval flips. This is
		// the concurrency guard: the winner matches one row; any concurrent decider
		// matches zero and is refused, so a later commit can never overwrite an
		// already-recorded decision.
		res := tx.Model(&models.ApprovalRequest{}).
			Where("id = ? AND decision = ?", approvalID, models.ApprovalDecisionPending).
			Updates(map[string]any{
				"decision":   decision,
				"decider":    decider,
				"reason":     reason,
				"decided_at": now,
				"updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrApprovalNotPending
		}

		// Mirror the decision onto the audit-spine action row.
		if err := tx.Model(&models.AgentAction{}).
			Where("id = ?", approval.ActionID).
			Updates(map[string]any{
				"status":     actionStatus,
				"updated_at": now,
			}).Error; err != nil {
			return err
		}

		// Advance the incident IN THE SAME TRANSACTION so a decision can never
		// commit while leaving the incident parked in awaiting_approval. An incident
		// already advanced past awaiting_approval (human take-over, terminal) is left
		// as-is — the decision is still legitimately recorded.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&incident, "id = ?", incidentID).Error; err != nil {
			return err
		}
		if incident.Status == models.IncidentStatusAwaitingApproval {
			if !incidentcore.CanTransition(incident.Status, target) {
				return fmt.Errorf("incident: cannot transition %s → %s", incident.Status, target)
			}
			if err := tx.Model(&models.Incident{}).
				Where("id = ?", incidentID).
				Updates(map[string]any{
					"status":     target,
					"updated_at": now,
				}).Error; err != nil {
				return err
			}
			incident.Status = target
			statusChanged = true
		}

		// Reflect the committed decision on the returned approval struct.
		approval.Decision = decision
		approval.Decider = decider
		approval.Reason = reason
		approval.DecidedAt = &now
		approval.UpdatedAt = now
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &DecideResult{
		Approval:      approval,
		Incident:      incident,
		StatusChanged: statusChanged,
	}, nil
}
