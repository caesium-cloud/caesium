package incident

import (
	"fmt"
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
)

// TestClassifySecretDenialIsConfig pins that a namespace secret denial — as
// the executors write it into TaskRun.Error — is the config class, ahead of
// the transient_infra branch (which would auto-retry it) and of the
// auth_failure log rule, and that only the executor-written Error counts,
// never the container's own log.
func TestClassifySecretDenialIsConfig(t *testing.T) {
	c := NewClassifier()
	denial := &secret.SecretDeniedError{Namespace: "marketing", Ref: "env/FINANCE_TOKEN/…", Reason: "no rule matches"}
	for _, text := range []string{
		fmt.Errorf("env FINANCE: %w", denial).Error(),
		fmt.Errorf("resolve env FINANCE: %w", denial).Error(),
		fmt.Errorf("replay secret for env FINANCE: baseline identity verification failed: %w", denial).Error(),
		fmt.Errorf("access denied: %w", denial).Error(),
	} {
		for _, sig := range []Signal{
			{EventType: string(event.TypeTaskFailed), Error: text},
			{EventType: string(event.TypeTaskFailed), Error: text, Result: string(atom.StartupFailure)},
			{EventType: string(event.TypeRunFailed), Error: text, ExitCode: intPtr(137)},
		} {
			if got := c.Classify(sig); got != ClassConfig {
				t.Fatalf("Classify(%+v) = %q, want %q", sig, got, ClassConfig)
			}
		}
	}
	// The marker in the container log is not the executor's verdict.
	logOnly := Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.StartupFailure), LogTail: denial.Error()}
	if got := c.Classify(logOnly); got != ClassTransientInfra {
		t.Fatalf("Classify(log-only marker) = %q, want %q", got, ClassTransientInfra)
	}
	// SLA precedence still wins.
	if got := c.Classify(Signal{EventType: string(event.TypeRunTimedOut), Error: denial.Error()}); got != ClassSLARisk {
		t.Fatalf("run timeout with a denial = %q, want %q", got, ClassSLARisk)
	}
	// No deterministic rule acts on config: a denial is never auto-retried.
	if _, ok := DefaultRuleSet().Match(ClassConfig); ok {
		t.Fatal("config must not match a deterministic remediation rule")
	}
}

func intPtr(value int) *int { return &value }

func TestClassifyStructuredSignals(t *testing.T) {
	c := NewClassifier()
	cases := []struct {
		name string
		sig  Signal
		want FailureClass
	}{
		{"run_timed_out", Signal{EventType: string(event.TypeRunTimedOut)}, ClassSLARisk},
		{"sla_missed", Signal{EventType: string(event.TypeSLAMissed)}, ClassSLARisk},
		{"schema_event", Signal{EventType: string(event.TypeSchemaViolationRecorded)}, ClassSchemaViolation},
		{"schema_flag", Signal{EventType: string(event.TypeTaskFailed), HasSchemaViolations: true}, ClassSchemaViolation},
		{"startup_failure", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.StartupFailure)}, ClassTransientInfra},
		{"resource_failure_oom", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.ResourceFailure), OOMKilled: true}, ClassOOM},
		{"resource_failure", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.ResourceFailure)}, ClassTransientInfra},
		{"legacy_exit_137_without_runtime_evidence", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: intPtr(137)}, ClassOOM},
		{"observed_non_oom_exit_137", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: intPtr(137), OOMKnown: true}, ClassUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.Classify(tc.sig); got != tc.want {
				t.Fatalf("Classify(%+v) = %q, want %q", tc.sig, got, tc.want)
			}
		})
	}
}

func TestClassifyLogTable(t *testing.T) {
	c := NewClassifier()
	cases := []struct {
		name string
		log  string
		want FailureClass
	}{
		{"oom", "container killed process: out of memory", ClassOOM},
		{"oomkilled", "reason: OOMKilled", ClassOOM},
		{"auth", "Error: 403 Forbidden: invalid credentials", ClassAuthFailure},
		{"auth_permission", "permission denied while reading /data", ClassAuthFailure},
		{"quota", "API quota exceeded, too many requests (429)", ClassQuota},
		{"data_unavailable", "psycopg2: could not connect to server: connection refused", ClassDataUnavailable},
		{"data_notfound", "FileNotFoundError: no such file or directory: input.csv", ClassDataUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Failure), LogTail: tc.log})
			if got != tc.want {
				t.Fatalf("Classify(log=%q) = %q, want %q", tc.log, got, tc.want)
			}
		})
	}
}

func TestClassifyLogFallsBackToErrorText(t *testing.T) {
	c := NewClassifier()
	got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Error: "unauthorized: token expired"})
	if got != ClassAuthFailure {
		t.Fatalf("expected auth_failure from Error text, got %q", got)
	}
}

func TestClassifyExitCodeTable(t *testing.T) {
	c := NewClassifier()
	if got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: new(137)}); got != ClassOOM {
		t.Fatalf("expected oom from unavailable exit 137, got %q", got)
	}
	if got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: new(137), OOMKnown: true}); got != ClassUnknown {
		t.Fatalf("expected unknown from observed non-OOM exit 137, got %q", got)
	}
}

func TestClassifyObservedNonOOMPreservesCustom137Rule(t *testing.T) {
	c := NewClassifier().WithExitCodeRule(137, ClassTransientInfra)
	got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: new(137), OOMKnown: true, OOMKilled: false})
	if got != ClassTransientInfra {
		t.Fatalf("expected custom 137 class from observed non-OOM exit, got %q", got)
	}
}

func TestClassifyFallbackUnknown(t *testing.T) {
	c := NewClassifier()
	cases := []Signal{
		{EventType: string(event.TypeTaskFailed)},
		{EventType: string(event.TypeTaskFailed), Result: string(atom.Failure), ExitCode: new(1)},
		{EventType: string(event.TypeRunFailed), LogTail: "some generic stack trace with no known signature"},
	}
	for i, sig := range cases {
		if got := c.Classify(sig); got != ClassUnknown {
			t.Fatalf("case %d: Classify(%+v) = %q, want unknown", i, sig, got)
		}
	}
}

func TestClassifyPrecedenceSchemaOverLog(t *testing.T) {
	c := NewClassifier()
	// A schema violation should win even if the log contains an auth-like string.
	got := c.Classify(Signal{
		EventType:           string(event.TypeSchemaViolationRecorded),
		HasSchemaViolations: true,
		LogTail:             "403 forbidden",
	})
	if got != ClassSchemaViolation {
		t.Fatalf("expected schema_violation precedence, got %q", got)
	}
}

func TestClassifierConfigurableRules(t *testing.T) {
	c := NewClassifier().WithExitCodeRule(42, ClassDataUnavailable)
	if got := c.Classify(Signal{Result: string(atom.Failure), ExitCode: new(42)}); got != ClassDataUnavailable {
		t.Fatalf("custom exit-code rule not applied: got %q", got)
	}
	if _, err := c.WithLogRule(`(?i)deadlock detected`, ClassTransientInfra); err != nil {
		t.Fatalf("WithLogRule: %v", err)
	}
	if got := c.Classify(Signal{LogTail: "ERROR: deadlock detected"}); got != ClassTransientInfra {
		t.Fatalf("custom log rule not applied: got %q", got)
	}
}
