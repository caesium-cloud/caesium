package freshness

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
)

func tPtr(t time.Time) *time.Time { return &t }

// t0 is a fixed base time so the ordering assertions are deterministic.
var t0 = time.Date(2026, 7, 3, 4, 0, 0, 0, time.UTC)

// TestApplyContract is the watermark contract table — the heart of B2. It
// exercises applyContract directly (pure w.r.t. the DB) so every branch of the
// advance/verify decision is covered without a database.
func TestApplyContract(t *testing.T) {
	run := uuid.New()

	cases := []struct {
		name     string
		state    models.DatasetState // starting state
		in       AdvanceInput
		want     Outcome
		wantWM   string     // expected watermark after
		advanced *time.Time // expected advanced_at (nil = unchanged/absent)
		verified *time.Time // expected verified_at (nil = unchanged/absent)
	}{
		{
			name:     "first numeric value always advances",
			state:    models.DatasetState{},
			in:       AdvanceInput{Name: "d", Watermark: "100", RunID: run, CompletedAt: t0},
			want:     OutcomeAdvanced,
			wantWM:   "100",
			advanced: tPtr(t0),
		},
		{
			name:     "numeric increase advances",
			state:    models.DatasetState{Watermark: "100", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "200", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeAdvanced,
			wantWM:   "200",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "numeric regression recorded, never advances",
			state:    models.DatasetState{Watermark: "200", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "100", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeRegressionDropped,
			wantWM:   "200",
			advanced: tPtr(t0), // untouched
		},
		{
			name:     "unchanged numeric value verifies, not advances",
			state:    models.DatasetState{Watermark: "200", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "200", RunID: run, CompletedAt: t0.Add(2 * time.Hour)},
			want:     OutcomeVerified,
			wantWM:   "200",
			advanced: tPtr(t0), // untouched
			verified: tPtr(t0.Add(2 * time.Hour)),
		},
		{
			name:     "RFC3339 increase advances",
			state:    models.DatasetState{Watermark: "2026-07-03T04:00:00Z", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "2026-07-03T05:00:00Z", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeAdvanced,
			wantWM:   "2026-07-03T05:00:00Z",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "RFC3339 regression recorded, never advances",
			state:    models.DatasetState{Watermark: "2026-07-03T05:00:00Z", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "2026-07-03T04:00:00Z", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeRegressionDropped,
			wantWM:   "2026-07-03T05:00:00Z",
			advanced: tPtr(t0),
		},
		{
			name:     "opaque SHA from a newer run advances",
			state:    models.DatasetState{Watermark: "abc123", AdvancedAt: tPtr(t0), WatermarkRunAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "def456", RunID: run, RunOrder: t0.Add(time.Hour), CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeAdvanced,
			wantWM:   "def456",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "opaque SHA from an older run dropped out-of-order",
			state:    models.DatasetState{Watermark: "def456", AdvancedAt: tPtr(t0.Add(time.Hour)), WatermarkRunAt: tPtr(t0.Add(time.Hour))},
			in:       AdvanceInput{Name: "d", Watermark: "abc123", RunID: run, RunOrder: t0, CompletedAt: t0},
			want:     OutcomeOutOfOrderDropped,
			wantWM:   "def456",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "unchanged opaque SHA verifies",
			state:    models.DatasetState{Watermark: "abc123", AdvancedAt: tPtr(t0), WatermarkRunAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "abc123", RunID: run, RunOrder: t0.Add(time.Hour), CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeVerified,
			wantWM:   "abc123",
			advanced: tPtr(t0),
			verified: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "degraded mode (no watermark key) refreshes verified_at",
			state:    models.DatasetState{Watermark: "200", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "", RunID: run, CompletedAt: t0.Add(3 * time.Hour)},
			want:     OutcomeVerified,
			wantWM:   "200",
			advanced: tPtr(t0),
			verified: tPtr(t0.Add(3 * time.Hour)),
		},
		{
			name:     "backfill run never advances even on a higher value",
			state:    models.DatasetState{Watermark: "100", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "999", RunID: run, CompletedAt: t0.Add(time.Hour), Backfill: true},
			want:     OutcomeBackfillDropped,
			wantWM:   "100",
			advanced: tPtr(t0),
		},
		{
			name:     "opaque replacing an orderable value gates by run order",
			state:    models.DatasetState{Watermark: "100", AdvancedAt: tPtr(t0), WatermarkRunAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "sha-xyz", RunID: run, RunOrder: t0.Add(time.Hour), CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeAdvanced,
			wantWM:   "sha-xyz",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			// 9007199254740992 = 2^53 (float64's last exactly-representable int);
			// 2^53+1 rounds to 2^53 as a float64, so a float compare would tie.
			// int64 parsing keeps them distinct -> a real advance.
			name:     "large-int increase advances beyond float64 exact range",
			state:    models.DatasetState{Watermark: "9007199254740992", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "9007199254740993", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeAdvanced,
			wantWM:   "9007199254740993",
			advanced: tPtr(t0.Add(time.Hour)),
		},
		{
			name:     "large-int regression dropped where float64 would tie",
			state:    models.DatasetState{Watermark: "9007199254740993", AdvancedAt: tPtr(t0)},
			in:       AdvanceInput{Name: "d", Watermark: "9007199254740992", RunID: run, CompletedAt: t0.Add(time.Hour)},
			want:     OutcomeRegressionDropped,
			wantWM:   "9007199254740993",
			advanced: tPtr(t0),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := tc.state
			state.Name = tc.in.Name
			// Normalize RunOrder fallback the way Advance does.
			if tc.in.RunOrder.IsZero() {
				tc.in.RunOrder = tc.in.CompletedAt
			}
			got := applyContract(&state, tc.in)
			if got.Outcome != tc.want {
				t.Fatalf("outcome = %q, want %q", got.Outcome, tc.want)
			}
			if state.Watermark != tc.wantWM {
				t.Fatalf("watermark = %q, want %q", state.Watermark, tc.wantWM)
			}
			if !eqTime(state.AdvancedAt, tc.advanced) {
				t.Fatalf("advanced_at = %v, want %v", state.AdvancedAt, tc.advanced)
			}
			if !eqTime(state.VerifiedAt, tc.verified) {
				t.Fatalf("verified_at = %v, want %v", state.VerifiedAt, tc.verified)
			}
		})
	}
}

func eqTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// TestFreshAt proves freshness is measured against max(advanced_at, verified_at).
func TestFreshAt(t *testing.T) {
	adv := t0
	ver := t0.Add(time.Hour)
	at, seen := FreshAt(models.DatasetState{AdvancedAt: &adv, VerifiedAt: &ver})
	if !seen || !at.Equal(ver) {
		t.Fatalf("FreshAt = (%v, %v), want (%v, true)", at, seen, ver)
	}
	if _, seen := FreshAt(models.DatasetState{}); seen {
		t.Fatalf("FreshAt on unobserved state should report unseen")
	}
	// verified_at older than advanced_at: advanced wins.
	at, _ = FreshAt(models.DatasetState{AdvancedAt: &ver, VerifiedAt: &adv})
	if !at.Equal(ver) {
		t.Fatalf("FreshAt max = %v, want %v", at, ver)
	}
}

// TestAdvancePersistsAndVerifies drives Advance through the real store + SQLite
// so the transaction, find-or-create, and monotonic guard are exercised end to
// end (not just the pure contract).
func TestAdvancePersistsAndVerifies(t *testing.T) {
	db := openRegistryDB(t)
	s := NewStore(db)
	ctx := context.Background()
	run1, run2 := uuid.New(), uuid.New()

	// First advance creates the row.
	r, err := s.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "100", RunID: run1, CompletedAt: t0})
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r.Outcome != OutcomeAdvanced {
		t.Fatalf("outcome 1 = %q, want advanced", r.Outcome)
	}

	// Unchanged value on a later run verifies, does not advance.
	r, err = s.Advance(ctx, AdvanceInput{Name: "staging.orders", Watermark: "100", RunID: run2, CompletedAt: t0.Add(time.Hour)})
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if r.Outcome != OutcomeVerified {
		t.Fatalf("outcome 2 = %q, want verified", r.Outcome)
	}

	got, ok, err := s.Get(ctx, nil, "staging.orders")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	if got.Watermark != "100" {
		t.Fatalf("persisted watermark = %q, want 100", got.Watermark)
	}
	if got.AdvancedAt == nil || !got.AdvancedAt.Equal(t0) {
		t.Fatalf("advanced_at = %v, want %v", got.AdvancedAt, t0)
	}
	if got.VerifiedAt == nil || !got.VerifiedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("verified_at = %v, want %v", got.VerifiedAt, t0.Add(time.Hour))
	}
	if got.LastRunID == nil || *got.LastRunID != run2 {
		t.Fatalf("last_run_id = %v, want %v", got.LastRunID, run2)
	}

	// Only one row per dataset (natural key enforced by the unique index).
	var count int64
	db.Model(&models.DatasetState{}).Where("name = ?", "staging.orders").Count(&count)
	if count != 1 {
		t.Fatalf("row count = %d, want 1", count)
	}
}

// TestAdvanceConcurrentSingleRow proves two concurrent Advance calls for the
// same previously-unknown dataset collapse into exactly one row (the ON CONFLICT
// upsert on the natural-key unique index), never a duplicate.
func TestAdvanceConcurrentSingleRow(t *testing.T) {
	db := openRegistryDB(t)
	s := NewStore(db)
	ctx := context.Background()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct increasing numeric watermarks so every call is a valid
			// advance; the winner is whichever commits last.
			_, errs[i] = s.Advance(ctx, AdvanceInput{
				Name:        "hot.dataset",
				Watermark:   strconv.Itoa(100 + i),
				RunID:       uuid.New(),
				CompletedAt: t0.Add(time.Duration(i) * time.Minute),
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent advance %d failed: %v", i, err)
		}
	}

	var count int64
	db.Model(&models.DatasetState{}).Where("name = ?", "hot.dataset").Count(&count)
	if count != 1 {
		t.Fatalf("concurrent advances produced %d rows, want exactly 1", count)
	}
}

// TestRecordConsumed snapshots consumed watermarks onto the produced state row.
func TestRecordConsumed(t *testing.T) {
	db := openRegistryDB(t)
	s := NewStore(db)
	ctx := context.Background()

	if _, err := s.Advance(ctx, AdvanceInput{Name: "analytics.orders_daily", Watermark: "5", RunID: uuid.New(), CompletedAt: t0}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	consumed := map[string]string{"staging.orders": "100", "raw.vendor_x": "key-9"}
	if err := s.RecordConsumed(ctx, nil, "analytics.orders_daily", consumed); err != nil {
		t.Fatalf("record consumed: %v", err)
	}

	got, ok, err := s.Get(ctx, nil, "analytics.orders_daily")
	if err != nil || !ok {
		t.Fatalf("get: %v ok=%v", err, ok)
	}
	var round map[string]string
	if err := json.Unmarshal(got.ConsumedWatermarks, &round); err != nil {
		t.Fatalf("unmarshal consumed: %v", err)
	}
	if round["staging.orders"] != "100" || round["raw.vendor_x"] != "key-9" {
		t.Fatalf("consumed snapshot = %v, want %v", round, consumed)
	}
	// Advancing must not have been clobbered by the consumed snapshot.
	if got.Watermark != "5" {
		t.Fatalf("watermark after RecordConsumed = %q, want 5", got.Watermark)
	}
}
