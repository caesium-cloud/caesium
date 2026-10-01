//go:build !integration

package robustness

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

func mustWorkload(t *testing.T, profile string) SoakWorkload {
	t.Helper()
	w, err := LoadSoakWorkload(profile)
	if err != nil {
		t.Fatalf("load %s: %v", profile, err)
	}
	return w
}

func TestSoakWorkloadsLoadAndValidate(t *testing.T) {
	for _, p := range []string{"short", "nightly"} {
		w := mustWorkload(t, p)
		if w.Drain.Samples < 2 {
			t.Fatalf("%s: stabilization needs >= 2 post-drain samples", p)
		}
	}
	if _, err := LoadSoakWorkload("weekly"); err == nil {
		t.Fatal("an unknown profile must be refused")
	}
	short := mustWorkload(t, "short")
	if short.MaxEpisodes != len(SoakFamilies) {
		t.Fatalf("the short profile is exactly the mandatory pass, got max_episodes=%d", short.MaxEpisodes)
	}
	nightly := mustWorkload(t, "nightly")
	if nightly.ScheduleBudget.Duration < 30*time.Minute || nightly.ScheduleBudget.Duration > time.Hour {
		t.Fatalf("nightly budget %s outside 30-60 min", nightly.ScheduleBudget)
	}
}

func TestSoakWorkloadValidateRefusesVacuousFamilies(t *testing.T) {
	w := mustWorkload(t, "short")
	w.Failover.MandatoryKinds = []string{KillOwnerIsLeader}
	if err := w.Validate(); err == nil || !strings.Contains(err.Error(), "repeat") {
		t.Fatalf("a single kill is not repeated failover: %v", err)
	}
	w = mustWorkload(t, "short")
	w.Failover.MandatoryKinds = []string{KillOwnerIsLeader, "power_loss"}
	if err := w.Validate(); err == nil {
		t.Fatal("an unknown kill kind must be refused")
	}
	w = mustWorkload(t, "short")
	w.Drain.Samples = 1
	if err := w.Validate(); err == nil {
		t.Fatal("one post-drain sample cannot show stabilization")
	}
	w = mustWorkload(t, "short")
	w.MaxEpisodes = len(SoakFamilies) - 1
	if err := w.Validate(); err == nil {
		t.Fatal("max_episodes below the mandatory pass must be refused")
	}
}

func TestPlanSoakScheduleIsSeedDeterministic(t *testing.T) {
	w := mustWorkload(t, "nightly")
	a := PlanSoakSchedule(424242, w)
	b := PlanSoakSchedule(424242, w)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("the same seed produced two different plans")
	}
	c := PlanSoakSchedule(424243, w)
	if reflect.DeepEqual(a, c) {
		t.Fatal("different seeds produced identical plans")
	}
	if len(a) != w.MaxEpisodes {
		t.Fatalf("planned %d episodes, want %d", len(a), w.MaxEpisodes)
	}
}

func TestPlanSoakScheduleMandatoryPassCoversEveryFamily(t *testing.T) {
	for _, profile := range []string{"short", "nightly"} {
		w := mustWorkload(t, profile)
		for seed := int64(1); seed <= 200; seed++ {
			plan := PlanSoakSchedule(seed, w)
			seen := map[string]bool{}
			for _, ep := range plan[:len(SoakFamilies)] {
				if !ep.Mandatory {
					t.Fatalf("seed %d: episode %d in the first pass is not mandatory", seed, ep.Index)
				}
				seen[ep.Family] = true
				if len(ep.Draws) != soakDrawsPerEpisode {
					t.Fatalf("seed %d: episode %d has %d draws", seed, ep.Index, len(ep.Draws))
				}
				if ep.Family == FamilyRepeatedFailover {
					kinds := map[string]bool{}
					for _, k := range ep.Kinds {
						kinds[k] = true
					}
					if !kinds[KillOwnerIsLeader] || !kinds[KillOwnerIsNotLeader] || len(ep.Kinds) < 2 {
						t.Fatalf("seed %d: the mandatory failover episode must repeat owner-is-leader and owner-is-not-leader, got %v", seed, ep.Kinds)
					}
				}
			}
			if len(seen) != len(SoakFamilies) {
				t.Fatalf("seed %d %s: mandatory pass covers %v", seed, profile, seen)
			}
			for _, ep := range plan[len(SoakFamilies):] {
				if ep.Mandatory {
					t.Fatalf("seed %d: episode %d after the first pass is marked mandatory", seed, ep.Index)
				}
			}
		}
	}
}

