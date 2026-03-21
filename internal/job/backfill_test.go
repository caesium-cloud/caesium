package job

import (
	"testing"
	"time"

	"github.com/robfig/cron"
)

func mustParseCron(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	s, err := parser.Parse(expr)
	if err != nil {
		t.Fatalf("mustParseCron(%q): %v", expr, err)
	}
	return s
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustParseTime(%q): %v", s, err)
	}
	return ts
}

// TestEnumerateLogicalDates_HourlyInOneHourWindow expects exactly 1 fire time.
func TestEnumerateLogicalDates_HourlyInOneHourWindow(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *") // every hour on the hour
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T01:00:00Z") // exclusive

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 1 {
		t.Fatalf("expected 1 date, got %d: %v", len(dates), dates)
	}
	want := mustParseTime(t, "2024-01-01T00:00:00Z")
	if !dates[0].Equal(want) {
		t.Errorf("dates[0] = %v, want %v", dates[0], want)
	}
}

// TestEnumerateLogicalDates_HourlyInTwelveHourWindow expects 12 fire times.
func TestEnumerateLogicalDates_HourlyInTwelveHourWindow(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T12:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 12 {
		t.Fatalf("expected 12 dates, got %d", len(dates))
	}
}

// TestEnumerateLogicalDates_EndBeforeStart returns empty.
func TestEnumerateLogicalDates_EndBeforeStart(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T12:00:00Z")
	end := mustParseTime(t, "2024-01-01T00:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 0 {
		t.Fatalf("expected 0 dates, got %d", len(dates))
	}
}

// TestEnumerateLogicalDates_FiveMinuteSchedule expects 12 fire times in 1 hour.
func TestEnumerateLogicalDates_FiveMinuteSchedule(t *testing.T) {
	sched := mustParseCron(t, "*/5 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	end := mustParseTime(t, "2024-01-01T01:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 12 {
		t.Fatalf("expected 12 dates, got %d: %v", len(dates), dates)
	}
}

// TestEnumerateLogicalDates_ExactBoundary verifies that end is exclusive.
func TestEnumerateLogicalDates_ExactBoundary(t *testing.T) {
	sched := mustParseCron(t, "0 * * * *")
	start := mustParseTime(t, "2024-01-01T00:00:00Z")
	// end is exactly on a fire time — should NOT be included
	end := mustParseTime(t, "2024-01-01T02:00:00Z")

	dates := EnumerateLogicalDates(sched, start, end, time.UTC)
	if len(dates) != 2 {
		t.Fatalf("expected 2 dates (00:00 and 01:00), got %d: %v", len(dates), dates)
	}
}
