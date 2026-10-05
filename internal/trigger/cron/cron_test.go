package cron

import (
	"context"
	"errors"
	"strings"
	"testing"

	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	robcron "github.com/robfig/cron"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"time"
)

func TestScheduledRunParamsInjectsLogicalDateAndPreservesDefaults(t *testing.T) {
	c := &Cron{
		defaultParams: map[string]string{
			"region": "us-east-1",
		},
	}

	logicalDate := time.Date(2026, 3, 31, 6, 0, 0, 0, time.UTC)
	params := c.scheduledRunParams(logicalDate)

	if params["region"] != "us-east-1" {
		t.Fatalf("region = %q, want %q", params["region"], "us-east-1")
	}
	if params["logical_date"] != "2026-03-31T06:00:00Z" {
		t.Fatalf("logical_date = %q, want %q", params["logical_date"], "2026-03-31T06:00:00Z")
	}
}

func TestScheduledRunParamsOverridesUserLogicalDate(t *testing.T) {
	c := &Cron{
		defaultParams: map[string]string{
			"logical_date": "stale-value",
			"env":          "prod",
		},
	}

	logicalDate := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	params := c.scheduledRunParams(logicalDate)

	if params["env"] != "prod" {
		t.Fatalf("env = %q, want %q", params["env"], "prod")
	}
	if params["logical_date"] != "2026-04-01T00:00:00Z" {
		t.Fatalf("logical_date = %q, want %q", params["logical_date"], "2026-04-01T00:00:00Z")
	}
}

