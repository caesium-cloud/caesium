package incident

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// redrive.go closes the recovery hole in the tier-3 approval pipeline.
//
// A human decision commits in its own transaction and the approved action is
// dispatched AFTER it (api/rest/service/incident/approvals.go — deliberately not
// one transaction, so a crash mid-dispatch cannot roll back a decision whose
// side effects already landed). The cost of that ordering is a window: if the
// process dies between the commit and the dispatch, the action sits `approved`
// forever. Nothing retried it, and the decision cannot simply be repeated —
// `decide` is guarded on `decision = pending`, so re-approving the same request
// is refused. The remediation a human explicitly authorised silently never
// happened.
//
// ApprovalRedriver is the leader-gated sweep that finishes the job. It is
// modelled on TimerSupervisor: a ticker, a leader gate so an N-node cluster acts
// once, and per-row errors that are logged rather than aborting the batch. The
// once-only guarantee is NOT the sweep's own — it belongs to
// Executor.ExecuteApproved's conditional approved → executing claim, which is
// why the synchronous fast path and this sweep can both target the same action.

// ApprovedExecutor is the post-approval dispatch entry point the sweeper drives.
// *Executor satisfies it; the interface keeps the sweeper testable against a
// fake without constructing an executor's ActionOps and event sink.
type ApprovedExecutor interface {
	ExecuteApproved(ctx context.Context, actionID uuid.UUID) (*models.AgentAction, error)
}

// Redrive defaults. The grace period is the load-bearing one: it must comfortably
// exceed a normal synchronous dispatch so the sweep never races the request that
// is already executing an action. It is a backstop, not a scheduler — losing that
// race is harmless (the claim refuses the loser) but wastes a dispatch attempt.
const (
	defaultRedriveInterval = 30 * time.Second
	defaultRedriveGrace    = 2 * time.Minute
	defaultRedriveBatch    = 50
)

// ApprovalRedriver re-dispatches approved-but-unexecuted tier-3 actions.
type ApprovalRedriver struct {
	db          *gorm.DB
	exec        ApprovedExecutor
	leaderCheck LeaderCheck
	interval    time.Duration
	grace       time.Duration
	batch       int
}

// NewApprovalRedriver constructs the sweeper. A zero interval or grace takes the
// package default; a nil leaderCheck means "always act" (single-node, tests),
// matching TimerSupervisor.
func NewApprovalRedriver(db *gorm.DB, exec ApprovedExecutor, leaderCheck LeaderCheck, interval, grace time.Duration) *ApprovalRedriver {
	if interval <= 0 {
		interval = defaultRedriveInterval
	}
	if grace <= 0 {
		grace = defaultRedriveGrace
	}
	return &ApprovalRedriver{
		db:          db,
		exec:        exec,
		leaderCheck: leaderCheck,
		interval:    interval,
		grace:       grace,
		batch:       defaultRedriveBatch,
	}
}

// Run drives the sweep loop until ctx is cancelled.
//
// It sweeps once BEFORE entering the ticker loop, unlike TimerSupervisor: the
// stranded-action case this exists for is created by a process death, so the
// rows most in need of redriving are the ones already on disk at startup.
func (r *ApprovalRedriver) Run(ctx context.Context) {
	if err := r.SweepOnce(ctx); err != nil && ctx.Err() == nil {
		log.Error("incident approval redrive sweep failed", "error", err)
	}
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.SweepOnce(ctx); err != nil && ctx.Err() == nil {
				log.Error("incident approval redrive sweep failed", "error", err)
			}
		}
	}
}

// SweepOnce dispatches every approved action that has been waiting longer than
// the grace period. Leader-gated so an N-node cluster redrives each action once.
//
// Only `approved` rows are picked up. A row left `executing` by a process death
// is deliberately NOT redriven: the dispatch had already begun, and re-running a
// half-applied tier-3 mutation — a jobdef patch, a skipped task, a bypassed
// schema gate — unattended is worse than leaving a visible stuck row for a human.
func (r *ApprovalRedriver) SweepOnce(ctx context.Context) error {
	if r.exec == nil {
		return nil
	}
	if r.leaderCheck != nil {
		leader, err := r.leaderCheck(ctx)
		if err != nil {
			return err
		}
		if !leader {
			return nil
		}
	}

	var stranded []models.AgentAction
	if err := r.db.WithContext(ctx).
		Select("id", "incident_id", "type").
		Where("status = ? AND updated_at <= ?", models.AgentActionStatusApproved, time.Now().UTC().Add(-r.grace)).
		Order("updated_at ASC").
		Limit(r.batch).
		Find(&stranded).Error; err != nil {
		return err
	}

	for i := range stranded {
		action := stranded[i]
		log.Info("incident: redriving approved action that was never executed",
			"action_id", action.ID, "incident_id", action.IncidentID, "type", action.Type)
		if _, err := r.exec.ExecuteApproved(ctx, action.ID); err != nil {
			// A lost claim is the expected benign outcome when the sweep races a
			// live dispatch; anything else is a genuine dispatch failure, already
			// recorded on the row by ExecuteApproved's finish path.
			log.Warn("incident: approved-action redrive did not execute",
				"action_id", action.ID, "incident_id", action.IncidentID, "error", err)
		}
	}
	return nil
}
