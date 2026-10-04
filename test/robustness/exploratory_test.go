//go:build integration

package robustness

// TestExploratory is F3's seeded single-host soak and exploratory runner.
//
// scripts/soak-tests.sh provisions an owned kind cluster with three persistent
// chart members (B3's harness values), runs this binary in-cluster, executes
// every fault this runner asks for from the host (so the host keeps the ACTUAL
// fault schedule with wall-clock timestamps), and folds the records sent back
// through the host-request protocol into soak.json. The plan is a pure
// function of CAESIUM_SOAK_SEED and the committed workload file
// (exploratory_plan.go); OS scheduling is not reproducible, which is why the
// actual schedule is retained next to the seed.
//
// Families (each has a short and a nightly form in workloads/*.json):
//   - slow_consumers: slow SSE readers (one replaying the whole retained store)
//     back-pressure the event stream while a burst runs; a fast observer's
//     deliveries are compared with the store as a SET (history.Compare) and
//     the raw effect ledger is correlated per task (history.CorrelateEffects).
//   - queue_overload: admission above a maxRuns=1 job's capacity with high and
//     low priorities past RUN_QUEUE_MAX_DEPTH; the drop-oldest bound, the
//     priority order and the concurrency limit are checked against the raw
//     effect ledger, and every start is reconciled by its Idempotency-Key.
//   - retention: a sustained burst plus a long run; retained runs and events
//     grow by exactly what was admitted and the run-owner checkpoint pruning
//     path is observed deleting older checkpoints while bounding the rest.
//   - repeated_failover: B1/B3 owner and leader process kills (kubelet stopped,
//     then SIGKILL of the member's container, kill evidence required). A
//     process kill is never power-loss qualification: the node's kernel and
//     page cache survive.
//   - node_replacement: a member's pod and PVC are deleted so a fresh volume
//     rejoins through F2's path; the stale raft entry is removed through the
//     operator path when this candidate ships it.
//
// Safety is checked inside every episode and across the whole schedule (every
// admitted run is terminal-succeeded with ordered external effects at drain);
// progress after every heal is a fresh run completing within a stated bound.
// Missing evidence is recorded blocked, never passed.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/faults"
	"github.com/caesium-cloud/caesium/test/robustness/history"
	"github.com/caesium-cloud/caesium/test/robustness/internal/sqlcell"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
)

// Host actions added by the soak controller. cordon/kill/restart/done keep
// B1's protocol and semantics.
const (
	soakActionRecord     = "record"
	soakActionSample     = "sample"
	soakActionContainers = "containers"
	soakActionReplace    = "replace"
)

const soakRecordLimit = 200_000

const (
	soakStatusPass    = "pass"
	soakStatusFail    = "fail"
	soakStatusBlocked = "blocked"

	soakStepHold  = "hold"
	soakQueueHigh = "high"
	soakQueueLow  = "low"
)

// soakRun is one admitted (or attempted) start in the ledger.
type soakRun struct {
	Key        string            `json:"key"`
	Source     string            `json:"source"`
	JobID      string            `json:"job_id"`
	Steps      []string          `json:"steps,omitempty"`
	Member     string            `json:"member"`
	Params     map[string]string `json:"params,omitempty"`
	Priority   string            `json:"priority,omitempty"`
	AdmittedAt time.Time         `json:"admitted_at"`
	HTTPStatus int               `json:"http_status"`
	Outcome    string            `json:"outcome"`
	RunID      string            `json:"run_id,omitempty"`
	QueueID    string            `json:"queue_id,omitempty"`
	Err        string            `json:"error,omitempty"`
	Reconciled bool              `json:"reconciled,omitempty"`
	Final      string            `json:"final_status,omitempty"`
	// Checked is false for runs whose safety is judged inside their episode
	// (queue holders, owner-crash fixtures).
	Checked bool `json:"checked"`
}

type startOutcome struct {
	HTTPStatus int
	Outcome    string
	RunID      string
	QueueID    string
	Replayed   bool
	Err        string
	Raw        string
}

// uncertain reports a start whose effect is unknown: a transport failure or a
// server error. It is reconciled by replaying the same Idempotency-Key.
func (o startOutcome) uncertain() bool {
	return o.Err != "" || o.HTTPStatus == 0 || o.HTTPStatus >= 500
}

type episodeRecord struct {
	Key          string         `json:"key"`
	Family       string         `json:"family"`
	Index        int            `json:"index"`
	Mandatory    bool           `json:"mandatory"`
	Status       string         `json:"status"`
	Detail       string         `json:"detail,omitempty"`
	SoakID       string         `json:"soak_id"`
	Seed         int64          `json:"seed"`
	StartedAt    time.Time      `json:"started_at"`
	EndedAt      time.Time      `json:"ended_at"`
	DurationS    float64        `json:"duration_s"`
	Params       map[string]int `json:"params"`
	Kinds        []string       `json:"kinds,omitempty"`
	Observations map[string]any `json:"observations"`
}

type soakRunner struct {
	fe      *faultEnv
	w       SoakWorkload
	seed    int64
	profile string
	budget  time.Duration
	token   string
	soakID  string
	depth   int
	plan    []SoakEpisode
	started time.Time
	client  *http.Client

	mu        sync.Mutex
	faulted   map[string]string
	ledger    []*soakRun
	trickleID string
	queueJobs []string

	trickleStop chan struct{}
	trickleDone chan struct{}
	haveRemove  bool
}

func TestExploratory(t *testing.T) {
	ctx := context.Background()
	env, err := cluster.LoadEnv()
	if err != nil {
		t.Fatalf("environment: %v", err)
	}
	seed, err := strconv.ParseInt(strings.TrimSpace(os.Getenv("CAESIUM_SOAK_SEED")), 10, 64)
	if err != nil {
		t.Fatalf("CAESIUM_SOAK_SEED must be an integer: %v", err)
	}
	profile := firstNonEmptyStr(os.Getenv("CAESIUM_SOAK_PROFILE"), "short")
	w, err := LoadSoakWorkload(profile)
	if err != nil {
		t.Fatalf("workload: %v", err)
	}
	budget := w.ScheduleBudget.Duration
	if raw := strings.TrimSpace(os.Getenv("CAESIUM_SOAK_DURATION")); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			t.Fatalf("CAESIUM_SOAK_DURATION %q is not a positive Go duration", raw)
		}
		budget = d
	}
	token := strings.TrimSpace(os.Getenv("CAESIUM_SOAK_OWNER_TOKEN"))
	if len(token) < 8 {
		t.Fatalf("CAESIUM_SOAK_OWNER_TOKEN is required (ownership token for task containers)")
	}
	kube, err := cluster.InClusterClient()
	if err != nil {
		t.Fatalf("kube client: %v", err)
	}
	sink := recorder.New()
	if err := sink.Start(); err != nil {
		t.Fatalf("recorder listen %s: %v", recorder.ListenAddr, err)
	}
	t.Cleanup(func() { _ = sink.Close(context.Background()) })
	if err := cluster.EnsureRecorderService(ctx, kube, env.Namespace); err != nil {
		t.Fatalf("recorder service: %v", err)
	}

	sr := &soakRunner{
		fe: &faultEnv{
			env:     env,
			kube:    kube,
			sink:    sink,
			httpAPI: cluster.NewHTTP(env.ManualKey),
			host:    faults.HostController{Kube: kube, Namespace: env.Namespace},
		},
		w:           w,
		seed:        seed,
		profile:     profile,
		budget:      budget,
		token:       token,
		soakID:      env.RobustnessID,
		client:      &http.Client{Timeout: 20 * time.Second},
		faulted:     map[string]string{},
		trickleStop: make(chan struct{}),
		trickleDone: make(chan struct{}),
	}
	sr.plan = PlanSoakSchedule(seed, w)
	sr.haveRemove = removalCLIAvailable()

	if _, err := sr.waitHealthy(ctx, 3*time.Minute); err != nil {
		t.Fatalf("cluster not healthy before the soak (inconclusive): %v", err)
	}
	topo := sr.members()
	for _, m := range topo {
		if v := strings.TrimSpace(m.EnvValue("CAESIUM_RUN_QUEUE_MAX_DEPTH")); v != "" {
			sr.depth, _ = strconv.Atoi(v)
		}
	}
	if sr.depth <= 0 {
		t.Fatalf("members do not advertise CAESIUM_RUN_QUEUE_MAX_DEPTH; the queue-overload oracle needs the deployed bound")
	}

	// The plan is retained first, so an aborted run still shows what it meant
	// to execute.
	sr.sendRecord(t, "plan", map[string]any{
		"soak_id":             sr.soakID,
		"seed":                seed,
		"profile":             profile,
		"schedule_budget":     budget.String(),
		"workload":            w,
		"plan":                sr.plan,
		"mandatory_episodes":  len(SoakFamilies),
		"families":            SoakFamilies,
		"queue_max_depth":     sr.depth,
		"member_removal_path": sr.haveRemove,
		"fault_class":         "process kill (container SIGKILL with kubelet stopped); not power-loss qualification",
	})

	// Warm-up: task/sink connectivity, then the background job and one run of
	// it, so the baseline sample is taken on a warm server.
	leader := sr.leader()
	probeID := "soak-probe-" + uuid.NewString()
	probeAlias := "soak-probe-" + shortID()
	if err := sr.fe.httpAPI.Apply(ctx, leader.HTTPBase(), []jobdef.Definition{sr.own(cluster.ProbeDefinition(probeAlias, probeID, env.TaskImage))}); err != nil {
		t.Fatalf("apply probe job: %v", err)
	}
	probeJob, err := sr.fe.httpAPI.JobByAlias(ctx, leader.HTTPBase(), probeAlias)
	if err != nil {
		t.Fatalf("read probe job: %v", err)
	}
	if _, _, err := sr.fe.httpAPI.TriggerRun(ctx, leader.HTTPBase(), probeJob.ID); err != nil {
		t.Fatalf("trigger probe job: %v", err)
	}
	probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Minute)
	err = cluster.WaitProbe(probeCtx, sink, probeID)
	probeCancel()
	if err != nil {
		t.Fatalf("task/sink connectivity probe missing (inconclusive): %v", err)
	}
	trickle := sr.applyChain(t, leader, "trickle", []string{firstStep, secondStep}, w.Background.HoldSeconds, nil)
	sr.trickleID = trickle.ID
	warm := sr.startTracked(ctx, leader, trickle.ID, sr.key("warmup", 0), nil, "", "warmup", []string{firstStep, secondStep}, true)
	if warm.RunID == "" {
		t.Fatalf("warm-up start was not admitted: %+v", warm)
	}
	if _, err := sr.waitTerminal(ctx, trickle.ID, warm.RunID, w.RunBound.Duration); err != nil {
		t.Fatalf("warm-up run: %v", err)
	}
	sr.hostSample(t, "baseline")
	sr.hostContainers(t, "baseline")

	go sr.trickle()
	sr.started = time.Now()

	blockedFrom := -1
	var blockReason string
	for _, ep := range sr.plan {
		if !ep.Mandatory && time.Since(sr.started) >= sr.budget {
			break
		}
		if blockedFrom >= 0 {
			// Only the mandatory pass is owed a record; opportunistic
			// (budgeted) episodes after an unhealthy cluster are simply not run.
			if !ep.Mandatory {
				break
			}
			sr.sendEpisode(t, blockedEpisode(sr, ep, blockReason))
			continue
		}
		if _, err := sr.waitHealthy(ctx, 4*time.Minute); err != nil {
			blockedFrom = ep.Index
			blockReason = fmt.Sprintf("cluster did not return to three healthy voters before this episode: %v", err)
			sr.sendEpisode(t, blockedEpisode(sr, ep, blockReason))
			t.Errorf("episode %s blocked: %s", ep.Key(), blockReason)
			continue
		}
		ep := ep
		t.Run(ep.Key(), func(t *testing.T) { sr.runEpisode(t, ep) })
	}

	t.Run("drain", func(t *testing.T) { sr.drain(t) })

	doneCtx, doneCancel := context.WithTimeout(ctx, 30*time.Second)
	defer doneCancel()
	if _, err := cluster.RequestHost(doneCtx, kube, env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    cluster.ActionDone,
	}); err != nil {
		t.Logf("host done: %v", err)
	}
}

func blockedEpisode(sr *soakRunner, ep SoakEpisode, reason string) episodeRecord {
	now := time.Now().UTC()
	return episodeRecord{
		Key: ep.Key(), Family: ep.Family, Index: ep.Index, Mandatory: ep.Mandatory,
		Status: soakStatusBlocked, Detail: reason, SoakID: sr.soakID, Seed: sr.seed,
		StartedAt: now, EndedAt: now, Params: ep.Params, Kinds: ep.Kinds,
		Observations: map[string]any{},
	}
}

// ---------------------------------------------------------------------------
// Episode dispatch and record hand-off.
// ---------------------------------------------------------------------------

