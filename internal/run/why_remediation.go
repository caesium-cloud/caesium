package run

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/google/uuid"
)

// why_remediation.go answers "who decided this, and when" for a task whose
// outcome an approved tier-3 remediation changed (trust-the-substrate C8).
//
// The two run-scoped tier-3 actions are the ones a `caesium why` reader can
// otherwise not account for:
//
//   - skip_task — the task row says "skipped" with no upstream reason;
//   - override_schema_gate — a declared output schema was simply not enforced.
//
// apply_jobdef_patch is deliberately NOT surfaced here: it is job-scoped and
// mutates the definition for FUTURE runs, so attributing it to an already-recorded
// task run would assert a causal link that may not exist. It belongs on the
// incident timeline and the job's own history.

// remediationActionTypes are the run-scoped approved actions `why` attributes.
var remediationActionTypes = []string{"skip_task", "override_schema_gate"}

// remediationScope classifies how broadly an action applies.
const (
	remediationScopeTask = "task"
	remediationScopeRun  = "run"
)

// approvedRemediationRow is the joined shape read from the audit spine: the
// action, plus the human decision that authorised it.
type approvedRemediationRow struct {
	ActionID   uuid.UUID  `gorm:"column:action_id"`
	IncidentID uuid.UUID  `gorm:"column:incident_id"`
	Type       string     `gorm:"column:type"`
	Tier       int        `gorm:"column:tier"`
	Result     []byte     `gorm:"column:result"`
	ExecutedAt *time.Time `gorm:"column:executed_at"`
	Decider    string     `gorm:"column:decider"`
	Reason     string     `gorm:"column:reason"`
	DecidedAt  *time.Time `gorm:"column:decided_at"`
}

// loadRemediation returns the approved remediation provenance for one task of a
// run, newest last.
//
// It is best-effort by design: `why` is a read-only explainer, and an incident
// substrate that is disabled, empty, or erroring must degrade to "no
// remediation provenance" rather than failing the explanation the operator
// actually asked for.
func (s *Store) loadRemediation(ctx context.Context, jobRun *models.JobRun, taskID uuid.UUID) []WhyRemediation {
	if s.db == nil || jobRun == nil {
		return nil
	}

	var rows []approvedRemediationRow
	err := s.db.WithContext(ctx).
		Table("agent_actions").
		Select("agent_actions.id AS action_id, agent_actions.incident_id AS incident_id, "+
			"agent_actions.type AS type, agent_actions.tier AS tier, agent_actions.result AS result, "+
			"agent_actions.updated_at AS executed_at, "+
			"approval_requests.decider AS decider, approval_requests.reason AS reason, "+
			"approval_requests.decided_at AS decided_at").
		Joins("JOIN approval_requests ON approval_requests.action_id = agent_actions.id").
		Joins("JOIN incidents ON incidents.id = agent_actions.incident_id").
		Where("incidents.job_id = ?", jobRun.JobID).
		Where("agent_actions.status = ?", string(models.AgentActionStatusExecuted)).
		Where("approval_requests.decision = ?", string(models.ApprovalDecisionApproved)).
		Where("agent_actions.type IN ?", remediationActionTypes).
		Order("agent_actions.updated_at ASC").
		Scan(&rows).Error
	if err != nil {
		log.Debug("why: could not read remediation provenance", "run_id", jobRun.ID, "error", err)
		return nil
	}

	var out []WhyRemediation
	for i := range rows {
		scope, ok := rows[i].appliesTo(jobRun.ID, taskID)
		if !ok {
			continue
		}
		out = append(out, WhyRemediation{
			ActionID:   rows[i].ActionID,
			IncidentID: rows[i].IncidentID,
			Type:       rows[i].Type,
			Tier:       rows[i].Tier,
			ApprovedBy: rows[i].Decider,
			ApprovedAt: rows[i].DecidedAt,
			ExecutedAt: rows[i].ExecutedAt,
			Reason:     rows[i].Reason,
			Scope:      scope,
		})
	}
	return out
}

// appliesTo reports whether this action touched the given (run, task), and at
// which scope.
//
// The targets are read from the action's RESULT payload — what the executor
// actually did — not from its params, which record what was asked for. An action
// whose result names no run cannot be attributed and is dropped: an over-broad
// attribution ("some approved action ran, maybe this is why") is worse than
// none, because it invites an operator to stop looking.
func (r approvedRemediationRow) appliesTo(runID, taskID uuid.UUID) (string, bool) {
	if len(r.Result) == 0 {
		return "", false
	}
	var result struct {
		RunID  string `json:"run_id"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(r.Result, &result); err != nil {
		return "", false
	}
	if !strings.EqualFold(strings.TrimSpace(result.RunID), runID.String()) {
		return "", false
	}
	if target := strings.TrimSpace(result.TaskID); target != "" {
		if !strings.EqualFold(target, taskID.String()) {
			return "", false
		}
		return remediationScopeTask, true
	}
	// No task target: the action applied to the whole run (override_schema_gate).
	return remediationScopeRun, true
}

// summarizeRemediation renders the provenance clause appended to the `why`
// headline, so the decider's name is in the FIRST line of output rather than
// buried in a table an operator has to know to read.
func summarizeRemediation(entries []WhyRemediation) string {
	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		who := e.ApprovedBy
		if who == "" {
			who = "an operator"
		}
		clause := fmt.Sprintf("`%s` approved by %s", e.Type, who)
		if e.ExecutedAt != nil {
			clause += ", executed at " + e.ExecutedAt.UTC().Format(time.RFC3339)
		}
		parts = append(parts, clause)
	}
	return "remediation: " + strings.Join(parts, "; ")
}
