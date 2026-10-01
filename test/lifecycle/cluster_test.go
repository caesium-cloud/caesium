//go:build integration

package lifecycle

// F2's in-cluster assertion runner. The host controller owns kind, Helm and
// storage faults; this runner observes the live Kubernetes, dqlite, HTTP and
// raw-effect surfaces. A missing observation is blocked, never a pass.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/test/robustness/cluster"
	"github.com/caesium-cloud/caesium/test/robustness/recorder"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const clusterRecorderURL = "http://lifecycle-recorder:8090"

var errTaskProofUnavailable = errors.New("durable task proof query unavailable")

// A demonstrated mismatch is a failed qualification case. Missing evidence
// uses blockf so the report keeps those two outcomes distinct.
func failClusterCase(t *testing.T, name, format string, args ...any) {
	t.Helper()
	detail := fmt.Sprintf(format, args...)
	writeCase(t, caseRecord{Name: name, Status: statusFail, Detail: detail})
	t.Fatalf("FAIL %s: %s", name, detail)
}

type clusterMemberEvidence struct {
	Name     string `json:"name"`
	UID      string `json:"uid"`
	IP       string `json:"ip"`
	Node     string `json:"node"`
	PVC      string `json:"pvc"`
	Volume   string `json:"volume"`
	Image    string `json:"image"`
	ImageID  string `json:"image_id"`
	DqliteID uint64 `json:"dqlite_id"`
	Address  string `json:"address"`
}

type clusterFixture struct {
	LifecycleID    string                      `json:"lifecycle_id"`
	Members        []clusterMemberEvidence     `json:"members"`
	Jobs           map[string]jobFixture       `json:"jobs"`
	DurableTasks   map[string]clusterTaskProof `json:"durable_tasks"`
	RawStartNonces map[string][]string         `json:"raw_start_nonces"`
	Succeeded      runFixture                  `json:"succeeded"`
	Failed         runFixture                  `json:"failed"`
	InFlight       runFixture                  `json:"in_flight"`
	Predecessor    runFixture                  `json:"predecessor"`
	QueuedRow      queueRowFixture             `json:"queued_row"`
	QueueToken     string                      `json:"queue_token"`
}

type clusterInfo struct {
	Name    string `json:"name"`
	ID      uint64 `json:"id"`
	Address string `json:"address"`
}

type clusterHostObservation struct {
	BeforeInfo           []clusterInfo     `json:"before_info"`
	AfterInfo            []clusterInfo     `json:"after_info"`
	ManifestDiff         []string          `json:"manifest_diff"`
	ManifestLiveCaptured bool              `json:"manifest_live_captured"`
	ManifestLiveDiff     []string          `json:"manifest_live_diff"`
	HelmExitCode         int               `json:"helm_exit_code"`
	PodLogs              map[string]string `json:"pod_logs"`
	PreviousPodLogs      map[string]string `json:"previous_pod_logs"`
	PodStatuses          map[string]struct {
		Phase        string                     `json:"phase"`
		PodIP        string                     `json:"pod_ip"`
		Ready        bool                       `json:"ready"`
		RestartCount int                        `json:"restart_count"`
		State        map[string]json.RawMessage `json:"state"`
		LastState    map[string]json.RawMessage `json:"last_state"`
	} `json:"pod_statuses"`
}

type clusterTaskProof struct {
	ID              string `json:"id"`
	RunID           string `json:"job_run_id"`
	TaskID          string `json:"task_id"`
	ClaimedBy       string `json:"claimed_by"`
	OwnerGeneration int64  `json:"owner_generation"`
	Attempt         int    `json:"attempt"`
	ClaimAttempt    int    `json:"claim_attempt"`
	RuntimeID       string `json:"runtime_id"`
	Status          string `json:"status"`
	RecorderNonce   string `json:"recorder_nonce"`
	RecorderPod     string `json:"recorder_pod"`
}

// The public run projection identifies an unfanned task by catalog task_id,
// while task_runs.id identifies its durable attempt. Resolve the one row for
// this run and catalog task before comparing its durable identity over time.
func readClusterTaskProof(ctx context.Context, h *cluster.HTTP, base, runID, publicTaskID string) (clusterTaskProof, error) {
	rid, err := uuid.Parse(runID)
	if err != nil {
		return clusterTaskProof{}, fmt.Errorf("run id %q: %w", runID, err)
	}
	tid, err := uuid.Parse(publicTaskID)
	if err != nil {
		return clusterTaskProof{}, fmt.Errorf("public task id %q: %w", publicTaskID, err)
	}
	sql := fmt.Sprintf("SELECT id, job_run_id, task_id, claimed_by, owner_generation, attempt, claim_attempt, runtime_id, status, output FROM task_runs WHERE job_run_id = '%s' AND task_id = '%s' LIMIT 2", rid, tid)
	response, _, err := h.Query(ctx, base, sql, 2)
	if err != nil {
		return clusterTaskProof{}, fmt.Errorf("%w: %v", errTaskProofUnavailable, err)
	}
	return parseClusterTaskProof(response, rid.String(), tid.String())
}

func parseClusterTaskProof(response cluster.QueryResponse, runID, publicTaskID string) (clusterTaskProof, error) {
	if len(response.Rows) != 1 || response.RowCount != 1 {
		return clusterTaskProof{}, fmt.Errorf("run %s public task %s query returned %d rows (reported %d); require exactly one durable attempt", runID, publicTaskID, len(response.Rows), response.RowCount)
	}
	row := response.Rows[0]
	if len(row) != 10 {
		return clusterTaskProof{}, fmt.Errorf("run %s public task %s query returned %d columns, want 10", runID, publicTaskID, len(row))
	}
	durableID, err := uuid.Parse(fmt.Sprint(row[0]))
	if err != nil {
		return clusterTaskProof{}, fmt.Errorf("durable task id: %w", err)
	}
	gotRunID, err := uuid.Parse(fmt.Sprint(row[1]))
	if err != nil || gotRunID.String() != runID {
		return clusterTaskProof{}, fmt.Errorf("durable task %s belongs to run %v, want %s", durableID, row[1], runID)
	}
	gotTaskID, err := uuid.Parse(fmt.Sprint(row[2]))
	if err != nil || gotTaskID.String() != publicTaskID {
		return clusterTaskProof{}, fmt.Errorf("durable task %s maps to public task %v, want %s", durableID, row[2], publicTaskID)
	}
	generation, err := strconv.ParseInt(fmt.Sprint(row[4]), 10, 64)
	if err != nil {
		return clusterTaskProof{}, err
	}
	attempt, err := strconv.Atoi(fmt.Sprint(row[5]))
	if err != nil {
		return clusterTaskProof{}, err
	}
	claimAttempt, err := strconv.Atoi(fmt.Sprint(row[6]))
	if err != nil {
		return clusterTaskProof{}, err
	}
	runtimeID := ""
	if row[7] != nil {
		runtimeID = fmt.Sprint(row[7])
	}
	proof := clusterTaskProof{ID: durableID.String(), RunID: gotRunID.String(), TaskID: gotTaskID.String(),
		ClaimedBy: fmt.Sprint(row[3]), OwnerGeneration: generation, Attempt: attempt,
		ClaimAttempt: claimAttempt, RuntimeID: runtimeID, Status: fmt.Sprint(row[8])}
	if row[9] != nil {
		var raw []byte
		switch v := row[9].(type) {
		case string:
			raw = []byte(v)
		default:
			raw, err = json.Marshal(v)
			if err != nil {
				return clusterTaskProof{}, err
			}
		}
		if len(raw) > 0 {
			var output map[string]string
			if err := json.Unmarshal(raw, &output); err != nil {
				return clusterTaskProof{}, fmt.Errorf("durable task %s output: %w", durableID, err)
			}
			proof.RecorderNonce = output["recorder_nonce"]
			proof.RecorderPod = output["recorder_pod"]
		}
	}
	return proof, nil
}

func verifyRetainedTaskIdentity(seed, current clusterTaskProof, runID, publicTaskID string) error {
	if _, err := uuid.Parse(seed.ID); err != nil {
		return fmt.Errorf("run %s has no valid pre-upgrade durable task id: %w", runID, err)
	}
	if seed.RunID != runID || seed.TaskID != publicTaskID || seed.Attempt < 1 {
		return fmt.Errorf("run %s public task %s has mismatched pre-upgrade durable proof: %+v", runID, publicTaskID, seed)
	}
	if current.RunID != runID || current.TaskID != publicTaskID {
		return fmt.Errorf("durable task %s maps to run %s public task %s, want run %s public task %s", current.ID, current.RunID, current.TaskID, runID, publicTaskID)
	}
	if current.ID != seed.ID {
		return fmt.Errorf("run %s public task %s replaced durable row %s with %s", runID, publicTaskID, seed.ID, current.ID)
	}
	if seed.Status == "running" {
		if current.Attempt < seed.Attempt {
			return fmt.Errorf("run %s public task %s regressed attempt from %d to %d", runID, publicTaskID, seed.Attempt, current.Attempt)
		}
	} else if current.Attempt != seed.Attempt {
		return fmt.Errorf("terminal run %s public task %s changed attempt from %d to %d", runID, publicTaskID, seed.Attempt, current.Attempt)
	}
	return nil
}

func readRetainedClusterTaskProof(ctx context.Context, h *cluster.HTTP, base string, seed map[string]clusterTaskProof, runID, publicTaskID string) (clusterTaskProof, error) {
	before, ok := seed[runID]
	if !ok {
		return clusterTaskProof{}, fmt.Errorf("%w: run %s has no pre-upgrade durable task proof", errTaskProofUnavailable, runID)
	}
	after, err := readClusterTaskProof(ctx, h, base, runID, publicTaskID)
	if err != nil {
		return clusterTaskProof{}, err
	}
	if err := verifyRetainedTaskIdentity(before, after, runID, publicTaskID); err != nil {
		return clusterTaskProof{}, err
	}
	return after, nil
}

func rawStartNonceSet(runID string, events []recorder.Event) map[string]bool {
	starts := map[string]bool{}
	for _, event := range events {
		if event.RunID == runID && event.Step == "hold" && event.Kind == "start" && event.Nonce != "" {
			starts[event.Nonce] = true
		}
	}
	return starts
}

type taskStartedAttempt struct {
	DurableID    string `json:"id"`
	RunID        string `json:"job_run_id"`
	TaskID       string `json:"task_id"`
	RuntimeID    string `json:"runtime_id"`
	Attempt      int    `json:"attempt"`
	ClaimAttempt int    `json:"claim_attempt"`
}

func parseTaskStartedAttempts(runID, taskID, durableID string, maxAttempt int, events []eventTuple) (map[string]taskStartedAttempt, error) {
	byRuntime := map[string]taskStartedAttempt{}
	for _, event := range events {
		if event.Type != "task_started" || event.TaskID != taskID {
			continue
		}
		payload := []byte(event.Payload)
		var quoted string
		if err := json.Unmarshal(payload, &quoted); err == nil {
			payload = []byte(quoted)
		}
		var started taskStartedAttempt
		if err := json.Unmarshal(payload, &started); err != nil {
			return nil, fmt.Errorf("task_started sequence %d payload: %w", event.Sequence, err)
		}
		if started.DurableID != durableID || started.RunID != runID || started.TaskID != taskID {
			return nil, fmt.Errorf("task_started sequence %d has wrong durable/run/task identity: %+v", event.Sequence, started)
		}
		if started.Attempt > maxAttempt {
			return nil, fmt.Errorf("task_started sequence %d advances beyond terminal durable attempt %d to %d", event.Sequence, maxAttempt, started.Attempt)
		}
		if started.RuntimeID == "" {
			continue // A claim can emit a pre-container task_started event.
		}
		if started.Attempt < 1 || started.ClaimAttempt < 1 {
			return nil, fmt.Errorf("task_started sequence %d lacks positive attempt/claim identity", event.Sequence)
		}
		prefix := fmt.Sprintf("%s-%s-attempt%d-", taskID, runID, started.ClaimAttempt)
		if !strings.HasPrefix(started.RuntimeID, prefix) {
			return nil, fmt.Errorf("task_started sequence %d runtime %s does not match task/run/claim", event.Sequence, started.RuntimeID)
		}
		if _, err := uuid.Parse(strings.TrimPrefix(started.RuntimeID, prefix)); err != nil {
			return nil, fmt.Errorf("task_started sequence %d runtime %s has no pod UUID: %w", event.Sequence, started.RuntimeID, err)
		}
		if _, exists := byRuntime[started.RuntimeID]; exists {
			return nil, fmt.Errorf("runtime %s has more than one persisted task_started event", started.RuntimeID)
		}
		byRuntime[started.RuntimeID] = started
	}
	if len(byRuntime) == 0 {
		return nil, fmt.Errorf("run %s task %s has no persisted task_started runtime identity", runID, taskID)
	}
	return byRuntime, nil
}

type rawAttemptEffect struct {
	RunID   string `json:"run_id"`
	Step    string `json:"step"`
	Nonce   string `json:"nonce"`
	PodName string `json:"pod_name"`
	Event   string `json:"event"`
}

func parseRawAttemptEffect(event recorder.Event) (rawAttemptEffect, error) {
	var raw rawAttemptEffect
	if err := json.Unmarshal([]byte(event.Raw), &raw); err != nil {
		return rawAttemptEffect{}, fmt.Errorf("raw %s nonce %s payload: %w", event.Kind, event.Nonce, err)
	}
	if raw.RunID != event.RunID || raw.Step != event.Step || raw.Nonce == "" || raw.Nonce != event.Nonce ||
		raw.Event != event.Kind || raw.PodName == "" {
		return rawAttemptEffect{}, fmt.Errorf("raw %s nonce %s lacks matching run/step/nonce/kind/pod identity", event.Kind, event.Nonce)
	}
	return raw, nil
}

