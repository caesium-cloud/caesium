//go:build integration

package lifecycle

// F2's in-cluster assertion runner. The host controller owns kind, Helm and
// storage faults; this runner observes the live Kubernetes, dqlite, HTTP and
// raw-effect surfaces. A missing observation is blocked, never a pass.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const clusterRecorderURL = "http://lifecycle-recorder:8090"

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
	LifecycleID string                  `json:"lifecycle_id"`
	Members     []clusterMemberEvidence `json:"members"`
	Jobs        map[string]jobFixture   `json:"jobs"`
	Succeeded   runFixture              `json:"succeeded"`
	Failed      runFixture              `json:"failed"`
	InFlight    runFixture              `json:"in_flight"`
	Predecessor runFixture              `json:"predecessor"`
	QueuedRow   queueRowFixture         `json:"queued_row"`
	QueueToken  string                  `json:"queue_token"`
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
	Status          string `json:"status"`
	RecorderNonce   string `json:"recorder_nonce"`
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
	sql := fmt.Sprintf("SELECT id, job_run_id, task_id, claimed_by, owner_generation, attempt, status, output FROM task_runs WHERE job_run_id = '%s' AND task_id = '%s' LIMIT 2", rid, tid)
	response, _, err := h.Query(ctx, base, sql, 2)
	if err != nil {
		return clusterTaskProof{}, err
	}
	return parseClusterTaskProof(response, rid.String(), tid.String())
}

