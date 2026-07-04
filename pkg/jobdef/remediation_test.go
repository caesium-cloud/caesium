package jobdef

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/internal/cache"
)

const remediationJob = `
apiVersion: v1
kind: Job
metadata:
  alias: vendor-x-daily
  remediation:
    profile: default-triage
    classes: [data_unavailable, schema_violation, transient_infra, unknown]
    maxAttempts: 2
    autonomy:
      allow: [retry_from_failure, snooze_retry, quarantine_replay, rerun_with_params, pause_job]
      paramOverrides:
        badRowPolicy: [quarantine]
      perClass:
        auth_failure:
          allow: [pause_job, notify, escalate]
      requireApproval: [apply_jobdef_patch, skip_task, override_schema_gate]
    escalation:
      channel: data-oncall
      after: 15m
trigger:
  type: cron
  configuration:
    expression: "0 */6 * * *"
  defaultParams:
    badRowPolicy: quarantine
steps:
  - name: extract
    image: etl:1.4
`

func TestParseRemediationSurface(t *testing.T) {
	def, err := Parse([]byte(remediationJob))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	r := def.Metadata.Remediation
	if r == nil {
		t.Fatal("expected metadata.remediation to be set")
	}
	if r.Profile != "default-triage" {
		t.Fatalf("unexpected profile: %q", r.Profile)
	}
	if len(r.Classes) != 4 {
		t.Fatalf("expected 4 classes, got %+v", r.Classes)
	}
	if r.MaxAttempts != 2 {
		t.Fatalf("unexpected maxAttempts: %d", r.MaxAttempts)
	}
	if r.Autonomy == nil {
		t.Fatal("expected autonomy to be set")
	}
	if len(r.Autonomy.Allow) != 5 {
		t.Fatalf("unexpected allow list: %+v", r.Autonomy.Allow)
	}
	if vals := r.Autonomy.ParamOverrides["badRowPolicy"]; len(vals) != 1 || vals[0] != "quarantine" {
		t.Fatalf("unexpected paramOverrides: %+v", r.Autonomy.ParamOverrides)
	}
	perClass, ok := r.Autonomy.PerClass["auth_failure"]
	if !ok || len(perClass.Allow) != 3 {
		t.Fatalf("unexpected perClass: %+v", r.Autonomy.PerClass)
	}
	if len(r.Autonomy.RequireApproval) != 3 {
		t.Fatalf("unexpected requireApproval: %+v", r.Autonomy.RequireApproval)
	}
	if r.Escalation == nil || r.Escalation.Channel != "data-oncall" || r.Escalation.After != "15m" {
		t.Fatalf("unexpected escalation: %+v", r.Escalation)
	}
}

func TestValidateRemediation_RequiresProfile(t *testing.T) {
	y := strings.Replace(remediationJob, "profile: default-triage", "profile: \"\"", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "profile is required") {
		t.Fatalf("expected profile-required error, got %v", err)
	}
}

func TestValidateRemediation_RequiresClasses(t *testing.T) {
	y := strings.Replace(remediationJob, "classes: [data_unavailable, schema_violation, transient_infra, unknown]", "classes: []", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "classes must contain at least one entry") {
		t.Fatalf("expected classes-required error, got %v", err)
	}
}

func TestValidateRemediation_UnknownClass(t *testing.T) {
	y := strings.Replace(remediationJob, "classes: [data_unavailable, schema_violation, transient_infra, unknown]", "classes: [bogus_class]", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "not a known failure class") {
		t.Fatalf("expected unknown-class error, got %v", err)
	}
}

func TestValidateRemediation_DuplicateClass(t *testing.T) {
	y := strings.Replace(remediationJob, "classes: [data_unavailable, schema_violation, transient_infra, unknown]", "classes: [unknown, unknown]", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "duplicates another entry") {
		t.Fatalf("expected duplicate-class error, got %v", err)
	}
}

func TestValidateRemediation_UnknownAction(t *testing.T) {
	y := strings.Replace(remediationJob, "allow: [retry_from_failure, snooze_retry, quarantine_replay, rerun_with_params, pause_job]", "allow: [rm_rf_slash]", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "not a known remediation action") {
		t.Fatalf("expected unknown-action error, got %v", err)
	}
}

