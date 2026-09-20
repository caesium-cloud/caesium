package robustness

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Outcome labels for a mutating request. Quorum-loss and lost-response
// operations may only be accepted or possibly committed; a client timeout is
// never a rejection (DT-QUORUM-01).
const (
	OutcomeAccepted            = "accepted"
	OutcomeRejected            = "rejected"
	OutcomePossiblyCommitted   = "possibly_committed"
	RefusalStaleGeneration     = "stale_generation"
	RefusalUnauthorized        = "unauthorized"
	MetricDispatchRejected     = "caesium_dispatch_rejected_total"
	MetricDispatchStalled      = "caesium_dispatch_stalled_total"
	MetricDispatchSent         = "caesium_dispatch_sent_total"
	DispatchReasonNetworkError = "network_error"
)

// TaskFingerprint is the mutation-sensitive public/SQL view of one task-run.
// Secrets never belong here.
type TaskFingerprint struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	Image     string `json:"image"`
	Command   string `json:"command,omitempty"`
	ClaimedBy string `json:"claimed_by,omitempty"`
	Attempt   int    `json:"attempt"`
	Error     string `json:"error,omitempty"`
}

// StateFingerprint is compared before/after a denied operation. Equality is
// the DT-AUTH-01 / DT-COMPLETE-01 "no task/state/effect mutation" oracle.
type StateFingerprint struct {
	RunID           string            `json:"run_id"`
	JobID           string            `json:"job_id"`
	Status          string            `json:"status"`
	Tasks           []TaskFingerprint `json:"tasks"`
	EffectNonces    []string          `json:"effect_nonces"`
	LeaseOwner      string            `json:"lease_owner,omitempty"`
	LeaseGeneration int64             `json:"lease_generation,omitempty"`
}

// TimedStep is one raw effect or public event used by the fan-in checker.
type TimedStep struct {
	Step string
	Kind string
	At   time.Time
}

// StateDiffs returns the field paths that changed. An empty slice means the
// denied operation left observable state alone.
func StateDiffs(before, after StateFingerprint) []string {
	var diffs []string
	if before.RunID != after.RunID {
		diffs = append(diffs, "run_id")
	}
	if before.JobID != after.JobID {
		diffs = append(diffs, "job_id")
	}
	if !strings.EqualFold(before.Status, after.Status) {
		diffs = append(diffs, "status")
	}
	if before.LeaseOwner != after.LeaseOwner {
		diffs = append(diffs, "lease_owner")
	}
	if before.LeaseGeneration != after.LeaseGeneration {
		diffs = append(diffs, "lease_generation")
	}
	if !stringSliceEqual(before.EffectNonces, after.EffectNonces) {
		diffs = append(diffs, "effect_nonces")
	}
	if len(before.Tasks) != len(after.Tasks) {
		diffs = append(diffs, "tasks.len")
		return diffs
	}
	for i := range before.Tasks {
		prefix := fmt.Sprintf("tasks[%d]", i)
		a, b := before.Tasks[i], after.Tasks[i]
		if a.ID != b.ID {
			diffs = append(diffs, prefix+".id")
		}
		if a.TaskID != b.TaskID {
			diffs = append(diffs, prefix+".task_id")
		}
		if !strings.EqualFold(a.Status, b.Status) {
			diffs = append(diffs, prefix+".status")
		}
		if a.Image != b.Image {
			diffs = append(diffs, prefix+".image")
		}
		if a.Command != b.Command {
			diffs = append(diffs, prefix+".command")
		}
		if a.ClaimedBy != b.ClaimedBy {
			diffs = append(diffs, prefix+".claimed_by")
		}
		if a.Attempt != b.Attempt {
			diffs = append(diffs, prefix+".attempt")
		}
		if a.Error != b.Error {
			diffs = append(diffs, prefix+".error")
		}
	}
	return diffs
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ClassifyQuorumLossMutation never reports a rejection. A1 left DT-QUORUM-01
// unresolved: timeout, empty body, 5xx, and even a 4xx wrapping a DB error are
// possibly committed until post-heal identity reconciliation. Only a 202 with
// a run UUID is accepted.
func ClassifyQuorumLossMutation(status int, body []byte, transportErr error) string {
	if transportErr != nil || status == 0 || len(strings.TrimSpace(string(body))) == 0 {
		return OutcomePossiblyCommitted
	}
	if status == 202 && runIDFromBody(body) != "" {
		return OutcomeAccepted
	}
	return OutcomePossiblyCommitted
}

// ClassifyLostResponse is the commit-before-response-loss oracle: if the
// controller observed an upstream commit, the client's timeout is possibly
// committed, never rejected.
func ClassifyLostResponse(clientErr error, upstreamStatus int, upstreamRunID string) string {
	if clientErr == nil && upstreamStatus == 202 && upstreamRunID != "" {
		return OutcomeAccepted
	}
	if upstreamStatus == 202 && upstreamRunID != "" {
		return OutcomePossiblyCommitted
	}
	if clientErr != nil {
		return OutcomePossiblyCommitted
	}
	return OutcomePossiblyCommitted
}

func runIDFromBody(body []byte) string {
	var parsed struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return strings.TrimSpace(parsed.ID)
}

// ParseRefusal reads a 4xx internal/complete or /internal/dispatch body.
func ParseRefusal(status int, body []byte) (code, message string) {
	var parsed struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   string `json:"error"`
		Reason  string `json:"reason"`
	}
	_ = json.Unmarshal(body, &parsed)
	code = firstNonEmpty(parsed.Code, parsed.Reason)
	message = firstNonEmpty(parsed.Message, parsed.Error, strings.TrimSpace(string(body)))
	if status == 401 && code == "" {
		code = RefusalUnauthorized
	}
	return code, message
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// FanInStartedTooEarly reports a defect when the join step started before
// every named predecessor had a recorded completion. One successful partition
// is not a satisfied fan-in (DT-DAG-01).
func FanInStartedTooEarly(events []TimedStep, join string, predecessors []string) error {
	if join == "" || len(predecessors) == 0 {
		return fmt.Errorf("fan-in check needs a join step and at least one predecessor")
	}
	joinStart, ok := earliestKind(events, join, "start")
	if !ok {
		return fmt.Errorf("join step %q never started", join)
	}
	var missing []string
	for _, pred := range predecessors {
		done, ok := earliestKind(events, pred, "complete")
		if !ok {
			missing = append(missing, pred)
			continue
		}
		if !done.At.Before(joinStart.At) && !done.At.Equal(joinStart.At) {
			// completion after start is a defect; equal is tolerated only if
			// clocks collapse, which we still reject: join must be strictly
			// after the predecessor's recorded completion.
		}
		if !joinStart.At.After(done.At) {
			return fmt.Errorf("join %q started at %s before predecessor %q completed at %s",
				join, joinStart.At.UTC().Format(time.RFC3339Nano), pred, done.At.UTC().Format(time.RFC3339Nano))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("join %q started at %s without recorded completions for %s",
			join, joinStart.At.UTC().Format(time.RFC3339Nano), strings.Join(missing, ","))
	}
	return nil
}

func earliestKind(events []TimedStep, step, kind string) (TimedStep, bool) {
	var best TimedStep
	found := false
	for _, ev := range events {
		if ev.Step != step || !strings.EqualFold(ev.Kind, kind) || ev.At.IsZero() {
			continue
		}
		if !found || ev.At.Before(best.At) {
			best = ev
			found = true
		}
	}
	return best, found
}

// PromCounter reads one labelled counter from Prometheus text. An absent
// series is (0, false) so a missing metric cannot be treated as a rise.
func PromCounter(text, name string, labels map[string]string) (float64, bool) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.HasPrefix(line, name) {
			continue
		}
		series, value, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		matched := true
		for k, v := range labels {
			if !strings.Contains(series, fmt.Sprintf("%s=%q", k, v)) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			continue
		}
		return parsed, true
	}
	return 0, false
}

