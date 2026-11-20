package database

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestQueryStageVocabulary(t *testing.T) {
	secret := errors.New("driver: password=secret dsn=postgres://user:secret@db/app SELECT secret FROM users")
	if got := QueryStage(nil); got != "" {
		t.Fatalf("nil stage = %q", got)
	}
	if got := QueryStage(secret); got != "" {
		t.Fatalf("unstaged error stage = %q", got)
	}

	for _, stage := range []string{
		queryStageConnection,
		queryStageExecute,
		queryStageColumns,
		queryStageScan,
		queryStageCleanup,
	} {
		err := withQueryStage(stage, secret)
		if got := QueryStage(err); got != stage {
			t.Fatalf("stage %s = %q", stage, got)
		}
		if got := QueryStage(fmt.Errorf("inspect: %w", err)); got != stage {
			t.Fatalf("wrapped stage %s = %q", stage, got)
		}
		if !errors.Is(err, secret) {
			t.Fatal("staged error lost the driver error")
		}
		if err.Error() != secret.Error() {
			t.Fatalf("staged error text = %q", err.Error())
		}
	}

	if got := QueryStage(withQueryStage("rows", secret)); got != "" {
		t.Fatalf("unknown stage = %q", got)
	}
	if got := withQueryStage(queryStageScan, nil); got != nil {
		t.Fatalf("nil wrap = %v", got)
	}

	first := withQueryStage(queryStageExecute, secret)
	if got := QueryStage(withQueryStage(queryStageCleanup, first)); got != queryStageExecute {
		t.Fatalf("rewrapped stage = %q", got)
	}
}

func TestFailedQueryMessageKeepsDriverTextOffTheBody(t *testing.T) {
	secret := errors.New("driver: password=secret SELECT token FROM credentials")
	if got := FailedQueryMessage(secret); got != "failed to execute query" {
		t.Fatalf("unstaged message = %q", got)
	}
	if strings.Contains(FailedQueryMessage(secret), "secret") || strings.Contains(FailedQueryMessage(secret), "SELECT") {
		t.Fatal("unstaged message copied driver text")
	}

	for _, stage := range []string{
		queryStageConnection,
		queryStageExecute,
		queryStageColumns,
		queryStageScan,
		queryStageCleanup,
	} {
		got := FailedQueryMessage(withQueryStage(stage, secret))
		want := "failed to execute query (" + stage + ")"
		if got != want {
			t.Fatalf("message = %q, want %q", got, want)
		}
		if strings.Contains(got, "secret") || strings.Contains(got, "SELECT") || strings.Contains(got, "password") {
			t.Fatalf("message copied driver text: %q", got)
		}
	}

	if got := FailedQueryMessage(withQueryStage("rows", secret)); got != "failed to execute query" {
		t.Fatalf("unknown stage message = %q", got)
	}
}
