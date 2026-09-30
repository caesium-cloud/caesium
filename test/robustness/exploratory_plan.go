package robustness

// F3 seeded single-host soak: the hermetic half.
//
// Everything here is a pure function of (seed, workload file) or of recorded
// observations, so `just unit-test` proves the schedule is reproducible from
// its seed and the oracles reject what they must, without a cluster. The live
// half (TestExploratory, integration-tagged) executes the plan against the
// owned three-member kind/Helm topology that scripts/soak-tests.sh provisions.
//
// A seed reproduces the PLAN: episode order, per-episode parameters and the
// draws that pick targets. It does not reproduce OS scheduling, so the host
// controller retains the ACTUAL fault schedule (what was injected, when,
// against which pod/node/container) next to the seed.

import (
	"embed"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/test/robustness/recorder"
)

//go:embed workloads/*.json
var soakWorkloadFS embed.FS

// Scenario families. The names are the manifest keys the controller expects a
// record for; a family with no record is blocked, never passed.
const (
	FamilySlowConsumers    = "slow_consumers"
	FamilyQueueOverload    = "queue_overload"
	FamilyRetention        = "retention"
	FamilyRepeatedFailover = "repeated_failover"
	FamilyNodeReplacement  = "node_replacement"
)

// SoakFamilies is the canonical family order. Plans permute it; they never
// drop a member.
var SoakFamilies = []string{
	FamilySlowConsumers,
	FamilyQueueOverload,
	FamilyRetention,
	FamilyRepeatedFailover,
	FamilyNodeReplacement,
}

// Failover kill kinds. Every one is a process kill (SIGKILL of the member's
// container with kubelet stopped); none is power-loss qualification, because
// the kind node's kernel and page cache survive.
const (
	KillOwnerIsLeader    = "owner_is_leader"
	KillOwnerIsNotLeader = "owner_is_not_leader"
	KillLeaderOnly       = "leader_only"
)

// IntRange is an inclusive [min, max] draw, written as a two-element array.
type IntRange struct {
	Min int
	Max int
}

func (r *IntRange) UnmarshalJSON(b []byte) error {
	var pair []int
	if err := json.Unmarshal(b, &pair); err != nil {
		return fmt.Errorf("range must be [min, max]: %w", err)
	}
	if len(pair) != 2 {
		return fmt.Errorf("range must have exactly two elements, got %d", len(pair))
	}
	r.Min, r.Max = pair[0], pair[1]
	return nil
}

func (r IntRange) MarshalJSON() ([]byte, error) { return json.Marshal([]int{r.Min, r.Max}) }

func (r IntRange) valid(min int) bool { return r.Min >= min && r.Max >= r.Min }

func (r IntRange) draw(rng *rand.Rand) int {
	if r.Max <= r.Min {
		return r.Min
	}
	return r.Min + rng.Intn(r.Max-r.Min+1)
}