func TestValidateRemediation_ParamOverridesMustMatchDefaultParams(t *testing.T) {
	y := strings.Replace(remediationJob, "badRowPolicy: [quarantine]", "unknownParam: [quarantine]", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "does not match any trigger.defaultParams key") {
		t.Fatalf("expected paramOverrides-mismatch error, got %v", err)
	}
}

func TestValidateRemediation_PerClassKeyCanonicalized(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: j
  remediation:
    profile: default-triage
    classes: [unknown]
    autonomy:
      perClass:
        "  auth_failure  ":
          allow: [notify, escalate]
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
`
	def, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	pc := def.Metadata.Remediation.Autonomy.PerClass
	if _, ok := pc["auth_failure"]; !ok {
		t.Fatalf("expected canonical key \"auth_failure\", got keys %v", pc)
	}
	if _, ok := pc["  auth_failure  "]; ok {
		t.Fatalf("expected untrimmed key to be dropped, got keys %v", pc)
	}
}

func TestValidateRemediation_PerClassUnknownClass(t *testing.T) {
	y := strings.Replace(remediationJob, "auth_failure:", "bogus_class:", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "perClass key") {
		t.Fatalf("expected perClass-unknown-class error, got %v", err)
	}
}

func TestValidateRemediation_NegativeMaxAttempts(t *testing.T) {
	y := strings.Replace(remediationJob, "maxAttempts: 2", "maxAttempts: -1", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "maxAttempts must be >= 0") {
		t.Fatalf("expected maxAttempts error, got %v", err)
	}
}

func TestValidateRemediation_EscalationAfterMustBeDuration(t *testing.T) {
	y := strings.Replace(remediationJob, "after: 15m", "after: not-a-duration", 1)
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "must be a valid duration") {
		t.Fatalf("expected duration error, got %v", err)
	}
}

func TestValidateRemediation_EscalationRequiresChannelOrAfter(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: j
  remediation:
    profile: default-triage
    classes: [unknown]
    escalation: {}
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
`
	if _, err := Parse([]byte(y)); err == nil || !strings.Contains(err.Error(), "requires channel and/or after") {
		t.Fatalf("expected escalation error, got %v", err)
	}
}

func TestRemediationOptional(t *testing.T) {
	y := `
apiVersion: v1
kind: Job
metadata:
  alias: j
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
`
	def, err := Parse([]byte(y))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if def.Metadata.Remediation != nil {
		t.Fatalf("expected no remediation block, got %+v", def.Metadata.Remediation)
	}
}

// TestRemediationDoesNotAffectCacheHash proves the remediation block is
// policy metadata: adding it to a job's metadata leaves the container spec
// that feeds the cache identity — and thus HashInput.Compute() — byte
// identical, mirroring TestDatasetsDoNotAffectCacheHash.
func TestRemediationDoesNotAffectCacheHash(t *testing.T) {
	base := `
apiVersion: v1
kind: Job
metadata:
  alias: j
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
    command: ["sh", "-c", "run"]
`
	withRemediation := `
apiVersion: v1
kind: Job
metadata:
  alias: j
  remediation:
    profile: default-triage
    classes: [unknown, transient_infra]
    maxAttempts: 3
    autonomy:
      allow: [retry_from_failure, notify]
      requireApproval: [apply_jobdef_patch]
    escalation:
      channel: data-oncall
      after: 30m
trigger:
  type: cron
  configuration: {expression: "0 * * * *"}
steps:
  - name: extract
    image: etl:1.4
    command: ["sh", "-c", "run"]
`

	hashFor := func(y string) string {
		def, err := Parse([]byte(y))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		step := &def.Steps[0]
		spec, err := def.RuntimeSpecForStep(step)
		if err != nil {
			t.Fatalf("runtime spec: %v", err)
		}
		h := cache.HashInput{
			JobAlias:             def.Metadata.Alias,
			TaskName:             step.Name,
			Image:                step.Image,
			Command:              step.Command,
			Env:                  spec.Env,
			WorkDir:              spec.WorkDir,
			Mounts:               spec.Mounts,
			ResolvedVolumeMounts: spec.ResolvedVolumeMounts,
			Kubernetes:           spec.Kubernetes,
		}
		return h.Compute()
	}

	if got, want := hashFor(withRemediation), hashFor(base); got != want {
		t.Fatalf("remediation changed the cache hash: with=%s without=%s", got, want)
	}
}
