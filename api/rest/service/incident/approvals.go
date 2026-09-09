package incident

import (
	"context"
	"errors"
	"fmt"
	"time"

	incidentcore "github.com/caesium-cloud/caesium/internal/incident"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
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
	// decision. Used to decide SSE emission. False when the incident stays parked
	// because another ApprovalRequest on it is still pending, and when a human had
	// already advanced it past awaiting_approval.
	StatusChanged bool
	// Executed reports whether the approved action was actually dispatched.
	// False on a rejection, when no post-approval executor is registered (the
	// degraded "approved, not executed" path), and when the dispatch failed.
	Executed bool
	// ExecutionError carries the dispatch failure, if any. The DECISION still
	// stands: it committed before execution was attempted, and the AgentAction
	// row records the failure. Surfacing it here lets the caller say so instead
	// of implying the remediation ran.
	ExecutionError string
}

// ApprovedActionExecutor dispatches a tier-3 AgentAction after a human approved
// it. *incident.Executor satisfies it; the interface keeps this service free of
// a dependency on the executor's construction (its ActionOps, its event sink).
//
// This is the far end of the tier-3 pipeline. Before it existed, `decide`
// stamped the action `approved`, moved the incident back to triaging, and
// nothing ever ran the action — the comment said "the executor runs the approved
// action" and there was no executor (trust-the-substrate ledger L9).
type ApprovedActionExecutor interface {
	ExecuteApproved(ctx context.Context, actionID uuid.UUID) (*models.AgentAction, error)
}

// approvedExecutor is the process-wide post-approval executor, registered once
// at startup inside the remediation master gate.
var approvedExecutor ApprovedActionExecutor

// SetApprovedActionExecutor registers the post-approval executor.
func SetApprovedActionExecutor(e ApprovedActionExecutor) { approvedExecutor = e }

// Approve resolves a pending tier-3 approval as approved, then executes the
// approved action.
//
// The two steps are deliberately sequential and NOT in one transaction: the
// decision must be durable before anything acts on it, so a crash mid-dispatch
// leaves an approved-but-unexecuted action a human can see and re-drive, rather
// than a rolled-back approval whose side effects already landed. decider is the
// operator identity from the authenticated principal.
func (s *Service) Approve(incidentID, approvalID uuid.UUID, decider, reason string) (*DecideResult, error) {
	result, err := s.decide(incidentID, approvalID, models.ApprovalDecisionApproved, decider, reason)
	if err != nil {
		return nil, err
	}
	s.executeApproved(result)
	return result, nil
}

// executeApproved dispatches the just-approved action. When no executor is
// registered the approval degrades to "approved, not executed" with a logged
// warning rather than silently pretending the remediation ran — that degraded
// path is what a server started without the remediation master gate (and the
// service's own unit tests) exercise.
func (s *Service) executeApproved(result *DecideResult) {
	if result == nil {
		return
	}
	if approvedExecutor == nil {
		log.Warn("incident: approval recorded but no post-approval executor is registered; action approved, not executed",
			"incident_id", result.Incident.ID,
			"approval_id", result.Approval.ID,
			"action_id", result.Approval.ActionID,
		)
		return
	}
	if _, err := approvedExecutor.ExecuteApproved(s.ctx, result.Approval.ActionID); err != nil {
		result.ExecutionError = err.Error()
		log.Error("incident: approved action failed to execute",
			"incident_id", result.Incident.ID,
			"action_id", result.Approval.ActionID,
			"error", err,
		)
		return
	}
	result.Executed = true
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
// audit-spine AgentAction, and advances the incident once no approval on it is
// still pending — ALL in one transaction so the decision, the action, and the
// incident status can never drift. The decision write is a conditional update
// guarded on decision = pending, so two operators racing to decide the same
// approval cannot both "succeed": the loser matches zero rows and is refused with
// ErrApprovalNotPending (→ 409).
//
// This is the human-decision boundary the design's "tier 3 always terminates at a
// human" invariant rests on. The route-level auth (operator role) plus the
// authorizeScope agent-token rejection guarantee the caller is a human operator;
// this method records WHO decided (decider) on the approval row.
func (s *Service) decide(incidentID, approvalID uuid.UUID, decision models.ApprovalDecision, decider, reason string) (*DecideResult, error) {
	// Approve resumes triaging (Approve then runs the approved action through the
	// registered post-approval executor); reject escalates (a human owns it) —
	// reject must never re-enter the triage loop.
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
		// commit while leaving the incident parked in awaiting_approval with nothing
		// left to decide. An incident already advanced past awaiting_approval (human
		// take-over, terminal) is left as-is — the decision is still legitimately
		// recorded.
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&incident, "id = ?", incidentID).Error; err != nil {
			return err
		}

		// …but only once EVERY approval on the incident is decided. An incident can
		// hold more than one pending ApprovalRequest — a second tier-3 proposal finds
		// the incident already parked, so parkAwaitingApprovalTx is a no-op while the
		// approval row is still created — and advancing on the first decision left the
		// remaining ones pending under a status that says nobody is waiting on a
		// human. That is wrong twice over: the state machine mis-states the incident,
		// and the operator feed (which asks the same question) stopped listing it
		// (#417). The count is deliberately taken AFTER the row lock above and inside
		// this transaction, so two operators deciding two different approvals cannot
		// both read zero and both think they are last: on dqlite every transaction
		// serializes on the single write connection, and on Postgres the loser blocks
		// on the incident's FOR UPDATE lock and then re-reads the winner's committed
		// decision.
		var stillPending int64
		if err := tx.Model(&models.ApprovalRequest{}).
			Where("incident_id = ? AND decision = ?", incidentID, models.ApprovalDecisionPending).
			Count(&stillPending).Error; err != nil {
			return err
		}

		// The LAST decision sets the incident's disposition, with the same
		// approve→triaging / reject→escalated mapping a single approval has always
		// had. An earlier sibling decision does not veto it: each ApprovalRequest is a
		// decision about its own action, and the rejected action row is stamped
		// rejected either way, so nothing an operator refused becomes executable.
		if stillPending == 0 && incident.Status == models.IncidentStatusAwaitingApproval {
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
