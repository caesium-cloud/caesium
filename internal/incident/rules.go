package incident

import (
	"context"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
)

// Deterministic Phase-0 rule names (design-agent-in-the-loop.md, "Playbook
// match"): if a failure class maps to a deterministic rule, the incident manager
// executes it directly and records it as an AgentAction with actor=policy — same
// audit trail, no container launch.
const (
	// RuleAutoRetryBackoff re-runs a failed run after a fixed backoff — the
	// cheap path for transient-infra failures a plain retry clears.
	RuleAutoRetryBackoff = "auto_retry_backoff"
	// RuleSnoozeUntilCron defers a retry (e.g. wait for a late vendor file) via a
	// durable snooze timer rather than an in-process delay.
	RuleSnoozeUntilCron = "snooze_until_cron"
)

// DeterministicRule maps a failure class to a server-side remediation that runs
// without an agent container. Delay is the backoff/snooze applied before the
// concrete action.
type DeterministicRule struct {
	// Name is the operator-facing rule name recorded on the incident timeline.
	Name string
	// Class is the failure class this rule fires for.
	Class FailureClass
	// ActionType is the concrete catalog action the rule performs.
	ActionType string
	// Delay is the backoff before a retry (auto_retry_backoff) or the snooze
	// window (snooze_until_cron). Zero means immediate.
	Delay time.Duration
}

// DefaultRules ships the two Phase-0 deterministic rules the design names.
func DefaultRules() []DeterministicRule {
	return []DeterministicRule{
		{
			Name:       RuleAutoRetryBackoff,
			Class:      ClassTransientInfra,
			ActionType: ActionTypeRetryFromFailure,
			Delay:      0,
		},
		{
			Name:       RuleSnoozeUntilCron,
			Class:      ClassDataUnavailable,
			ActionType: ActionTypeSnoozeRetry,
			Delay:      45 * time.Minute,
		},
	}
}

// Rules is a deterministic rule table keyed by failure class. It holds no state
// beyond its table and is safe for concurrent reads.
type Rules struct {
	byClass map[FailureClass]DeterministicRule
}

// NewRules builds a rule table from the supplied rules (last rule for a class
// wins).
func NewRules(rules ...DeterministicRule) *Rules {
	byClass := make(map[FailureClass]DeterministicRule, len(rules))
	for _, r := range rules {
		byClass[r.Class] = r
	}
	return &Rules{byClass: byClass}
}

// DefaultRuleSet returns the shipped default deterministic rule table.
func DefaultRuleSet() *Rules { return NewRules(DefaultRules()...) }

// Match returns the deterministic rule for a class, if one exists.
func (r *Rules) Match(class FailureClass) (DeterministicRule, bool) {
	if r == nil {
		return DeterministicRule{}, false
	}
	rule, ok := r.byClass[class]
	return rule, ok
}

// ApplyDeterministicRule runs the deterministic rule for the incident's class (if
// any) as an actor=policy action, returning the recorded action and true. It
// returns (nil, false, nil) when no rule matches the class — the caller then
// dispatches an agent session instead. The rule's concrete action is executed
// through the same recording/metric/audit path as an agent action, so the
// timeline reconstructs uniformly.
func (e *Executor) ApplyDeterministicRule(ctx context.Context, inc *models.Incident, rules *Rules) (*models.AgentAction, bool, error) {
	rule, ok := rules.Match(FailureClass(inc.Class))
	if !ok {
		return nil, false, nil
	}
	params := ActionParams{}
	if rule.ActionType == ActionTypeSnoozeRetry {
		params.DelaySeconds = int64(rule.Delay / time.Second)
	}
	// The concrete run target defaults from the incident (resolveRunID) when the
	// rule does not carry an explicit one.
	action, err := e.ExecutePolicy(ctx, inc.ID, rule.ActionType, params)
	if err != nil {
		return action, true, err
	}
	return action, true, nil
}