func TestPlanSoakScheduleParamsInsideWorkloadRanges(t *testing.T) {
	w := mustWorkload(t, "nightly")
	in := func(v int, r IntRange) bool { return v >= r.Min && v <= r.Max }
	for seed := int64(0); seed < 50; seed++ {
		for _, ep := range PlanSoakSchedule(seed, w) {
			p := ep.Params
			ok := true
			switch ep.Family {
			case FamilySlowConsumers:
				ok = in(p["consumers"], w.SlowConsumers.Consumers) && in(p["read_delay_ms"], w.SlowConsumers.ReadDelayMS) &&
					in(p["burst_runs"], w.SlowConsumers.BurstRuns) && in(p["hold_seconds"], w.SlowConsumers.HoldSeconds)
			case FamilyQueueOverload:
				ok = in(p["low"], w.QueueOverload.Low) && in(p["high"], w.QueueOverload.High)
			case FamilyRetention:
				ok = in(p["burst_runs"], w.Retention.BurstRuns) && in(p["long_steps"], w.Retention.LongSteps)
			case FamilyRepeatedFailover:
				ok = p["kills"] == len(ep.Kinds) && len(ep.Kinds) >= 2 && len(ep.Kinds) <= 2+w.Failover.ExtraKills.Max
			case FamilyNodeReplacement:
				ok = in(p["replacements"], w.Replacement.Replacements)
			}
			if !ok {
				t.Fatalf("seed %d episode %d (%s) params %v outside the workload", seed, ep.Index, ep.Family, p)
			}
		}
	}
}

func TestSoakEpisodeKeyAndDraw(t *testing.T) {
	ep := SoakEpisode{Index: 3, Family: FamilyRetention, Draws: []uint32{7, 8}}
	if got := ep.Key(); got != "episode-03-retention" {
		t.Fatalf("key %q", got)
	}
	if ep.Draw(0, 3) != 1 || ep.Draw(1, 3) != 2 || ep.Draw(2, 3) != 1 {
		t.Fatalf("draw mapping changed: %d %d %d", ep.Draw(0, 3), ep.Draw(1, 3), ep.Draw(2, 3))
	}
	if (SoakEpisode{}).Draw(0, 3) != 0 {
		t.Fatal("an episode without draws must map to the first candidate")
	}
	if DerivedSeed(1, "trickle") == DerivedSeed(1, "other") || DerivedSeed(1, "trickle") != DerivedSeed(1, "trickle") {
		t.Fatal("derived streams must be distinct and stable")
	}
}

func TestExpectQueueDropsOldestThenPriorityFIFO(t *testing.T) {
	subs := []QueueSubmission{
		{1, "low"}, {2, "low"}, {3, "low"}, {4, "low"},
		{5, "high"}, {6, "high"},
	}
	got := ExpectQueue(subs, 4)
	if !reflect.DeepEqual(got.Dropped, []int{1, 2}) {
		t.Fatalf("dropped %v, want the two oldest", got.Dropped)
	}
	if !reflect.DeepEqual(got.StartOrder, []int{5, 6, 3, 4}) {
		t.Fatalf("start order %v, want highs FIFO then lows FIFO", got.StartOrder)
	}
	got = ExpectQueue(subs, 100)
	if len(got.Dropped) != 0 || !reflect.DeepEqual(got.StartOrder, []int{5, 6, 1, 2, 3, 4}) {
		t.Fatalf("under the bound nothing drops: %+v", got)
	}
	// The drop is by age, not priority: a high submitted first is dropped
	// before a later low.
	got = ExpectQueue([]QueueSubmission{{1, "high"}, {2, "low"}, {3, "low"}}, 2)
	if !reflect.DeepEqual(got.Dropped, []int{1}) || !reflect.DeepEqual(got.StartOrder, []int{2, 3}) {
		t.Fatalf("drop-oldest ignores priority: %+v", got)
	}
}

func ev(kind, run, step string, at time.Time) recorder.Event {
	return recorder.Event{Kind: kind, RunID: run, Step: step, At: at, Nonce: run + step}
}