func TestExtractExpressionPrefersExpression(t *testing.T) {
	cfg := map[string]any{
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
	cfg := map[string]any{
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
	if _, err := extractExpression(map[string]any{}); err == nil {
		t.Fatal("expected error when expression is missing")
	}
}

func TestExtractLocationParsesTimezone(t *testing.T) {
	loc, err := extractLocation(map[string]any{"timezone": "UTC"})
	if err != nil {
		t.Fatalf("extractLocation returned error: %v", err)
	}

	if loc != time.UTC {
		t.Fatalf("expected UTC, got %v", loc)
	}
}

func TestExtractLocationIgnoresEmpty(t *testing.T) {
	loc, err := extractLocation(map[string]any{"timezone": ""})
	if err != nil {
		t.Fatalf("extractLocation returned error: %v", err)
	}

	if loc != nil {
		t.Fatalf("expected nil location, got %v", loc)
	}
}

func TestExtractDefaultParamsReturnsNilWhenAbsent(t *testing.T) {
	params, err := extractDefaultParams(map[string]any{})
	if err != nil {
		t.Fatalf("extractDefaultParams returned error: %v", err)
	}
	if params != nil {
		t.Fatalf("expected nil params, got %v", params)
	}
}

func TestExtractDefaultParamsReturnsNilWhenNilValue(t *testing.T) {
	params, err := extractDefaultParams(map[string]any{"defaultParams": nil})
	if err != nil {
		t.Fatalf("extractDefaultParams returned error: %v", err)
	}
	if params != nil {
		t.Fatalf("expected nil params, got %v", params)
	}
}

func TestExtractDefaultParamsParsesStringValues(t *testing.T) {
	cfg := map[string]any{
		"defaultParams": map[string]any{
			"date": "2026-03-10",
			"env":  "staging",
		},
	}

	params, err := extractDefaultParams(cfg)
	if err != nil {
		t.Fatalf("extractDefaultParams returned error: %v", err)
	}

	if params["date"] != "2026-03-10" {
		t.Fatalf("date = %q, want %q", params["date"], "2026-03-10")
	}
	if params["env"] != "staging" {
		t.Fatalf("env = %q, want %q", params["env"], "staging")
	}
}

func TestExtractDefaultParamsReturnsErrorForInvalidType(t *testing.T) {
	cfg := map[string]any{
		"defaultParams": "not-a-map",
	}
	if _, err := extractDefaultParams(cfg); err == nil {
		t.Fatal("expected error for non-map defaultParams")
	}
}

func TestListenKeepsRecurringAfterFireError(t *testing.T) {
	sched, err := robcron.NewParser(robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow).Parse("* * * * *")
	require.NoError(t, err)
	c := &Cron{schedule: sched}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	var waits, fires []time.Time
	c.listen(ctx, func() time.Time { return now }, func(_ context.Context, next time.Time) error { waits = append(waits, next); now = next; return nil }, func(_ context.Context, date time.Time) error {
		fires = append(fires, date)
		if len(fires) == 2 {
			cancel()
			return nil
		}
		return errors.New("first fire failed")
	})
	require.Equal(t, []time.Time{time.Date(2026, 10, 3, 12, 1, 0, 0, time.UTC), time.Date(2026, 10, 3, 12, 2, 0, 0, time.UTC)}, fires)
	require.Equal(t, fires, waits)
}

func TestListenCancellationDuringWaitDoesNotFire(t *testing.T) {
	sched, err := robcron.NewParser(robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow).Parse("* * * * *")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waits := 0
	(&Cron{schedule: sched}).listen(ctx, time.Now, func(ctx context.Context, _ time.Time) error { waits++; cancel(); return ctx.Err() }, func(context.Context, time.Time) error { t.Fatal("fired after cancellation"); return nil })
	require.Equal(t, 1, waits)
	// The real timer's wait is cancellable even for a far-future tick.
	require.ErrorIs(t, waitUntil(ctx, time.Now().Add(time.Hour)), context.Canceled)
}

func TestListenLogicalTicksSurviveClockDiscontinuities(t *testing.T) {
	anchor := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		clocks []time.Duration
		ticks  []time.Duration
	}{
		{"backward after failed fire", []time.Duration{0, 30 * time.Second, -time.Minute}, []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}},
		{"repeated wall clock", []time.Duration{0, 0, 0}, []time.Duration{time.Minute, 2 * time.Minute, 3 * time.Minute}},
		{"forward jump skips missed ticks", []time.Duration{0, 5*time.Minute + 25*time.Second, 6 * time.Minute}, []time.Duration{time.Minute, 6 * time.Minute, 7 * time.Minute}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := robcron.NewParser(robcron.Minute | robcron.Hour | robcron.Dom | robcron.Month | robcron.Dow).Parse("* * * * *")
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waits, fires []time.Time
			clockCalls := 0
			(&Cron{schedule: sched}).listen(ctx, func() time.Time {
				require.Less(t, clockCalls, len(tc.clocks), "listener failed to progress")
				clock := anchor.Add(tc.clocks[clockCalls])
				clockCalls++
				return clock
			}, func(_ context.Context, tick time.Time) error {
				waits = append(waits, tick)
				return nil
			}, func(_ context.Context, tick time.Time) error {
				fires = append(fires, tick)
				if len(fires) == len(tc.ticks) {
					cancel()
				}
				return errors.New("attempted fire failed")
			})
			want := make([]time.Time, len(tc.ticks))
			for i, offset := range tc.ticks {
				want[i] = anchor.Add(offset)
			}
			require.Equal(t, want, fires)
			require.Equal(t, want, waits)
			require.Equal(t, len(tc.clocks), clockCalls)
		})
	}
}

type noFutureSchedule struct{ entered chan struct{} }

func (s noFutureSchedule) Next(time.Time) time.Time { close(s.entered); return time.Time{} }

func TestListenNoFutureWaitsForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, done := make(chan struct{}), make(chan struct{})
	c := &Cron{schedule: noFutureSchedule{entered}}
	go func() {
		c.listen(ctx, time.Now, func(context.Context, time.Time) error { t.Error("wait called for absent tick"); return nil }, func(context.Context, time.Time) error { t.Error("fire called for absent tick"); return nil })
		close(done)
	}()
	<-entered
	select {
	case <-done:
		t.Fatal("listener returned before cancellation")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("listener did not cancel")
	}
}