func verifySeedHeldAttempt(proof clusterTaskProof, rawEvents []recorder.Event, taskEvents []eventTuple) error {
	if proof.Status != "running" || proof.RuntimeID == "" || proof.Attempt < 1 || proof.ClaimAttempt < 1 {
		return fmt.Errorf("seed held task has no current running durable runtime/attempt/claim: %+v", proof)
	}
	startedByRuntime, err := parseTaskStartedAttempts(proof.RunID, proof.TaskID, proof.ID, proof.Attempt, taskEvents)
	if err != nil {
		return err
	}
	started, ok := startedByRuntime[proof.RuntimeID]
	if !ok || started.Attempt != proof.Attempt || started.ClaimAttempt != proof.ClaimAttempt {
		return fmt.Errorf("seed held task %s current runtime %s has no matching persisted task_started attempt/claim", proof.ID, proof.RuntimeID)
	}
	currentStart := false
	for _, event := range rawEvents {
		if event.RunID != proof.RunID || event.Step != "hold" || (event.Kind != "start" && event.Kind != "complete") {
			continue
		}
		raw, err := parseRawAttemptEffect(event)
		if err != nil {
			return err
		}
		if raw.PodName == proof.RuntimeID {
			if event.Kind == "complete" {
				return fmt.Errorf("seed held task %s current runtime already emitted a completion", proof.ID)
			}
			currentStart = true
		}
	}
	if !currentStart {
		return fmt.Errorf("seed held task %s current runtime %s has no raw start", proof.ID, proof.RuntimeID)
	}
	return nil
}

func verifyQueuedHeldAttempt(run apiRun, wantRunID, wantJobID, wantToken string, proof clusterTaskProof,
	rawEvents []recorder.Event, taskEvents []eventTuple) ([]string, error) {
	if run.ID != wantRunID || run.JobID != wantJobID || run.Params["TOKEN"] != wantToken ||
		run.Status != "running" || len(run.Tasks) != 1 {
		return nil, fmt.Errorf("queued run has no unique current running public task: run=%s job=%s status=%s tasks=%d", run.ID, run.JobID, run.Status, len(run.Tasks))
	}
	task := run.Tasks[0]
	if task.ID == "" || task.Status != "running" || task.Attempt < 1 ||
		proof.RunID != run.ID || proof.TaskID != task.ID || proof.Status != "running" || proof.Attempt != task.Attempt {
		return nil, fmt.Errorf("queued run %s public task and durable running attempt disagree: public=%+v durable=%+v", run.ID, task, proof)
	}
	if err := verifySeedHeldAttempt(proof, rawEvents, taskEvents); err != nil {
		return nil, err
	}
	starts := map[string]bool{}
	for _, event := range rawEvents {
		if event.RunID != run.ID || event.Step != "hold" || event.Kind != "start" {
			continue
		}
		raw, err := parseRawAttemptEffect(event)
		if err != nil {
			return nil, err
		}
		if raw.PodName == proof.RuntimeID {
			starts[raw.Nonce] = true
		}
	}
	nonces := make([]string, 0, len(starts))
	for nonce := range starts {
		nonces = append(nonces, nonce)
	}
	sort.Strings(nonces)
	if len(nonces) == 0 {
		return nil, fmt.Errorf("queued run %s current durable runtime has no raw start nonce", run.ID)
	}
	return nonces, nil
}

func reconcileRetainedAttemptEffects(seed, terminal clusterTaskProof, seedStarts []string, rawEvents []recorder.Event, taskEvents []eventTuple) error {
	if terminal.Attempt < seed.Attempt {
		return fmt.Errorf("run %s task attempt regressed from %d to %d", seed.RunID, seed.Attempt, terminal.Attempt)
	}
	if len(seedStarts) == 0 {
		return fmt.Errorf("run %s has no pre-upgrade raw start nonce", seed.RunID)
	}
	startedByRuntime, err := parseTaskStartedAttempts(seed.RunID, seed.TaskID, seed.ID, terminal.Attempt, taskEvents)
	if err != nil {
		return err
	}
	type nonceProof struct {
		pod      string
		started  bool
		complete bool
		attempt  int
	}
	byNonce := map[string]*nonceProof{}
	witnessedAttempts := map[int]bool{}
	for _, event := range rawEvents {
		if event.RunID != seed.RunID || event.Step != "hold" || (event.Kind != "start" && event.Kind != "complete") {
			continue
		}
		raw, err := parseRawAttemptEffect(event)
		if err != nil {
			return err
		}
		started, ok := startedByRuntime[raw.PodName]
		if !ok {
			return fmt.Errorf("raw %s nonce %s pod %s has no persisted task_started runtime", event.Kind, event.Nonce, raw.PodName)
		}
		proof := byNonce[event.Nonce]
		if proof == nil {
			proof = &nonceProof{pod: raw.PodName, attempt: started.Attempt}
			byNonce[event.Nonce] = proof
		} else if proof.pod != raw.PodName || proof.attempt != started.Attempt {
			return fmt.Errorf("raw nonce %s spans different pod or durable attempts", event.Nonce)
		}
		if event.Kind == "start" {
			proof.started = true
			witnessedAttempts[started.Attempt] = true
		} else {
			proof.complete = true
		}
	}
	baseline := map[string]bool{}
	for _, nonce := range seedStarts {
		proof := byNonce[nonce]
		if nonce == "" || baseline[nonce] || proof == nil || !proof.started || proof.attempt > seed.Attempt {
			return fmt.Errorf("run %s has missing, duplicate, or post-seed baseline nonce %s", seed.RunID, nonce)
		}
		baseline[nonce] = true
	}
	for nonce, proof := range byNonce {
		if proof.complete && !proof.started {
			return fmt.Errorf("run %s raw completion nonce %s has no matching start", seed.RunID, nonce)
		}
	}
	for attempt := seed.Attempt; attempt <= terminal.Attempt; attempt++ {
		if !witnessedAttempts[attempt] {
			return fmt.Errorf("run %s durable attempt %d has no task_started/pod/raw-start witness", seed.RunID, attempt)
		}
	}
	final := byNonce[terminal.RecorderNonce]
	if final == nil || !final.started || !final.complete || terminal.RecorderPod == "" ||
		final.pod != terminal.RecorderPod || final.pod != terminal.RuntimeID {
		return fmt.Errorf("run %s terminal output nonce/pod does not match raw start, completion, and durable runtime_id", seed.RunID)
	}
	started := startedByRuntime[terminal.RuntimeID]
	if started.Attempt != terminal.Attempt || started.ClaimAttempt != terminal.ClaimAttempt ||
		started.DurableID != terminal.ID || final.attempt != terminal.Attempt {
		return fmt.Errorf("run %s terminal runtime does not match durable attempt/claim identity", seed.RunID)
	}
	return nil
}

func rawCompletionMatchesTask(runID string, proof clusterTaskProof, events []recorder.Event) bool {
	if proof.RecorderNonce == "" || proof.RecorderPod == "" || proof.RuntimeID != proof.RecorderPod {
		return false
	}
	started, completed := false, false
	for _, e := range events {
		if e.RunID != runID || e.Nonce != proof.RecorderNonce || e.Step != "hold" {
			continue
		}
		raw, err := parseRawAttemptEffect(e)
		if err != nil || raw.PodName != proof.RuntimeID {
			return false
		}
		started = started || e.Kind == "start"
		completed = completed || e.Kind == "complete"
	}
	return started && completed
}

func clusterPair(t *testing.T) pairMatrix {
	t.Helper()
	p := loadMatrix(t)
	var raw struct {
		Pairs []struct {
			ID      string `json:"id"`
			Cluster struct {
				Replicas                int `json:"replicas"`
				DatabaseShards          int `json:"database_shards"`
				DatabaseVoters          int `json:"database_voters"`
				DatabaseStandbys        int `json:"database_standbys"`
				ExpectedProtocolVersion int `json:"expected_protocol_version"`
			} `json:"cluster"`
		} `json:"pairs"`
	}
	data, err := os.ReadFile(filepath.Join(artifactsDir(t), "versions.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &raw))
	for _, item := range raw.Pairs {
		if item.ID == p.ID {
			require.Equal(t, 3, item.Cluster.Replicas)
			require.Equal(t, 1, item.Cluster.DatabaseShards)
			require.Equal(t, 3, item.Cluster.DatabaseVoters)
			require.Zero(t, item.Cluster.DatabaseStandbys)
			require.Equal(t, 2, item.Cluster.ExpectedProtocolVersion)
			return p
		}
	}
	blockf(t, "cluster-version-matrix", "pair %s lacks an F2 cluster matrix", p.ID)
	return pairMatrix{}
}

func clusterKube(t *testing.T) (string, *cluster.HTTP, cluster.Topology) {
	t.Helper()
	ns := mustEnv(t, "CAESIUM_LIFECYCLE_ID")
	kube, err := cluster.InClusterClient()
	if err != nil {
		blockf(t, "cluster-kubernetes-client", "%v", err)
	}
	topo, err := cluster.RequireReadyTopology(t.Context(), kube, ns, mustEnv(t, "CAESIUM_LIFECYCLE_EXPECTED_IMAGE_ID"))
	if err != nil {
		blockf(t, "three-persistent-voters", "%v", err)
	}
	for _, m := range topo.Members {
		for _, key := range []string{"CAESIUM_DATABASE_SHARDS", "CAESIUM_DATABASE_VOTERS", "CAESIUM_DATABASE_STANDBYS"} {
			want := map[string]string{"CAESIUM_DATABASE_SHARDS": "1", "CAESIUM_DATABASE_VOTERS": "3", "CAESIUM_DATABASE_STANDBYS": "0"}[key]
			require.Equalf(t, want, m.EnvValue(key), "%s on %s changed across the transition", key, m.Name)
		}
	}
	return ns, cluster.NewHTTP(mustEnv(t, "CAESIUM_MANUAL_TRIGGER_API_KEY")), topo
}

func clusterMembership(t *testing.T, topo cluster.Topology) cluster.Membership {
	t.Helper()
	addrs := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		addrs = append(addrs, m.DqliteAddr())
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	m, err := cluster.WaitMembership(ctx, addrs)
	if err != nil {
		blockf(t, "three-persistent-voters", "direct Leader/Cluster RPC: %v", err)
	}
	seen := map[string]bool{}
	for _, member := range m.Members {
		require.NotZero(t, member.ID)
		require.Equal(t, "voter", member.Role)
		seen[member.Address] = true
	}
	for _, addr := range addrs {
		require.Truef(t, seen[addr], "live pod address %s is absent from direct dqlite membership", addr)
	}
	return m
}

func memberEvidence(topo cluster.Topology, membership cluster.Membership) []clusterMemberEvidence {
	byAddr := map[string]cluster.DqliteNode{}
	for _, n := range membership.Members {
		byAddr[n.Address] = n
	}
	out := make([]clusterMemberEvidence, 0, len(topo.Members))
	for _, m := range topo.Members {
		n := byAddr[m.DqliteAddr()]
		out = append(out, clusterMemberEvidence{Name: m.Name, UID: m.UID, IP: m.IP, Node: m.Node,
			PVC: m.PVCName, Volume: m.VolumeName, Image: m.Image, ImageID: m.ImageID,
			DqliteID: n.ID, Address: n.Address})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func clusterBase(topo cluster.Topology) string {
	for _, m := range topo.Members {
		if m.Name == "caesium-0" {
			return m.HTTPBase()
		}
	}
	return topo.Members[0].HTTPBase()
}

func recorderEvents(t *testing.T) []recorder.Event {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, clusterRecorderURL+"/records", nil)
	if err != nil {
		blockf(t, "raw-effect-ledger", "build GET /records: %v", err)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		blockf(t, "raw-effect-ledger", "GET /records: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		blockf(t, "raw-effect-ledger", "GET /records status %d", resp.StatusCode)
	}
	var events []recorder.Event
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		blockf(t, "raw-effect-ledger", "decode /records: %v", err)
	}
	return events
}

func hasHeldRecorderStart(runID string, events []recorder.Event) bool {
	for _, event := range events {
		if event.RunID != runID || event.Step != "hold" || event.Kind != "start" || event.Nonce == "" {
			continue
		}
		if _, err := parseRawAttemptEffect(event); err == nil {
			return true
		}
	}
	return false
}

func waitRecorderStart(t *testing.T, caseName, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if hasHeldRecorderStart(runID, recorderEvents(t)) {
			return
		}
		time.Sleep(time.Second)
	}
	blockf(t, caseName, "recorder never saw held-task start for run %s; full pod-name lookup or task start may have failed", runID)
}

func releaseRecordedRun(t *testing.T, runID string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, clusterRecorderURL+"/release?run_id="+runID, nil)
	require.NoError(t, err)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

func assertRawCompletion(t *testing.T, runID string, events []recorder.Event) {
	t.Helper()
	starts := map[string]bool{}
	effects := map[string]bool{}
	for _, e := range events {
		if e.RunID != runID {
			continue
		}
		switch e.Kind {
		case "start":
			starts[e.Nonce] = true
		case "complete":
			effects[e.Nonce] = true
		}
	}
	require.NotEmpty(t, starts, "no raw start effect for %s", runID)
	require.NotEmpty(t, effects, "no raw completion effect for %s", runID)
	for nonce := range effects {
		require.Truef(t, starts[nonce], "raw completion nonce %s has no start for %s", nonce, runID)
	}
}

func clusterManifest(t *testing.T, kind, alias, taskImage string) jobdef.Definition {
	t.Helper()
	var step string
	switch kind {
	case "history":
		step = `set -eu; N="$(cat /proc/sys/kernel/random/uuid)"; R="http://lifecycle-recorder:8090"; wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"history\",\"nonce\":\"$N\",\"event\":\"start\"}" "$R/start"; sleep 2; echo "##caesium::output {\"token\": \"$CAESIUM_PARAM_TOKEN\"}"; if [ "$CAESIUM_PARAM_EXIT" = 0 ]; then wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"history\",\"nonce\":\"$N\",\"event\":\"complete\"}" "$R/effect"; else exit "$CAESIUM_PARAM_EXIT"; fi`
	case "held":
		step = `set -eu; N="$(cat /proc/sys/kernel/random/uuid)"; P=""; i=0; while [ "$i" -lt 30 ]; do if P="$(wget -qO- http://lifecycle-recorder:8091/pod-name 2>/dev/null)" && test -n "$P"; then break; fi; P=""; i=$((i+1)); sleep 1; done; test -n "$P"; R="http://lifecycle-recorder:8090"; wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"hold\",\"nonce\":\"$N\",\"pod_name\":\"$P\",\"event\":\"start\"}" "$R/start"; i=0; while [ "$i" -lt 900 ]; do if wget -qO- "$R/wait?run_id=$CAESIUM_RUN_ID" | grep -q released; then wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"hold\",\"nonce\":\"$N\",\"pod_name\":\"$P\",\"event\":\"complete\"}" "$R/effect"; echo "##caesium::output {\"recorder_nonce\":\"$N\",\"recorder_pod\":\"$P\"}"; exit 0; fi; i=$((i+1)); sleep 1; done; exit 1`
	default:
		t.Fatalf("unknown fixture kind %q", kind)
	}
	// Build a real public job definition. The Kubernetes engine is explicit;
	// this fixture cannot accidentally fall back to the Docker engine.
	def := jobdef.Definition{APIVersion: jobdef.APIVersionV1, Kind: jobdef.KindJob,
		Metadata: jobdef.Metadata{Alias: alias},
		Trigger:  jobdef.Trigger{Type: jobdef.TriggerHTTP, Configuration: map[string]any{"path": alias}},
		Steps: []jobdef.Step{{Name: "hold", Type: jobdef.StepTypeTask, Engine: jobdef.EngineKubernetes,
			Image: taskImage, Command: []string{"sh", "-c", step}}}}
	if kind == "held" && strings.Contains(alias, "queue") {
		// The public schema keeps queue admission behind maxRuns=1.
		def.Metadata.Concurrency = &jobdef.Concurrency{MaxRuns: 1, Strategy: jobdef.ConcurrencyStrategyQueue}
	}
	require.NoError(t, def.Validate())
	return def
}

// The sidecar persists the raw effect ledger for the full upgrade. A normal
// test phase exits; the dedicated recorder sidecar stays alive across phases.
func TestLifecycleClusterRecorder(t *testing.T) {
	if os.Getenv("CAESIUM_LIFECYCLE_CLUSTER_RECORDER") != "1" {
		t.Skip("recorder sidecar only")
	}
	// Kubelet shortens long pod names in /etc/hostname to 63 characters.
	// The recorder's service account can instead resolve the caller's pod IP
	// to the full Kubernetes metadata.name before the task writes any effect.
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	namespaceBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	require.NoError(t, err)
	namespace := strings.TrimSpace(string(namespaceBytes))
	require.NotEmpty(t, namespace)
	podLookup := &http.Server{Addr: ":8091", ReadHeaderTimeout: 10 * time.Second}
	podLookup.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/pod-name" {
			http.NotFound(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "pod-name lookup from %s: list failed: %v\n", r.RemoteAddr, err)
			http.Error(w, fmt.Sprintf("list caller pods: %v", err), http.StatusServiceUnavailable)
			return
		}
		name, err := resolvePodNameBySourceIP(r.RemoteAddr, pods.Items)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pod-name lookup from %s: %v\n", r.RemoteAddr, err)
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, name)
	})
	sink := recorder.New()
	require.NoError(t, sink.Start())
	defer sink.Close(context.Background())
	podListener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", podLookup.Addr)
	require.NoError(t, err)
	defer func() { _ = podLookup.Close() }()
	podLookupDone := make(chan error, 1)
	go func() { podLookupDone <- podLookup.Serve(podListener) }()
	select {
	case err := <-podLookupDone:
		require.ErrorIs(t, err, http.ErrServerClosed)
	case <-t.Context().Done():
	}
}