func TestRunIntervalsAndOverlap(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	events := []recorder.Event{
		ev("start", "a", "hold", t0), ev("complete", "a", "hold", t0.Add(2*time.Second)),
		ev("start", "b", "hold", t0.Add(3*time.Second)), ev("complete", "b", "hold", t0.Add(5*time.Second)),
		// a duplicate attempt of b is legal and only widens b's window
		ev("start", "b", "hold", t0.Add(4*time.Second)), ev("complete", "b", "hold", t0.Add(6*time.Second)),
		ev("start", "c", "hold", t0.Add(7*time.Second)),
	}
	ivs, missing := RunIntervals(events, []string{"a", "b", "c"})
	if !reflect.DeepEqual(missing, []string{"c"}) {
		t.Fatalf("missing %v, want c (no completion)", missing)
	}
	if len(ivs) != 2 || OverlappingRuns(ivs) != nil {
		t.Fatalf("sequential runs must not overlap: %+v %v", ivs, OverlappingRuns(ivs))
	}
	events = append(events, ev("start", "d", "hold", t0.Add(5500*time.Millisecond)), ev("complete", "d", "hold", t0.Add(8*time.Second)))
	ivs, _ = RunIntervals(events, []string{"a", "b", "d"})
	if got := OverlappingRuns(ivs); len(got) != 1 || !strings.Contains(got[0], "b [") || !strings.Contains(got[0], "d [") {
		t.Fatalf("b and d overlap: %v", got)
	}
}

func TestCheckStepOrder(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	ok := []recorder.Event{
		ev("start", "r", "first", t0), ev("complete", "r", "first", t0.Add(time.Second)),
		ev("start", "r", "second", t0.Add(2*time.Second)), ev("complete", "r", "second", t0.Add(3*time.Second)),
		// duplicate first attempt completing later is legal
		ev("start", "r", "first", t0.Add(1500*time.Millisecond)), ev("complete", "r", "first", t0.Add(4*time.Second)),
	}
	if err := CheckStepOrder(ok, "r", "first", "second"); err != nil {
		t.Fatalf("legal history rejected: %v", err)
	}
	early := append([]recorder.Event(nil), ok...)
	early = append(early, ev("start", "r", "second", t0.Add(500*time.Millisecond)))
	if err := CheckStepOrder(early, "r", "first", "second"); err == nil {
		t.Fatal("a successor starting before its predecessor completed must fail")
	}
	if err := CheckStepOrder(ok[:3], "r", "first", "second"); err == nil {
		t.Fatal("a successor with no external completion must fail")
	}
	if err := CheckStepOrder(nil, "r", "first", "second"); err == nil {
		t.Fatal("an empty ledger is not an ordered history")
	}
}

func TestCheckpointRetentionOracle(t *testing.T) {
	t0 := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	obs := []CheckpointObservation{
		{At: t0, Sequences: []int64{2}},
		{At: t0.Add(time.Second), Sequences: []int64{2, 4, 6}},
		{At: t0.Add(2 * time.Second), Sequences: []int64{2, 4, 6, 8}},
		{At: t0.Add(3 * time.Second), Sequences: []int64{6, 8, 10}},
	}
	sum := SummariseCheckpoints(obs)
	if !reflect.DeepEqual(sum.Pruned, []int64{2, 4}) || !reflect.DeepEqual(sum.Final, []int64{6, 8, 10}) {
		t.Fatalf("summary %+v", sum)
	}
	if v := sum.Violations(3); len(v) != 0 {
		t.Fatalf("a write-then-prune window of keep+1 is legal: %v", v)
	}
	bad := SummariseCheckpoints(append(obs, CheckpointObservation{At: t0.Add(4 * time.Second), Sequences: []int64{6, 8, 10, 12}}))
	if v := bad.Violations(3); len(v) == 0 {
		t.Fatal("four checkpoints at rest exceed keep_fulls=3")
	}
	wrong := SummariseCheckpoints([]CheckpointObservation{{Sequences: []int64{2, 4, 6}}, {Sequences: []int64{2, 4}}})
	if v := wrong.Violations(3); len(v) == 0 {
		t.Fatal("pruning the newest checkpoint while keeping older ones is a violation")
	}
	none := SummariseCheckpoints([]CheckpointObservation{{Sequences: []int64{2}}, {Sequences: []int64{2, 4}}})
	if len(none.Pruned) != 0 {
		t.Fatal("no prune happened, none may be reported")
	}
}

func TestIntRangeJSON(t *testing.T) {
	var r IntRange
	if err := json.Unmarshal([]byte(`[2, 5]`), &r); err != nil || r.Min != 2 || r.Max != 5 {
		t.Fatalf("decode: %+v %v", r, err)
	}
	if err := json.Unmarshal([]byte(`[2]`), &r); err == nil {
		t.Fatal("a one-element range must be refused")
	}
	raw, _ := json.Marshal(IntRange{1, 3})
	if string(raw) != "[1,3]" {
		t.Fatalf("encode %s", raw)
	}
}