// Duration is a time.Duration written as a Go duration string.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// SoakWorkload is one profile's workload mix (test/robustness/workloads/*.json).
type SoakWorkload struct {
	Profile     string `json:"profile"`
	Description string `json:"description"`
	// ScheduleBudget is the default CAESIUM_SOAK_DURATION. The mandatory first
	// pass (one episode of every family) always runs; further planned episodes
	// start only while the elapsed schedule time is inside the budget.
	ScheduleBudget Duration `json:"schedule_budget"`
	// MaxEpisodes caps the planned sequence, mandatory pass included.
	MaxEpisodes int `json:"max_episodes"`
	// HealProgressBound: after every heal a new run must dispatch and complete
	// inside this bound.
	HealProgressBound Duration `json:"heal_progress_bound"`
	// RunBound bounds a single fixture run's completion inside an episode.
	RunBound Duration `json:"run_bound"`

	Background struct {
		TrickleIntervalMS IntRange `json:"trickle_interval_ms"`
		HoldSeconds       int      `json:"hold_seconds"`
	} `json:"background"`

	SlowConsumers struct {
		Consumers   IntRange `json:"consumers"`
		ReadDelayMS IntRange `json:"read_delay_ms"`
		ChunkBytes  int      `json:"chunk_bytes"`
		BurstRuns   IntRange `json:"burst_runs"`
		HoldSeconds IntRange `json:"hold_seconds"`
		// GoroutineSlack is the stated tolerance for the server-side
		// goroutine count after the consumers disconnect.
		GoroutineSlack int      `json:"goroutine_slack"`
		SettleBound    Duration `json:"settle_bound"`
	} `json:"slow_consumers"`

	QueueOverload struct {
		Low         IntRange `json:"low"`
		High        IntRange `json:"high"`
		HoldSeconds IntRange `json:"hold_seconds"`
	} `json:"queue_overload"`

	Retention struct {
		BurstRuns        IntRange `json:"burst_runs"`
		LongSteps        IntRange `json:"long_steps"`
		LongStepSeconds  IntRange `json:"long_step_seconds"`
		KeepFulls        int      `json:"keep_fulls"`
		TriggerParallism int      `json:"trigger_parallelism"`
	} `json:"retention"`

	Failover struct {
		// MandatoryKinds run, in a seeded order, in the first failover
		// episode; extra kills draw from ExtraKinds.
		MandatoryKinds []string `json:"mandatory_kinds"`
		ExtraKinds     []string `json:"extra_kinds"`
		ExtraKills     IntRange `json:"extra_kills"`
		RecoveryBound  Duration `json:"recovery_bound"`
		RejoinBound    Duration `json:"rejoin_bound"`
	} `json:"repeated_failover"`

	Replacement struct {
		Replacements IntRange `json:"replacements"`
		RejoinBound  Duration `json:"rejoin_bound"`
	} `json:"node_replacement"`

	Drain struct {
		Bound          Duration `json:"bound"`
		Settle         Duration `json:"settle"`
		Samples        int      `json:"samples"`
		SampleInterval Duration `json:"sample_interval"`
		ContainerGrace Duration `json:"container_grace"`
	} `json:"drain"`
}

// LoadSoakWorkload reads and validates the committed workload for profile.
func LoadSoakWorkload(profile string) (SoakWorkload, error) {
	if !validProfile(profile) {
		return SoakWorkload{}, fmt.Errorf("unknown soak profile %q (want short or nightly)", profile)
	}
	raw, err := soakWorkloadFS.ReadFile("workloads/" + profile + ".json")
	if err != nil {
		return SoakWorkload{}, err
	}
	var w SoakWorkload
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return SoakWorkload{}, fmt.Errorf("workloads/%s.json: %w", profile, err)
	}
	if w.Profile != profile {
		return SoakWorkload{}, fmt.Errorf("workloads/%s.json declares profile %q", profile, w.Profile)
	}
	if err := w.Validate(); err != nil {
		return SoakWorkload{}, fmt.Errorf("workloads/%s.json: %w", profile, err)
	}
	return w, nil
}

func validProfile(p string) bool { return p == "short" || p == "nightly" }

func validKillKind(k string) bool {
	return k == KillOwnerIsLeader || k == KillOwnerIsNotLeader || k == KillLeaderOnly
}

