package job

import (
	"testing"

	"github.com/google/uuid"
)

func TestBuildParamEnvAlwaysIncludesRunIDAndAlias(t *testing.T) {
	runID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	env := buildParamEnv(runID, "my-job", nil)

	if got := env["CAESIUM_RUN_ID"]; got != runID.String() {
		t.Fatalf("CAESIUM_RUN_ID = %q, want %q", got, runID.String())
	}
	if got := env["CAESIUM_JOB_ALIAS"]; got != "my-job" {
		t.Fatalf("CAESIUM_JOB_ALIAS = %q, want %q", got, "my-job")
	}
}

func TestBuildParamEnvInjectsParams(t *testing.T) {
	runID := uuid.New()
	params := map[string]string{
		"date": "2026-03-10",
		"env":  "staging",
	}

	env := buildParamEnv(runID, "my-job", params)

	if got := env["CAESIUM_PARAM_DATE"]; got != "2026-03-10" {
		t.Fatalf("CAESIUM_PARAM_DATE = %q, want %q", got, "2026-03-10")
	}
	if got := env["CAESIUM_PARAM_ENV"]; got != "staging" {
		t.Fatalf("CAESIUM_PARAM_ENV = %q, want %q", got, "staging")
	}
}

func TestBuildParamEnvUppercasesKeys(t *testing.T) {
	runID := uuid.New()
	params := map[string]string{
		"myKey": "myValue",
	}

	env := buildParamEnv(runID, "job", params)

	if got, ok := env["CAESIUM_PARAM_MYKEY"]; !ok || got != "myValue" {
		t.Fatalf("CAESIUM_PARAM_MYKEY = %q (ok=%v), want %q", got, ok, "myValue")
	}
	// lowercase key should NOT be present
	if _, ok := env["CAESIUM_PARAM_myKey"]; ok {
		t.Fatal("expected no lowercase key in env, but found CAESIUM_PARAM_myKey")
	}
}

func TestBuildParamEnvEmptyParamsHasTwoEntries(t *testing.T) {
	runID := uuid.New()
	env := buildParamEnv(runID, "alias", map[string]string{})

	if len(env) != 2 {
		t.Fatalf("expected 2 entries (CAESIUM_RUN_ID, CAESIUM_JOB_ALIAS), got %d", len(env))
	}
}