func TestScheduleEntryPointsPreserveConfigurationSemantics(t *testing.T) {
	for _, tc := range []struct {
		name, config, location string
		wantErr                bool
	}{
		{"default", `{"expression":"0 0 * * *","defaultParams":{"region":"east","count":2},"catchup":true}`, "", false},
		{"blank timezone", `{"cron":"0 0 * * *","timezone":" "}`, "", false},
		{"timezone", `{"schedule":"0 0 * * *","timezone":"America/New_York"}`, "America/New_York", false},
		{"invalid expression", `{"expression":"not cron"}`, "", true},
		{"invalid timezone", `{"expression":"0 0 * * *","timezone":"invalid/location"}`, "", true},
		{"timezone type", `{"expression":"0 0 * * *","timezone":42}`, "", true},
		{"malformed JSON", `{"expression":`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, newErr := New(&models.Trigger{Type: models.TriggerTypeCron, Configuration: tc.config})
			sched, loc, parseErr := ParseSchedule(tc.config)
			if tc.wantErr {
				require.Error(t, newErr)
				require.Error(t, parseErr)
				if tc.name == "malformed JSON" {
					require.True(t, strings.HasPrefix(parseErr.Error(), "cron: invalid trigger configuration: "))
					require.False(t, strings.HasPrefix(newErr.Error(), "cron: invalid trigger configuration: "))
				} else {
					require.Equal(t, newErr.Error(), parseErr.Error())
				}
				return
			}
			require.NoError(t, newErr)
			require.NoError(t, parseErr)
			if tc.location == "" {
				require.Nil(t, c.location)
				require.Same(t, time.UTC, loc)
			} else {
				require.Equal(t, tc.location, c.location.String())
				require.Equal(t, tc.location, loc.String())
			}
			base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC).In(loc)
			require.Equal(t, c.schedule.Next(base), sched.Next(base))
			if tc.name == "default" {
				require.True(t, c.catchup)
				require.Equal(t, map[string]string{"region": "east", "count": "2"}, c.defaultParams)
			}
		})
	}
}

func TestFireAtConcurrentFailuresStayWithTheirJob(t *testing.T) {
	core, observed := observer.New(zapcore.ErrorLevel)
	logged := make(chan struct{}, 2)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "job run failure" {
			logged <- struct{}{}
		}
		return nil
	}))
	restore := zap.ReplaceGlobals(logger)
	defer restore()
	jobs := models.Jobs{{ID: uuid.New()}, {ID: uuid.New()}, {ID: uuid.New(), Paused: true}}
	failures := map[uuid.UUID]error{jobs[0].ID: errors.New("job zero failure"), jobs[1].ID: errors.New("job one failure")}
	started := make(chan uuid.UUID, 2)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	c := &Cron{id: uuid.New()}
	date := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	err := c.fireAtWith(context.Background(), date, func(req *jsvc.ListRequest) (models.Jobs, error) {
		require.Equal(t, c.id.String(), req.TriggerID)
		return jobs, nil
	}, func(j *models.Job, params map[string]string) error {
		started <- j.ID
		<-release
		if params["logical_date"] != date.Format(time.RFC3339) {
			return errors.New("wrong logical date")
		}
		return failures[j.ID]
	})
	require.NoError(t, err)
	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("jobs did not run concurrently")
		}
	}
	close(release)
	for range 2 {
		select {
		case <-logged:
		case <-time.After(time.Second):
			t.Fatal("missing job failure log")
		}
	}
	require.Len(t, observed.All(), 2)
	for _, entry := range observed.All() {
		fields := entry.ContextMap()
		id, err := uuid.Parse(fields["id"].(string))
		require.NoError(t, err)
		require.Equal(t, failures[id].Error(), fields["error"])
	}
}