// Validate refuses a workload that could make a family vacuous.
func (w SoakWorkload) Validate() error {
	var errs []string
	need := func(ok bool, what string) {
		if !ok {
			errs = append(errs, what)
		}
	}
	need(w.ScheduleBudget.Duration > 0, "schedule_budget must be positive")
	need(w.MaxEpisodes >= len(SoakFamilies), "max_episodes must cover the mandatory pass")
	need(w.HealProgressBound.Duration > 0, "heal_progress_bound must be positive")
	need(w.RunBound.Duration > 0, "run_bound must be positive")
	need(w.Background.TrickleIntervalMS.valid(250), "background.trickle_interval_ms must be >= 250ms")
	need(w.Background.HoldSeconds >= 0, "background.hold_seconds must be >= 0")
	sc := w.SlowConsumers
	need(sc.Consumers.valid(1), "slow_consumers.consumers must be >= 1")
	need(sc.ReadDelayMS.valid(1), "slow_consumers.read_delay_ms must be >= 1")
	need(sc.ChunkBytes > 0, "slow_consumers.chunk_bytes must be positive")
	need(sc.BurstRuns.valid(1), "slow_consumers.burst_runs must be >= 1")
	need(sc.HoldSeconds.valid(0), "slow_consumers.hold_seconds must be >= 0")
	need(sc.GoroutineSlack > 0, "slow_consumers.goroutine_slack must be positive")
	need(sc.SettleBound.Duration > 0, "slow_consumers.settle_bound must be positive")
	q := w.QueueOverload
	need(q.Low.valid(1) && q.High.valid(1), "queue_overload.low/high must be >= 1")
	need(q.HoldSeconds.valid(1), "queue_overload.hold_seconds must be >= 1")
	r := w.Retention
	need(r.BurstRuns.valid(1), "retention.burst_runs must be >= 1")
	need(r.LongSteps.valid(2), "retention.long_steps must be >= 2")
	need(r.LongStepSeconds.valid(1), "retention.long_step_seconds must be >= 1")
	need(r.KeepFulls >= 1, "retention.keep_fulls must be >= 1")
	need(r.TriggerParallism >= 1, "retention.trigger_parallelism must be >= 1")
	f := w.Failover
	need(len(f.MandatoryKinds) >= 2, "repeated_failover.mandatory_kinds must repeat (>= 2 kills)")
	for _, k := range append(append([]string{}, f.MandatoryKinds...), f.ExtraKinds...) {
		need(validKillKind(k), "unknown kill kind "+k)
	}
	need(f.ExtraKills.valid(0), "repeated_failover.extra_kills must be >= 0")
	need(f.ExtraKills.Max == 0 || len(f.ExtraKinds) > 0, "repeated_failover.extra_kinds is empty but extra_kills > 0")
	need(f.RecoveryBound.Duration > 0 && f.RejoinBound.Duration > 0, "repeated_failover bounds must be positive")
	need(w.Replacement.Replacements.valid(1), "node_replacement.replacements must be >= 1")
	need(w.Replacement.RejoinBound.Duration > 0, "node_replacement.rejoin_bound must be positive")
	d := w.Drain
	need(d.Bound.Duration > 0, "drain.bound must be positive")
	need(d.Settle.Duration >= 0, "drain.settle must be >= 0")
	need(d.Samples >= 2, "drain.samples must be >= 2 to judge stabilization")
	need(d.SampleInterval.Duration > 0, "drain.sample_interval must be positive")
	need(d.ContainerGrace.Duration > 0, "drain.container_grace must be positive")
	if len(errs) > 0 {
		return fmt.Errorf("invalid workload: %s", strings.Join(errs, "; "))
	}
	return nil
}

// SoakEpisode is one planned step of the schedule.
type SoakEpisode struct {
	Index     int            `json:"index"`
	Family    string         `json:"family"`
	Mandatory bool           `json:"mandatory"`
	Params    map[string]int `json:"params"`
	// Kinds is the ordered kill list of a failover episode.
	Kinds []string `json:"kinds,omitempty"`
	// Draws are pre-drawn uint32s the live runner maps onto whatever
	// candidate set exists when the episode runs (members, trigger targets).
	Draws []uint32 `json:"draws"`
}

// Key is the record key the controller's manifest expects.
func (e SoakEpisode) Key() string { return fmt.Sprintf("episode-%02d-%s", e.Index, e.Family) }

// Draw maps pre-drawn value i onto n candidates (i wraps).
func (e SoakEpisode) Draw(i, n int) int {
	if n <= 0 || len(e.Draws) == 0 {
		return 0
	}
	return int(e.Draws[i%len(e.Draws)] % uint32(n))
}

const soakDrawsPerEpisode = 16

