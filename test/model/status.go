package model

// TaskStatus is a modelled task outcome. The vocabulary matches the product's
// so a differential test can compare statuses directly, but the predicates
// below are written from the specification rather than copied.
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"
	StatusRunning   TaskStatus = "running"
	StatusSucceeded TaskStatus = "succeeded"
	StatusFailed    TaskStatus = "failed"
	StatusSkipped   TaskStatus = "skipped"
	StatusCached    TaskStatus = "cached"
	StatusCancelled TaskStatus = "cancelled"
)

// Terminal reports whether a task will not transition again.
func Terminal(s TaskStatus) bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusSkipped, StatusCached, StatusCancelled:
		return true
	default:
		return false
	}
}

// Succeeded reports whether an outcome counts as success for trigger-rule
// purposes. A cache hit is a success: it is a statement about where the result
// came from, not about the result.
func Succeeded(s TaskStatus) bool {
	return s == StatusSucceeded || s == StatusCached
}

// WorkerOutcomes are the terminal statuses a worker can report. Everything else
// in the vocabulary is an owner decision (skipped) or an operator one
// (cancelled), and generating them as worker reports would test a transition
// the product never makes.
func WorkerOutcomes() []TaskStatus {
	return []TaskStatus{StatusSucceeded, StatusFailed, StatusCached}
}

// TriggerRule decides whether a step runs given its predecessors' outcomes.
type TriggerRule string

const (
	AllSuccess TriggerRule = "all_success"
	AllDone    TriggerRule = "all_done"
	AllFailed  TriggerRule = "all_failed"
	OneSuccess TriggerRule = "one_success"
	Always     TriggerRule = "always"
)

// TriggerRules is every rule the model generates.
func TriggerRules() []TriggerRule {
	return []TriggerRule{AllSuccess, AllDone, AllFailed, OneSuccess, Always}
}

// Tolerant reports whether a rule explicitly handles upstream FAILURE, and so
// must never be pre-emptively skipped when a predecessor fails: its own
// evaluation, once every predecessor is terminal, is what decides.
func (r TriggerRule) Tolerant() bool {
	switch r {
	case AllDone, AllFailed, Always, OneSuccess:
		return true
	default:
		return false
	}
}

// Satisfied evaluates the rule against the predecessor outcomes.
//
// Two specification details that are easy to get wrong and that the product
// also implements this way:
//
//   - A step with NO predecessors runs under every rule, including all_failed.
//     A root is not "waiting on nothing that failed"; it is simply a root.
//   - An unknown rule degrades to all_success rather than to always, so a typo
//     cannot turn a gate into a pass-through.
func (r TriggerRule) Satisfied(pred []TaskStatus) bool {
	if len(pred) == 0 {
		return true
	}
	switch r {
	case AllDone, Always:
		for _, s := range pred {
			if !Terminal(s) {
				return false
			}
		}
		return true
	case AllFailed:
		for _, s := range pred {
			if s != StatusFailed {
				return false
			}
		}
		return true
	case OneSuccess:
		for _, s := range pred {
			if Succeeded(s) {
				return true
			}
		}
		return false
	default: // AllSuccess, "" and anything unrecognized.
		for _, s := range pred {
			if !Succeeded(s) {
				return false
			}
		}
		return true
	}
}

// GroupStatus collapses a fanned step's instance outcomes into the single
// status its cross-step successors evaluate their trigger rule against.
//
// The mixed succeeded+skipped case resolves to FAILED, which is worth stating
// out loud because it reads like an oversight and is not: a group that partly
// did not run has not succeeded, and calling it skipped would let an
// all_success consumer that should be blocked infer "nothing to wait for".
func GroupStatus(members []TaskStatus) TaskStatus {
	if len(members) == 0 {
		return ""
	}
	// An in-flight member outranks a failed one: a group with work still
	// running has not resolved, so no successor may evaluate its rule yet.
	for _, s := range members {
		if !Terminal(s) {
			return StatusRunning
		}
	}
	allSuccess, allSkipped := true, true
	anyFailed := false
	for _, s := range members {
		switch {
		case s == StatusFailed:
			anyFailed = true
			allSuccess, allSkipped = false, false
		case Succeeded(s):
			allSkipped = false
		case s == StatusSkipped:
			allSuccess = false
		default: // cancelled
			allSuccess, allSkipped = false, false
		}
	}
	switch {
	case anyFailed:
		return StatusFailed
	case allSkipped:
		return StatusSkipped
	case allSuccess:
		return StatusSucceeded
	default:
		return StatusFailed
	}
}