func (sr *soakRunner) runEpisode(t *testing.T, ep SoakEpisode) {
	rec := &episodeRecord{
		Key: ep.Key(), Family: ep.Family, Index: ep.Index, Mandatory: ep.Mandatory,
		SoakID: sr.soakID, Seed: sr.seed, StartedAt: time.Now().UTC(),
		Params: ep.Params, Kinds: ep.Kinds, Observations: map[string]any{},
	}
	defer func() {
		rec.EndedAt = time.Now().UTC()
		rec.DurationS = rec.EndedAt.Sub(rec.StartedAt).Seconds()
		if rec.Status == "" {
			if t.Failed() {
				rec.Status = soakStatusFail
				rec.Detail = firstNonEmptyStr(rec.Detail, "an assertion failed; see runner.log for the failing line")
			} else {
				rec.Status = soakStatusPass
			}
		}
		sr.sendEpisode(t, *rec)
	}()
	t.Logf("episode %s params=%v kinds=%v draws=%v", ep.Key(), ep.Params, ep.Kinds, ep.Draws[:4])
	switch ep.Family {
	case FamilySlowConsumers:
		sr.episodeSlowConsumers(t, ep, rec)
	case FamilyQueueOverload:
		sr.episodeQueueOverload(t, ep, rec)
	case FamilyRetention:
		sr.episodeRetention(t, ep, rec)
	case FamilyRepeatedFailover:
		sr.episodeFailover(t, ep, rec)
	case FamilyNodeReplacement:
		sr.episodeReplacement(t, ep, rec)
	default:
		sr.blockf(t, rec, "unknown family %q", ep.Family)
	}
}

func (sr *soakRunner) failf(t *testing.T, rec *episodeRecord, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	rec.Status = soakStatusFail
	rec.Detail = msg
	t.Fatal(msg)
}

func (sr *soakRunner) blockf(t *testing.T, rec *episodeRecord, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	rec.Status = soakStatusBlocked
	rec.Detail = msg
	t.Fatal("inconclusive: " + msg)
}

func (sr *soakRunner) sendEpisode(t *testing.T, rec episodeRecord) {
	sr.sendRecord(t, rec.Key, rec)
}

// sendRecord hands one record to the host controller, which writes it under
// $CAESIUM_SOAK_ARTIFACTS/records/. A record the host never acknowledged makes
// the family blocked there, so a failure here is logged, not fatal.
func (sr *soakRunner) sendRecord(t *testing.T, key string, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		t.Errorf("marshal record %s: %v", key, err)
		return
	}
	payload := RedactSecrets(string(raw))
	// The request ConfigMap carries a record about 3.7 times (payload,
	// payload_b64 and params) and a ConfigMap is capped at 1 MiB.
	if len(payload) > soakRecordLimit {
		payload = fmt.Sprintf(`{"key":%q,"status":"blocked","detail":"record exceeded the %d-byte host-request bound (%d bytes)"}`, key, soakRecordLimit, len(payload))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err := cluster.RequestHost(ctx, sr.fe.kube, sr.fe.env.Namespace, cluster.HostRequest{
		RequestID: uuid.NewString(),
		Action:    soakActionRecord,
		Params:    map[string]string{"key": key, "payload": payload},
	}); err != nil {
		t.Errorf("host did not store record %s: %v", key, err)
	}
}

func (sr *soakRunner) hostRequest(ctx context.Context, ep *SoakEpisode, req cluster.HostRequest) (cluster.HostAck, error) {
	if req.RequestID == "" {
		req.RequestID = uuid.NewString()
	}
	if req.Params == nil {
		req.Params = map[string]string{}
	}
	if ep != nil {
		req.Params["episode"] = ep.Key()
		req.Params["family"] = ep.Family
	}
	return cluster.RequestHost(ctx, sr.fe.kube, sr.fe.env.Namespace, req)
}

func (sr *soakRunner) hostSample(t *testing.T, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ack, err := sr.hostRequest(ctx, nil, cluster.HostRequest{Action: soakActionSample, Params: map[string]string{"label": label}})
	if err != nil {
		t.Errorf("resource sample %s: %v", label, err)
		return
	}
	t.Logf("resource sample %s: %s", label, truncate([]byte(ack.Evidence), 600))
}

func (sr *soakRunner) hostContainers(t *testing.T, label string) string {
	evidence, _ := sr.hostContainersEvidence(t, label)
	return evidence
}

func (sr *soakRunner) hostContainersEvidence(t *testing.T, label string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ack, err := sr.hostRequest(ctx, nil, cluster.HostRequest{Action: soakActionContainers, Params: map[string]string{"label": label, "token": sr.token}})
	if err != nil {
		t.Errorf("container inventory %s: %v", label, err)
		return "", false
	}
	t.Logf("container inventory %s: %s", label, truncate([]byte(ack.Evidence), 600))
	return ack.Evidence, true
}

// ---------------------------------------------------------------------------
// Topology, membership and health.
// ---------------------------------------------------------------------------

func (sr *soakRunner) members() []cluster.Member {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return append([]cluster.Member(nil), sr.fe.topo.Members...)
}

func (sr *soakRunner) leader() cluster.Member {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.fe.leader
}

func (sr *soakRunner) liveMembers() []cluster.Member {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	var out []cluster.Member
	for _, m := range sr.fe.topo.Members {
		if _, bad := sr.faulted[m.Name]; !bad {
			out = append(out, m)
		}
	}
	return out
}

func (sr *soakRunner) setFaulted(name, why string) {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if why == "" {
		delete(sr.faulted, name)
		return
	}
	sr.faulted[name] = why
}

// soakMember is one member's view of the raft configuration.
type soakMember struct {
	ID      uint64 `json:"id"`
	Address string `json:"address"`
	Role    string `json:"role"`
	Live    bool   `json:"live"`
}

type soakMembership struct {
	Leader       string       `json:"leader"`
	LeaderPod    string       `json:"leader_pod"`
	Members      []soakMember `json:"members"`
	LiveVoters   int          `json:"live_voters"`
	StaleEntries []soakMember `json:"stale_entries,omitempty"`
}

// queryMembership asks every live pod's own dqlite node for the leader and the
// configuration. Unlike cluster.DiscoverMembership it tolerates stale entries
// a replaced member leaves behind, but it still requires one agreed leader,
// identical configurations everywhere, and exactly the three live pods as
// voters.
func (sr *soakRunner) queryMembership(ctx context.Context, topo []cluster.Member) (soakMembership, error) {
	live := map[string]string{}
	for _, m := range topo {
		live[m.DqliteAddr()] = m.Name
	}
	var out soakMembership
	var reference string
	for _, m := range topo {
		leader, members, err := cluster.QueryNode(ctx, m.DqliteAddr())
		if err != nil {
			return soakMembership{}, err
		}
		if leader == nil || leader.Address == "" {
			return soakMembership{}, fmt.Errorf("%s reports no leader", m.Name)
		}
		if out.Leader == "" {
			out.Leader = leader.Address
		} else if out.Leader != leader.Address {
			return soakMembership{}, fmt.Errorf("leader disagreement: %s vs %s (from %s)", out.Leader, leader.Address, m.Name)
		}
		var view []soakMember
		for _, n := range members {
			_, isLive := live[n.Address]
			view = append(view, soakMember{ID: n.ID, Address: n.Address, Role: strings.ToLower(n.Role.String()), Live: isLive})
		}
		slices.SortFunc(view, func(a, b soakMember) int { return cmp.Compare(a.ID, b.ID) })
		sig := jsonString(view)
		if reference == "" {
			reference = sig
			out.Members = view
		} else if sig != reference {
			return soakMembership{}, fmt.Errorf("configuration disagreement between members: %s vs %s", reference, sig)
		}
	}
	out.LeaderPod = live[out.Leader]
	for _, n := range out.Members {
		if n.Live && n.Role == "voter" {
			out.LiveVoters++
		}
		if !n.Live {
			out.StaleEntries = append(out.StaleEntries, n)
			if n.Role == "voter" {
				return out, fmt.Errorf("stale entry %d@%s is still a voter", n.ID, n.Address)
			}
		}
	}
	if out.LiveVoters != 3 {
		return out, fmt.Errorf("expected the three live pods as voters, found %d (%s)", out.LiveVoters, jsonString(out.Members))
	}
	if out.LeaderPod == "" {
		return out, fmt.Errorf("leader %s is not a live caesium pod", out.Leader)
	}
	return out, nil
}

// waitHealthy refreshes the topology and requires three Ready candidate
// members answering /health with three live voters and one agreed leader.
func (sr *soakRunner) waitHealthy(ctx context.Context, bound time.Duration) (soakMembership, error) {
	pollCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var last error
	var found soakMembership
	err := cluster.Poll(pollCtx, 2*time.Second, func() (bool, error) {
		topo, err := cluster.RequireReadyTopology(pollCtx, sr.fe.kube, sr.fe.env.Namespace, sr.fe.env.CandidateDigest)
		if err != nil {
			last = err
			return false, nil
		}
		for _, m := range topo.Members {
			hctx, hcancel := context.WithTimeout(pollCtx, 5*time.Second)
			err := sr.fe.httpAPI.Health(hctx, m.HTTPBase())
			hcancel()
			if err != nil {
				last = fmt.Errorf("%s /health: %w", m.Name, err)
				return false, nil
			}
		}
		mctx, mcancel := context.WithTimeout(pollCtx, 20*time.Second)
		ms, err := sr.queryMembership(mctx, topo.Members)
		mcancel()
		if err != nil {
			last = err
			return false, nil
		}
		leader, _ := topo.ByIP(cluster.HostIP(ms.Leader))
		sr.mu.Lock()
		sr.fe.topo = topo
		sr.fe.leader = leader
		sr.mu.Unlock()
		found = ms
		return true, nil
	})
	if err != nil {
		if last != nil {
			return found, fmt.Errorf("%w (last: %v)", err, last)
		}
		return found, err
	}
	return found, nil
}

// ---------------------------------------------------------------------------
// Fixtures, admission and the run ledger.
// ---------------------------------------------------------------------------

func shortID() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:10] }

func (sr *soakRunner) key(source string, n int) string {
	return fmt.Sprintf("soak-%s-%s-%04d", sr.token[:8], source, n)
}

// own stamps every step with the ownership token, so the host can tell this
// invocation's task containers from anything else on the kind nodes.
func (sr *soakRunner) own(def jobdef.Definition) jobdef.Definition {
	for i := range def.Steps {
		if def.Steps[i].Env == nil {
			def.Steps[i].Env = map[string]string{}
		}
		def.Steps[i].Env["CAESIUM_SOAK_OWNER"] = sr.token
	}
	if def.Metadata.Labels == nil {
		def.Metadata.Labels = map[string]string{}
	}
	def.Metadata.Labels["caesium-soak"] = sr.token[:8]
	return def
}

func soakEffectScript(step string, holdSeconds int) string {
	return fmt.Sprintf(`set -eu
RECORDER=%q
STEP=%q
NONCE="$(cat /proc/sys/kernel/random/uuid)"
post() { i=0; until wget -qO- -T 5 --header='Content-Type: application/json' --post-data="$2" "${RECORDER}/$1" >/dev/null; do i=$((i + 1)); [ "$i" -lt 6 ] || return 1; sleep 1; done; }
post start "{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"start\"}"
sleep %d
post effect "{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"complete\"}"
`, cluster.RecorderURL(), step, holdSeconds)
}

// chainDefinition is a sequential job whose every step records an external
// start and completion with a fresh nonce.
func chainDefinition(alias, taskImage string, steps []string, holdSeconds int, meta *jobdef.Metadata) jobdef.Definition {
	def := jobdef.Definition{
		APIVersion: jobdef.APIVersionV1,
		Kind:       jobdef.KindJob,
		Metadata:   jobdef.Metadata{Alias: alias, Labels: map[string]string{"caesium-robustness": "soak"}},
		Trigger: jobdef.Trigger{
			Type:          jobdef.TriggerHTTP,
			Configuration: map[string]any{"path": "robustness-soak-" + alias},
		},
	}
	if meta != nil {
		def.Metadata.Priority = meta.Priority
		def.Metadata.Concurrency = meta.Concurrency
	}
	for i, name := range steps {
		st := jobdef.Step{
			Name:    name,
			Type:    jobdef.StepTypeTask,
			Engine:  jobdef.EngineKubernetes,
			Image:   taskImage,
			Command: []string{"sh", "-c", soakEffectScript(name, holdSeconds)},
		}
		if i+1 < len(steps) {
			st.Next = []string{steps[i+1]}
		}
		if i > 0 {
			st.DependsOn = []string{steps[i-1]}
		}
		def.Steps = append(def.Steps, st)
	}
	return def
}

func (sr *soakRunner) applyDefinition(t *testing.T, member cluster.Member, def jobdef.Definition) cluster.Job {
	t.Helper()
	ctx := context.Background()
	def = sr.own(def)
	if err := def.Validate(); err != nil {
		t.Fatalf("fixture %s invalid: %v", def.Metadata.Alias, err)
	}
	if err := sr.fe.httpAPI.Apply(ctx, member.HTTPBase(), []jobdef.Definition{def}); err != nil {
		t.Fatalf("apply %s: %v", def.Metadata.Alias, err)
	}
	job, err := sr.fe.httpAPI.JobByAlias(ctx, member.HTTPBase(), def.Metadata.Alias)
	if err != nil {
		t.Fatalf("read applied job %s: %v", def.Metadata.Alias, err)
	}
	return job
}