// RedactSecrets strips bearer tokens, PEM blocks, and known robustness token
// needles so persisted records never carry credentials.
func RedactSecrets(s string) string {
	out := s
	out = redactPEM(out)
	out = redactBearer(out)
	for _, needle := range []string{
		"caesium-robustness-internal-token-not-for-production-use",
		"caesium-robustness-manual-key",
	} {
		out = strings.ReplaceAll(out, needle, "[redacted]")
	}
	return out
}

func redactBearer(s string) string {
	lower := strings.ToLower(s)
	var b strings.Builder
	i := 0
	for i < len(s) {
		idx := strings.Index(lower[i:], "bearer ")
		if idx < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+idx])
		b.WriteString("Bearer [redacted]")
		i += idx + len("bearer ")
		for i < len(s) && !strings.ContainsRune(" \t\r\n\"'", rune(s[i])) {
			i++
		}
	}
	return b.String()
}

func redactPEM(s string) string {
	const begin = "-----BEGIN "
	const end = "-----END "
	var b strings.Builder
	rest := s
	for {
		start := strings.Index(rest, begin)
		if start < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:start])
		rest = rest[start:]
		endIdx := strings.Index(rest, end)
		if endIdx < 0 {
			b.WriteString("[redacted-pem]")
			return b.String()
		}
		nl := strings.Index(rest[endIdx:], "\n")
		if nl < 0 {
			b.WriteString("[redacted-pem]")
			return b.String()
		}
		b.WriteString("[redacted-pem]")
		rest = rest[endIdx+nl:]
	}
}

// FrozenRecipeChanged reports whether a retried task's persisted image/command
// drifted from the values frozen at first registration.
func FrozenRecipeChanged(original, retried TaskFingerprint) error {
	if original.Image != retried.Image {
		return fmt.Errorf("retried task %s image changed from %q to %q; recipe is not frozen", retried.ID, original.Image, retried.Image)
	}
	if original.Command != "" && retried.Command != "" && original.Command != retried.Command {
		return fmt.Errorf("retried task %s command changed; recipe is not frozen", retried.ID)
	}
	return nil
}