// PlanSoakSchedule is the seed -> schedule function. The first
// len(SoakFamilies) episodes are a seeded permutation of every family (the
// mandatory pass, which is the whole short profile); the rest are uniform
// family draws up to MaxEpisodes. Parameters are drawn in a fixed order per
// family, so a given seed and workload file always yield the same plan.
func PlanSoakSchedule(seed int64, w SoakWorkload) []SoakEpisode {
	rng := rand.New(rand.NewSource(seed))
	order := rng.Perm(len(SoakFamilies))
	var plan []SoakEpisode
	failoverSeen := false
	for i := 0; i < w.MaxEpisodes; i++ {
		var family string
		mandatory := i < len(SoakFamilies)
		if mandatory {
			family = SoakFamilies[order[i]]
		} else {
			family = SoakFamilies[rng.Intn(len(SoakFamilies))]
		}
		ep := SoakEpisode{Index: i, Family: family, Mandatory: mandatory, Params: map[string]int{}}
		switch family {
		case FamilySlowConsumers:
			s := w.SlowConsumers
			ep.Params["consumers"] = s.Consumers.draw(rng)
			ep.Params["read_delay_ms"] = s.ReadDelayMS.draw(rng)
			ep.Params["burst_runs"] = s.BurstRuns.draw(rng)
			ep.Params["hold_seconds"] = s.HoldSeconds.draw(rng)
		case FamilyQueueOverload:
			q := w.QueueOverload
			ep.Params["low"] = q.Low.draw(rng)
			ep.Params["high"] = q.High.draw(rng)
			ep.Params["hold_seconds"] = q.HoldSeconds.draw(rng)
		case FamilyRetention:
			r := w.Retention
			ep.Params["burst_runs"] = r.BurstRuns.draw(rng)
			ep.Params["long_steps"] = r.LongSteps.draw(rng)
			ep.Params["long_step_seconds"] = r.LongStepSeconds.draw(rng)
		case FamilyRepeatedFailover:
			f := w.Failover
			if !failoverSeen {
				// The first failover episode is always the repeated pair
				// (owner-is-leader and owner-is-not-leader) in seeded order.
				perm := rng.Perm(len(f.MandatoryKinds))
				for _, p := range perm {
					ep.Kinds = append(ep.Kinds, f.MandatoryKinds[p])
				}
			} else {
				for range 2 {
					ep.Kinds = append(ep.Kinds, f.MandatoryKinds[rng.Intn(len(f.MandatoryKinds))])
				}
			}
			extra := f.ExtraKills.draw(rng)
			for range extra {
				ep.Kinds = append(ep.Kinds, f.ExtraKinds[rng.Intn(len(f.ExtraKinds))])
			}
			ep.Params["kills"] = len(ep.Kinds)
			failoverSeen = true
		case FamilyNodeReplacement:
			ep.Params["replacements"] = w.Replacement.Replacements.draw(rng)
		}
		for range soakDrawsPerEpisode {
			ep.Draws = append(ep.Draws, rng.Uint32())
		}
		plan = append(plan, ep)
	}
	return plan
}

// DerivedSeed gives an independent, reproducible stream (background trickle,
// per-episode member choice) without perturbing the plan's own sequence.
func DerivedSeed(seed int64, stream string) int64 {
	h := uint64(1469598103934665603)
	for _, b := range []byte(stream) {
		h ^= uint64(b)
		h *= 1099511628211
	}
	return seed ^ int64(h&0x7fffffffffffffff)
}

// ---------------------------------------------------------------------------
// Queue-overload oracle.
// ---------------------------------------------------------------------------

// QueueSubmission is one start admitted into a busy maxRuns=1 job's queue, in
// the order it was submitted (each submission waits for the previous 202).
type QueueSubmission struct {
	Seq      int    `json:"seq"`
	Priority string `json:"priority"` // high|low
}

// QueueExpectation is what the product's documented queue semantics predict:
// run_queue keeps at most maxDepth unclaimed rows per job and drops the OLDEST
// unclaimed rows on overflow (internal/run enqueueRunTx); the dequeuer starts
// rows by priority DESC, created_at ASC.
type QueueExpectation struct {
	Dropped    []int `json:"dropped"`
	StartOrder []int `json:"start_order"`
}