func (sr *soakRunner) applyChain(t *testing.T, member cluster.Member, kind string, steps []string, hold int, meta *jobdef.Metadata) cluster.Job {
	t.Helper()
	return sr.applyDefinition(t, member, chainDefinition("soak-"+kind+"-"+shortID(), sr.fe.env.TaskImage, steps, hold, meta))
}

// start posts one run start with an Idempotency-Key, so an uncertain answer
// can be reconciled later by replaying the identical request.
func (sr *soakRunner) start(ctx context.Context, m cluster.Member, jobID, key string, params map[string]string, priority string) startOutcome {
	body := map[string]any{}
	if len(params) > 0 {
		body["params"] = params
	}
	if priority != "" {
		body["priority"] = priority
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(m.HTTPBase(), "/")+"/v1/jobs/"+jobID+"/run", bytes.NewReader(raw))
	if err != nil {
		return startOutcome{Err: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Caesium-Manual-Trigger-Key", sr.fe.env.ManualKey)
	resp, err := sr.client.Do(req)
	if err != nil {
		return startOutcome{Err: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	out := startOutcome{HTTPStatus: resp.StatusCode, Raw: truncate(b, 400), Replayed: resp.Header.Get("Idempotent-Replayed") == "true"}
	if resp.StatusCode != http.StatusAccepted {
		return out
	}
	var parsed struct {
		Outcome string `json:"outcome"`
		ID      string `json:"id"`
		QueueID string `json:"queue_id"`
		RunID   string `json:"run_id"`
	}
	if err := json.Unmarshal(b, &parsed); err != nil {
		out.Err = "202 body is not JSON: " + err.Error()
		return out
	}
	out.Outcome = parsed.Outcome
	switch parsed.Outcome {
	case "created":
		if _, err := uuid.Parse(parsed.ID); err != nil {
			out.Err = "created outcome has an invalid run id"
			return out
		}
		out.RunID = parsed.ID
	case "queued", "dropped":
		if parsed.QueueID != "" {
			if _, err := uuid.Parse(parsed.QueueID); err != nil {
				out.Err = parsed.Outcome + " outcome has an invalid queue id"
				return out
			}
			out.QueueID = parsed.QueueID
		}
	case "skipped":
		if parsed.RunID != "" {
			if _, err := uuid.Parse(parsed.RunID); err != nil {
				out.Err = "skipped outcome has an invalid run id"
				return out
			}
			out.RunID = parsed.RunID
		}
	default:
		out.Err = fmt.Sprintf("202 body has unsupported outcome %q", parsed.Outcome)
		return out
	}
	return out
}

// startTracked admits a start and appends it to the ledger.
func (sr *soakRunner) startTracked(ctx context.Context, m cluster.Member, jobID, key string, params map[string]string, priority, source string, steps []string, checked bool) *soakRun {
	res := sr.start(ctx, m, jobID, key, params, priority)
	entry := &soakRun{
		Key: key, Source: source, JobID: jobID, Steps: steps, Member: m.Name,
		Params: params, Priority: priority, AdmittedAt: time.Now().UTC(),
		HTTPStatus: res.HTTPStatus, Outcome: res.Outcome, RunID: res.RunID,
		QueueID: res.QueueID, Err: res.Err, Checked: checked,
	}
	if res.uncertain() && entry.Err == "" {
		entry.Err = fmt.Sprintf("HTTP %d: %s", res.HTTPStatus, res.Raw)
	}
	sr.mu.Lock()
	sr.ledger = append(sr.ledger, entry)
	sr.mu.Unlock()
	return entry
}

// reconcile replays an entry's start on live members until the answer is
// certain. A replay of a key the server never recorded admits the run now,
// which is still a definite outcome for that key.
func (sr *soakRunner) reconcile(ctx context.Context, e *soakRun) error {
	return sr.reconcileWithRetry(ctx, e, 6, 5*time.Second)
}

func (sr *soakRunner) reconcileWithRetry(ctx context.Context, e *soakRun, maxAttempts int, retryDelay time.Duration) error {
	var last startOutcome
	for attempt := 0; attempt < maxAttempts; attempt++ {
		members := sr.liveMembers()
		if len(members) == 0 {
			time.Sleep(retryDelay)
			continue
		}
		m := members[attempt%len(members)]
		last = sr.start(ctx, m, e.JobID, e.Key, e.Params, e.Priority)
		if !last.uncertain() {
			sr.mu.Lock()
			e.HTTPStatus, e.Outcome, e.RunID, e.QueueID = last.HTTPStatus, last.Outcome, last.RunID, last.QueueID
			e.Reconciled = true
			sr.mu.Unlock()
			return nil
		}
		time.Sleep(retryDelay)
	}
	return fmt.Errorf("start %s stayed uncertain after replays: %+v", e.Key, last)
}

func (sr *soakRunner) waitTerminal(ctx context.Context, jobID, runID string, bound time.Duration) (cluster.Run, error) {
	pollCtx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var final cluster.Run
	var last error
	n := 0
	err := cluster.Poll(pollCtx, 2*time.Second, func() (bool, error) {
		members := sr.liveMembers()
		if len(members) == 0 {
			return false, nil
		}
		m := members[n%len(members)]
		n++
		rctx, rcancel := context.WithTimeout(pollCtx, 10*time.Second)
		got, err := sr.fe.httpAPI.GetRun(rctx, m.HTTPBase(), jobID, runID)
		rcancel()
		if err != nil {
			last = err
			return false, nil
		}
		switch strings.ToLower(strings.TrimSpace(got.Status)) {
		case "succeeded", "failed", "cancelled", "skipped":
			final = got
			return true, nil
		}
		final = got
		return false, nil
	})
	if err != nil {
		return final, fmt.Errorf("run %s not terminal within %s (last status %q, last error %v)", runID, bound, final.Status, last)
	}
	return final, nil
}

// trickle is the background load that runs under every episode, so faults
// and workloads always combine with ordinary traffic.
func (sr *soakRunner) trickle() {
	defer close(sr.trickleDone)
	rng := rand.New(rand.NewSource(DerivedSeed(sr.seed, "trickle")))
	n := 0
	for {
		wait := time.Duration(sr.w.Background.TrickleIntervalMS.draw(rng)) * time.Millisecond
		pick := rng.Intn(1 << 16)
		select {
		case <-sr.trickleStop:
			return
		case <-time.After(wait):
		}
		members := sr.liveMembers()
		if len(members) == 0 {
			continue
		}
		m := members[pick%len(members)]
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		sr.startTracked(ctx, m, sr.trickleID, sr.key("trickle", n), nil, "", "trickle", []string{firstStep, secondStep}, true)
		cancel()
		n++
	}
}

// progressProbe is the post-heal progress check: a new run, admitted through
// a seeded live member, must complete within HealProgressBound.
func (sr *soakRunner) progressProbe(t *testing.T, ep SoakEpisode, rec *episodeRecord, label string, draw int) map[string]any {
	t.Helper()
	ctx := context.Background()
	members := sr.liveMembers()
	if len(members) == 0 {
		sr.blockf(t, rec, "%s: no live member for the progress probe", label)
	}
	m := members[ep.Draw(draw, len(members))]
	began := time.Now()
	e := sr.startTracked(ctx, m, sr.trickleID, sr.key(fmt.Sprintf("e%02d-%s", ep.Index, label), 0), nil, "", ep.Key()+"/"+label, []string{firstStep, secondStep}, true)
	if e.RunID == "" {
		sr.failf(t, rec, "%s: progress probe via %s was not admitted after heal: %+v", label, m.Name, *e)
	}
	final, err := sr.waitTerminal(ctx, sr.trickleID, e.RunID, sr.w.HealProgressBound.Duration)
	if err != nil {
		sr.failf(t, rec, "%s: no progress after heal: %v", label, err)
	}
	if !strings.EqualFold(final.Status, "succeeded") {
		sr.failf(t, rec, "%s: progress probe run %s ended %s (tasks: %s)", label, e.RunID, final.Status, runTaskSummary(final))
	}
	obs := map[string]any{"run_id": e.RunID, "member": m.Name, "seconds": time.Since(began).Seconds(), "bound": sr.w.HealProgressBound.String()}
	t.Logf("%s progress probe %s via %s completed in %.1fs", label, e.RunID, m.Name, time.Since(began).Seconds())
	return obs
}

// ---------------------------------------------------------------------------
// slow_consumers
// ---------------------------------------------------------------------------

// throttledReader hands the SSE parser at most chunk bytes per read and
// sleeps between reads, so a subscriber really drains slowly and the
// server's writes queue behind it.
type throttledReader struct {
	r     io.Reader
	chunk int
	delay time.Duration
	ctx   context.Context
	total int64
}

func (t *throttledReader) Read(p []byte) (int, error) {
	if len(p) > t.chunk {
		p = p[:t.chunk]
	}
	n, err := t.r.Read(p)
	t.total += int64(n)
	select {
	case <-t.ctx.Done():
	case <-time.After(t.delay):
	}
	return n, err
}

type slowConsumer struct {
	Member      string    `json:"member"`
	FromStart   bool      `json:"from_start"`
	DelayMS     int       `json:"delay_ms"`
	ChunkBytes  int       `json:"chunk_bytes"`
	TipAtOpen   uint64    `json:"store_tip_at_open"`
	Status      int       `json:"status"`
	Frames      int       `json:"frames"`
	Bytes       int64     `json:"bytes"`
	HighestSeq  uint64    `json:"highest_sequence"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	EndErr      string    `json:"end_error,omitempty"`
	ServerEnded bool      `json:"ended_by_server"`
	Behind      bool      `json:"behind_at_close"`

	mu     sync.Mutex
	events []recorder.SSEEvent
}

func slowTransport() *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) {
				// A small receive window keeps the backlog on the server side.
				serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 4096)
			}); err != nil {
				return err
			}
			return serr
		},
	}
	return &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}
}

func (c *slowConsumer) run(ctx context.Context, url, manualKey string, lastEventID string) {
	c.StartedAt = time.Now().UTC()
	defer func() { c.EndedAt = time.Now().UTC() }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		c.EndErr = err.Error()
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("X-Caesium-Manual-Trigger-Key", manualKey)
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{Transport: slowTransport()}).Do(req)
	if err != nil {
		c.EndErr = err.Error()
		return
	}
	defer func() { _ = resp.Body.Close() }()
	c.Status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		c.EndErr = fmt.Sprintf("status %d", resp.StatusCode)
		return
	}
	tr := &throttledReader{r: resp.Body, chunk: c.ChunkBytes, delay: time.Duration(c.DelayMS) * time.Millisecond, ctx: ctx}
	perr := recorder.ParseSSE(tr, func(f recorder.SSEFrame) {
		ev := recorder.SSEEvent{At: time.Now().UTC(), RawID: f.ID, Sequence: f.Sequence(), Type: f.Event, Data: json.RawMessage(f.Data)}
		var payload struct {
			Sequence uint64 `json:"sequence"`
			Type     string `json:"type"`
			RunID    string `json:"run_id"`
			TaskID   string `json:"task_id"`
		}
		if json.Unmarshal([]byte(f.Data), &payload) == nil {
			if ev.Sequence == 0 {
				ev.Sequence = payload.Sequence
			}
			if ev.Type == "" {
				ev.Type = payload.Type
			}
			ev.RunID, ev.TaskID = payload.RunID, payload.TaskID
		}
		ev.Data = nil
		c.mu.Lock()
		c.events = append(c.events, ev)
		c.Frames++
		if ev.Sequence > c.HighestSeq {
			c.HighestSeq = ev.Sequence
		}
		c.mu.Unlock()
	})
	c.Bytes = tr.total
	if ctx.Err() == nil {
		c.ServerEnded = true
		if perr != nil {
			c.EndErr = perr.Error()
		}
	}
}

func (c *slowConsumer) Events() []recorder.SSEEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]recorder.SSEEvent(nil), c.events...)
}

func (sr *soakRunner) goroutines(ctx context.Context, m cluster.Member) (float64, error) {
	text, err := sr.fe.httpAPI.Metrics(ctx, m.HTTPBase())
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "go_goroutines ") {
			return strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "go_goroutines ")), 64)
		}
	}
	return 0, fmt.Errorf("go_goroutines absent from %s /metrics", m.Name)
}

func (sr *soakRunner) episodeSlowConsumers(t *testing.T, ep SoakEpisode, rec *episodeRecord) {
	ctx := context.Background()
	members := sr.members()
	cfg := sr.w.SlowConsumers
	before := map[string]float64{}
	for _, m := range members {
		g, err := sr.goroutines(ctx, m)
		if err != nil {
			sr.blockf(t, rec, "goroutine baseline on %s: %v", m.Name, err)
		}
		before[m.Name] = g
	}
	job := sr.applyChain(t, sr.leader(), "slow", []string{firstStep, secondStep}, ep.Params["hold_seconds"], nil)

	observerMember := members[ep.Draw(0, len(members))]
	tipCtx, tipCancel := context.WithTimeout(ctx, 30*time.Second)
	tip, err := latestSequence(tipCtx, sr.fe.httpAPI, observerMember.HTTPBase())
	tipCancel()
	if err != nil {
		sr.blockf(t, rec, "read store tip: %v", err)
	}

	consumerCtx, consumerCancel := context.WithCancel(ctx)
	defer consumerCancel()
	var consumers []*slowConsumer
	var wg sync.WaitGroup
	// Every slow consumer attaches to the member that admits (and so owns and
	// publishes) the burst, so the back-pressure lands on the publishing path.
	for i := 0; i < ep.Params["consumers"]; i++ {
		m := observerMember
		c := &slowConsumer{
			Member: m.Name, FromStart: i == 0, DelayMS: ep.Params["read_delay_ms"],
			ChunkBytes: cfg.ChunkBytes, TipAtOpen: tip,
		}
		cursor := fmt.Sprint(tip)
		if c.FromStart {
			// Replays the whole retained store through the slow reader.
			cursor = ""
		}
		consumers = append(consumers, c)
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.run(consumerCtx, m.HTTPBase()+"/v1/events", sr.fe.env.ManualKey, cursor)
		}()
	}

	observer := recorder.NewSSESubscriber(observerMember.HTTPBase(), "", sr.fe.env.ManualKey)
	obsCtx, obsCancel := context.WithCancel(ctx)
	defer obsCancel()
	// The API server's 30 s WriteTimeout ends every /v1/events stream (a
	// finding this family records). The observer therefore reconnects the way
	// a real client does, always from the episode's starting cursor, so a
	// server-initiated cut can neither hide an event (catch-up replays the
	// store above the cursor) nor make the comparison inconclusive; each cut
	// is counted and every connection must have been established.
	obsDone := make(chan struct{})
	serverCuts := 0
	go func() {
		defer close(obsDone)
		for obsCtx.Err() == nil {
			_, _ = observer.Connect(obsCtx, fmt.Sprint(tip))
			if obsCtx.Err() != nil {
				return
			}
			serverCuts++
			select {
			case <-obsCtx.Done():
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	}()
	time.Sleep(3 * time.Second)

	var burst []*soakRun
	var latencies []float64
	for i := 0; i < ep.Params["burst_runs"]; i++ {
		m := observerMember
		began := time.Now()
		tctx, tcancel := context.WithTimeout(ctx, 20*time.Second)
		e := sr.startTracked(tctx, m, job.ID, sr.key(fmt.Sprintf("e%02d-slow", ep.Index), i), nil, "", ep.Key(), []string{firstStep, secondStep}, true)
		tcancel()
		latencies = append(latencies, time.Since(began).Seconds())
		if e.RunID == "" {
			sr.failf(t, rec, "burst start %d via %s was not admitted while slow consumers were attached: %+v", i, m.Name, *e)
		}
		burst = append(burst, e)
	}
	for _, e := range burst {
		final, err := sr.waitTerminal(ctx, job.ID, e.RunID, sr.w.RunBound.Duration)
		if err != nil {
			sr.failf(t, rec, "progress stalled behind slow consumers: %v", err)
		}
		if !strings.EqualFold(final.Status, "succeeded") {
			sr.failf(t, rec, "burst run %s ended %s (tasks: %s)", e.RunID, final.Status, runTaskSummary(final))
		}
	}
	time.Sleep(5 * time.Second)
	obsCancel()
	<-obsDone
	obsConns := observer.Connections()
	for _, c := range obsConns {
		if c.Status != http.StatusOK || c.DecodeFailures > 0 {
			sr.blockf(t, rec, "observer connection %d was not a clean stream (status %d, decode failures %d, error %q)", c.Gen, c.Status, c.DecodeFailures, c.Err)
		}
	}
	obsConn := sseConnection(obsConns, nil, false)

	// Fast observer: deliveries vs the store as a SET, per run, plus the raw
	// effect ledger per task.
	stepCtx, stepCancel := context.WithTimeout(ctx, 60*time.Second)
	taskSteps, err := taskStepNames(stepCtx, sr.fe.httpAPI, observerMember.HTTPBase(), job.ID)
	stepCancel()
	if err != nil {
		sr.blockf(t, rec, "task identities: %v", err)
	}
	var reports []map[string]any
	var slowDefects []string
	persistedByRun := map[string][]persistedEvent{}
	for _, e := range burst {
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		rows, scope, err := readPersistedEvents(rctx, sr.fe.httpAPI, observerMember.HTTPBase(), e.RunID, 2000)
		rcancel()
		if err != nil || len(rows) == 0 {
			sr.blockf(t, rec, "persisted events for %s: %v (rows=%d)", e.RunID, err, len(rows))
		}
		persistedByRun[e.RunID] = rows
		delivered := deliveredForRun(observer.Events(), e.RunID)
		rep := history.Compare(delivered, toHistoryPersisted(rows), scope, obsConn)
		if defects := rep.Defects(); len(defects) > 0 {
			sr.failf(t, rec, "event history defects for %s under slow consumers: %v", e.RunID, defects)
		}
		if !rep.Conclusive() {
			sr.blockf(t, rec, "event history for %s inconclusive: %v", e.RunID, rep.Inconclusive)
		}
		effRep := history.CorrelateEffects(e.RunID, effectsForRun(sr.fe.sink.Events(), e.RunID), delivered, toHistoryPersisted(rows), completionEventTypes, taskSteps)
		if defects := effRep.Defects(); len(defects) > 0 {
			sr.failf(t, rec, "effect correlation defects for %s: %v", e.RunID, defects)
		}
		if !effRep.Conclusive() {
			sr.blockf(t, rec, "effect correlation for %s inconclusive: %v", e.RunID, effRep.Inconclusive)
		}
		// Live loss is legal (DT-EVENT-01); durable catch-up is not. With the
		// slow consumers still attached, a resume from the run's lowest
		// persisted sequence must replay every row above it.
		cursor := rows[0].Sequence
		expected := sequencesAbove(rows, cursor, scope)
		if len(expected) == 0 {
			sr.blockf(t, rec, "run %s has no persisted row above cursor %d to replay", e.RunID, cursor)
		}
		resumed := recorder.NewSSESubscriber(observerMember.HTTPBase(), e.RunID, sr.fe.env.ManualKey)
		resumeCtx, resumeCancel := context.WithTimeout(ctx, 90*time.Second)
		resumeWatch := startSSE(resumed, resumeCtx, fmt.Sprint(cursor))
		waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
		_ = cluster.Poll(waitCtx, time.Second, func() (bool, error) {
			return len(deliveredForRun(resumed.Events(), e.RunID)) >= len(expected), nil
		})
		waitCancel()
		time.Sleep(time.Second)
		resumeCancel()
		resumeWatch.wait()
		rrep := history.CompareReconnect(delivered, deliveredForRun(resumed.Events(), e.RunID), toHistoryPersisted(rows), cursor, scope, resumeWatch.result(resumed))
		if defects := rrep.Defects(); len(defects) > 0 {
			sr.failf(t, rec, "catch-up under slow consumers lost durable events for %s: %v", e.RunID, defects)
		}
		if !rrep.Conclusive() {
			sr.blockf(t, rec, "catch-up comparison for %s inconclusive: %v", e.RunID, rrep.Inconclusive)
		}
		reports = append(reports, map[string]any{
			"run_id": e.RunID, "delivered": rep.DeliveredCount, "distinct": rep.DeliveredDistinct,
			"persisted": rep.PersistedCount, "missing_from_live_delivery": len(rep.MissingFromDelivery),
			"duplicates": len(rep.Duplicates), "raw_completions": effRep.RawCompletions,
			"catch_up_cursor": cursor, "catch_up_expected": len(expected), "catch_up_replayed": rrep.ResumedDistinct,
		})
	}

	closeCtx, closeCancel := context.WithTimeout(ctx, 30*time.Second)
	tipClose, err := latestSequence(closeCtx, sr.fe.httpAPI, observerMember.HTTPBase())
	closeCancel()
	if err != nil {
		sr.blockf(t, rec, "read store tip at close: %v", err)
	}
	consumerCancel()
	wg.Wait()
	behind := 0
	for _, c := range consumers {
		if c.Status != http.StatusOK {
			sr.blockf(t, rec, "slow consumer on %s never streamed: %s", c.Member, c.EndErr)
		}
		// Behind at close: events persisted (and published) before the
		// consumer closed had not reached it — the stream was queued behind
		// the slow reader or dropped for it, never waiting on the server.
		c.Behind = c.HighestSeq < tipClose
		if c.Behind {
			behind++
		}
		// Whatever a slow consumer did receive for the burst must exist in
		// the store with the same type: loss is legal, invention is not.
		for _, e := range burst {
			rows := persistedByRun[e.RunID]
			byseq := map[uint64]string{}
			for _, r := range rows {
				byseq[r.Sequence] = r.Type
			}
			for _, d := range deliveredForRun(c.Events(), e.RunID) {
				if typ, ok := byseq[d.Sequence]; !ok {
					slowDefects = append(slowDefects, fmt.Sprintf("%s delivered %d for %s which is not persisted", c.Member, d.Sequence, e.RunID))
				} else if d.Type != "" && typ != d.Type {
					slowDefects = append(slowDefects, fmt.Sprintf("%s delivered %d as %s, persisted as %s", c.Member, d.Sequence, d.Type, typ))
				}
			}
		}
	}
	if len(slowDefects) > 0 {
		sr.failf(t, rec, "slow-consumer deliveries contradict the store: %v", slowDefects)
	}
	if behind == 0 {
		sr.blockf(t, rec, "back-pressure not observed: every slow consumer had reached the store tip %d before close", tipClose)
	}

	// The server must release every slow subscription once its client goes.
	settle := map[string]float64{}
	settleCtx, settleCancel := context.WithTimeout(ctx, cfg.SettleBound.Duration)
	err = cluster.Poll(settleCtx, 3*time.Second, func() (bool, error) {
		ok := true
		for _, m := range sr.members() {
			g, gerr := sr.goroutines(settleCtx, m)
			if gerr != nil {
				return false, nil
			}
			settle[m.Name] = g
			if base, has := before[m.Name]; has && g > base+float64(cfg.GoroutineSlack) {
				ok = false
			}
		}
		return ok, nil
	})
	settleCancel()
	if err != nil {
		sr.failf(t, rec, "server goroutines did not return within %d of the pre-episode count after slow consumers closed: before=%v after=%v", cfg.GoroutineSlack, before, settle)
	}
	rec.Observations = map[string]any{
		"job_id": job.ID, "observer_member": observerMember.Name, "store_tip": tip, "store_tip_at_close": tipClose,
		"consumers": consumers, "behind_at_close": behind, "trigger_latency_s": latencies,
		"history": reports, "goroutines_before": before, "goroutines_after": settle,
		"goroutine_slack": cfg.GoroutineSlack, "observer_connection": obsConn,
		"observer_server_cuts": serverCuts, "observer_connections": len(obsConns),
	}
}

// ---------------------------------------------------------------------------
// queue_overload
// ---------------------------------------------------------------------------

func queueDefinition(alias, taskImage string) jobdef.Definition {
	script := fmt.Sprintf(`set -eu
RECORDER=%q
STEP=%q
SEQ="${CAESIUM_PARAM_SEQ:-x}"
HOLD="${CAESIUM_PARAM_HOLD:-0}"
SLEEP="${CAESIUM_PARAM_SLEEP:-1}"
NONCE="q${SEQ}-$(cat /proc/sys/kernel/random/uuid)"
post() { i=0; until wget -qO- -T 5 --header='Content-Type: application/json' --post-data="$2" "${RECORDER}/$1" >/dev/null; do i=$((i + 1)); [ "$i" -lt 6 ] || return 1; sleep 1; done; }
post start "{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"start\"}"
if [ "$HOLD" = "1" ]; then
  i=0
  until wget -qO- "${RECORDER}/wait?run_id=${CAESIUM_RUN_ID}" | grep -q released; do
    i=$((i + 1))
    [ "$i" -lt 300 ] || exit 1
    sleep 1
  done
else
  sleep "$SLEEP"
fi
post effect "{\"run_id\":\"${CAESIUM_RUN_ID}\",\"step\":\"${STEP}\",\"nonce\":\"${NONCE}\",\"event\":\"complete\"}"
`, cluster.RecorderURL(), soakStepHold)
	return jobdef.Definition{
		APIVersion: jobdef.APIVersionV1,
		Kind:       jobdef.KindJob,
		Metadata: jobdef.Metadata{
			Alias:       alias,
			Labels:      map[string]string{"caesium-robustness": "soak-queue"},
			Concurrency: &jobdef.Concurrency{MaxRuns: 1, Strategy: jobdef.ConcurrencyStrategyQueue},
		},
		Trigger: jobdef.Trigger{
			Type:          jobdef.TriggerHTTP,
			Configuration: map[string]any{"path": "robustness-soak-" + alias},
		},
		Steps: []jobdef.Step{{
			Name:    soakStepHold,
			Type:    jobdef.StepTypeTask,
			Engine:  jobdef.EngineKubernetes,
			Image:   taskImage,
			Command: []string{"sh", "-c", script},
		}},
	}
}

func (sr *soakRunner) queueDepth(ctx context.Context, m cluster.Member, jobID string) (int, error) {
	status, raw, err := sr.fe.httpAPI.Do(ctx, http.MethodGet, m.HTTPBase()+"/v1/jobs/"+jobID+"/queue", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("queue status %d: %s", status, truncate(raw, 300))
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, fmt.Errorf("queue body: %w", err)
	}
	return len(rows), nil
}

func (sr *soakRunner) episodeQueueOverload(t *testing.T, ep SoakEpisode, rec *episodeRecord) {
	ctx := context.Background()
	job := sr.applyDefinition(t, sr.leader(), queueDefinition("soak-queue-"+shortID(), sr.fe.env.TaskImage))
	sr.mu.Lock()
	sr.queueJobs = append(sr.queueJobs, job.ID)
	sr.mu.Unlock()
	hold := strconv.Itoa(ep.Params["hold_seconds"])
	source := ep.Key()

	pick := func(i int) cluster.Member {
		live := sr.liveMembers()
		return live[ep.Draw(i, len(live))]
	}
	// startCertain resolves an uncertain answer by replaying the same key.
	startCertain := func(i int, key string, params map[string]string, prio string) *soakRun {
		e := sr.startTracked(ctx, pick(i), job.ID, key, params, prio, source, []string{soakStepHold}, false)
		if e.Err != "" || e.HTTPStatus >= 500 || e.HTTPStatus == 0 {
			if err := sr.reconcile(ctx, e); err != nil {
				sr.blockf(t, rec, "queue start %s uncertain: %v", key, err)
			}
		}
		return e
	}

	holder := startCertain(0, sr.key(fmt.Sprintf("e%02d-q", ep.Index), 0), map[string]string{"seq": "0", "hold": "1"}, "")
	if holder.Outcome != "created" || holder.RunID == "" {
		sr.blockf(t, rec, "the slot holder was not created (outcome=%q status=%d): the job slot was not free", holder.Outcome, holder.HTTPStatus)
	}
	hctx, hcancel := context.WithTimeout(ctx, 2*time.Minute)
	err := cluster.Poll(hctx, time.Second, func() (bool, error) {
		return len(sr.fe.sink.StartsFor(holder.RunID, soakStepHold)) > 0, nil
	})
	hcancel()
	if err != nil {
		sr.blockf(t, rec, "slot holder %s never started: %v", holder.RunID, err)
	}

	var subs []QueueSubmission
	var entries []*soakRun
	seq := 1
	for _, class := range []struct {
		prio string
		n    int
	}{{soakQueueLow, ep.Params["low"]}, {soakQueueHigh, ep.Params["high"]}} {
		for i := 0; i < class.n; i++ {
			key := sr.key(fmt.Sprintf("e%02d-q", ep.Index), seq)
			e := startCertain(seq, key, map[string]string{"seq": strconv.Itoa(seq), "hold": "0", "sleep": hold}, class.prio)
			if e.Outcome != "queued" {
				sr.failf(t, rec, "start %d (%s) above maxRuns=1 answered %q (HTTP %d, %s), want queued", seq, class.prio, e.Outcome, e.HTTPStatus, e.Err)
			}
			subs = append(subs, QueueSubmission{Seq: seq, Priority: class.prio})
			entries = append(entries, e)
			seq++
		}
	}
	dctx, dcancel := context.WithTimeout(ctx, 30*time.Second)
	pending, err := sr.queueDepth(dctx, sr.leader(), job.ID)
	dcancel()
	if err != nil {
		sr.blockf(t, rec, "read queue depth: %v", err)
	}
	wantPending := len(subs)
	if wantPending > sr.depth {
		wantPending = sr.depth
	}
	if pending != wantPending {
		sr.failf(t, rec, "queue holds %d pending starts after %d submissions, want min(submissions, depth %d)=%d", pending, len(subs), sr.depth, wantPending)
	}
	exp := ExpectQueue(subs, sr.depth)

	sr.fe.sink.Release(holder.RunID)
	if _, err := sr.waitTerminal(ctx, job.ID, holder.RunID, sr.w.RunBound.Duration); err != nil {
		sr.failf(t, rec, "slot holder: %v", err)
	}

	// Every key is replayed until its start is no longer queued: it either
	// became a run (the promotion rewrites the idempotency record) or was
	// dropped by the depth bound.
	bound := sr.w.RunBound.Duration + time.Duration(len(subs)*(ep.Params["hold_seconds"]+15))*time.Second
	resolveCtx, resolveCancel := context.WithTimeout(ctx, bound)
	outcomes := map[int]startOutcome{}
	err = cluster.Poll(resolveCtx, 3*time.Second, func() (bool, error) {
		for i, e := range entries {
			if o, ok := outcomes[subs[i].Seq]; ok && o.Outcome != "queued" {
				continue
			}
			o := sr.start(resolveCtx, pick(100+i), job.ID, e.Key, e.Params, e.Priority)
			if o.uncertain() {
				return false, nil
			}
			outcomes[subs[i].Seq] = o
		}
		for _, o := range outcomes {
			if o.Outcome == "queued" {
				return false, nil
			}
		}
		return len(outcomes) == len(entries), nil
	})
	resolveCancel()
	if err != nil {
		sr.failf(t, rec, "queued starts did not resolve within %s: %v", bound, jsonString(outcomes))
	}

	var dropped []int
	runBySeq := map[int]string{}
	seqByRun := map[string]int{}
	for i, e := range entries {
		o := outcomes[subs[i].Seq]
		switch o.Outcome {
		case "dropped":
			dropped = append(dropped, subs[i].Seq)
		case "created":
			runBySeq[subs[i].Seq] = o.RunID
			seqByRun[o.RunID] = subs[i].Seq
			sr.mu.Lock()
			e.RunID, e.Outcome, e.Reconciled = o.RunID, "created", true
			sr.mu.Unlock()
		default:
			sr.failf(t, rec, "start %d replayed as %q (HTTP %d)", subs[i].Seq, o.Outcome, o.HTTPStatus)
		}
	}
	sort.Ints(dropped)
	if jsonString(nilToEmpty(dropped)) != jsonString(nilToEmpty(exp.Dropped)) {
		sr.failf(t, rec, "drop-oldest bound violated: dropped %v, expected %v (depth %d, submissions %v)", dropped, exp.Dropped, sr.depth, subs)
	}
	runIDs := []string{holder.RunID}
	for _, s := range exp.StartOrder {
		id, ok := runBySeq[s]
		if !ok {
			sr.failf(t, rec, "retained start %d never became a run", s)
		}
		runIDs = append(runIDs, id)
		if final, err := sr.waitTerminal(ctx, job.ID, id, sr.w.RunBound.Duration); err != nil || !strings.EqualFold(final.Status, "succeeded") {
			sr.failf(t, rec, "promoted run %s (start %d): status %q err %v (tasks: %s)", id, s, final.Status, err, runTaskSummary(final))
		}
	}
	ivs, missing := RunIntervals(sr.fe.sink.Events(), runIDs)
	if len(missing) > 0 {
		sr.blockf(t, rec, "effect ledger lacks a start or completion for runs %v", missing)
	}
	if overlaps := OverlappingRuns(ivs); len(overlaps) > 0 {
		sr.failf(t, rec, "maxRuns=1 violated: %v", overlaps)
	}
	var observedOrder []int
	for _, iv := range ivs {
		if iv.RunID == holder.RunID {
			continue
		}
		observedOrder = append(observedOrder, seqByRun[iv.RunID])
	}
	if jsonString(nilToEmpty(observedOrder)) != jsonString(nilToEmpty(exp.StartOrder)) {
		sr.failf(t, rec, "dequeue order %v, expected priority DESC then FIFO %v", observedOrder, exp.StartOrder)
	}
	qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
	left, err := sr.queueDepth(qctx, sr.leader(), job.ID)
	qcancel()
	if err != nil || left != 0 {
		sr.failf(t, rec, "queue not empty after drain: %d (%v)", left, err)
	}
	rec.Observations = map[string]any{
		"job_id": job.ID, "holder_run": holder.RunID, "depth": sr.depth, "submissions": subs,
		"pending_while_held": pending, "expected": exp, "dropped": dropped,
		"observed_start_order": observedOrder, "intervals": ivs,
		"overflow_exercised": len(subs) > sr.depth,
	}
}

// runTaskSummary names each public task's status, attempt and error, so a
// failed run's record says why without the (truncated) server logs.
func runTaskSummary(r cluster.Run) string {
	var parts []string
	for _, tk := range r.Tasks {
		id := tk.TaskID
		if len(id) > 8 {
			id = id[:8]
		}
		part := fmt.Sprintf("%s:%s#%d", id, tk.Status, tk.Attempt)
		if tk.ClaimedBy != "" {
			part += "@" + tk.ClaimedBy
		}
		if tk.Error != "" {
			part += " error=" + truncate([]byte(tk.Error), 200)
		}
		parts = append(parts, part)
	}
	if r.Error != "" {
		parts = append(parts, "run error="+truncate([]byte(r.Error), 200))
	}
	return strings.Join(parts, "; ")
}

func nilToEmpty(v []int) []int {
	if v == nil {
		return []int{}
	}
	return v
}

// ---------------------------------------------------------------------------
// retention
// ---------------------------------------------------------------------------

func (sr *soakRunner) scalar(ctx context.Context, m cluster.Member, sql string) (int64, error) {
	_, raw, err := sr.fe.httpAPI.Query(ctx, m.HTTPBase(), sql, 1)
	if err != nil {
		return 0, fmt.Errorf("scalar query %q: %w", sql, err)
	}
	var resp cluster.QueryResponse
	if err := sqlcell.Decode(raw, &resp); err != nil {
		return 0, fmt.Errorf("scalar query %q decode: %w", sql, err)
	}
	if resp.Truncated {
		return 0, fmt.Errorf("scalar query %q returned truncated evidence", sql)
	}
	if len(resp.Rows) == 0 || len(resp.Rows[0]) == 0 {
		return 0, fmt.Errorf("%q returned no row", sql)
	}
	n, err := sqlcell.Int64(resp.Rows[0][0])
	if err != nil {
		return 0, fmt.Errorf("scalar query %q cell: %w", sql, err)
	}
	return n, nil
}

func queryText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		if b, err := cluster.DecodeBlob(x); err == nil && len(b) == 16 {
			if id, err := uuid.FromBytes(b); err == nil {
				return id.String()
			}
		}
		return x
	default:
		return fmt.Sprint(x)
	}
}

func (sr *soakRunner) checkpointSeqs(ctx context.Context, m cluster.Member, runID string) ([]int64, error) {
	if _, err := uuid.Parse(runID); err != nil {
		return nil, err
	}
	_, raw, err := sr.fe.httpAPI.Query(ctx, m.HTTPBase(), fmt.Sprintf("SELECT sequence_high FROM run_checkpoints WHERE run_id = '%s' ORDER BY sequence_high", runID), 100)
	if err != nil {
		return nil, fmt.Errorf("checkpoint sequences for run %s: %w", runID, err)
	}
	var resp cluster.QueryResponse
	if err := sqlcell.Decode(raw, &resp); err != nil {
		return nil, fmt.Errorf("checkpoint sequences for run %s decode: %w", runID, err)
	}
	if resp.Truncated {
		return nil, fmt.Errorf("checkpoint sequences for run %s returned truncated evidence", runID)
	}
	var out []int64
	for i, row := range resp.Rows {
		if len(row) == 0 {
			return nil, fmt.Errorf("checkpoint sequences for run %s row %d has no sequence cell", runID, i)
		}
		n, err := sqlcell.Int64(row[0])
		if err != nil {
			return nil, fmt.Errorf("checkpoint sequences for run %s row %d: %w", runID, i, err)
		}
		out = append(out, n)
	}
	return out, nil
}

type checkpointPoller struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func startCheckpointPoller(parent context.Context, interval, queryTimeout time.Duration,
	query func(context.Context) ([]int64, error), observe func([]int64, error),
) *checkpointPoller {
	ctx, cancel := context.WithCancel(parent)
	poller := &checkpointPoller{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(poller.done)
		for {
			if ctx.Err() != nil {
				return
			}
			queryCtx, queryCancel := context.WithTimeout(ctx, queryTimeout)
			seqs, err := query(queryCtx)
			queryCancel()
			if ctx.Err() != nil {
				return
			}
			observe(seqs, err)

			timer := time.NewTimer(interval)
			select {
			case <-ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				return
			case <-timer.C:
			}
		}
	}()
	return poller
}

func (p *checkpointPoller) stop() {
	p.once.Do(func() {
		p.cancel()
		<-p.done
	})
}

func (sr *soakRunner) episodeRetention(t *testing.T, ep SoakEpisode, rec *episodeRecord) {
	ctx := context.Background()
	cfg := sr.w.Retention
	leader := sr.leader()
	burstJob := sr.applyChain(t, leader, "burst", []string{firstStep, secondStep}, 0, nil)
	var longSteps []string
	for i := 0; i < ep.Params["long_steps"]; i++ {
		longSteps = append(longSteps, fmt.Sprintf("s%02d", i))
	}
	longJob := sr.applyChain(t, leader, "long", longSteps, ep.Params["long_step_seconds"], nil)

	cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
	runsBefore, err1 := sr.scalar(cctx, leader, "SELECT COUNT(*) FROM job_runs")
	eventsBefore, err2 := sr.scalar(cctx, leader, "SELECT COUNT(*) FROM execution_events")
	ccancel()
	if err1 != nil || err2 != nil {
		sr.blockf(t, rec, "retention baseline counts: %v %v", err1, err2)
	}

	long := sr.startTracked(ctx, sr.liveMembers()[ep.Draw(0, len(sr.liveMembers()))], longJob.ID, sr.key(fmt.Sprintf("e%02d-long", ep.Index), 0), nil, "", ep.Key(), longSteps, true)
	if long.RunID == "" {
		sr.failf(t, rec, "long run not admitted: %+v", *long)
	}
	var obs []CheckpointObservation
	var pollErrs int
	poller := startCheckpointPoller(ctx, time.Second, 10*time.Second,
		func(pctx context.Context) ([]int64, error) {
			return sr.checkpointSeqs(pctx, sr.leader(), long.RunID)
		},
		func(seqs []int64, err error) {
			if err != nil {
				pollErrs++
			} else {
				obs = append(obs, CheckpointObservation{At: time.Now().UTC(), Sequences: seqs})
			}
		},
	)
	defer poller.stop()

	// Sustained burst, from several members at once.
	var mu sync.Mutex
	var burst []*soakRun
	var wg sync.WaitGroup
	sem := make(chan struct{}, cfg.TriggerParallism)
	for i := 0; i < ep.Params["burst_runs"]; i++ {
		i := i
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			live := sr.liveMembers()
			m := live[ep.Draw(2+i, len(live))]
			tctx, tcancel := context.WithTimeout(ctx, 20*time.Second)
			e := sr.startTracked(tctx, m, burstJob.ID, sr.key(fmt.Sprintf("e%02d-burst", ep.Index), i), nil, "", ep.Key(), []string{firstStep, secondStep}, true)
			tcancel()
			if e.uncertainOrEmpty() {
				_ = sr.reconcile(ctx, e)
			}
			mu.Lock()
			burst = append(burst, e)
			mu.Unlock()
		}()
	}
	wg.Wait()
	admitted := map[string]bool{}
	for _, e := range burst {
		if e.RunID == "" {
			sr.failf(t, rec, "burst start %s not admitted: %+v", e.Key, *e)
		}
		admitted[e.RunID] = true
	}
	for _, e := range append(burst, long) {
		final, err := sr.waitTerminal(ctx, e.JobID, e.RunID, sr.w.RunBound.Duration+time.Duration(len(longSteps)*(ep.Params["long_step_seconds"]+10))*time.Second)
		if err != nil || !strings.EqualFold(final.Status, "succeeded") {
			sr.failf(t, rec, "retention run %s: status %q err %v (tasks: %s)", e.RunID, final.Status, err, runTaskSummary(final))
		}
	}
	time.Sleep(3 * time.Second)
	poller.stop()
	if len(obs) < 3 {
		sr.blockf(t, rec, "only %d checkpoint polls succeeded (%d errors)", len(obs), pollErrs)
	}
	ckpt := SummariseCheckpoints(obs)
	if v := ckpt.Violations(cfg.KeepFulls); len(v) > 0 {
		sr.failf(t, rec, "checkpoint retention bound violated: %v (summary %s)", v, jsonString(ckpt))
	}
	if len(ckpt.Pruned) == 0 {
		sr.blockf(t, rec, "the pruning path was not observed: written %v, final %v", ckpt.Written, ckpt.Final)
	}
	for i := 1; i < len(longSteps); i++ {
		if err := CheckStepOrder(sr.fe.sink.Events(), long.RunID, longSteps[i-1], longSteps[i]); err != nil {
			sr.failf(t, rec, "long run ordering: %v", err)
		}
	}

	// Retained history grows by what was admitted, and every member serves it.
	perMember := map[string]int{}
	for _, m := range sr.members() {
		qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
		resp, _, err := sr.fe.httpAPI.Query(qctx, m.HTTPBase(), fmt.Sprintf("SELECT id, status FROM job_runs WHERE job_id = '%s'", burstJob.ID), 1000)
		qcancel()
		if err != nil {
			sr.blockf(t, rec, "retained runs via %s: %v", m.Name, err)
		}
		seen := map[string]bool{}
		for _, row := range resp.Rows {
			if len(row) < 2 {
				continue
			}
			id := queryText(row[0])
			if !admitted[id] {
				sr.failf(t, rec, "%s retains run %s that was never admitted", m.Name, id)
			}
			if !strings.EqualFold(queryText(row[1]), "succeeded") {
				sr.failf(t, rec, "%s retains run %s as %v", m.Name, id, row[1])
			}
			seen[id] = true
		}
		if len(seen) != len(admitted) {
			sr.failf(t, rec, "%s retains %d of %d admitted burst runs", m.Name, len(seen), len(admitted))
		}
		perMember[m.Name] = len(seen)
	}
	var persistedTotal int
	for _, e := range burst {
		rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
		rows, _, err := readPersistedEvents(rctx, sr.fe.httpAPI, sr.leader().HTTPBase(), e.RunID, 2000)
		rcancel()
		if err != nil {
			sr.blockf(t, rec, "events for %s: %v", e.RunID, err)
		}
		succeeded := 0
		for _, r := range rows {
			if r.Type == "task_succeeded" {
				succeeded++
			}
		}
		if succeeded < 2 {
			sr.failf(t, rec, "run %s retains %d task_succeeded events for 2 succeeded steps", e.RunID, succeeded)
		}
		if err := CheckStepOrder(sr.fe.sink.Events(), e.RunID, firstStep, secondStep); err != nil {
			sr.failf(t, rec, "burst ordering: %v", err)
		}
		persistedTotal += len(rows)
	}
	actx, acancel := context.WithTimeout(ctx, 30*time.Second)
	runsAfter, err1 := sr.scalar(actx, sr.leader(), "SELECT COUNT(*) FROM job_runs")
	eventsAfter, err2 := sr.scalar(actx, sr.leader(), "SELECT COUNT(*) FROM execution_events")
	acancel()
	if err1 != nil || err2 != nil {
		sr.blockf(t, rec, "retention counts after: %v %v", err1, err2)
	}
	if runsAfter < runsBefore+int64(len(burst)+1) {
		sr.failf(t, rec, "job_runs grew %d -> %d for %d admitted runs", runsBefore, runsAfter, len(burst)+1)
	}
	if eventsAfter < eventsBefore+int64(persistedTotal) {
		sr.failf(t, rec, "execution_events grew %d -> %d but the burst alone persisted %d", eventsBefore, eventsAfter, persistedTotal)
	}
	rec.Observations = map[string]any{
		"burst_job": burstJob.ID, "long_job": longJob.ID, "long_run": long.RunID,
		"burst_runs": len(burst), "long_steps": len(longSteps),
		"job_runs_before": runsBefore, "job_runs_after": runsAfter,
		"events_before": eventsBefore, "events_after": eventsAfter, "burst_persisted_events": persistedTotal,
		"retained_per_member": perMember, "checkpoints": ckpt, "keep_fulls": cfg.KeepFulls,
		"checkpoint_poll_errors": pollErrs,
	}
}

func (e *soakRun) uncertainOrEmpty() bool {
	return e.RunID == "" && (e.Err != "" || e.HTTPStatus == 0 || e.HTTPStatus >= 500)
}

// ---------------------------------------------------------------------------
// repeated_failover
// ---------------------------------------------------------------------------

func (sr *soakRunner) restartNode(ep *SoakEpisode, m cluster.Member) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	_, err := sr.hostRequest(ctx, ep, cluster.HostRequest{Action: cluster.ActionRestart, OwnerPod: m.Name, OwnerKindNode: m.Node})
	return err
}

// waitRejoin requires the SAME pod (UID, IP and volume retained) Ready again
// and the three live voters restored.
func (sr *soakRunner) waitRejoin(ctx context.Context, old cluster.Member, bound time.Duration) (soakMembership, error) {
	rctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	var last error
	err := cluster.Poll(rctx, 2*time.Second, func() (bool, error) {
		back, err := cluster.RefreshMember(rctx, sr.fe.kube, sr.fe.env.Namespace, old.Name)
		if err != nil {
			last = err
			return false, nil
		}
		if back.UID != old.UID {
			return false, fmt.Errorf("pod UID changed on restart: %s -> %s", old.UID, back.UID)
		}
		if back.VolumeName != "" && old.VolumeName != "" && back.VolumeName != old.VolumeName {
			return false, fmt.Errorf("PVC volume changed: %s -> %s", old.VolumeName, back.VolumeName)
		}
		if !cluster.PodReady(&back.Pod) {
			last = fmt.Errorf("%s not Ready", old.Name)
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return soakMembership{}, fmt.Errorf("%w (last %v)", err, last)
	}
	return sr.waitHealthy(ctx, bound)
}

func (sr *soakRunner) killMember(t *testing.T, ep *SoakEpisode, rec *episodeRecord, m cluster.Member, runID string) (string, time.Time) {
	t.Helper()
	ctx := context.Background()
	live, err := cluster.RefreshMember(ctx, sr.fe.kube, sr.fe.env.Namespace, m.Name)
	if err != nil || live.ContainerID == "" {
		sr.blockf(t, rec, "refresh %s container: %v", m.Name, err)
	}
	kctx, kcancel := context.WithTimeout(ctx, 90*time.Second)
	ack, err := sr.hostRequest(kctx, ep, cluster.HostRequest{
		Action: cluster.ActionKill, OwnerPod: m.Name, OwnerKindNode: m.Node,
		OwnerContainerID: live.ContainerID, RunID: runID,
	})
	kcancel()
	if err != nil {
		sr.blockf(t, rec, "kill %s: %v", m.Name, err)
	}
	if !killEvidenceShowsDeath(ack.Evidence, live.ContainerID) {
		sr.blockf(t, rec, "kill evidence did not show %s stopped:\n%s", live.ContainerID, truncate([]byte(ack.Evidence), 800))
	}
	return live.ContainerID, time.Now().UTC()
}

func (sr *soakRunner) cordon(t *testing.T, ep *SoakEpisode, rec *episodeRecord, m cluster.Member) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := sr.hostRequest(ctx, ep, cluster.HostRequest{Action: cluster.ActionCordon, OwnerPod: m.Name, OwnerKindNode: m.Node}); err != nil {
		sr.blockf(t, rec, "cordon %s: %v", m.Node, err)
	}
}

func (sr *soakRunner) episodeFailover(t *testing.T, ep SoakEpisode, rec *episodeRecord) {
	var kills []map[string]any
	for k, kind := range ep.Kinds {
		ms, err := sr.waitHealthy(context.Background(), 3*time.Minute)
		if err != nil {
			sr.blockf(t, rec, "kill %d (%s): cluster not healthy before the kill: %v", k, kind, err)
		}
		var obs map[string]any
		switch kind {
		case KillOwnerIsLeader, KillOwnerIsNotLeader:
			obs = sr.ownerKill(t, &ep, rec, k, kind == KillOwnerIsLeader, ms)
		case KillLeaderOnly:
			obs = sr.leaderKill(t, &ep, rec, k, ms)
		default:
			sr.blockf(t, rec, "unknown kill kind %q", kind)
		}
		obs["progress_after_heal"] = sr.progressProbe(t, ep, rec, fmt.Sprintf("kill%d", k), 12+k)
		kills = append(kills, obs)
		rec.Observations["kills"] = kills
	}
}

func (sr *soakRunner) ownerKill(t *testing.T, ep *SoakEpisode, rec *episodeRecord, k int, wantLeader bool, ms soakMembership) map[string]any {
	ctx := context.Background()
	kind := KillOwnerIsNotLeader
	if wantLeader {
		kind = KillOwnerIsLeader
	}
	members := sr.members()
	topo := cluster.Topology{Members: members}
	leader, _ := topo.ByName(ms.LeaderPod)
	owner := leader
	if !wantLeader {
		survivors := topo.Survivors(leader)
		owner = survivors[ep.Draw(4+k, len(survivors))]
	}
	job := sr.applyDefinition(t, owner, cluster.FixtureDefinition(fmt.Sprintf("soak-owner-%d-%s", k, shortID()), sr.fe.env.TaskImage))
	sr.cordon(t, ep, rec, owner)
	restarted := false
	defer func() {
		if !restarted {
			_ = sr.restartNode(ep, owner)
			sr.setFaulted(owner.Name, "")
		}
	}()
	e := sr.startTracked(ctx, owner, job.ID, sr.key(fmt.Sprintf("e%02d-owner%d", ep.Index, k), 0), nil, "", ep.Key(), []string{cluster.BlockStep, cluster.SuccessorStep}, false)
	if e.RunID == "" {
		sr.blockf(t, rec, "owner fixture not admitted via %s: %+v", owner.Name, *e)
	}
	survivor := topo.Survivors(owner)[0]
	lctx, lcancel := context.WithTimeout(ctx, 90*time.Second)
	lease, err := cluster.WaitLease(lctx, sr.fe.httpAPI, survivor.HTTPBase(), e.RunID, owner.NodeAddress)
	lcancel()
	if err != nil {
		sr.blockf(t, rec, "lease before kill: %v", err)
	}
	sctx, scancel := context.WithTimeout(ctx, 90*time.Second)
	err = cluster.Poll(sctx, 500*time.Millisecond, func() (bool, error) {
		return len(sr.fe.sink.StartsFor(e.RunID, cluster.BlockStep)) > 0, nil
	})
	scancel()
	if err != nil {
		sr.blockf(t, rec, "blocked step never started before the kill: %v", err)
	}
	mctx, mcancel := context.WithTimeout(ctx, 20*time.Second)
	now, err := sr.queryMembership(mctx, members)
	mcancel()
	if err != nil {
		sr.blockf(t, rec, "membership before kill: %v", err)
	}
	if (now.LeaderPod == owner.Name) != wantLeader {
		sr.blockf(t, rec, "%s invalidated: leader moved to %s before the kill", kind, now.LeaderPod)
	}
	sr.setFaulted(owner.Name, kind)
	cid, killedAt := sr.killMember(t, ep, rec, owner, e.RunID)
	sr.fe.sink.Release(e.RunID)

	var recovered cluster.Lease
	var final cluster.Run
	rctx, rcancel := context.WithTimeout(ctx, sr.w.Failover.RecoveryBound.Duration)
	err = cluster.Poll(rctx, time.Second, func() (bool, error) {
		got, err := sr.fe.httpAPI.QueryLease(rctx, survivor.HTTPBase(), e.RunID)
		if err != nil || got.Generation <= lease.Generation || got.OwnerNode == owner.NodeAddress {
			return false, nil
		}
		recovered = got
		run, err := sr.fe.httpAPI.GetRun(rctx, survivor.HTTPBase(), job.ID, e.RunID)
		if err != nil || !strings.EqualFold(run.Status, "succeeded") {
			return false, nil
		}
		final = run
		return true, nil
	})
	rcancel()
	if err != nil {
		sr.failf(t, rec, "%s: no takeover and completion within %s (lease %+v -> %+v)", kind, sr.w.Failover.RecoveryBound, lease, recovered)
	}
	recoverySeconds := time.Since(killedAt).Seconds()
	if err := checkSuccessorOrdering(sr.fe.sink.Events(), e.RunID, cluster.BlockStep, cluster.SuccessorStep); err != nil {
		sr.failf(t, rec, "%s successor ordering: %v", kind, err)
	}
	catalog, err := sr.fe.httpAPI.ListJobTasks(ctx, survivor.HTTPBase(), job.ID)
	if err != nil {
		sr.blockf(t, rec, "task catalog: %v", err)
	}
	names := map[string]string{}
	for _, ct := range catalog {
		names[ct.ID] = ct.Name
	}
	var pub []publicTask
	for _, tr := range final.Tasks {
		pub = append(pub, publicTask{Step: names[tr.TaskID], Status: tr.Status})
	}
	if err := checkPublicTerminalAgreement(pub, cluster.BlockStep, cluster.SuccessorStep); err != nil {
		sr.failf(t, rec, "%s public task states: %v", kind, err)
	}
	sr.mu.Lock()
	e.Final = final.Status
	sr.mu.Unlock()

	if err := sr.restartNode(ep, owner); err != nil {
		sr.failf(t, rec, "restart %s: %v", owner.Name, err)
	}
	restarted = true
	after, err := sr.waitRejoin(ctx, owner, sr.w.Failover.RejoinBound.Duration)
	if err != nil {
		sr.failf(t, rec, "%s did not rejoin with its retained volume and address: %v", owner.Name, err)
	}
	sr.setFaulted(owner.Name, "")
	return map[string]any{
		"kind": kind, "owner": owner.Name, "owner_node": owner.Node, "container_id": cid,
		"killed_at": killedAt, "run_id": e.RunID, "lease_before": lease, "lease_after": recovered,
		"recovery_seconds": recoverySeconds, "leader_before": ms.LeaderPod, "leader_after": after.LeaderPod,
		"block_starts": len(sr.fe.sink.StartsFor(e.RunID, cluster.BlockStep)),
	}
}

func (sr *soakRunner) leaderKill(t *testing.T, ep *SoakEpisode, rec *episodeRecord, k int, ms soakMembership) map[string]any {
	ctx := context.Background()
	members := sr.members()
	topo := cluster.Topology{Members: members}
	leader, ok := topo.ByName(ms.LeaderPod)
	if !ok {
		sr.blockf(t, rec, "leader %s is not a live pod", ms.LeaderPod)
	}
	survivors := topo.Survivors(leader)
	sr.cordon(t, ep, rec, leader)
	restarted := false
	defer func() {
		if !restarted {
			_ = sr.restartNode(ep, leader)
			sr.setFaulted(leader.Name, "")
		}
	}()
	sr.setFaulted(leader.Name, KillLeaderOnly)
	cid, killedAt := sr.killMember(t, ep, rec, leader, "")

	var newLeader string
	ectx, ecancel := context.WithTimeout(ctx, sr.w.Failover.RecoveryBound.Duration)
	err := cluster.Poll(ectx, time.Second, func() (bool, error) {
		agreed := ""
		for _, s := range survivors {
			l, _, err := cluster.QueryNode(ectx, s.DqliteAddr())
			if err != nil || l == nil || l.Address == "" || l.Address == leader.DqliteAddr() {
				return false, nil
			}
			if agreed != "" && agreed != l.Address {
				return false, nil
			}
			agreed = l.Address
		}
		newLeader = agreed
		return true, nil
	})
	ecancel()
	if err != nil {
		sr.failf(t, rec, "survivors did not agree on a new leader within %s after killing %s", sr.w.Failover.RecoveryBound, leader.Name)
	}
	electionSeconds := time.Since(killedAt).Seconds()
	degraded := sr.progressProbe(t, *ep, rec, fmt.Sprintf("kill%d-degraded", k), 20+k)

	if err := sr.restartNode(ep, leader); err != nil {
		sr.failf(t, rec, "restart %s: %v", leader.Name, err)
	}
	restarted = true
	if _, err := sr.waitRejoin(ctx, leader, sr.w.Failover.RejoinBound.Duration); err != nil {
		sr.failf(t, rec, "%s did not rejoin with its retained volume and address: %v", leader.Name, err)
	}
	sr.setFaulted(leader.Name, "")
	return map[string]any{
		"kind": KillLeaderOnly, "killed": leader.Name, "node": leader.Node, "container_id": cid,
		"killed_at": killedAt, "new_leader": newLeader, "election_seconds": electionSeconds,
		"progress_while_degraded": degraded,
	}
}

// ---------------------------------------------------------------------------
// node_replacement
// ---------------------------------------------------------------------------

func removalCLIAvailable() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/bin/caesium", "system", "nodes", "remove", "--help").Run() == nil
}

func (sr *soakRunner) removeStale(ctx context.Context, base string, id uint64, bound time.Duration) (string, error) {
	deadline := time.Now().Add(bound)
	var last string
	for {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(cctx, "/bin/caesium", "system", "nodes", "remove", strconv.FormatUint(id, 10), "--server", base, "--json")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		cancel()
		last = strings.TrimSpace(stdout.String())
		if err == nil {
			return last, nil
		}
		var refusal struct {
			Status    string `json:"status"`
			Reason    string `json:"reason"`
			Retryable bool   `json:"retryable"`
		}
		_ = json.Unmarshal(stdout.Bytes(), &refusal)
		if !refusal.Retryable || time.Now().After(deadline) {
			return last, fmt.Errorf("remove %d: %v (stdout %s, stderr %s)", id, err, last, strings.TrimSpace(stderr.String()))
		}
		time.Sleep(5 * time.Second)
	}
}

func (sr *soakRunner) episodeReplacement(t *testing.T, ep SoakEpisode, rec *episodeRecord) {
	ctx := context.Background()
	var reps []map[string]any
	for r := 0; r < ep.Params["replacements"]; r++ {
		ms, err := sr.waitHealthy(ctx, 3*time.Minute)
		if err != nil {
			sr.blockf(t, rec, "replacement %d: cluster not healthy: %v", r, err)
		}
		members := sr.members()
		target := members[ep.Draw(6+r, len(members))]
		var oldID uint64
		for _, n := range ms.Members {
			if n.Address == target.DqliteAddr() {
				oldID = n.ID
			}
		}
		if oldID == 0 {
			sr.blockf(t, rec, "no raft entry for %s at %s", target.Name, target.DqliteAddr())
		}
		// A run completed before the replacement must still read back through
		// the replaced member afterwards (snapshot/log catch-up).
		var witness *soakRun
		sr.mu.Lock()
		for i := len(sr.ledger) - 1; i >= 0; i-- {
			if strings.EqualFold(sr.ledger[i].Final, "succeeded") || (sr.ledger[i].Source == "warmup" && sr.ledger[i].RunID != "") {
				witness = sr.ledger[i]
				break
			}
		}
		sr.mu.Unlock()

		sr.setFaulted(target.Name, "replacement")
		rctx, rcancel := context.WithTimeout(ctx, 2*time.Minute)
		ack, err := sr.hostRequest(rctx, &ep, cluster.HostRequest{Action: soakActionReplace, OwnerPod: target.Name, OwnerKindNode: target.Node, Params: map[string]string{"pod": target.Name}})
		rcancel()
		if err != nil {
			sr.setFaulted(target.Name, "")
			sr.blockf(t, rec, "replace %s: %v", target.Name, err)
		}
		began := time.Now()
		var fresh cluster.Member
		wctx, wcancel := context.WithTimeout(ctx, sr.w.Replacement.RejoinBound.Duration)
		err = cluster.Poll(wctx, 2*time.Second, func() (bool, error) {
			m, err := cluster.RefreshMember(wctx, sr.fe.kube, sr.fe.env.Namespace, target.Name)
			if err != nil || m.UID == target.UID || !cluster.PodReady(&m.Pod) || m.VolumeName == "" {
				return false, nil
			}
			if m.VolumeName == target.VolumeName {
				return false, fmt.Errorf("replacement %s kept its old volume %s: disk loss was not injected", target.Name, m.VolumeName)
			}
			fresh = m
			return true, nil
		})
		wcancel()
		if err != nil {
			sr.failf(t, rec, "replacement %s did not come back Ready on a fresh volume within %s: %v", target.Name, sr.w.Replacement.RejoinBound, err)
		}
		after, err := sr.waitHealthy(ctx, sr.w.Replacement.RejoinBound.Duration)
		if err != nil {
			sr.failf(t, rec, "replaced %s did not rejoin as a live voter: %v", target.Name, err)
		}
		var newID uint64
		staleSeen := false
		for _, n := range after.Members {
			if n.Address == fresh.DqliteAddr() {
				newID = n.ID
			}
			if n.ID == oldID {
				staleSeen = true
			}
		}
		if newID == 0 || newID == oldID {
			sr.failf(t, rec, "replacement %s joined with id %d (old %d)", target.Name, newID, oldID)
		}
		obs := map[string]any{
			"target": target.Name, "node": target.Node, "old_uid": target.UID, "new_uid": fresh.UID,
			"old_volume": target.VolumeName, "new_volume": fresh.VolumeName, "old_ip": target.IP, "new_ip": fresh.IP,
			"old_id": oldID, "new_id": newID, "stale_entry_after_join": staleSeen,
			"rejoin_seconds": time.Since(began).Seconds(), "host_evidence": truncate([]byte(ack.Evidence), 600),
			"leader_before": ms.LeaderPod, "leader_after": after.LeaderPod,
		}
		if staleSeen && sr.haveRemove {
			out, err := sr.removeStale(ctx, sr.leader().HTTPBase(), oldID, 2*time.Minute)
			obs["removal_output"] = out
			if err != nil {
				sr.failf(t, rec, "operator removal of stale member %d: %v", oldID, err)
			}
			final, err := sr.waitHealthy(ctx, 2*time.Minute)
			if err != nil {
				sr.failf(t, rec, "cluster after removal: %v", err)
			}
			if len(final.Members) != 3 || len(final.StaleEntries) != 0 {
				sr.failf(t, rec, "after removal the configuration is %s, want exactly the three live voters", jsonString(final.Members))
			}
			obs["members_after_removal"] = final.Members
		} else if staleSeen {
			obs["stale_entry_retained"] = "this candidate has no operator removal path; the entry is recorded, not removed"
		}
		if witness != nil && witness.RunID != "" {
			gctx, gcancel := context.WithTimeout(ctx, 60*time.Second)
			got, err := sr.fe.httpAPI.GetRun(gctx, fresh.HTTPBase(), witness.JobID, witness.RunID)
			gcancel()
			if err != nil || !strings.EqualFold(got.Status, "succeeded") {
				sr.failf(t, rec, "pre-replacement run %s does not read back through the replaced member: status %q err %v", witness.RunID, got.Status, err)
			}
			obs["witness_run"] = witness.RunID
		}
		sr.setFaulted(target.Name, "")
		obs["progress_after_heal"] = sr.progressProbe(t, ep, rec, fmt.Sprintf("replace%d", r), 10+r)
		reps = append(reps, obs)
		rec.Observations["replacements"] = reps
	}
}

// ---------------------------------------------------------------------------
// drain: whole-schedule safety, then the host's post-drain samples.
// ---------------------------------------------------------------------------

type taskPodInventoryEntry struct {
	Pod   string `json:"pod"`
	Phase string `json:"phase"`
	Owned bool   `json:"owned"`
}

type containerInventoryEvidence struct {
	Counts   map[string]int          `json:"counts"`
	Error    string                  `json:"error"`
	TaskPods []taskPodInventoryEntry `json:"task_pods"`
}

type finalDrainObservation struct {
	podInventoryObserved       bool
	podInventoryReadable       bool
	taskPods                   []corev1.Pod
	containerInventoryObserved bool
	containerInventoryReadable bool
	containerInventory         containerInventoryEvidence
}

func parseContainerInventoryEvidence(raw string) (containerInventoryEvidence, error) {
	var evidence containerInventoryEvidence
	if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
		return evidence, fmt.Errorf("malformed container inventory: %w", err)
	}
	if evidence.Error != "" {
		return evidence, fmt.Errorf("container inventory reports an error: %s", evidence.Error)
	}
	if evidence.Counts == nil || evidence.TaskPods == nil {
		return evidence, fmt.Errorf("container inventory is missing counts or task_pods")
	}
	for _, key := range []string{"owned_task_containers", "task_running", "task_pods"} {
		count, ok := evidence.Counts[key]
		if !ok || count < 0 {
			return evidence, fmt.Errorf("container inventory has invalid %s count", key)
		}
	}
	return evidence, nil
}

func containerInventorySettled(evidence containerInventoryEvidence) bool {
	if evidence.Counts["owned_task_containers"] != 0 || evidence.Counts["task_running"] != 0 {
		return false
	}
	for _, pod := range evidence.TaskPods {
		if pod.Owned {
			return false
		}
	}
	return true
}

func finalDrainEvidence(observation finalDrainObservation, token string) (failures, inconclusive []string) {
	if !observation.podInventoryObserved || !observation.podInventoryReadable {
		inconclusive = append(inconclusive, "final Kubernetes task-pod inventory is missing or unreadable")
	}
	var ownedPods []string
	ownedPodNames := make(map[string]struct{})
	for _, pod := range observation.taskPods {
		if taskPodOwned(pod, token) {
			ownedPods = append(ownedPods, pod.Name)
			ownedPodNames[pod.Name] = struct{}{}
		}
	}
	if len(ownedPods) > 0 {
		failures = append(failures, fmt.Sprintf("%d owned task pods remain after drain: %v", len(ownedPods), ownedPods))
	}

	if !observation.containerInventoryObserved || !observation.containerInventoryReadable {
		inconclusive = append(inconclusive, "final host container inventory is missing, malformed, or reports an error")
	}
	if !observation.containerInventoryObserved {
		return failures, inconclusive
	}
	counts := observation.containerInventory.Counts
	if counts["owned_task_containers"] > 0 {
		failures = append(failures, fmt.Sprintf("%d owned task containers remain after drain", counts["owned_task_containers"]))
	}
	if counts["task_running"] > 0 {
		failures = append(failures, fmt.Sprintf("%d task containers are still running after drain", counts["task_running"]))
	}
	var hostOwnedPods []string
	for _, pod := range observation.containerInventory.TaskPods {
		if pod.Owned {
			if _, alreadyReported := ownedPodNames[pod.Pod]; pod.Pod != "" && alreadyReported {
				continue
			}
			hostOwnedPods = append(hostOwnedPods, pod.Pod)
		}
	}
	if len(hostOwnedPods) > 0 {
		failures = append(failures, fmt.Sprintf("%d owned task pods remain in host inventory: %v", len(hostOwnedPods), hostOwnedPods))
	}
	return failures, inconclusive
}

func (sr *soakRunner) drain(t *testing.T) {
	ctx := context.Background()
	rec := &episodeRecord{Key: "drain", Family: "drain", SoakID: sr.soakID, Seed: sr.seed, StartedAt: time.Now().UTC(), Observations: map[string]any{}}
	defer func() {
		rec.EndedAt = time.Now().UTC()
		rec.DurationS = rec.EndedAt.Sub(rec.StartedAt).Seconds()
		if rec.Status == "" {
			if t.Failed() {
				rec.Status = soakStatusFail
				rec.Detail = firstNonEmptyStr(rec.Detail, "an assertion failed; see runner.log")
			} else {
				rec.Status = soakStatusPass
			}
		}
		sr.mu.Lock()
		ledger := append([]*soakRun(nil), sr.ledger...)
		sr.mu.Unlock()
		chunks := 0
		for i := 0; i < len(ledger); i += 150 {
			end := i + 150
			if end > len(ledger) {
				end = len(ledger)
			}
			sr.sendRecord(t, fmt.Sprintf("ledger-%03d", chunks), map[string]any{"soak_id": sr.soakID, "seed": sr.seed, "runs": ledger[i:end]})
			chunks++
		}
		rec.Observations["ledger_chunks"] = chunks
		sr.sendRecord(t, "drain", rec)
	}()
	close(sr.trickleStop)
	<-sr.trickleDone
	scheduleSeconds := time.Since(sr.started).Seconds()
	if _, err := sr.waitHealthy(ctx, 4*time.Minute); err != nil {
		sr.blockf(t, rec, "cluster not healthy at drain: %v", err)
	}
	deadline := time.Now().Add(sr.w.Drain.Bound.Duration)
	sr.mu.Lock()
	ledger := append([]*soakRun(nil), sr.ledger...)
	sr.mu.Unlock()

	var uncertain, refused []string
	for _, e := range ledger {
		if e.RunID == "" && (e.Err != "" || e.HTTPStatus == 0 || e.HTTPStatus >= 500) {
			if err := sr.reconcile(ctx, e); err != nil {
				uncertain = append(uncertain, err.Error())
			}
		}
		if e.RunID == "" && e.Outcome != "dropped" && e.Outcome != "queued" && e.HTTPStatus != 0 && e.HTTPStatus < 500 && e.HTTPStatus != http.StatusAccepted {
			refused = append(refused, fmt.Sprintf("%s: HTTP %d", e.Key, e.HTTPStatus))
		}
	}
	if len(uncertain) > 0 {
		sr.blockf(t, rec, "%d starts stayed uncertain after replay: %v", len(uncertain), uncertain)
	}

	var failures []string
	statuses := map[string]int{}
	for _, e := range ledger {
		if e.RunID == "" {
			continue
		}
		remaining := time.Until(deadline)
		if remaining < 10*time.Second {
			remaining = 10 * time.Second
		}
		final, err := sr.waitTerminal(ctx, e.JobID, e.RunID, remaining)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		sr.mu.Lock()
		e.Final = final.Status
		sr.mu.Unlock()
		statuses[strings.ToLower(final.Status)]++
		if !strings.EqualFold(final.Status, "succeeded") {
			failures = append(failures, fmt.Sprintf("%s (%s) ended %s (tasks: %s)", e.RunID, e.Source, final.Status, runTaskSummary(final)))
			continue
		}
		if e.Checked {
			for i := 1; i < len(e.Steps); i++ {
				if err := CheckStepOrder(sr.fe.sink.Events(), e.RunID, e.Steps[i-1], e.Steps[i]); err != nil {
					failures = append(failures, err.Error())
				}
			}
		}
	}
	sr.mu.Lock()
	queueJobs := append([]string(nil), sr.queueJobs...)
	sr.mu.Unlock()
	for _, id := range queueJobs {
		qctx, qcancel := context.WithTimeout(ctx, 30*time.Second)
		n, err := sr.queueDepth(qctx, sr.leader(), id)
		qcancel()
		if err != nil || n != 0 {
			failures = append(failures, fmt.Sprintf("queue of job %s holds %d after drain (%v)", id, n, err))
		}
	}

	// Task pods are deleted by their worker on completion, and kubelet's
	// container GC (one-minute period) then removes the exited containers of
	// deleted pods. Both are waited for, inside one grace bound, before the
	// final host inventory is judged; whatever remains after it is a leak.
	graceEnd := time.Now().Add(sr.w.Drain.ContainerGrace.Duration)
	var podsLeft []string
	var lastTaskPods []corev1.Pod
	podInventoryObserved, podInventoryReadable := false, false
	gctx, gcancel := context.WithDeadline(ctx, graceEnd)
	_ = cluster.Poll(gctx, 3*time.Second, func() (bool, error) {
		pods, err := cluster.ListTaskPods(gctx, sr.fe.kube, sr.fe.env.Namespace)
		if err != nil {
			podInventoryReadable = false
			return false, nil
		}
		podInventoryObserved = true
		podInventoryReadable = true
		lastTaskPods = slices.Clone(pods)
		podsLeft = podsLeft[:0]
		ownedPods := 0
		for _, p := range pods {
			podsLeft = append(podsLeft, fmt.Sprintf("%s phase=%s node=%s owned=%t", p.Name, p.Status.Phase, p.Spec.NodeName, taskPodOwned(p, sr.token)))
			if taskPodOwned(p, sr.token) {
				ownedPods++
			}
		}
		return ownedPods == 0, nil
	})
	gcancel()
	rec.Observations["task_pods_after_grace"] = podsLeft
	rec.Observations["schedule_seconds"] = scheduleSeconds
	rec.Observations["final_statuses"] = statuses
	rec.Observations["refused_starts"] = refused
	rec.Observations["admitted_runs"] = len(ledger)
	inventories := 0
	var lastContainerInventory containerInventoryEvidence
	containerInventoryObserved, containerInventoryReadable := false, false
	var inventoryReadError string
	for {
		inventories++
		raw, transportReadable := sr.hostContainersEvidence(t, "post-drain")
		var settled bool
		if transportReadable {
			inv, err := parseContainerInventoryEvidence(raw)
			if err != nil {
				containerInventoryReadable = false
				inventoryReadError = err.Error()
			} else {
				lastContainerInventory = inv
				containerInventoryObserved = true
				containerInventoryReadable = true
				inventoryReadError = ""
				settled = containerInventorySettled(inv)
			}
		} else {
			containerInventoryReadable = false
			inventoryReadError = "host inventory request failed"
		}
		if settled || time.Now().Add(15*time.Second).After(graceEnd) {
			break
		}
		time.Sleep(15 * time.Second)
	}
	rec.Observations["container_inventories"] = inventories
	rec.Observations["task_pod_inventory_observed"] = podInventoryObserved
	rec.Observations["task_pod_inventory_readable"] = podInventoryReadable
	rec.Observations["container_inventory_observed"] = containerInventoryObserved
	rec.Observations["container_inventory_readable"] = containerInventoryReadable
	rec.Observations["container_inventory_read_error"] = inventoryReadError
	if containerInventoryObserved {
		rec.Observations["container_inventory_after_grace"] = map[string]any{
			"counts":    lastContainerInventory.Counts,
			"task_pods": lastContainerInventory.TaskPods,
		}
	}
	drainFailures, drainInconclusive := finalDrainEvidence(finalDrainObservation{
		podInventoryObserved:       podInventoryObserved,
		podInventoryReadable:       podInventoryReadable,
		taskPods:                   lastTaskPods,
		containerInventoryObserved: containerInventoryObserved,
		containerInventoryReadable: containerInventoryReadable,
		containerInventory:         lastContainerInventory,
	}, sr.token)
	failures = append(failures, drainFailures...)
	rec.Observations["failures"] = failures
	rec.Observations["drain_inconclusive"] = drainInconclusive

	time.Sleep(sr.w.Drain.Settle.Duration)
	for i := 1; i <= sr.w.Drain.Samples; i++ {
		sr.hostSample(t, fmt.Sprintf("post-drain-%d", i))
		if i < sr.w.Drain.Samples {
			time.Sleep(sr.w.Drain.SampleInterval.Duration)
		}
	}
	if len(failures) > 0 {
		sr.failf(t, rec, "%d safety/progress failures across the schedule: %v", len(failures), failures)
	}
	if len(drainInconclusive) > 0 {
		rec.Status = soakStatusBlocked
		rec.Detail = strings.Join(drainInconclusive, "; ")
		if !t.Failed() {
			t.Errorf("inconclusive drain evidence: %s", rec.Detail)
		}
	}
}

// taskPodOwned reports whether a task pod carries this invocation's
// ownership token (set on every soak step by own()).
func taskPodOwned(p corev1.Pod, token string) bool {
	for _, c := range p.Spec.Containers {
		for _, e := range c.Env {
			if e.Name == "CAESIUM_SOAK_OWNER" && e.Value == token {
				return true
			}
		}
	}
	return false
}