func parseClusterTaskProof(response cluster.QueryResponse, runID, publicTaskID string) (clusterTaskProof, error) {
	if len(response.Rows) != 1 || response.RowCount != 1 {
		return clusterTaskProof{}, fmt.Errorf("run %s public task %s query returned %d rows (reported %d); require exactly one durable attempt", runID, publicTaskID, len(response.Rows), response.RowCount)
	}
	row := response.Rows[0]
	if len(row) != 8 {
		return clusterTaskProof{}, fmt.Errorf("run %s public task %s query returned %d columns, want 8", runID, publicTaskID, len(row))
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
	proof := clusterTaskProof{ID: durableID.String(), RunID: gotRunID.String(), TaskID: gotTaskID.String(),
		ClaimedBy: fmt.Sprint(row[3]), OwnerGeneration: generation, Attempt: attempt, Status: fmt.Sprint(row[6])}
	if row[7] != nil {
		var raw []byte
		switch v := row[7].(type) {
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
		}
	}
	return proof, nil
}

func rawCompletionMatchesTask(runID string, proof clusterTaskProof, events []recorder.Event) bool {
	if proof.RecorderNonce == "" {
		return false
	}
	started, completed := false, false
	for _, e := range events {
		if e.RunID != runID || e.Nonce != proof.RecorderNonce || e.Step != "hold" {
			continue
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

func waitRecorderStart(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		for _, e := range recorderEvents(t) {
			if e.RunID == runID && e.Kind == "start" {
				return
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("raw effect recorder never saw start for run %s", runID)
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
		step = `set -eu; N="$(cat /proc/sys/kernel/random/uuid)"; R="http://lifecycle-recorder:8090"; wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"hold\",\"nonce\":\"$N\",\"event\":\"start\"}" "$R/start"; i=0; while [ "$i" -lt 900 ]; do if wget -qO- "$R/wait?run_id=$CAESIUM_RUN_ID" | grep -q released; then wget -qO- --header='Content-Type: application/json' --post-data="{\"run_id\":\"$CAESIUM_RUN_ID\",\"step\":\"hold\",\"nonce\":\"$N\",\"event\":\"complete\"}" "$R/effect"; echo "##caesium::output {\"recorder_nonce\":\"$N\"}"; exit 0; fi; i=$((i+1)); sleep 1; done; exit 1`
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
	sink := recorder.New()
	require.NoError(t, sink.Start())
	defer sink.Close(context.Background())
	<-t.Context().Done()
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
	waitRecorderStart(t, inflight.ID)
	inflight, err = c.run(ctx, inflight.JobID, inflight.ID)
	require.NoError(t, err)
	require.Equal(t, "running", inflight.Status)
	require.Len(t, inflight.Tasks, 1, "in-flight task attempt identity missing before upgrade")
	predecessor, started, err := c.triggerRun(ctx, jobs["queue"].ID, nil)
	require.NoError(t, err)
	require.True(t, started)
	waitRecorderStart(t, predecessor.ID)
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
		Predecessor: predecessor.fixture(), QueuedRow: queued[0], QueueToken: queueToken}
	for _, run := range []*runFixture{&fx.Succeeded, &fx.Failed} {
		run.Events, err = readEventBacklog(ctx, c, run.ID, 0)
		require.NoError(t, err)
		require.NotEmpty(t, run.Events)
		low := lowestSequence(run.Events)
		require.Positive(t, low)
		run.ResumeCursor = low - 1
	}
	writeJSON(t, "cluster-fixture.json", fx)
	writeJSON(t, "cluster-raw-before.json", recorderEvents(t))
	writeCase(t, caseRecord{Name: "three-persistent-voters-before", Status: statusPass,
		Detail:       fmt.Sprintf("three bound PVCs, distinct worker nodes, three direct dqlite voters, leader %s", membership.Leader.Address),
		Observations: map[string]any{"members": fx.Members, "leader": membership.Leader}})
}

func TestLifecycleClusterMixedWindow(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "mixed-version-dispatch-and-completion", "seed fixture missing")
	}
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 7*time.Minute)
	defer cancel()
	oldID := mustEnv(t, "CAESIUM_LIFECYCLE_PREVIOUS_IMAGE_ID")
	newID := mustEnv(t, "CAESIUM_LIFECYCLE_CANDIDATE_IMAGE_ID")
	var observed []map[string]any
	lastProbeErr := ""
	for ctx.Err() == nil {
		topo, err := cluster.DiscoverTopology(ctx, kube, fx.LifecycleID)
		if err == nil && len(topo.Members) == 3 {
			var oldMembers, newMembers []cluster.Member
			for _, m := range topo.Members {
				if !cluster.PodReady(&m.Pod) {
					continue
				}
				switch {
				case cluster.ImageIDMatchesCandidate(m.ImageID, oldID):
					oldMembers = append(oldMembers, m)
				case cluster.ImageIDMatchesCandidate(m.ImageID, newID):
					newMembers = append(newMembers, m)
				}
			}
			if len(oldMembers) > 0 && len(newMembers) > 0 {
				base := newMembers[0].HTTPBase()
				h := cluster.NewHTTP(mustEnv(t, "CAESIUM_MANUAL_TRIGGER_API_KEY"))
				ic, err := cluster.MintInternalClient(ctx, h, base, mustEnv(t, "CAESIUM_LIFECYCLE_INTERNAL_TOKEN"), mustEnv(t, "CAESIUM_LIFECYCLE_INTERNAL_TOKEN"))
				if err != nil {
					lastProbeErr = fmt.Sprintf("cannot authenticate protocol probe: %v", err)
					time.Sleep(300 * time.Millisecond)
					continue
				}
				caps := map[string]int{}
				probeOK := true
				for _, m := range append(oldMembers, newMembers...) {
					req, err := http.NewRequestWithContext(ctx, http.MethodGet, cluster.InternalBase(m.IP)+"/internal/capabilities", nil)
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+ic.Token)
					resp, err := ic.HTTP.Do(req)
					if err != nil {
						lastProbeErr = fmt.Sprintf("%s protocol probe: %v", m.Name, err)
						probeOK = false
						break
					}
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					if resp.StatusCode != http.StatusOK {
						lastProbeErr = fmt.Sprintf("%s capabilities status %d: %s", m.Name, resp.StatusCode, body)
						probeOK = false
						break
					}
					var cap struct {
						NodeID          string `json:"node_id"`
						ProtocolVersion int    `json:"protocol_version"`
					}
					if err := json.Unmarshal(body, &cap); err != nil {
						t.Fatal(err)
					}
					require.Equal(t, 2, cap.ProtocolVersion)
					caps[m.Name] = cap.ProtocolVersion
				}
				if !probeOK {
					time.Sleep(300 * time.Millisecond)
					continue
				}
				observed = append(observed, map[string]any{"old": oldMembers, "new": newMembers, "protocol": caps, "at": time.Now().UTC()})
				writeJSON(t, "cluster-mixed-window.json", observed)
				versionByIP := map[string]string{}
				memberByIP := map[string]cluster.Member{}
				for _, m := range oldMembers {
					versionByIP[m.IP] = "previous"
					memberByIP[m.IP] = m
				}
				for _, m := range newMembers {
					versionByIP[m.IP] = "candidate"
					memberByIP[m.IP] = m
				}
				c := newClient(t)
				c.base = base
				for attempt := 0; attempt < 12 && ctx.Err() == nil; attempt++ {
					// Hold one task so the specific dispatch attempt can be read
					// before its raw effect and fenced owner completion.
					run, started, err := c.triggerRun(ctx, fx.Jobs["inflight"].ID, nil)
					if err != nil || !started {
						continue
					}
					startSeen := false
					for until := time.Now().Add(10 * time.Second); time.Now().Before(until) && !startSeen; {
						for _, event := range recorderEvents(t) {
							startSeen = startSeen || event.RunID == run.ID && event.Kind == "start"
						}
						if !startSeen {
							time.Sleep(300 * time.Millisecond)
						}
					}
					if !startSeen {
						releaseRecordedRun(t, run.ID)
						continue
					}
					leaseCtx, cancelLease := context.WithTimeout(ctx, 10*time.Second)
					lease, err := cluster.WaitLease(leaseCtx, h, base, run.ID, "")
					cancelLease()
					if err != nil {
						releaseRecordedRun(t, run.ID)
						continue
					}
					live, err := h.GetRun(ctx, base, run.JobID, run.ID)
					if err != nil || len(live.Tasks) != 1 {
						releaseRecordedRun(t, run.ID)
						continue
					}
					beforeTask, err := readClusterTaskProof(ctx, h, base, run.ID, live.Tasks[0].ID)
					if err != nil || beforeTask.ClaimedBy == "" || beforeTask.OwnerGeneration != lease.Generation ||
						beforeTask.Attempt != live.Tasks[0].Attempt || beforeTask.Status != "running" {
						releaseRecordedRun(t, run.ID)
						continue
					}
					ownerIP := cluster.HostIP(lease.OwnerNode)
					workerIP := cluster.HostIP(beforeTask.ClaimedBy)
					ownerVersion, workerVersion := versionByIP[ownerIP], versionByIP[workerIP]
					releaseRecordedRun(t, run.ID)
					final, err := c.awaitRunStatus(ctx, run.JobID, run.ID,
						func(r apiRun) bool { return isTerminal(r.Status) }, 40*time.Second)
					if err != nil || final.Status != "succeeded" || len(final.Tasks) != 1 ||
						final.Tasks[0].ID != live.Tasks[0].ID || final.Tasks[0].Attempt != beforeTask.Attempt ||
						ownerVersion == "" || workerVersion == "" || ownerVersion == workerVersion {
						continue
					}
					afterTask, err := readClusterTaskProof(ctx, h, base, run.ID, beforeTask.TaskID)
					if err != nil || afterTask.ID != beforeTask.ID || afterTask.Attempt != beforeTask.Attempt ||
						afterTask.ClaimedBy != beforeTask.ClaimedBy || afterTask.OwnerGeneration != lease.Generation ||
						afterTask.Status != "succeeded" || !rawCompletionMatchesTask(run.ID, afterTask, recorderEvents(t)) {
						continue
					}
					postTopo, err := cluster.DiscoverTopology(ctx, kube, fx.LifecycleID)
					if err != nil {
						continue
					}
					stable := true
					for _, original := range []cluster.Member{memberByIP[ownerIP], memberByIP[workerIP]} {
						post, ok := postTopo.ByName(original.Name)
						stable = stable && ok && post.UID == original.UID && post.ImageID == original.ImageID
					}
					if !stable {
						continue
					}
					cross := map[string]any{"run_id": run.ID, "owner_node": lease.OwnerNode,
						"owner_version": ownerVersion, "owner_generation": lease.Generation,
						"worker_node": beforeTask.ClaimedBy, "worker_version": workerVersion,
						"task_before_release": beforeTask, "task_after_completion": afterTask,
						"owner_member": memberByIP[ownerIP], "worker_member": memberByIP[workerIP],
						"terminal_status": final.Status, "raw_effect_nonce": afterTask.RecorderNonce}
					writeJSON(t, "cluster-mixed-crossing.json", cross)
					writeCase(t, caseRecord{Name: "mixed-version-dispatch-and-completion", Status: statusPass,
						Detail:       "same task attempt and fenced owner generation dispatched and completed across protocol-2 image IDs; raw nonce persisted in terminal task output",
						Observations: cross})
					return
				}
				writeCase(t, caseRecord{Name: "mixed-version-dispatch-and-completion", Status: statusBlocked,
					Detail: "both protocol-2 images were observed, but no opposite-version lease owner, claimed worker and raw completion were jointly observed"})
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	blockf(t, "mixed-version-dispatch-and-completion", "no live mixed-version window with both protocol-2 members was observed; last probe: %s", lastProbeErr)
}

func TestLifecycleClusterAfterUpgrade(t *testing.T) {
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
	for _, want := range []runFixture{fx.Succeeded, fx.Failed} {
		assertRunUnchanged(t, ctx, c, want, want.Status)
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
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		current, err := c.run(ctx, r.JobID, r.ID)
		if err != nil {
			blockf(t, "retained-history-and-raw-effects", "in-flight run %s unobservable before controlled release: %v", r.ID, err)
		}
		preRelease[r.ID] = current
		writeJSON(t, "cluster-inflight-before-release.json", preRelease)
		if current.Status != "running" || len(r.Tasks) != 1 || len(current.Tasks) != 1 ||
			current.Tasks[0].ID != r.Tasks[0].ID || current.Tasks[0].Attempt != r.Tasks[0].Attempt ||
			current.Tasks[0].Status != "running" {
			blockf(t, "retained-history-and-raw-effects", "in-flight task attempt %s did not survive upgrade to controlled release; status=%s", r.ID, current.Status)
		}
	}
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		releaseRecordedRun(t, r.ID)
	}
	attemptProofs := map[string][]clusterTaskProof{}
	for _, r := range []runFixture{fx.InFlight, fx.Predecessor} {
		got, err := c.awaitRunStatus(ctx, r.JobID, r.ID, func(x apiRun) bool { return isTerminal(x.Status) }, 5*time.Minute)
		require.NoError(t, err)
		require.NotEmpty(t, got.Tasks, "in-flight run %s lost its task attempt rows", r.ID)
		// Duplicate attempts remain in the raw ledger; only a missing or
		// unpaired visible effect is rejected.
		events := recorderEvents(t)
		starts := map[string]bool{}
		effects := map[string]bool{}
		for _, e := range events {
			if e.RunID == r.ID {
				if e.Kind == "start" {
					starts[e.Nonce] = true
				}
				if e.Kind == "complete" {
					effects[e.Nonce] = true
				}
			}
		}
		require.NotEmpty(t, starts, "no raw start effect for %s", r.ID)
		for nonce := range effects {
			require.Truef(t, starts[nonce], "raw completion nonce %s has no start", nonce)
		}
		matchedAttempt := false
		for _, task := range got.Tasks {
			proof, err := readClusterTaskProof(ctx, h, base, got.ID, task.ID)
			require.NoError(t, err)
			attemptProofs[r.ID] = append(attemptProofs[r.ID], proof)
			if proof.TaskID == r.Tasks[0].ID && proof.Attempt == r.Tasks[0].Attempt &&
				proof.Status == "succeeded" && rawCompletionMatchesTask(r.ID, proof, events) {
				matchedAttempt = true
			}
		}
		writeJSON(t, "cluster-inflight-outcome-"+r.ID+".json", map[string]any{
			"run": got, "seed_attempt": r.Tasks[0], "task_proofs": attemptProofs[r.ID],
			"raw_events": events, "matched_seed_attempt": matchedAttempt})
		if got.Status != "succeeded" || !matchedAttempt {
			blockf(t, "retained-history-and-raw-effects", "controlled release did not complete the surviving task attempt for run %s: status=%s matched_seed_attempt=%t", r.ID, got.Status, matchedAttempt)
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
	queued, err := c.awaitRunStatus(ctx, queued.JobID, queued.ID, func(r apiRun) bool { return isTerminal(r.Status) }, 5*time.Minute)
	require.NoError(t, err)
	require.Equal(t, "succeeded", queued.Status)
	require.NotEmpty(t, queued.Tasks, "queued run has no task attempt")
	assertRawCompletion(t, queued.ID, recorderEvents(t))
	queuedMatched := false
	for _, task := range queued.Tasks {
		proof, err := readClusterTaskProof(ctx, h, base, queued.ID, task.ID)
		require.NoError(t, err)
		attemptProofs[queued.ID] = append(attemptProofs[queued.ID], proof)
		queuedMatched = queuedMatched || proof.Status == "succeeded" && rawCompletionMatchesTask(queued.ID, proof, recorderEvents(t))
	}
	require.True(t, queuedMatched, "queued run has no raw effect tied to its terminal task attempt")
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
	var crossing map[string]any
	if !readJSON(t, "cluster-mixed-crossing.json", &crossing) {
		writeCase(t, caseRecord{Name: "mixed-version-dispatch-and-completion", Status: statusBlocked,
			Detail: "no opposite-version lease owner, claimed worker, terminal run and raw completion effect were jointly observed"})
	} else {
		writeCase(t, caseRecord{Name: "mixed-version-dispatch-and-completion", Status: statusPass,
			Detail:       "protocol-2 mixed window and opposite-version dispatch/completion were observed",
			Observations: crossing})
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
	writeCase(t, caseRecord{Name: "joining-ordinal-1-replacement", Status: statusPass,
		Detail: "fresh PVC and node ID joined every member's direct Cluster RPC; old member entry remains",
		Observations: map[string]any{"old_node_id": old["caesium-1"].DqliteID, "old_volume": old["caesium-1"].Volume,
			"new_volume": replaced.VolumeName, "new_pod_uid": replaced.UID}})
}

func TestLifecycleClusterOrdinalZeroLoss(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "ordinal-0-disk-loss", "seed fixture missing")
	}
	kube, err := cluster.InClusterClient()
	require.NoError(t, err)
	old := map[string]clusterMemberEvidence{}
	for _, m := range fx.Members {
		old[m.Name] = m
	}
	var evidence = map[string]any{"old_id": old["caesium-0"].DqliteID}
	var leaderAddress, survivorView string
	for _, name := range []string{"caesium-1", "caesium-2"} {
		pod, err := kube.CoreV1().Pods(fx.LifecycleID).Get(t.Context(), name, metav1.GetOptions{})
		require.NoError(t, err)
		addr := pod.Status.PodIP + ":9001"
		leader, members, err := cluster.QueryNode(t.Context(), addr)
		if err != nil {
			blockf(t, "ordinal-0-disk-loss", "surviving member %s did not answer direct RPC: %v", name, err)
		}
		if leaderAddress == "" {
			leaderAddress = leader.Address
		}
		require.Equal(t, leaderAddress, leader.Address, "survivors disagree on their leader")
		view := make([]string, 0, len(members))
		for _, member := range members {
			view = append(view, fmt.Sprintf("%d/%s/%s", member.ID, member.Address, member.Role.String()))
		}
		sort.Strings(view)
		joined := strings.Join(view, ",")
		if survivorView == "" {
			survivorView = joined
		}
		require.Equal(t, survivorView, joined, "survivors disagree on membership")
		evidence[name+"_leader"] = leader
		evidence[name+"_membership"] = members
	}
	var host map[string]any
	if !readJSON(t, "cluster-ordinal0-host.json", &host) {
		blockf(t, "ordinal-0-disk-loss", "fresh PVC, info.yaml and node-store evidence absent")
	}
	for k, v := range host {
		evidence[k] = v
	}
	require.NotEqual(t, host["old_uid"], host["new_uid"], "ordinal-0 pod was not recreated")
	var pvc struct {
		Spec struct {
			VolumeName string `json:"volumeName"`
		} `json:"spec"`
	}
	pvcRaw, ok := host["pvc"].(string)
	require.True(t, ok)
	if err := json.Unmarshal([]byte(pvcRaw), &pvc); err != nil {
		blockf(t, "ordinal-0-disk-loss", "fresh PVC JSON unobservable: %v", err)
	}
	require.NotEmpty(t, pvc.Spec.VolumeName)
	require.NotEqual(t, old["caesium-0"].Volume, pvc.Spec.VolumeName, "ordinal-0 PVC retained the old PV")
	infoRaw, ok := host["info_yaml"].(string)
	require.True(t, ok)
	var info struct {
		ID      uint64 `yaml:"ID"`
		Address string `yaml:"Address"`
	}
	if err := yaml.Unmarshal([]byte(infoRaw), &info); err != nil {
		blockf(t, "ordinal-0-disk-loss", "fresh info.yaml unobservable: %v", err)
	}
	require.NotZero(t, info.ID)
	require.NotEmpty(t, info.Address)
	evidence["fresh_info_id"] = info.ID
	evidence["fresh_info_address"] = info.Address
	evidence["fresh_volume"] = pvc.Spec.VolumeName
	require.NotEmpty(t, strings.TrimSpace(fmt.Sprint(host["node_store"])), "fresh node store not recorded")
	writeCase(t, caseRecord{Name: "ordinal-0-disk-loss", Status: statusBlocked,
		Detail:       "fresh ordinal-0 evidence and surviving membership recorded; no product rejoin/re-bootstrap procedure is qualified",
		Observations: evidence})
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
	writeJSON(t, "cluster-snapshot-leader.json", map[string]any{
		"name": leaderMember.Name, "uid": leaderMember.UID,
		"ip": leaderMember.IP, "address": leaderMember.DqliteAddr(),
		"image_id": leaderMember.ImageID, "observed_at": time.Now().UTC(),
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
	for i := 0; i < 1400; i++ {
		def := clusterManifest(t, "history", fmt.Sprintf("lifecycle-snapshot-%s-%04d", suffixOf(fx.LifecycleID), i), image)
		if err := h.Apply(t.Context(), base, []jobdef.Definition{def}); err != nil {
			t.Fatalf("catalog write %d/1400: %v", i, err)
		}
	}
	writeJSON(t, "cluster-snapshot-write-count.json", map[string]any{"acknowledged_applies": 1400})
}

func TestLifecycleClusterPostStorage(t *testing.T) {
	fx := clusterFixture{}
	if !readJSON(t, "cluster-fixture.json", &fx) {
		blockf(t, "storage-rejoin", "seed fixture missing")
	}
	_, _, topo := clusterKube(t)
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
	writeJSON(t, "cluster-post-storage.json", map[string]any{"members": memberEvidence(topo, membership),
		"leader": membership.Leader, "retained_run_ids": []string{fx.Succeeded.ID, fx.Failed.ID}})
}
