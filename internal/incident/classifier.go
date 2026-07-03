// Package incident implements the Phase-0 incident substrate for
// agent-in-the-loop remediation: a deterministic failure classifier, a
// leader-gated dedupe subscriber, the incident store and status machine, a
// free-text log scrubber, and a durable-timer supervisor. Nothing in this
// package invokes an LLM or takes an autonomous action — it diagnoses failures
// and records incidents.
package incident

import (
	"regexp"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/event"
)

// FailureClass buckets a failure for remediation routing. The classifier is
// purely deterministic — the same Signal always yields the same class.
type FailureClass string

const (
	// ClassTransientInfra covers startup/resource engine failures that a plain
	// retry often clears.
	ClassTransientInfra FailureClass = "transient_infra"
	// ClassSchemaViolation covers a task whose output violated its declared
	// schema (in warn mode the task did not fail).
	ClassSchemaViolation FailureClass = "schema_violation"
	// ClassSLARisk covers run timeouts and SLA misses.
	ClassSLARisk FailureClass = "sla_risk"
	// ClassDataUnavailable covers missing/unreachable inputs.
	ClassDataUnavailable FailureClass = "data_unavailable"
	// ClassAuthFailure covers credential/permission failures.
	ClassAuthFailure FailureClass = "auth_failure"
	// ClassOOM covers out-of-memory kills. Best-effort until the OOM-flag
	// detection from design-resource-right-sizing lands.
	ClassOOM FailureClass = "oom"
	// ClassQuota covers rate limits / quota exhaustion.
	ClassQuota FailureClass = "quota"
	// ClassUnknown is the fallback — always agent-eligible.
	ClassUnknown FailureClass = "unknown"
)

// Signal is the deterministic input the classifier consumes. It is derived from
// a bus event plus the persisted TaskRun (Result/ExitCode/SchemaViolations/
// LogText/Error), never from an LLM.
type Signal struct {
	// EventType is the bus event.Type string that triggered classification.
	EventType string
	// Result is the atom.Result string persisted on TaskRun.Result.
	Result string
	// HasSchemaViolations reports whether the task run recorded schema violations.
	HasSchemaViolations bool
	// ExitCode is the raw process exit code, or nil when none was captured.
	ExitCode *int
	// LogTail is the (already-scrubbed) tail of the task log.
	LogTail string
	// Error is the TaskRun.Error text.
	Error string
}

// logRule maps a compiled regex over the log tail / error text to a class. Rules
// are evaluated in slice order so classification is deterministic.
type logRule struct {
	re    *regexp.Regexp
	class FailureClass
}

// Classifier maps a Signal to a FailureClass using a fixed precedence of
// structured signals (event type, schema violations, engine result) followed by
// a configurable exit-code table and log-tail regex table. It holds no state
// beyond its rule tables and is safe for concurrent use.
type Classifier struct {
	// exitCodeRules maps a raw exit code to a class. Consulted after the log
	// rules so a specific log pattern wins over a coarse code.
	exitCodeRules map[int]FailureClass
	// logRules are evaluated in order against LogTail then Error.
	logRules []logRule
}

// NewClassifier returns a Classifier seeded with sane default rules.
func NewClassifier() *Classifier {
	return &Classifier{
		exitCodeRules: defaultExitCodeRules(),
		logRules:      defaultLogRules(),
	}
}

// defaultExitCodeRules ships one conservative default: 137 (SIGKILL, the code an
// OOM kill surfaces as) maps to oom. This stays best-effort until the OOM-flag
// detection substrate lands — 137 can also be a deliberate kill.
func defaultExitCodeRules() map[int]FailureClass {
	return map[int]FailureClass{
		137: ClassOOM,
	}
}

// Default log-rule patterns, compiled once at package load rather than on every
// NewClassifier() call.
var (
	oomLogRe             = regexp.MustCompile(`(?i)(out of memory|oomkilled|oom-kill|cannot allocate memory|killed process|memory cgroup out of memory)`)
	authFailureLogRe     = regexp.MustCompile(`(?i)(permission denied|unauthorized|forbidden|authentication failed|invalid credentials|access denied|401|403|expired token|invalid api key)`)
	quotaLogRe           = regexp.MustCompile(`(?i)(quota exceeded|rate limit|ratelimit|too many requests|429|throttl|limit exceeded)`)
	dataUnavailableLogRe = regexp.MustCompile(`(?i)(no such file|not found|does not exist|doesn't exist|connection refused|could not connect|could not resolve|no route to host|no data available|table.*(not exist|missing))`)
)

// defaultLogRules ships case-insensitive patterns for the four log-driven
// classes. Order encodes precedence: oom, then auth, then quota, then data.
func defaultLogRules() []logRule {
	return []logRule{
		{re: oomLogRe, class: ClassOOM},
		{re: authFailureLogRe, class: ClassAuthFailure},
		{re: quotaLogRe, class: ClassQuota},
		{re: dataUnavailableLogRe, class: ClassDataUnavailable},
	}
}

// WithExitCodeRule overrides or adds an exit-code rule (configuration hook).
func (c *Classifier) WithExitCodeRule(code int, class FailureClass) *Classifier {
	if c.exitCodeRules == nil {
		c.exitCodeRules = map[int]FailureClass{}
	}
	c.exitCodeRules[code] = class
	return c
}

// WithLogRule appends a log-tail rule (configuration hook). Appended rules are
// evaluated after the defaults.
func (c *Classifier) WithLogRule(pattern string, class FailureClass) (*Classifier, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return c, err
	}
	c.logRules = append(c.logRules, logRule{re: re, class: class})
	return c, nil
}

// Classify maps a Signal to a FailureClass. Precedence, top to bottom:
//
//  1. run_timed_out / sla_missed          → sla_risk
//  2. schema_violation event / violations → schema_violation
//  3. StartupFailure / ResourceFailure    → transient_infra
//  4. log-tail regex table                → data_unavailable|auth_failure|oom|quota
//  5. exit-code table                     → (default 137 → oom)
//  6. fallback                            → unknown
func (c *Classifier) Classify(sig Signal) FailureClass {
	switch event.Type(sig.EventType) {
	case event.TypeRunTimedOut, event.TypeSLAMissed:
		return ClassSLARisk
	case event.TypeSchemaViolationRecorded:
		return ClassSchemaViolation
	}

	if sig.HasSchemaViolations {
		return ClassSchemaViolation
	}

	switch atom.Result(sig.Result) {
	case atom.StartupFailure, atom.ResourceFailure:
		return ClassTransientInfra
	}

	// Log-tail regex table (LogTail first, then Error text).
	for _, corpus := range []string{sig.LogTail, sig.Error} {
		if corpus == "" {
			continue
		}
		for _, rule := range c.logRules {
			if rule.re.MatchString(corpus) {
				return rule.class
			}
		}
	}

	// Exit-code table.
	if sig.ExitCode != nil {
		if class, ok := c.exitCodeRules[*sig.ExitCode]; ok {
			return class
		}
	}

	return ClassUnknown
}