func resolvePodNameBySourceIP(remoteAddr string, pods []corev1.Pod) (string, error) {
	address, _, err := net.SplitHostPort(remoteAddr)
	if err != nil || net.ParseIP(address) == nil {
		return "", fmt.Errorf("invalid recorder caller address %q", remoteAddr)
	}
	name := ""
	for _, pod := range pods {
		if pod.Status.PodIP != address {
			continue
		}
		if pod.Name == "" || name != "" {
			return "", fmt.Errorf("caller IP %s maps to ambiguous live pods", address)
		}
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			return "", fmt.Errorf("caller IP %s maps to a pod that is not running", address)
		}
		name = pod.Name
	}
	if name == "" {
		return "", fmt.Errorf("caller IP %s has no unique live pod", address)
	}
	return name, nil
}

func TestLifecycleClusterSeed(t *testing.T) {
	clusterPair(t)
	ns, h, topo := clusterKube(t)
	membership := clusterMembership(t, topo)
	base := clusterBase(topo)
	c := newClient(t)
	c.base = base
	ctx := t.Context()
	for _, m := range topo.Members {
		require.NoError(t, h.Health(ctx, m.HTTPBase()))
	}
	schema, err := c.schema(ctx)
	require.NoError(t, err)
	require.Equal(t, 40, len(schema.Tables), "previous-release table count changed")
	alias := suffixOf(ns)
	defs := []jobdef.Definition{
		clusterManifest(t, "history", "lifecycle-history-"+alias, mustEnv(t, "CAESIUM_LIFECYCLE_TASK_IMAGE")),
		clusterManifest(t, "held", "lifecycle-queue-"+alias, mustEnv(t, "CAESIUM_LIFECYCLE_TASK_IMAGE")),
		clusterManifest(t, "held", "lifecycle-inflight-"+alias, mustEnv(t, "CAESIUM_LIFECYCLE_TASK_IMAGE")),
	}
	require.NoError(t, h.Apply(ctx, base, defs))
	jobs := map[string]jobFixture{}
	for i, key := range []string{"history", "queue", "inflight"} {
		job, err := h.JobByAlias(ctx, base, defs[i].Metadata.Alias)
		require.NoError(t, err)
		jobs[key] = jobFixture{ID: job.ID, Alias: job.Alias}
	}
	ok, started, err := c.triggerRun(ctx, jobs["history"].ID, map[string]string{"EXIT": "0", "TOKEN": "success-" + alias})
	require.NoError(t, err)
	require.True(t, started)
	ok, err = c.awaitRunStatus(ctx, ok.JobID, ok.ID, func(r apiRun) bool { return isTerminal(r.Status) }, runDeadline)
	require.NoError(t, err)
	require.Equal(t, "succeeded", ok.Status)
	bad, started, err := c.triggerRun(ctx, jobs["history"].ID, map[string]string{"EXIT": "7", "TOKEN": "failure-" + alias})
	require.NoError(t, err)
	require.True(t, started)
	bad, err = c.awaitRunStatus(ctx, bad.JobID, bad.ID, func(r apiRun) bool { return isTerminal(r.Status) }, runDeadline)
	require.NoError(t, err)
	require.Equal(t, "failed", bad.Status)
	assertRawCompletion(t, ok.ID, recorderEvents(t))
	inflight, started, err := c.triggerRun(ctx, jobs["inflight"].ID, nil)
	require.NoError(t, err)
	require.True(t, started)
	waitRecorderStart(t, "raw-effect-ledger", inflight.ID)
	inflight, err = c.run(ctx, inflight.JobID, inflight.ID)
	require.NoError(t, err)
	require.Equal(t, "running", inflight.Status)
	require.Len(t, inflight.Tasks, 1, "in-flight task attempt identity missing before upgrade")
	predecessor, started, err := c.triggerRun(ctx, jobs["queue"].ID, nil)
	require.NoError(t, err)
	require.True(t, started)
	waitRecorderStart(t, "raw-effect-ledger", predecessor.ID)
	predecessor, err = c.run(ctx, predecessor.JobID, predecessor.ID)
	require.NoError(t, err)
	require.Equal(t, "running", predecessor.Status)
	require.Len(t, predecessor.Tasks, 1, "queue predecessor task attempt identity missing before upgrade")
	queueToken := "queued-" + alias
	_, started, err = c.triggerRun(ctx, jobs["queue"].ID, map[string]string{"TOKEN": queueToken})
	require.NoError(t, err)
	require.False(t, started, "second run did not queue behind held predecessor")
	queued, err := c.queue(ctx, jobs["queue"].ID)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, queueToken, queued[0].Params["TOKEN"])
	require.Empty(t, queued[0].ClaimedBy)
	fx := clusterFixture{LifecycleID: ns, Members: memberEvidence(topo, membership), Jobs: jobs,
		Succeeded: ok.fixture(), Failed: bad.fixture(), InFlight: inflight.fixture(),
		Predecessor: predecessor.fixture(), QueuedRow: queued[0], QueueToken: queueToken,
		DurableTasks: make(map[string]clusterTaskProof, 4), RawStartNonces: make(map[string][]string, 2)}
	for _, run := range []runFixture{fx.Succeeded, fx.Failed, fx.InFlight, fx.Predecessor} {
		require.Len(t, run.Tasks, 1, "seed run %s must have one public task", run.ID)
		var proof clusterTaskProof
		var err error
		for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
			proof, err = readClusterTaskProof(ctx, h, base, run.ID, run.Tasks[0].ID)
			if err == nil && (run.Status != "running" || proof.RuntimeID != "") {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
		require.NoErrorf(t, err, "seed run %s durable task proof missing", run.ID)
		if run.Status == "running" {
			require.NotEmptyf(t, proof.RuntimeID, "seed run %s has no current durable runtime", run.ID)
		}
		require.Equal(t, run.Tasks[0].Attempt, proof.Attempt, "seed public and durable attempts disagree")
		require.Equal(t, run.Tasks[0].Status, proof.Status, "seed public and durable statuses disagree")
		fx.DurableTasks[run.ID] = proof
	}
	require.Len(t, fx.DurableTasks, 4)
	for _, run := range []*runFixture{&fx.Succeeded, &fx.Failed, &fx.InFlight, &fx.Predecessor} {
		run.Events, err = readEventBacklog(ctx, c, run.ID, 0)
		require.NoError(t, err)
		require.NotEmpty(t, run.Events)
		low := lowestSequence(run.Events)
		require.Positive(t, low)
		run.ResumeCursor = low - 1
	}
	rawBefore := recorderEvents(t)
	for _, run := range []runFixture{fx.InFlight, fx.Predecessor} {
		if err := verifySeedHeldAttempt(fx.DurableTasks[run.ID], rawBefore, run.Events); err != nil {
			blockf(t, "retained-history-and-raw-effects", "seed held run %s lacks current durable attempt proof: %v", run.ID, err)
		}
		for nonce := range rawStartNonceSet(run.ID, rawBefore) {
			fx.RawStartNonces[run.ID] = append(fx.RawStartNonces[run.ID], nonce)
		}
		require.NotEmptyf(t, fx.RawStartNonces[run.ID], "seed run %s has no raw start nonce", run.ID)
		sort.Strings(fx.RawStartNonces[run.ID])
	}
	writeJSON(t, "cluster-fixture.json", fx)
	writeJSON(t, "cluster-raw-before.json", rawBefore)
	writeCase(t, caseRecord{Name: "three-persistent-voters-before", Status: statusPass,
		Detail:       fmt.Sprintf("three bound PVCs, distinct worker nodes, three direct dqlite voters, leader %s", membership.Leader.Address),
		Observations: map[string]any{"members": fx.Members, "leader": membership.Leader}})
}

// mixed-version-dispatch-and-completion (F2, H3 W9-δ).
//
// The host controller holds the rolling upgrade at exactly one upgraded
// member: before the unchanged image-only `helm upgrade --wait` it sets the
// live StatefulSet's rollingUpdate.partition to 2, so only caesium-2 is
// replaced and caesium-0/1 keep their previous-release pods (see
// lc_mixed_hold_* in scripts/lifecycle-tests.sh). This runner is started only
// once that hold is verified, and the host releases the rollout after it
// exits, so the observation no longer races the StatefulSet controller.
//
// Before the hold (W8) the runner polled a live rollout. In
// lifecycle-w8b-r1-fbbe76d2 the window lasted 62 s (caesium-2 Ready 03:00:09,
// caesium-0 deleted 03:01:11). The first attempt's task ran on the previous
// member at 03:00:12, but its completion could not be persisted: from 03:00:15
// every write through the leader failed with `database is locked` (and the
// candidate's write connection with `cannot start a transaction within a
// transaction`) until the previous-release leader itself was replaced, so the
// 40 s completion wait expired and the remaining attempts ran after the window
// had closed. Holding the window removes the timing dependency without
// weakening the assertion: both crossing directions must be observed on one
// stable set of pods, each with the same durable attempt, fenced owner
// generation and raw completion nonce.
const (
	mixedCaseName            = "mixed-version-dispatch-and-completion"
	mixedHeldCandidateMember = "caesium-2"
	mixedWindowBudget        = 9 * time.Minute
	mixedDirectionBudget     = 3 * time.Minute
	mixedProbeBudget         = 90 * time.Second
)

var mixedPreviousMembers = []string{"caesium-0", "caesium-1"}