// ExpectQueue applies the drop-oldest bound, then the priority order, to
// submissions made while the job's only run slot is held.
func ExpectQueue(subs []QueueSubmission, maxDepth int) QueueExpectation {
	var exp QueueExpectation
	keep := subs
	if maxDepth > 0 && len(subs) > maxDepth {
		for _, s := range subs[:len(subs)-maxDepth] {
			exp.Dropped = append(exp.Dropped, s.Seq)
		}
		keep = subs[len(subs)-maxDepth:]
	}
	ordered := append([]QueueSubmission(nil), keep...)
	rank := func(p string) int {
		switch p {
		case "high":
			return 3
		case "low":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(ordered, func(i, j int) bool { return rank(ordered[i].Priority) > rank(ordered[j].Priority) })
	for _, s := range ordered {
		exp.StartOrder = append(exp.StartOrder, s.Seq)
	}
	return exp
}

// RunInterval is one run's externally observed execution window.
type RunInterval struct {
	RunID string    `json:"run_id"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// RunIntervals builds each run's [earliest start, latest complete] window
// from the raw effect ledger. Runs missing either edge are reported, not
// silently dropped.
func RunIntervals(events []recorder.Event, runIDs []string) ([]RunInterval, []string) {
	want := map[string]bool{}
	for _, id := range runIDs {
		want[id] = true
	}
	byRun := map[string]*RunInterval{}
	for _, ev := range events {
		if !want[ev.RunID] || ev.At.IsZero() {
			continue
		}
		iv := byRun[ev.RunID]
		if iv == nil {
			iv = &RunInterval{RunID: ev.RunID}
			byRun[ev.RunID] = iv
		}
		switch ev.Kind {
		case "start":
			if iv.Start.IsZero() || ev.At.Before(iv.Start) {
				iv.Start = ev.At
			}
		case "complete":
			if ev.At.After(iv.End) {
				iv.End = ev.At
			}
		}
	}
	var out []RunInterval
	var missing []string
	for _, id := range runIDs {
		iv := byRun[id]
		if iv == nil || iv.Start.IsZero() || iv.End.IsZero() {
			missing = append(missing, id)
			continue
		}
		out = append(out, *iv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, missing
}

// OverlappingRuns returns every pair of DISTINCT runs whose windows overlap.
// For a maxRuns=1 job each pair is a concurrency-limit violation; duplicate
// attempts of the same run are legal and never reach this check.
func OverlappingRuns(ivs []RunInterval) []string {
	sorted := append([]RunInterval(nil), ivs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start.Before(sorted[j].Start) })
	var out []string
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if !sorted[j].Start.Before(sorted[i].End) {
				break
			}
			out = append(out, fmt.Sprintf("%s [%s,%s] overlaps %s [%s,%s]",
				sorted[i].RunID, sorted[i].Start.Format(time.RFC3339Nano), sorted[i].End.Format(time.RFC3339Nano),
				sorted[j].RunID, sorted[j].Start.Format(time.RFC3339Nano), sorted[j].End.Format(time.RFC3339Nano)))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Two-step effect ordering (the trickle and burst fixtures).
// ---------------------------------------------------------------------------

// CheckStepOrder requires both steps to have left an external start and
// completion, and every start of `second` to follow the earliest completion of
// `first`. Duplicate attempts are legal; an unordered successor is not.
func CheckStepOrder(events []recorder.Event, runID, first, second string) error {
	var firstDone, secondStarts []time.Time
	firstStarts, secondDone := 0, 0
	for _, ev := range events {
		if ev.RunID != runID {
			continue
		}
		if ev.At.IsZero() {
			return fmt.Errorf("recorder %s event for %s has no timestamp", ev.Kind, ev.Step)
		}
		switch {
		case ev.Step == first && ev.Kind == "start":
			firstStarts++
		case ev.Step == first && ev.Kind == "complete":
			firstDone = append(firstDone, ev.At)
		case ev.Step == second && ev.Kind == "start":
			secondStarts = append(secondStarts, ev.At)
		case ev.Step == second && ev.Kind == "complete":
			secondDone++
		}
	}
	if firstStarts == 0 || len(firstDone) == 0 {
		return fmt.Errorf("run %s: step %s left starts=%d completions=%d", runID, first, firstStarts, len(firstDone))
	}
	if len(secondStarts) == 0 || secondDone == 0 {
		return fmt.Errorf("run %s: step %s left starts=%d completions=%d", runID, second, len(secondStarts), secondDone)
	}
	earliest := firstDone[0]
	for _, at := range firstDone[1:] {
		if at.Before(earliest) {
			earliest = at
		}
	}
	for _, at := range secondStarts {
		if at.Before(earliest) {
			return fmt.Errorf("run %s: %s started at %s before %s first completed at %s",
				runID, second, at.Format(time.RFC3339Nano), first, earliest.Format(time.RFC3339Nano))
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Checkpoint-retention oracle.
// ---------------------------------------------------------------------------

// CheckpointObservation is one poll of run_checkpoints for a run.
type CheckpointObservation struct {
	At        time.Time `json:"at"`
	Sequences []int64   `json:"sequences"`
}

// CheckpointRetention summarises the pruning path. A sequence observed and
// later absent while the run still had checkpoints is direct evidence that
// PruneCheckpoints ran; MaxObserved bounds the transient window between a
// write and its prune.
type CheckpointRetention struct {
	Polls       int     `json:"polls"`
	Written     []int64 `json:"written"`
	Pruned      []int64 `json:"pruned"`
	MaxObserved int     `json:"max_observed"`
	Final       []int64 `json:"final"`
}

func SummariseCheckpoints(obs []CheckpointObservation) CheckpointRetention {
	sum := CheckpointRetention{Polls: len(obs)}
	seen := map[int64]bool{}
	for _, o := range obs {
		if len(o.Sequences) > sum.MaxObserved {
			sum.MaxObserved = len(o.Sequences)
		}
		for _, s := range o.Sequences {
			if !seen[s] {
				seen[s] = true
				sum.Written = append(sum.Written, s)
			}
		}
	}
	if len(obs) > 0 {
		sum.Final = append([]int64(nil), obs[len(obs)-1].Sequences...)
	}
	final := map[int64]bool{}
	for _, s := range sum.Final {
		final[s] = true
	}
	for _, s := range sum.Written {
		if !final[s] {
			sum.Pruned = append(sum.Pruned, s)
		}
	}
	sort.Slice(sum.Written, func(i, j int) bool { return sum.Written[i] < sum.Written[j] })
	sort.Slice(sum.Pruned, func(i, j int) bool { return sum.Pruned[i] < sum.Pruned[j] })
	sort.Slice(sum.Final, func(i, j int) bool { return sum.Final[i] < sum.Final[j] })
	return sum
}

// Violations applies the retention bound: at rest a run keeps at most
// keepFulls full checkpoints (all v1 checkpoints are full), and the only legal
// excess is the single write that precedes its own prune.
func (c CheckpointRetention) Violations(keepFulls int) []string {
	var out []string
	if len(c.Final) > keepFulls {
		out = append(out, fmt.Sprintf("run retains %d checkpoints at rest, bound is %d", len(c.Final), keepFulls))
	}
	if c.MaxObserved > keepFulls+1 {
		out = append(out, fmt.Sprintf("observed %d checkpoints at once, bound is keep_fulls+1=%d", c.MaxObserved, keepFulls+1))
	}
	if len(c.Final) > 0 && len(c.Pruned) > 0 {
		lowestKept := c.Final[0]
		for _, p := range c.Pruned {
			if p > lowestKept {
				out = append(out, fmt.Sprintf("pruned checkpoint %d is newer than retained %d", p, lowestKept))
			}
		}
	}
	return out
}
