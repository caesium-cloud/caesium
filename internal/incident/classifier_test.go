package incident

import (
	"testing"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/event"
)

func intp(v int) *int { return &v }

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
		{"resource_failure", Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.ResourceFailure)}, ClassTransientInfra},
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
	got := c.Classify(Signal{EventType: string(event.TypeTaskFailed), Result: string(atom.Killed), ExitCode: intp(137)})
	if got != ClassOOM {
		t.Fatalf("expected oom from exit 137, got %q", got)
	}
}

func TestClassifyFallbackUnknown(t *testing.T) {
	c := NewClassifier()
	cases := []Signal{
		{EventType: string(event.TypeTaskFailed)},
		{EventType: string(event.TypeTaskFailed), Result: string(atom.Failure), ExitCode: intp(1)},
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
	if got := c.Classify(Signal{Result: string(atom.Failure), ExitCode: intp(42)}); got != ClassDataUnavailable {
		t.Fatalf("custom exit-code rule not applied: got %q", got)
	}
	if _, err := c.WithLogRule(`(?i)deadlock detected`, ClassTransientInfra); err != nil {
		t.Fatalf("WithLogRule: %v", err)
	}
	if got := c.Classify(Signal{LogTail: "ERROR: deadlock detected"}); got != ClassTransientInfra {
		t.Fatalf("custom log rule not applied: got %q", got)
	}
}
