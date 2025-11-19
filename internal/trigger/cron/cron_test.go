package cron

import (
	"testing"
	"time"
)

func TestExtractExpressionPrefersExpression(t *testing.T) {
	cfg := map[string]interface{}{
		"expression": "0 0 * * *",
		"cron":       "ignored",
	}

	expr, err := extractExpression(cfg)
	if err != nil {
		t.Fatalf("extractExpression returned error: %v", err)
	}

	if expr != "0 0 * * *" {
		t.Fatalf("expression = %q, want %q", expr, "0 0 * * *")
	}
}

func TestExtractExpressionFallsBack(t *testing.T) {
	cfg := map[string]interface{}{
		"cron": "*/5 * * * *",
	}

	expr, err := extractExpression(cfg)
	if err != nil {
		t.Fatalf("extractExpression returned error: %v", err)
	}

	if expr != "*/5 * * * *" {
		t.Fatalf("expression = %q, want %q", expr, "*/5 * * * *")
	}
}

func TestExtractExpressionError(t *testing.T) {
	if _, err := extractExpression(map[string]interface{}{}); err == nil {
		t.Fatal("expected error when expression is missing")
	}
}

func TestExtractLocationParsesTimezone(t *testing.T) {
	loc, err := extractLocation(map[string]interface{}{"timezone": "UTC"})
	if err != nil {
		t.Fatalf("extractLocation returned error: %v", err)
	}

	if loc != time.UTC {
		t.Fatalf("expected UTC, got %v", loc)
	}
}

func TestExtractLocationIgnoresEmpty(t *testing.T) {
	loc, err := extractLocation(map[string]interface{}{"timezone": ""})
	if err != nil {
		t.Fatalf("extractLocation returned error: %v", err)
	}

	if loc != nil {
		t.Fatalf("expected nil location, got %v", loc)
	}
}