type mixedHeldMember struct {
	Name    string `json:"name"`
	UID     string `json:"uid"`
	IP      string `json:"ip"`
	Node    string `json:"node"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	Version string `json:"version"`
	// Protocol is the /internal/capabilities protocol_version.
	Protocol int       `json:"protocol_version"`
	NodeID   string    `json:"node_id"`
	Observed time.Time `json:"observed_at"`
	member   cluster.Member
}

// mixedVerdict classifies one held-window attempt. Only a proved
// cross-version run can cross or fail; every other attempt is retried.
type mixedVerdict string

const (
	mixedRetry   mixedVerdict = "retry"
	mixedCrossed mixedVerdict = "crossed"
	mixedFailed  mixedVerdict = "failed"
)

type mixedAttempt struct {
	Direction     string `json:"direction"`
	TriggerMember string `json:"trigger_member"`
	RunID         string `json:"run_id,omitempty"`
	OwnerNode     string `json:"owner_node,omitempty"`
	OwnerVersion  string `json:"owner_version,omitempty"`
	WorkerNode    string `json:"worker_node,omitempty"`
	WorkerVersion string `json:"worker_version,omitempty"`
	// CrossVersion is set once the lease owner and the claimed worker of the
	// running durable attempt are held members on different releases.
	CrossVersion   bool         `json:"cross_version"`
	ObservedStatus string       `json:"observed_status,omitempty"`
	Verdict        mixedVerdict `json:"verdict"`
	Outcome        string       `json:"outcome"`
	StartedAt      time.Time    `json:"started_at"`
	Seconds        float64      `json:"seconds"`
}

type mixedDirection struct {
	Name         string
	OwnerVersion string
	Triggers     []string
}

var mixedDirections = []mixedDirection{
	{Name: "candidate-owner-previous-worker", OwnerVersion: "candidate", Triggers: []string{mixedHeldCandidateMember}},
	{Name: "previous-owner-candidate-worker", OwnerVersion: "previous", Triggers: mixedPreviousMembers},
}

// heldMixedWindow verifies the held topology directly from the Kubernetes API:
// exactly three Ready members, caesium-2 recreated on the candidate image, and
// caesium-0/1 still the pre-upgrade pods (fixture UIDs) on the previous image.
func heldMixedWindow(ctx context.Context, fx clusterFixture, oldID, newID string) (map[string]mixedHeldMember, error) {
	kube, err := cluster.InClusterClient()
	if err != nil {
		return nil, err
	}
	topo, err := cluster.DiscoverTopology(ctx, kube, fx.LifecycleID)
	if err != nil {
		return nil, fmt.Errorf("discover topology: %w", err)
	}
	if len(topo.Members) != 3 {
		return nil, fmt.Errorf("held window has %d live caesium pods, want 3", len(topo.Members))
	}
	seeded := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		seeded[m.Name] = m
	}
	out := map[string]mixedHeldMember{}
	for _, m := range topo.Members {
		if !cluster.PodReady(&m.Pod) {
			return nil, fmt.Errorf("%s is not Ready inside the held window", m.Name)
		}
		prior, ok := seeded[m.Name]
		if !ok {
			return nil, fmt.Errorf("%s is not a seeded member", m.Name)
		}
		held := mixedHeldMember{Name: m.Name, UID: m.UID, IP: m.IP, Node: m.Node, Image: m.Image,
			ImageID: m.ImageID, Observed: time.Now().UTC(), member: m}
		switch {
		case cluster.ImageIDMatchesCandidate(m.ImageID, newID):
			held.Version = "candidate"
		case cluster.ImageIDMatchesCandidate(m.ImageID, oldID):
			held.Version = "previous"
		default:
			return nil, fmt.Errorf("%s runs neither verified image: %s", m.Name, m.ImageID)
		}
		if m.Name == mixedHeldCandidateMember {
			if held.Version != "candidate" || m.UID == prior.UID {
				return nil, fmt.Errorf("%s was not recreated on the candidate (version=%s uid=%s seeded uid=%s)",
					m.Name, held.Version, m.UID, prior.UID)
			}
		} else if held.Version != "previous" || m.UID != prior.UID {
			return nil, fmt.Errorf("%s is not the seeded previous-release pod (version=%s uid=%s seeded uid=%s)",
				m.Name, held.Version, m.UID, prior.UID)
		}
		out[m.Name] = held
	}
	for _, name := range append([]string{mixedHeldCandidateMember}, mixedPreviousMembers...) {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("held window lacks %s", name)
		}
	}
	return out, nil
}

// probeMixedProtocols reads /internal/capabilities from every held member.
// It returns (nil, reason) when a probe could not be made and records a
// failed case when a member answers with a protocol other than 2.
func probeMixedProtocols(ctx context.Context, t *testing.T, h *cluster.HTTP, window map[string]mixedHeldMember) (map[string]mixedHeldMember, string) {
	t.Helper()
	candidate := window[mixedHeldCandidateMember]
	ic, err := cluster.MintInternalClient(ctx, h, candidate.member.HTTPBase(),
		mustEnv(t, "CAESIUM_LIFECYCLE_INTERNAL_TOKEN"), mustEnv(t, "CAESIUM_LIFECYCLE_INTERNAL_TOKEN"))
	if err != nil {
		return nil, fmt.Sprintf("cannot authenticate protocol probe: %v", err)
	}
	probed := map[string]mixedHeldMember{}
	for name, m := range window {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cluster.InternalBase(m.IP)+"/internal/capabilities", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+ic.Token)
		resp, err := ic.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Sprintf("%s protocol probe: %v", name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Sprintf("%s capabilities status %d: %s", name, resp.StatusCode, body)
		}
		var capability struct {
			NodeID          string `json:"node_id"`
			ProtocolVersion int    `json:"protocol_version"`
		}
		if err := json.Unmarshal(body, &capability); err != nil {
			return nil, fmt.Sprintf("%s capabilities body: %v", name, err)
		}
		if capability.ProtocolVersion != 2 {
			failClusterCase(t, mixedCaseName, "%s (%s) reports internal protocol %d; F1 permits a mixed window only at protocol 2",
				name, m.Version, capability.ProtocolVersion)
		}
		m.Protocol, m.NodeID = capability.ProtocolVersion, capability.NodeID
		probed[name] = m
	}
	return probed, ""
}

// attemptMixedCrossing triggers one held run through trigger's public API.
// The run is a proved cross-version execution once its lease owner and the
// claimed worker of its running durable attempt are held members on different
// releases. From then on it either crosses, when the same durable attempt,
// fenced owner generation and raw nonce reach succeeded, or it fails: any other
// terminal status, a replaced or re-claimed durable attempt, or a missing raw
// completion is a demonstrated mixed-version failure (mixedFailed) that the
// caller records and never retries away. An attempt that never reached that
// proof, a same-version dispatch, a proved run still in flight when the wait
// closes, an unreadable terminal proof and a proved success in the other
// direction are retried (mixedRetry).
func attemptMixedCrossing(ctx context.Context, t *testing.T, fx clusterFixture, h *cluster.HTTP, readBase string,
	window map[string]mixedHeldMember, direction mixedDirection, trigger mixedHeldMember) (cross map[string]any, a mixedAttempt) {
	t.Helper()
	a = mixedAttempt{Direction: direction.Name, TriggerMember: trigger.Name, Verdict: mixedRetry, StartedAt: time.Now().UTC()}
	defer func() { a.Seconds = time.Since(a.StartedAt).Seconds() }()
	failed := func(format string, args ...any) (map[string]any, mixedAttempt) {
		a.Verdict, a.Outcome = mixedFailed, fmt.Sprintf(format, args...)
		return nil, a
	}
	byIP := map[string]mixedHeldMember{}
	for _, m := range window {
		byIP[m.IP] = m
	}
	tc := newClient(t)
	tc.base = trigger.member.HTTPBase()
	run, started, err := tc.triggerRun(ctx, fx.Jobs["inflight"].ID, nil)
	if err != nil {
		a.Outcome = "trigger failed: " + err.Error()
		return nil, a
	}
	if !started {
		a.Outcome = "trigger admitted to the queue instead of starting"
		return nil, a
	}
	a.RunID = run.ID
	startSeen := false
	for until := time.Now().Add(15 * time.Second); time.Now().Before(until) && !startSeen; {
		for _, event := range recorderEvents(t) {
			startSeen = startSeen || event.RunID == run.ID && event.Kind == "start"
		}
		if !startSeen {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if !startSeen {
		releaseRecordedRun(t, run.ID)
		a.Outcome = "no raw start effect within 15s"
		return nil, a
	}
	leaseCtx, cancelLease := context.WithTimeout(ctx, 15*time.Second)
	lease, err := cluster.WaitLease(leaseCtx, h, readBase, run.ID, "")
	cancelLease()
	if err != nil {
		releaseRecordedRun(t, run.ID)
		a.Outcome = "run lease unobservable: " + err.Error()
		return nil, a
	}
	live, err := h.GetRun(ctx, readBase, run.JobID, run.ID)
	if err != nil || len(live.Tasks) != 1 {
		releaseRecordedRun(t, run.ID)
		a.Outcome = fmt.Sprintf("running projection unreadable or not one task: err=%v", err)
		return nil, a
	}
	beforeTask, err := readClusterTaskProof(ctx, h, readBase, run.ID, live.Tasks[0].ID)
	if err != nil || beforeTask.ClaimedBy == "" || beforeTask.RuntimeID == "" || beforeTask.ClaimAttempt < 1 ||
		beforeTask.OwnerGeneration != lease.Generation ||
		beforeTask.Attempt != live.Tasks[0].Attempt || beforeTask.Status != "running" {
		releaseRecordedRun(t, run.ID)
		a.Outcome = fmt.Sprintf("durable running attempt not proved before release: err=%v proof=%+v lease_generation=%d", err, beforeTask, lease.Generation)
		return nil, a
	}
	a.OwnerNode, a.WorkerNode = lease.OwnerNode, beforeTask.ClaimedBy
	owner, ownerOK := byIP[cluster.HostIP(lease.OwnerNode)]
	worker, workerOK := byIP[cluster.HostIP(beforeTask.ClaimedBy)]
	a.OwnerVersion, a.WorkerVersion = owner.Version, worker.Version
	a.CrossVersion = ownerOK && workerOK && owner.Version != worker.Version
	releaseRecordedRun(t, run.ID)
	rc := newClient(t)
	rc.base = readBase
	final, err := rc.awaitRunStatus(ctx, run.JobID, run.ID, func(r apiRun) bool { return isTerminal(r.Status) }, 60*time.Second)
	a.ObservedStatus = final.Status
	switch {
	case !ownerOK || !workerOK:
		a.Outcome = "lease owner or claimed worker is not a held member"
		return nil, a
	case !a.CrossVersion:
		a.Outcome = "owner and worker ran the same version"
		return nil, a
	case err != nil:
		a.Outcome = "proved cross-version run still in flight 60s after release: " + err.Error()
		return nil, a
	case final.Status != "succeeded":
		return failed("proved cross-version run ended %s (error %q)", final.Status, final.Error)
	case len(final.Tasks) != 1 || final.Tasks[0].ID != live.Tasks[0].ID || final.Tasks[0].Attempt != beforeTask.Attempt:
		return failed("proved cross-version run succeeded, but its terminal projection is not the released attempt (task %s attempt %d; %d terminal tasks)",
			live.Tasks[0].ID, beforeTask.Attempt, len(final.Tasks))
	}
	afterTask, err := readClusterTaskProof(ctx, h, readBase, run.ID, beforeTask.TaskID)
	switch {
	case errors.Is(err, errTaskProofUnavailable):
		a.Outcome = "terminal durable attempt unreadable: " + err.Error()
		return nil, a
	case err != nil || afterTask.ID != beforeTask.ID || afterTask.Attempt != beforeTask.Attempt ||
		afterTask.ClaimAttempt != beforeTask.ClaimAttempt || afterTask.RuntimeID != beforeTask.RuntimeID ||
		afterTask.ClaimedBy != beforeTask.ClaimedBy || afterTask.OwnerGeneration != lease.Generation ||
		afterTask.Status != "succeeded":
		return failed("proved cross-version run changed its durable attempt: err=%v before=%+v after=%+v lease_generation=%d",
			err, beforeTask, afterTask, lease.Generation)
	case !rawCompletionMatchesTask(run.ID, afterTask, recorderEvents(t)):
		return failed("proved cross-version run succeeded without the raw completion of its durable attempt: proof=%+v", afterTask)
	case owner.Version != direction.OwnerVersion:
		a.Outcome = fmt.Sprintf("owner ran %s, direction needs a %s owner (the opposite crossing succeeded)", owner.Version, direction.OwnerVersion)
		return nil, a
	}
	a.Verdict, a.Outcome = mixedCrossed, "crossed"
	return map[string]any{"direction": direction.Name, "run_id": run.ID, "trigger_member": trigger.Name,
		"owner_node": lease.OwnerNode, "owner_version": owner.Version, "owner_generation": lease.Generation,
		"worker_node": beforeTask.ClaimedBy, "worker_version": worker.Version,
		"task_before_release": beforeTask, "task_after_completion": afterTask,
		"owner_member": owner, "worker_member": worker,
		"terminal_status": final.Status, "raw_effect_nonce": afterTask.RecorderNonce}, a
}

// mixedObservation is everything one held-window observation saw.
type mixedObservation struct {
	Budget    time.Duration
	Attempts  []mixedAttempt
	Crossings map[string]map[string]any
	// Failures are proved cross-version runs that failed. The first one
	// decides the case; no later crossing, in either direction, clears it.
	Failures []mixedAttempt
}

// observeMixedDirections tries each direction until it crosses, a proved
// cross-version run fails, or the direction's budget closes. A failure stops
// that direction's retries; the next direction is still observed so the record
// carries evidence for both.
func observeMixedDirections(ctx context.Context, directions []mixedDirection, budget, pause time.Duration,
	attempt func(direction mixedDirection, n int) (map[string]any, mixedAttempt), logged func([]mixedAttempt)) mixedObservation {
	obs := mixedObservation{Budget: budget, Crossings: map[string]map[string]any{}}
	for _, direction := range directions {
		deadline := time.Now().Add(budget)
		for n := 0; ctx.Err() == nil && time.Now().Before(deadline); n++ {
			cross, a := attempt(direction, n)
			obs.Attempts = append(obs.Attempts, a)
			logged(obs.Attempts)
			if a.Verdict == mixedFailed {
				obs.Failures = append(obs.Failures, a)
				break
			}
			if a.Verdict == mixedCrossed && cross != nil {
				obs.Crossings[direction.Name] = cross
				break
			}
			time.Sleep(pause)
		}
	}
	return obs
}

// mixedCaseRecord decides the case from one observation; the caller attaches
// the window evidence. A proved cross-version failure outranks both a later
// crossing and a direction that never crossed.
func mixedCaseRecord(obs mixedObservation, directions []mixedDirection) caseRecord {
	if len(obs.Failures) > 0 {
		f := obs.Failures[0]
		return caseRecord{Name: mixedCaseName, Status: statusFail, Detail: fmt.Sprintf(
			"proved cross-version run %s (%s owner %s, %s worker %s; attempted for %s via %s) observed status %q: %s (%d proved failure(s), %d attempts in cluster-mixed-attempts.json)",
			f.RunID, f.OwnerVersion, f.OwnerNode, f.WorkerVersion, f.WorkerNode, f.Direction, f.TriggerMember,
			f.ObservedStatus, f.Outcome, len(obs.Failures), len(obs.Attempts))}
	}
	for _, direction := range directions {
		if obs.Crossings[direction.Name] != nil {
			continue
		}
		last := "no attempt ran"
		for _, a := range obs.Attempts {
			if a.Direction == direction.Name {
				last = a.Outcome
			}
		}
		return caseRecord{Name: mixedCaseName, Status: statusBlocked, Detail: fmt.Sprintf(
			"held window open, but no %s dispatch and completion was jointly observed within %s (%d attempts in cluster-mixed-attempts.json; last: %s)",
			direction.Name, obs.Budget, len(obs.Attempts), last)}
	}
	return caseRecord{Name: mixedCaseName, Status: statusPass,
		Detail: "inside a held window (caesium-2 candidate, caesium-0/1 previous, all protocol 2) the same task attempt and fenced owner generation dispatched and completed in both directions across the version boundary; raw nonces persisted in terminal task output"}
}

// mixedCaseAfterUpgrade re-derives the case from the held observation once the
// rollout finished. It never replaces a failed record the held window wrote.
func mixedCaseAfterUpgrade(t *testing.T) (rec caseRecord, write bool) {
	t.Helper()
	var held caseRecord
	if readJSON(t, filepath.Join("cases", sanitize(mixedCaseName)+".json"), &held) && held.Status == statusFail &&
		held.LifecycleID == envOr("CAESIUM_LIFECYCLE_ID", "") {
		return held, false
	}
	var crossing map[string]any
	crossed := readJSON(t, "cluster-mixed-crossing.json", &crossing)
	directions, _ := crossing["crossings"].(map[string]any)
	failures, _ := crossing["failures"].([]any)
	_, forward := directions["candidate-owner-previous-worker"]
	_, reverse := directions["previous-owner-candidate-worker"]
	switch {
	case len(failures) > 0:
		return caseRecord{Name: mixedCaseName, Status: statusFail, Observations: crossing,
			Detail: "cluster-mixed-crossing.json records a proved cross-version run that failed inside the held window"}, true
	case !crossed || !forward || !reverse:
		return caseRecord{Name: mixedCaseName, Status: statusBlocked,
			Detail: "the held window did not record both opposite-version crossings (candidate owner with a previous worker, and previous owner with a candidate worker), each with a terminal run and raw completion effect"}, true
	}
	return caseRecord{Name: mixedCaseName, Status: statusPass,
		Detail:       "held protocol-2 mixed window; opposite-version dispatch and completion observed in both directions",
		Observations: crossing}, true
}

func TestLifecycleClusterMixedWindow(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, mixedCaseName, "seed fixture missing")
	}
	var hold map[string]any
	if !readJSON(t, "cluster-mixed-hold.json", &hold) || hold["held"] != true || hold["lifecycle_id"] != fx.LifecycleID ||
		hold["updated_member"] != mixedHeldCandidateMember {
		blockf(t, mixedCaseName, "host did not record a verified hold at exactly %s upgraded (cluster-mixed-hold.json)", mixedHeldCandidateMember)
	}
	ctx, cancel := context.WithTimeout(t.Context(), mixedWindowBudget)
	defer cancel()
	oldID := mustEnv(t, "CAESIUM_LIFECYCLE_PREVIOUS_IMAGE_ID")
	newID := mustEnv(t, "CAESIUM_LIFECYCLE_CANDIDATE_IMAGE_ID")
	h := cluster.NewHTTP(mustEnv(t, "CAESIUM_MANUAL_TRIGGER_API_KEY"))

	// The candidate pod was Ready before Helm returned, but its internal mTLS
	// listener and CA read can trail readiness; retry the probe, bounded.
	var window map[string]mixedHeldMember
	lastProbe := ""
	for deadline := time.Now().Add(mixedProbeBudget); ctx.Err() == nil; {
		held, err := heldMixedWindow(ctx, fx, oldID, newID)
		if err != nil {
			lastProbe = err.Error()
		} else if probed, reason := probeMixedProtocols(ctx, t, h, held); reason != "" {
			lastProbe = reason
		} else {
			window = probed
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	if window == nil {
		blockf(t, mixedCaseName, "held mixed-version window with three protocol-2 members was not observable within %s; last probe: %s",
			mixedProbeBudget, lastProbe)
	}
	writeJSON(t, "cluster-mixed-window.json", map[string]any{"start": window})
	readBase := window[mixedHeldCandidateMember].member.HTTPBase()

	obs := observeMixedDirections(ctx, mixedDirections, mixedDirectionBudget, time.Second,
		func(direction mixedDirection, n int) (map[string]any, mixedAttempt) {
			trigger := window[direction.Triggers[n%len(direction.Triggers)]]
			cross, attempt := attemptMixedCrossing(ctx, t, fx, h, readBase, window, direction, trigger)
			t.Logf("mixed attempt %s via %s: run=%s owner=%s(%s) worker=%s(%s) status=%s %.1fs: %s: %s", attempt.Direction,
				attempt.TriggerMember, attempt.RunID, attempt.OwnerNode, attempt.OwnerVersion,
				attempt.WorkerNode, attempt.WorkerVersion, attempt.ObservedStatus, attempt.Seconds, attempt.Verdict, attempt.Outcome)
			return cross, attempt
		},
		func(attempts []mixedAttempt) { writeJSON(t, "cluster-mixed-attempts.json", attempts) })
	rec := mixedCaseRecord(obs, mixedDirections)
	cross := map[string]any{"hold": hold, "window_start": window, "crossings": obs.Crossings,
		"attempts": obs.Attempts, "failures": obs.Failures}
	if rec.Status != statusPass {
		// A proved cross-version failure is decisive and a missing direction
		// is blocked; both keep everything observed, including the end window.
		if end, err := heldMixedWindow(ctx, fx, oldID, newID); err != nil {
			cross["window_end_error"] = err.Error()
		} else {
			cross["window_end"] = end
		}
		writeJSON(t, "cluster-mixed-crossing.json", cross)
		rec.Observations = cross
		writeCase(t, rec)
		t.Fatalf("%s %s: %s", strings.ToUpper(rec.Status), mixedCaseName, rec.Detail)
	}

	// Every crossing must have happened inside one held window: the same three
	// pods, UIDs and image IDs at the end as at the start.
	end, err := heldMixedWindow(ctx, fx, oldID, newID)
	if err != nil {
		blockf(t, mixedCaseName, "held window did not survive the observation: %v", err)
	}
	for name, first := range window {
		last := end[name]
		if last.UID != first.UID || last.ImageID != first.ImageID || last.IP != first.IP {
			blockf(t, mixedCaseName, "%s changed inside the held window (uid %s→%s, image %s→%s)",
				name, first.UID, last.UID, first.ImageID, last.ImageID)
		}
	}
	writeJSON(t, "cluster-mixed-window.json", map[string]any{"start": window, "end": end})
	cross["window_end"] = end
	writeJSON(t, "cluster-mixed-crossing.json", cross)
	rec.Observations = cross
	writeCase(t, rec)
}

func TestLifecycleClusterAfterUpgrade(t *testing.T) {
	caseName := "rolling-upgrade-three-voters"
	defer func() {
		if !t.Failed() {
			return
		}
		caseDir := filepath.Join(artifactsDir(t), "cases")
		path := filepath.Join(caseDir, sanitize(caseName)+".json")
		// A prerequisite helper may have recorded its own blocked case (for
		// example unavailable direct Raft membership or raw recorder data).
		// That cannot turn into a failed product assertion here.
		files, err := filepath.Glob(filepath.Join(caseDir, "*.json"))
		if err != nil {
			return
		}
		for _, file := range files {
			raw, readErr := os.ReadFile(file)
			if readErr != nil {
				continue
			}
			var recorded caseRecord
			if json.Unmarshal(raw, &recorded) == nil && recorded.Phase == "AfterUpgrade" && recorded.Status == statusBlocked {
				return
			}
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			writeCase(t, caseRecord{Name: caseName, Status: statusFail,
				Detail: "runner assertion failed; see cluster-logs/AfterUpgrade.log for the observed mismatch"})
		}
	}()
	clusterPair(t)
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "rolling-upgrade-three-voters", "seed fixture missing")
	}
	_, h, topo := clusterKube(t)
	membership := clusterMembership(t, topo)
	base := clusterBase(topo)
	c := newClient(t)
	c.base = base
	ctx := t.Context()
	before := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		before[m.Name] = m
	}
	changedIPs := map[string]map[string]string{}
	for _, m := range memberEvidence(topo, membership) {
		prev, ok := before[m.Name]
		require.True(t, ok)
		require.NotEqual(t, prev.UID, m.UID, "pod was not recreated")
		require.Equal(t, prev.PVC, m.PVC)
		require.Equal(t, prev.Volume, m.Volume)
		require.Equal(t, prev.DqliteID, m.DqliteID, "retained member changed ID")
		if m.IP != prev.IP {
			changedIPs[m.Name] = map[string]string{"before": prev.IP, "after": m.IP}
		}
	}
	if len(changedIPs) == 0 {
		blockf(t, "rolling-upgrade-three-voters", "all recreated pods reused their IPs; retained-PVC address reconciliation from #536 was not exercised")
	}
	var host clusterHostObservation
	if !readJSON(t, "cluster-host-observation.json", &host) {
		blockf(t, "rolling-upgrade-three-voters", "host manifest/info/log observations absent")
	}
	require.Empty(t, host.ManifestDiff, "rendered Helm resources changed beyond image reference")
	require.True(t, host.ManifestLiveCaptured, "helm get manifest was not captured before and after the upgrade")
	require.Empty(t, host.ManifestLiveDiff, "installed Helm resources changed beyond image reference")
	require.Len(t, host.BeforeInfo, 3)
	require.Len(t, host.AfterInfo, 3)
	infos := map[string]clusterInfo{}
	for _, i := range host.AfterInfo {
		infos[i.Name] = i
	}
	for _, m := range memberEvidence(topo, membership) {
		i, ok := infos[m.Name]
		require.True(t, ok)
		require.Equal(t, m.DqliteID, i.ID)
		require.Equal(t, m.IP+":9001", i.Address, "#536 address reconciliation did not follow the recreated pod")
		status, ok := host.PodStatuses[m.Name]
		require.True(t, ok, "missing container status for %s", m.Name)
		require.Equal(t, "Running", status.Phase)
		require.Equal(t, m.IP, status.PodIP)
		require.True(t, status.Ready, "%s container not Ready", m.Name)
		require.Zero(t, status.RestartCount, "%s restarted during the candidate rollout; inspect previous log and exit state", m.Name)
		_, running := status.State["running"]
		require.True(t, running, "%s container is not running; state=%v", m.Name, status.State)
		_, terminated := status.LastState["terminated"]
		require.False(t, terminated, "%s has a prior terminated container: %v", m.Name, status.LastState)
		log, ok := host.PodLogs[m.Name]
		require.True(t, ok, "missing migration log for %s", m.Name)
		require.Contains(t, log, "migrating database")
		require.NotContains(t, log, "failed to connect to database")
		previousLog, ok := host.PreviousPodLogs[m.Name]
		require.True(t, ok, "missing previous-log probe for %s", m.Name)
		require.NotContains(t, previousLog, "failed to connect to database")
		require.NotContains(t, previousLog, "in info.yaml does not match")
	}
	// A nonzero Helm exit is not itself the verdict; all pod and Raft facts
	// above and below are gathered even after a --wait timeout.
	for _, m := range topo.Members {
		require.NoError(t, h.Health(ctx, m.HTTPBase()))
	}
	writeCase(t, caseRecord{Name: "rolling-upgrade-three-voters", Status: statusPass,
		Detail: fmt.Sprintf("three retained voters after rolling upgrade; Helm exit=%d; leader=%s", host.HelmExitCode, membership.Leader.Address),
		Observations: map[string]any{"members": memberEvidence(topo, membership), "changed_pod_ips": changedIPs,
			"helm_exit_code": host.HelmExitCode}})
	caseName = "retained-history-and-raw-effects"
	for _, want := range []runFixture{fx.Succeeded, fx.Failed} {
		assertRunUnchanged(t, ctx, c, want, want.Status)
		require.Len(t, want.Tasks, 1)
		proof, err := readRetainedClusterTaskProof(ctx, h, base, fx.DurableTasks, want.ID, want.Tasks[0].ID)
		if err != nil {
			if errors.Is(err, errTaskProofUnavailable) {
				blockf(t, "retained-history-and-raw-effects", "terminal run %s durable task query unavailable: %v", want.ID, err)
			}
			failClusterCase(t, "retained-history-and-raw-effects", "terminal run %s lost its pre-upgrade durable task row: %v", want.ID, err)
		}
		require.Equal(t, want.Tasks[0].Status, proof.Status, "terminal durable task status changed")
		got, err := readEventBacklog(ctx, c, want.ID, want.ResumeCursor)
		require.NoError(t, err)
		set := map[string]bool{}
		for _, e := range got {
			set[e.key()] = true
		}
		for _, e := range want.Events {
			if e.Sequence > want.ResumeCursor {
				require.Truef(t, set[e.key()], "event %s disappeared", e.key())
			}
		}
	}
	for _, j := range fx.Jobs {
		id, err := c.jobIDByAlias(ctx, j.Alias)
		require.NoError(t, err)
		require.Equal(t, j.ID, id)
	}
	preRelease := map[string]apiRun{}
	preReleaseProofs := map[string]clusterTaskProof{}
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		current, err := c.run(ctx, r.JobID, r.ID)
		if err != nil {
			blockf(t, "retained-history-and-raw-effects", "in-flight run %s unobservable before controlled release: %v", r.ID, err)
		}
		preRelease[r.ID] = current
		writeJSON(t, "cluster-inflight-before-release.json", preRelease)
		if current.Status != "running" || len(r.Tasks) != 1 || len(current.Tasks) != 1 ||
			current.Tasks[0].ID != r.Tasks[0].ID || current.Tasks[0].Attempt < r.Tasks[0].Attempt ||
			current.Tasks[0].Status != "running" {
			failClusterCase(t, "retained-history-and-raw-effects", "in-flight task attempt %s did not survive upgrade to controlled release; status=%s", r.ID, current.Status)
		}
		proof, err := readRetainedClusterTaskProof(ctx, h, base, fx.DurableTasks, r.ID, current.Tasks[0].ID)
		if err != nil {
			if errors.Is(err, errTaskProofUnavailable) {
				blockf(t, "retained-history-and-raw-effects", "in-flight run %s durable task query unavailable before release: %v", r.ID, err)
			}
			failClusterCase(t, "retained-history-and-raw-effects", "in-flight run %s lost its pre-upgrade durable task row before release: %v", r.ID, err)
		}
		if proof.Status != "running" || proof.Attempt != current.Tasks[0].Attempt {
			failClusterCase(t, "retained-history-and-raw-effects", "in-flight run %s durable attempt changed before release: proof=%+v", r.ID, proof)
		}
		preReleaseProofs[r.ID] = proof
	}
	writeJSON(t, "cluster-inflight-durable-before-release.json", preReleaseProofs)
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		releaseRecordedRun(t, r.ID)
	}
	attemptProofs := map[string][]clusterTaskProof{}
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		got, err := c.awaitRunStatus(ctx, r.JobID, r.ID, func(x apiRun) bool { return isTerminal(x.Status) }, 5*time.Minute)
		require.NoError(t, err)
		require.Len(t, got.Tasks, 1, "in-flight run %s changed its public task count", r.ID)
		// Preserve the pre-upgrade event tuples and join every raw pod effect
		// to the persisted task_started payload for that durable attempt.
		events := recorderEvents(t)
		taskEvents, err := readEventBacklog(ctx, c, r.ID, r.ResumeCursor)
		if err != nil {
			blockf(t, "retained-history-and-raw-effects", "controlled release run %s event backlog unavailable: %v", r.ID, err)
		}
		writeJSON(t, "cluster-inflight-events-"+r.ID+".json", taskEvents)
		retainedEvents := map[string]bool{}
		for _, event := range taskEvents {
			retainedEvents[event.key()] = true
		}
		for _, event := range r.Events {
			if event.Sequence > r.ResumeCursor && !retainedEvents[event.key()] {
				failClusterCase(t, "retained-history-and-raw-effects", "controlled release run %s lost pre-upgrade event %s", r.ID, event.key())
			}
		}
		matchedAttempt := false
		for _, task := range got.Tasks {
			proof, err := readRetainedClusterTaskProof(ctx, h, base, fx.DurableTasks, got.ID, task.ID)
			if err != nil {
				if errors.Is(err, errTaskProofUnavailable) {
					blockf(t, "retained-history-and-raw-effects", "controlled release run %s durable task query unavailable: %v", got.ID, err)
				}
				failClusterCase(t, "retained-history-and-raw-effects", "controlled release run %s lost its pre-upgrade durable task row: %v", got.ID, err)
			}
			if proof.Attempt < preReleaseProofs[r.ID].Attempt || proof.Attempt != task.Attempt {
				failClusterCase(t, "retained-history-and-raw-effects", "controlled release run %s task attempt regressed or disagreed with public projection: before=%d durable=%d public=%d", got.ID, preReleaseProofs[r.ID].Attempt, proof.Attempt, task.Attempt)
			}
			if err := reconcileRetainedAttemptEffects(fx.DurableTasks[r.ID], proof, fx.RawStartNonces[r.ID], events, taskEvents); err != nil {
				failClusterCase(t, "retained-history-and-raw-effects", "controlled release run %s raw attempts could not be reconciled: %v", got.ID, err)
			}
			attemptProofs[r.ID] = append(attemptProofs[r.ID], proof)
			if proof.TaskID == r.Tasks[0].ID && proof.Attempt >= r.Tasks[0].Attempt &&
				proof.Status == "succeeded" && rawCompletionMatchesTask(r.ID, proof, events) {
				matchedAttempt = true
			}
		}
		writeJSON(t, "cluster-inflight-outcome-"+r.ID+".json", map[string]any{
			"run": got, "seed_attempt": r.Tasks[0], "task_proofs": attemptProofs[r.ID],
			"raw_events": events, "matched_seed_attempt": matchedAttempt})
		if got.Status != "succeeded" || !matchedAttempt {
			failClusterCase(t, "retained-history-and-raw-effects", "controlled release did not complete the surviving task attempt for run %s: status=%s matched_seed_attempt=%t", r.ID, got.Status, matchedAttempt)
		}
	}
	var queued apiRun
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		runs, err := c.runs(ctx, fx.Jobs["queue"].ID)
		require.NoError(t, err)
		for _, r := range runs {
			if r.Params["TOKEN"] == fx.QueueToken {
				queued = r
				break
			}
		}
		if queued.ID != "" {
			break
		}
		time.Sleep(time.Second)
	}
	require.NotEmpty(t, queued.ID, "queued row never became a run")
	// The queued job uses the same held manifest as its predecessor. Admission
	// only starts its task; release the new run's own recorder barrier after
	// joining its current running durable attempt to task_started and raw start.
	waitRecorderStart(t, "retained-history-and-raw-effects", queued.ID)
	var queuedBefore apiRun
	var queuedBeforeProof clusterTaskProof
	var queuedBeforeStarts []string
	var queuedBeforeEvents []eventTuple
	var queuedBeforeRaw []recorder.Event
	var lastQueuedErr error
	for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); {
		live, err := c.run(ctx, queued.JobID, queued.ID)
		if err != nil {
			lastQueuedErr = err
		} else if isTerminal(live.Status) {
			failClusterCase(t, "retained-history-and-raw-effects", "queued run %s became terminal before its controlled release: %s", queued.ID, live.Status)
		} else if len(live.Tasks) != 1 {
			lastQueuedErr = fmt.Errorf("queued run %s exposes %d public tasks, want one running task", queued.ID, len(live.Tasks))
		} else {
			proof, proofErr := readClusterTaskProof(ctx, h, base, queued.ID, live.Tasks[0].ID)
			if proofErr != nil {
				lastQueuedErr = proofErr
			} else {
				taskEvents, eventErr := readEventBacklog(ctx, c, queued.ID, 0)
				if eventErr != nil {
					lastQueuedErr = eventErr
				} else {
					rawEvents := recorderEvents(t)
					starts, proofErr := verifyQueuedHeldAttempt(live, queued.ID, queued.JobID, fx.QueueToken, proof, rawEvents, taskEvents)
					if proofErr == nil {
						queuedBefore, queuedBeforeProof, queuedBeforeStarts = live, proof, starts
						queuedBeforeEvents, queuedBeforeRaw = taskEvents, rawEvents
						break
					}
					lastQueuedErr = proofErr
				}
			}
		}
		time.Sleep(time.Second)
	}
	if queuedBefore.ID == "" {
		blockf(t, "retained-history-and-raw-effects", "queued run %s has no current running durable task/start before release: %v", queued.ID, lastQueuedErr)
	}
	writeJSON(t, "cluster-queued-before-release.json", map[string]any{
		"run": queuedBefore, "durable_task": queuedBeforeProof, "current_start_nonces": queuedBeforeStarts,
		"task_events": queuedBeforeEvents, "raw_events": queuedBeforeRaw})
	releaseRecordedRun(t, queued.ID)
	queued, err := c.awaitRunStatus(ctx, queued.JobID, queued.ID, func(r apiRun) bool { return isTerminal(r.Status) }, 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, "succeeded", queued.Status)
	require.Len(t, queued.Tasks, 1, "queued run has no unique terminal task attempt")
	proof, err := readClusterTaskProof(ctx, h, base, queued.ID, queued.Tasks[0].ID)
	if err != nil {
		if errors.Is(err, errTaskProofUnavailable) {
			blockf(t, "retained-history-and-raw-effects", "queued run %s terminal durable task query unavailable: %v", queued.ID, err)
		}
		failClusterCase(t, "retained-history-and-raw-effects", "queued run %s terminal durable task missing: %v", queued.ID, err)
	}
	if err := verifyRetainedTaskIdentity(queuedBeforeProof, proof, queued.ID, queuedBeforeProof.TaskID); err != nil {
		failClusterCase(t, "retained-history-and-raw-effects", "queued run %s changed its pre-release durable task identity: %v", queued.ID, err)
	}
	if queued.Tasks[0].Status != "succeeded" || proof.Status != "succeeded" || proof.Attempt != queued.Tasks[0].Attempt {
		failClusterCase(t, "retained-history-and-raw-effects", "queued run %s terminal public/durable task attempt disagrees: public=%+v durable=%+v", queued.ID, queued.Tasks[0], proof)
	}
	queuedEvents, err := readEventBacklog(ctx, c, queued.ID, 0)
	if err != nil {
		blockf(t, "retained-history-and-raw-effects", "queued run %s terminal event backlog unavailable: %v", queued.ID, err)
	}
	rawAfterQueued := recorderEvents(t)
	if err := reconcileRetainedAttemptEffects(queuedBeforeProof, proof, queuedBeforeStarts, rawAfterQueued, queuedEvents); err != nil {
		failClusterCase(t, "retained-history-and-raw-effects", "queued run %s pre-release attempt did not reconcile with terminal effects: %v", queued.ID, err)
	}
	attemptProofs[queued.ID] = []clusterTaskProof{proof}
	writeJSON(t, "cluster-queued-after-release.json", map[string]any{
		"run": queued, "durable_task": proof, "task_events": queuedEvents, "raw_events": rawAfterQueued})
	rows, err := c.queue(ctx, fx.Jobs["queue"].ID)
	require.NoError(t, err)
	for _, row := range rows {
		require.NotEqual(t, fx.QueuedRow.ID, row.ID)
	}
	writeJSON(t, "cluster-raw-after.json", recorderEvents(t))
	writeJSON(t, "cluster-attempt-proofs.json", attemptProofs)
	writeCase(t, caseRecord{Name: "retained-history-and-raw-effects", Status: statusPass,
		Detail: "pre-upgrade job/run/task IDs and event tuple sets retained; in-flight raw ledger reconciled; queued row drained",
		Observations: map[string]any{"queued_run_id": queued.ID, "queued_row_id": fx.QueuedRow.ID,
			"in_flight_run_id": fx.InFlight.ID, "predecessor_run_id": fx.Predecessor.ID}})
	if rec, write := mixedCaseAfterUpgrade(t); write {
		writeCase(t, rec)
	}
}

// The joining ordinal is deliberately 1: its peer-discovery init container
// receives an older sibling, whereas ordinal 0 receives an empty peer list.
func TestLifecycleClusterJoiningOrdinalOne(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "joining-ordinal-1-replacement", "seed fixture missing")
	}
	_, _, topo := clusterKube(t)
	old := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		old[m.Name] = m
	}
	replaced, ok := topo.ByName("caesium-1")
	require.True(t, ok)
	require.NotEqual(t, old["caesium-1"].UID, replaced.UID, "pod was not replaced")
	require.NotEqual(t, old["caesium-1"].Volume, replaced.VolumeName, "PVC retained its old PV: disk loss was not injected")
	live := map[string]bool{}
	for _, member := range topo.Members {
		live[member.DqliteAddr()] = true
	}
	leaderAddress := ""
	for _, m := range topo.Members {
		leader, members, err := cluster.QueryNode(t.Context(), m.DqliteAddr())
		require.NoErrorf(t, err, "direct dqlite RPC to %s", m.Name)
		require.NotNil(t, leader)
		if leaderAddress == "" {
			leaderAddress = leader.Address
		}
		require.Equal(t, leaderAddress, leader.Address, "members disagree on leader")
		foundNew, foundOld, voterCount := false, false, 0
		liveVoters := map[string]bool{}
		for _, n := range members {
			if strings.EqualFold(n.Role.String(), "voter") {
				voterCount++
				if live[n.Address] {
					liveVoters[n.Address] = true
				}
			}
			if n.ID == old["caesium-1"].DqliteID {
				foundOld = true
			}
			if n.Address == replaced.DqliteAddr() && n.ID != old["caesium-1"].DqliteID {
				foundNew = true
			}
		}
		require.True(t, foundNew, "fresh ordinal 1 did not join %s's membership", m.Name)
		require.True(t, foundOld, "dead member's retained membership entry was not recorded")
		require.Len(t, liveVoters, 3, "not all three live members are voters")
		require.GreaterOrEqual(t, voterCount, 3)
	}
	// H1: the lost member's entry is removed with the operator command before
	// the case is judged (stale_member_removal_test.go); this phase proves the
	// entry was demoted and plans that removal.
	settleForStaleRemoval(t, "joining-ordinal-1-replacement", "ordinal1", topo, replaced.DqliteAddr(),
		[]uint64{old["caesium-1"].DqliteID},
		"fresh PVC and node ID joined every member's direct Cluster RPC; the lost member's entry was demoted to non-voting",
		map[string]any{"old_node_id": old["caesium-1"].DqliteID, "old_volume": old["caesium-1"].Volume,
			"new_volume": replaced.VolumeName, "new_pod_uid": replaced.UID})
}

// TestLifecycleClusterOrdinalZeroLoss qualifies #582: after caesium-0 loses its
// PVC while caesium-1/2 keep running, the replacement must join the existing
// cluster as a new member rather than bootstrap a divergent one. Every member's
// direct Cluster RPC, including the fresh node's own, must show one cluster
// with one leader, and the fixture must read the same through caesium-0. The
// case record is written by TestLifecycleClusterOrdinalZeroRemoved, after the
// host removed the stale entry with the operator command (H1).
func TestLifecycleClusterOrdinalZeroLoss(t *testing.T) {
	const name = "ordinal-0-disk-loss"
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, name, "seed fixture missing")
	}
	old := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		old[m.Name] = m
	}
	oldZero := old["caesium-0"]
	evidence := map[string]any{"old_id": oldZero.DqliteID, "old_address": oldZero.Address, "old_volume": oldZero.Volume}

	var host map[string]any
	if !readJSON(t, "cluster-ordinal0-host.json", &host) {
		blockf(t, name, "fresh PVC, info.yaml and node-store evidence absent")
	}
	for k, v := range host {
		evidence[k] = v
	}
	if host["old_uid"] == host["new_uid"] {
		failClusterCase(t, name, "ordinal-0 pod was not recreated (uid %v)", host["new_uid"])
	}
	var pvc struct {
		Spec struct {
			VolumeName string `json:"volumeName"`
		} `json:"spec"`
	}
	pvcRaw, _ := host["pvc"].(string)
	if err := json.Unmarshal([]byte(pvcRaw), &pvc); err != nil || strings.TrimSpace(pvc.Spec.VolumeName) == "" {
		blockf(t, name, "fresh PVC volumeName unobservable: %v", err)
	}
	if pvc.Spec.VolumeName == oldZero.Volume {
		failClusterCase(t, name, "ordinal-0 PVC retained the old PV %s: disk loss was not injected", oldZero.Volume)
	}
	evidence["fresh_volume"] = pvc.Spec.VolumeName
	infoRaw, _ := host["info_yaml"].(string)
	var info struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	if err := yaml.Unmarshal([]byte(infoRaw), &info); err != nil || info.ID == 0 || strings.TrimSpace(info.Address) == "" {
		blockf(t, name, "fresh info.yaml unobservable: id=%d address=%q err=%v", info.ID, info.Address, err)
	}
	evidence["fresh_info_id"] = info.ID
	evidence["fresh_info_address"] = info.Address
	if info.ID == oldZero.DqliteID || info.ID == dqliteBootstrapID {
		failClusterCase(t, name, "fresh ordinal 0 reused node ID %d (old %d, bootstrap %d): it did not join as a new member",
			info.ID, oldZero.DqliteID, dqliteBootstrapID)
	}

	_, h, topo := clusterKube(t)
	fresh, ok := topo.ByName("caesium-0")
	if !ok {
		blockf(t, name, "caesium-0 absent from the ready topology")
	}
	if fresh.DqliteAddr() != info.Address {
		failClusterCase(t, name, "fresh info.yaml address %s is not the pod's dqlite address %s", info.Address, fresh.DqliteAddr())
	}

	// Role adjustment runs on the leader every 30 s: it promotes the new
	// member, then demotes the lost member's still-voting entry. Poll for a
	// bounded window and judge the last complete observation, so a stale entry
	// that never stops voting fails the case instead of passing it.
	liveAddrs := make([]string, 0, len(topo.Members))
	for _, m := range topo.Members {
		liveAddrs = append(liveAddrs, m.DqliteAddr())
	}
	staleIDs := []uint64{oldZero.DqliteID, dqliteBootstrapID, old["caesium-1"].DqliteID}
	var views []ordinalZeroView
	var membership ordinalZeroMembership
	var settleErr, rpcErr error
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var round []ordinalZeroView
		rpcErr = nil
		for _, m := range topo.Members {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			leader, members, err := cluster.QueryNode(ctx, m.DqliteAddr())
			cancel()
			if err != nil || leader == nil {
				rpcErr = fmt.Errorf("%s: leader=%v err=%v", m.Name, leader, err)
				break
			}
			round = append(round, newOrdinalZeroView(m.Name, m.DqliteAddr(), *leader, members))
		}
		if rpcErr == nil {
			views = round
			membership, settleErr = ordinalZeroSettled(views, info.ID, fresh.DqliteAddr(), liveAddrs, staleIDs)
			if settleErr == nil {
				break
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if views == nil {
		blockf(t, name, "direct dqlite RPC unanswered after 3m: %v", rpcErr)
	}
	if membership.StaleEntries == nil {
		membership.StaleEntries = []ordinalZeroEntry{}
	}
	evidence["direct_cluster_views"] = views
	evidence["voter_addresses"] = membership.VoterAddresses
	evidence["stale_entries"] = membership.StaleEntries
	if rpcErr != nil {
		evidence["last_rpc_error"] = rpcErr.Error()
	}
	if settleErr != nil {
		writeCase(t, caseRecord{Name: name, Status: statusFail, Detail: settleErr.Error(), Observations: evidence})
		t.Fatalf("FAIL %s: %v", name, settleErr)
	}
	stale := make([]string, 0, len(membership.StaleEntries))
	for _, e := range membership.StaleEntries {
		stale = append(stale, fmt.Sprintf("%d/%s/%s", e.ID, e.Address, e.Role))
	}

	// The fixture must read the same through the fresh node as through the
	// survivors. A divergent node would serve its own empty database here.
	c := newClient(t)
	for _, m := range topo.Members {
		c.base = m.HTTPBase()
		assertRunUnchanged(t, t.Context(), c, fx.Succeeded, "succeeded via "+m.Name)
		assertRunUnchanged(t, t.Context(), c, fx.Failed, "failed via "+m.Name)
		raw, err := h.SystemNodes(t.Context(), m.HTTPBase())
		if err != nil {
			blockf(t, name, "GET /v1/system/nodes via %s: %v", m.Name, err)
		}
		evidence["system_nodes_"+m.Name] = json.RawMessage(raw)
	}
	// H1: the case is judged by TestLifecycleClusterOrdinalZeroRemoved once the
	// host has removed the stale entries with the operator command.
	writeStaleRemovalPlan(t, name, "ordinal0", topo, views, staleIDs,
		fmt.Sprintf("fresh ordinal 0 joined as new node %d at %s; every member's direct Cluster RPC agrees on one leader and one membership; "+
			"voters are exactly the three live members; retained runs read identically through caesium-0 and survivors; "+
			"non-voting stale entries before removal: %v", info.ID, fresh.DqliteAddr(), stale),
		evidence)
}

// The stopped-member snapshot experiment must inspect the actual surviving
// Raft leader, not assume that StatefulSet ordinal 0 owns the write stream.
func TestLifecycleClusterSnapshotLeader(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "snapshot-catch-up", "seed fixture missing")
	}
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	var leaderMember cluster.Member
	lastReason := "no stable surviving leader"
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	for ctx.Err() == nil && leaderMember.Name == "" {
		topo, err := cluster.DiscoverTopology(ctx, kube, fx.LifecycleID)
		if err != nil {
			lastReason = err.Error()
		} else if len(topo.Members) != 2 {
			lastReason = fmt.Sprintf("expected two surviving members, got %d", len(topo.Members))
		} else {
			leaders := map[string]bool{}
			for _, member := range topo.Members {
				if member.Name == "caesium-2" {
					lastReason = "stopped ordinal 2 is still present"
					break
				}
				leader, _, err := cluster.QueryNode(ctx, member.DqliteAddr())
				if err != nil || leader == nil {
					lastReason = fmt.Sprintf("direct Leader RPC on %s: %v", member.Name, err)
					break
				}
				leaders[leader.Address] = true
			}
			if len(leaders) == 1 {
				for _, member := range topo.Members {
					if leaders[member.DqliteAddr()] {
						leaderMember = member
					}
				}
			}
			if leaderMember.Name == "" {
				lastReason = "survivors do not agree on a live Raft leader"
			}
		}
		if leaderMember.Name == "" {
			time.Sleep(time.Second)
		}
	}
	if leaderMember.Name == "" {
		blockf(t, "snapshot-catch-up", "surviving Raft leader unobservable: %s", lastReason)
	}
	survivors := map[string]any{}
	for _, name := range []string{"caesium-0", "caesium-1"} {
		pod, err := kube.CoreV1().Pods(fx.LifecycleID).Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)
		var container *corev1.ContainerStatus
		for i := range pod.Status.ContainerStatuses {
			if pod.Status.ContainerStatuses[i].Name == "caesium" {
				container = &pod.Status.ContainerStatuses[i]
				break
			}
		}
		if container == nil || !container.Ready || container.State.Running == nil || container.ContainerID == "" {
			blockf(t, "snapshot-catch-up", "surviving %s container is not observably running and ready", name)
		}
		survivors[name] = map[string]any{
			"uid": string(pod.UID), "container_id": container.ContainerID,
			"restart_count": container.RestartCount, "started_at": container.State.Running.StartedAt,
		}
	}
	writeJSON(t, "cluster-snapshot-leader.json", map[string]any{
		"name": leaderMember.Name, "uid": leaderMember.UID,
		"ip": leaderMember.IP, "address": leaderMember.DqliteAddr(),
		"image_id": leaderMember.ImageID, "survivors": survivors,
		"observed_at": time.Now().UTC(),
	})
}

// Applying many distinct definitions drives real catalog writes while one
// member is stopped. A snapshot case still needs the host to measure actual
// leader truncation and local catch-up; this phase alone never passes it.
func TestLifecycleClusterGenerateSnapshotWrites(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "snapshot-catch-up", "seed fixture missing")
	}
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	pods, err := kube.CoreV1().Pods(fx.LifecycleID).List(t.Context(), cluster.ListOptions())
	require.NoError(t, err)
	require.Len(t, pods.Items, 2, "stopped member was not absent")
	base := "http://" + pods.Items[0].Status.PodIP + ":8080"
	h := cluster.NewHTTP(mustEnv(t, "CAESIUM_MANUAL_TRIGGER_API_KEY"))
	image := mustEnv(t, "CAESIUM_LIFECYCLE_TASK_IMAGE")
	acknowledged := 0
	defer func() {
		writeJSON(t, "cluster-snapshot-write-count.json", map[string]any{
			"acknowledged_applies": acknowledged, "required_applies": 1400,
		})
	}()
	for i := 0; i < 1400; i++ {
		def := clusterManifest(t, "history", fmt.Sprintf("lifecycle-snapshot-%s-%04d", suffixOf(fx.LifecycleID), i), image)
		if err := h.Apply(t.Context(), base, []jobdef.Definition{def}); err != nil {
			t.Fatalf("catalog write %d/1400: %v", i, err)
		}
		acknowledged++
	}
}

type snapshotDisputedReadback struct {
	Pod              string `json:"pod"`
	Base             string `json:"base"`
	HTTPStatus       int    `json:"http_status,omitempty"`
	Error            string `json:"error,omitempty"`
	JobID            string `json:"job_id"`
	Annotation       string `json:"annotation"`
	MatchesAttempted *bool  `json:"matches_attempted"`
}

func snapshotReadbackBases(expected, observed []corev1.Pod) map[string]string {
	bases := make(map[string]string, len(expected))
	for _, pod := range expected {
		bases[pod.Name] = ""
	}
	for _, pod := range observed {
		if _, wanted := bases[pod.Name]; wanted && pod.Status.PodIP != "" {
			bases[pod.Name] = "http://" + net.JoinHostPort(pod.Status.PodIP, "8080")
		}
	}
	return bases
}

// An EOF from Apply leaves the write's commit status unknown. Read each
// survivor directly with a short deadline and record what it serves. This is
// diagnostic evidence only: the caller still fails the snapshot phase.
func readSnapshotDisputedWrite(ctx context.Context, h *cluster.HTTP, bases map[string]string,
	jobID, expected string, timeout time.Duration) []snapshotDisputedReadback {
	names := make([]string, 0, len(bases))
	for name := range bases {
		names = append(names, name)
	}
	sort.Strings(names)
	readbacks := make([]snapshotDisputedReadback, 0, len(names))
	for _, name := range names {
		base := bases[name]
		row := snapshotDisputedReadback{Pod: name, Base: base}
		if base == "" {
			row.Error = "survivor has no observable HTTP address"
			readbacks = append(readbacks, row)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		status, raw, err := h.Do(probeCtx, http.MethodGet,
			strings.TrimRight(base, "/")+"/v1/jobs/"+jobID, nil)
		cancel()
		row.HTTPStatus = status
		if err != nil {
			row.Error = err.Error()
		} else if status != http.StatusOK {
			row.Error = fmt.Sprintf("job read returned HTTP %d (%d response bytes)", status, len(raw))
		} else {
			var stored struct {
				ID          string            `json:"id"`
				Annotations map[string]string `json:"annotations"`
			}
			if err := json.Unmarshal(raw, &stored); err != nil {
				row.Error = fmt.Sprintf("decode job readback: %v", err)
			} else {
				row.JobID = stored.ID
				row.Annotation = stored.Annotations["snapshot_write"]
				matches := stored.ID == jobID && row.Annotation == expected
				row.MatchesAttempted = &matches
			}
		}
		readbacks = append(readbacks, row)
	}
	return readbacks
}

// Further writes update one of the already-created jobs. Each annotation value
// changes monotonically, so the importer commits a real catalog mutation but
// its unchanged task topology does not add another DAG snapshot row. The host
// chooses another bounded batch only after measuring the surviving leader's
// on-disk snapshot and retained log segments.
func TestLifecycleClusterGenerateSnapshotUpdateBatch(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "snapshot-catch-up", "seed fixture missing")
	}
	batch, err := strconv.Atoi(mustEnv(t, "CAESIUM_LIFECYCLE_SNAPSHOT_BATCH"))
	require.NoError(t, err)
	require.GreaterOrEqual(t, batch, 1)
	require.LessOrEqual(t, batch, 18)
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	pods, err := kube.CoreV1().Pods(fx.LifecycleID).List(t.Context(), cluster.ListOptions())
	require.NoError(t, err)
	require.Len(t, pods.Items, 2, "stopped member was not absent")
	base := "http://" + pods.Items[0].Status.PodIP + ":8080"
	h := cluster.NewHTTP(mustEnv(t, "CAESIUM_MANUAL_TRIGGER_API_KEY"))
	alias := fmt.Sprintf("lifecycle-snapshot-%s-%04d", suffixOf(fx.LifecycleID), 0)
	before, err := h.JobByAlias(t.Context(), base, alias)
	require.NoError(t, err, "original catalog job must exist before update batch")
	def := clusterManifest(t, "history", alias, mustEnv(t, "CAESIUM_LIFECYCLE_TASK_IMAGE"))
	acknowledged := 0
	first := (batch-1)*500 + 1
	defer func() {
		writeJSON(t, fmt.Sprintf("cluster-snapshot-update-batch-%02d.json", batch), map[string]any{
			"batch": batch, "alias": alias, "job_id": before.ID,
			"acknowledged_applies": acknowledged, "required_applies": 500,
			"first_annotation": first, "last_annotation": first + acknowledged - 1,
		})
	}()
	for i := 0; i < 500; i++ {
		attempted := fmt.Sprintf("%06d", first+i)
		def.Metadata.Annotations = map[string]string{"snapshot_write": attempted}
		if err := h.Apply(t.Context(), base, []jobdef.Definition{def}); err != nil {
			readbackPods := pods.Items
			addressSource := "current pod listing"
			podRefreshError := ""
			refreshCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			currentPods, refreshErr := kube.CoreV1().Pods(fx.LifecycleID).List(refreshCtx, cluster.ListOptions())
			cancel()
			if refreshErr != nil {
				addressSource = "pre-batch pod listing"
				podRefreshError = refreshErr.Error()
			} else {
				readbackPods = currentPods.Items
			}
			bases := snapshotReadbackBases(pods.Items, readbackPods)
			writeJSON(t, fmt.Sprintf("cluster-snapshot-disputed-batch-%02d.json", batch), map[string]any{
				"lifecycle_id": fx.LifecycleID, "batch": batch, "write_index": i + 1, "alias": alias,
				"job_id": before.ID, "attempted_annotation": attempted,
				"apply_error": err.Error(), "observed_at": time.Now().UTC(),
				"address_source": addressSource, "pod_refresh_error": podRefreshError,
				"readbacks": readSnapshotDisputedWrite(t.Context(), h, bases, before.ID,
					attempted, 5*time.Second),
			})
			t.Fatalf("snapshot update batch %d write %d/500: %v", batch, i, err)
		}
		acknowledged++
	}
	after, err := h.JobByAlias(t.Context(), base, alias)
	require.NoError(t, err)
	require.Equal(t, before.ID, after.ID, "snapshot updates replaced the catalog job")
	status, raw, err := h.Do(t.Context(), http.MethodGet, base+"/v1/jobs/"+before.ID, nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)
	var stored struct {
		ID          string            `json:"id"`
		Annotations map[string]string `json:"annotations"`
	}
	require.NoError(t, json.Unmarshal(raw, &stored))
	require.Equal(t, before.ID, stored.ID)
	require.Equal(t, fmt.Sprintf("%06d", first+499), stored.Annotations["snapshot_write"],
		"last acknowledged annotation did not persist")
}

func TestLifecycleClusterPostStorage(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "storage-rejoin", "seed fixture missing")
	}
	_, h, topo := clusterKube(t)
	membership := clusterMembership(t, topo)
	old := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		old[m.Name] = m
	}
	for _, m := range memberEvidence(topo, membership) {
		require.Equalf(t, old[m.Name].DqliteID, m.DqliteID, "%s changed dqlite ID after stopped-volume recovery", m.Name)
		require.Equalf(t, old[m.Name].Volume, m.Volume, "%s did not rejoin on retained PVC", m.Name)
	}
	c := newClient(t)
	c.base = clusterBase(topo)
	assertRunUnchanged(t, t.Context(), c, fx.Succeeded, "succeeded")
	assertRunUnchanged(t, t.Context(), c, fx.Failed, "failed")
	var completedProofs map[string][]clusterTaskProof
	if !readJSON(t, "cluster-attempt-proofs.json", &completedProofs) {
		blockf(t, "storage-rejoin", "controlled-release durable task proofs missing")
	}
	for _, run := range []runFixture{fx.Succeeded, fx.Failed, fx.InFlight, fx.Predecessor} {
		require.Len(t, run.Tasks, 1)
		proof, err := readRetainedClusterTaskProof(t.Context(), h, c.base, fx.DurableTasks, run.ID, run.Tasks[0].ID)
		require.NoErrorf(t, err, "pre-upgrade durable task row for run %s changed after storage recovery", run.ID)
		if run.Status == "running" {
			require.Lenf(t, completedProofs[run.ID], 1, "released run %s has no terminal durable proof", run.ID)
			require.NoErrorf(t, verifyRetainedTaskIdentity(completedProofs[run.ID][0], proof, run.ID, run.Tasks[0].ID),
				"released run %s changed terminal durable attempt after storage recovery", run.ID)
			require.Equal(t, "succeeded", proof.Status, "released task did not remain terminal after storage recovery")
			require.Equal(t, completedProofs[run.ID][0].RecorderNonce, proof.RecorderNonce,
				"released task output nonce changed after storage recovery")
			require.Equal(t, completedProofs[run.ID][0].RecorderPod, proof.RecorderPod,
				"released task output pod changed after storage recovery")
			require.Equal(t, completedProofs[run.ID][0].RuntimeID, proof.RuntimeID,
				"released task runtime changed after storage recovery")
		} else {
			require.Equal(t, run.Tasks[0].Status, proof.Status, "terminal task status changed after storage recovery")
		}
	}
	writeJSON(t, "cluster-post-storage.json", map[string]any{"members": memberEvidence(topo, membership),
		"leader": membership.Leader, "retained_run_ids": []string{fx.Succeeded.ID, fx.Failed.ID, fx.InFlight.ID, fx.Predecessor.ID}})
}

// TestLifecycleClusterRollbackProbe is the in-cluster probe for F2's isolated
// rollback recorded-outcome case (W8-β). The host controller runs it in a pod
// of the ISOLATED rollback namespace, and in the main namespace's runner pod,
// because only in-cluster callers can dial pod IPs and only a caller inside
// the isolated namespace is permitted to reach its members. It never judges an
// outcome: every mode prints one marker-prefixed JSON line with what it saw,
// and the host decides whether the observation is complete. The test is
// skipped unless the host selects a mode.
//
//	listen  accept and close TCP connections on :9001 (isolation target)
//	dial    TCP-dial each label=host:port in CAESIUM_LIFECYCLE_ROLLBACK_TARGETS
//	http    read /health, /health/ready and the seeded fixture through each
//	        label=base URL in CAESIUM_LIFECYCLE_ROLLBACK_TARGETS
func TestLifecycleClusterRollbackProbe(t *testing.T) {
	const marker = "CAESIUM_ROLLBACK_PROBE_JSON "
	mode := strings.TrimSpace(os.Getenv("CAESIUM_LIFECYCLE_ROLLBACK_MODE"))
	if mode == "" {
		t.Skip("CAESIUM_LIFECYCLE_ROLLBACK_MODE is set only by scripts/lifecycle-tests.sh")
	}
	emit := func(v any) {
		raw, err := json.Marshal(v)
		require.NoError(t, err)
		fmt.Printf("%s%s\n", marker, raw)
	}
	type target struct{ Label, Value string }
	parseTargets := func() []target {
		var out []target
		for _, item := range strings.Split(mustEnv(t, "CAESIUM_LIFECYCLE_ROLLBACK_TARGETS"), ",") {
			label, value, ok := strings.Cut(strings.TrimSpace(item), "=")
			require.Truef(t, ok && label != "" && value != "", "malformed rollback probe target %q", item)
			out = append(out, target{Label: label, Value: value})
		}
		return out
	}
	switch mode {
	case "listen":
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", ":9001")
		require.NoError(t, err)
		defer func() { _ = ln.Close() }()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	case "dial":
		type dialResult struct {
			Label     string `json:"label"`
			Target    string `json:"target"`
			Reachable bool   `json:"reachable"`
			Error     string `json:"error,omitempty"`
			ElapsedMs int64  `json:"elapsed_ms"`
		}
		var results []dialResult
		for _, tg := range parseTargets() {
			start := time.Now()
			conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(t.Context(), "tcp", tg.Value)
			r := dialResult{Label: tg.Label, Target: tg.Value, ElapsedMs: time.Since(start).Milliseconds()}
			if err != nil {
				r.Error = err.Error()
			} else {
				r.Reachable = true
				_ = conn.Close()
			}
			results = append(results, r)
		}
		emit(map[string]any{"mode": "dial", "observed_at": time.Now().UTC(), "results": results})
	case "http":
		fx := clusterFixture{}
		require.True(t, readJSON(t, "cluster-fixture.json", &fx), "seed fixture missing from the rollback probe pod")
		type httpRead struct {
			Path   string `json:"path"`
			Status int    `json:"status,omitempty"`
			Body   string `json:"body,omitempty"`
			Error  string `json:"error,omitempty"`
		}
		ctx := t.Context()
		members := []map[string]any{}
		for _, tg := range parseTargets() {
			c := &client{base: strings.TrimRight(tg.Value, "/"), manualKey: os.Getenv("CAESIUM_MANUAL_TRIGGER_API_KEY"),
				http: &http.Client{Timeout: 10 * time.Second}}
			read := func(path string) (httpRead, []byte) {
				status, raw, err := c.do(ctx, http.MethodGet, path, nil)
				r := httpRead{Path: path, Status: status, Body: truncate(raw, 2048)}
				if err != nil {
					r.Error = err.Error()
				}
				return r, raw
			}
			obs := map[string]any{"member": tg.Label, "base": c.base, "observed_at": time.Now().UTC()}
			health, _ := read("/health")
			ready, _ := read("/health/ready")
			obs["health"], obs["health_ready"] = health, ready
			if health.Status == 0 && ready.Status == 0 {
				obs["fixture_reads"] = "not attempted: the member returned no HTTP answer on /health or /health/ready"
				members = append(members, obs)
				continue
			}
			obs["features"], _ = read("/v1/system/features")
			jobs, raw := read("/v1/jobs")
			jobsObs := map[string]any{"status": jobs.Status, "error": jobs.Error}
			if jobs.Status == http.StatusOK {
				var listed []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(raw, &listed); err != nil {
					jobsObs["decode_error"] = err.Error()
				} else {
					seen := map[string]bool{}
					for _, j := range listed {
						seen[j.ID] = true
					}
					present := map[string]bool{}
					for key, job := range fx.Jobs {
						present[key] = seen[job.ID]
					}
					jobsObs["listed"] = len(listed)
					jobsObs["fixture_jobs_present"] = present
				}
			}
			obs["jobs"] = jobsObs
			runs := map[string]any{}
			for key, run := range map[string]runFixture{"succeeded": fx.Succeeded, "failed": fx.Failed,
				"in_flight": fx.InFlight, "predecessor": fx.Predecessor} {
				row := map[string]any{"run_id": run.ID, "job_id": run.JobID, "seeded_status": run.Status,
					"seeded_tasks": len(run.Tasks), "seeded_events": len(run.Events)}
				got, err := c.run(ctx, run.JobID, run.ID)
				if err != nil {
					row["read_error"] = err.Error()
				} else {
					row["observed_status"] = got.Status
					row["observed_tasks"] = len(got.Tasks)
				}
				events, err := readEventBacklog(ctx, c, run.ID, 0)
				if err != nil {
					row["events_error"] = err.Error()
				} else {
					have := map[string]bool{}
					for _, e := range events {
						have[e.key()] = true
					}
					missing := 0
					for _, e := range run.Events {
						if !have[e.key()] {
							missing++
						}
					}
					row["observed_events"] = len(events)
					row["seeded_events_missing"] = missing
				}
				runs[key] = row
			}
			obs["fixture_runs"] = runs
			members = append(members, obs)
		}
		emit(map[string]any{"mode": "http", "members": members})
	default:
		t.Fatalf("unknown CAESIUM_LIFECYCLE_ROLLBACK_MODE %q", mode)
	}
}
