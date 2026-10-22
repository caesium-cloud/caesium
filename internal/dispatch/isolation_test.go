package dispatch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/auth/policy"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/metrics"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// These tests install the process-wide policy holder; none is parallel.

const isolationTestPolicy = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*"]}
  isolated:
    kubernetes: {namespace: caesium-isolated}
  finance: {}
`

func installIsolationPolicy(t *testing.T, body string) *policy.Holder {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	policy.SetProcessHolder(holder)
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	return holder
}

// isolationSubmitter is a worker submit seam that advertises isolation_v1
// (capable) or nothing (a worker that cannot honour a context).
type isolationSubmitter struct {
	mu       sync.Mutex
	capable  bool
	accepted []InboundDispatch
}

func (s *isolationSubmitter) SubmitDispatched(d InboundDispatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.accepted = append(s.accepted, d)
	return nil
}

func (s *isolationSubmitter) Capabilities() []string {
	if !s.capable {
		return nil
	}
	return []string{CapabilityIsolationV1}
}

func (s *isolationSubmitter) received() []InboundDispatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]InboundDispatch(nil), s.accepted...)
}

// seedNamespacedTaskRun is seedPendingTaskRun for a run in namespace.
func seedNamespacedTaskRun(t *testing.T, store *run.Store, namespace string, quarantine bool) (runID, taskID, taskRunID uuid.UUID) {
	t.Helper()
	db := store.DB()
	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "iso-trigger-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "iso-job-" + uuid.NewString()[:8], Namespace: namespace, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	atom := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "busybox:1.36.1", Command: `["true"]`, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atom).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atom.ID, Name: "step", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)
	jobRun := &models.JobRun{ID: uuid.New(), JobID: job.ID, Namespace: namespace, Quarantine: quarantine, TriggerID: trigger.ID, TriggerType: string(trigger.Type), Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(jobRun).Error)
	tr := &models.TaskRun{
		ID: uuid.New(), JobRunID: jobRun.ID, TaskID: task.ID, AtomID: atom.ID, Engine: models.AtomEngineDocker,
		Image: "busybox:1.36.1", Command: `["true"]`, Status: string(run.TaskStatusPending), Quarantine: quarantine,
		ReplaySafe: quarantine, Attempt: 1, MaxAttempts: 1, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(tr).Error)
	return jobRun.ID, task.ID, tr.ID
}

func marketingContext() *jobdefruntime.IsolationContext {
	return &jobdefruntime.IsolationContext{Namespace: "marketing", Secrets: &policy.SecretRules{Allow: []string{"env/MARKETING_*"}}}
}

// TestHandleDispatch_IsolationEnvelope pins the envelope contract: a
// non-default context arrives only tagged and is only taken by a node that
// advertises isolation_v1; untagged envelopes (no context, or a default one)
// behave as before; the context reaches the worker unchanged.
func TestHandleDispatch_IsolationEnvelope(t *testing.T) {
	cases := []struct {
		name      string
		capable   bool
		tagged    bool
		isolation *jobdefruntime.IsolationContext
		accepted  bool
	}{
		{"capable worker takes a tagged isolated task", true, true, marketingContext(), true},
		{"untagged isolated task is refused", true, false, marketingContext(), false},
		{"tagged envelope without a context is refused", true, true, nil, false},
		{"tagged envelope with a default context is refused", true, true, &jobdefruntime.IsolationContext{Namespace: "finance"}, false},
		{"default context dispatches untagged", true, false, &jobdefruntime.IsolationContext{Namespace: "finance"}, true},
		{"no context dispatches as before", true, false, nil, true},
		{"incapable worker refuses a tagged isolated task", false, true, marketingContext(), false},
		{"incapable worker refuses an untagged isolated task", false, false, marketingContext(), false},
		{"incapable worker takes a default context", false, false, &jobdefruntime.IsolationContext{Namespace: "finance"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, _, h := setupHandler(t)
			sub := &isolationSubmitter{capable: tc.capable}
			h = h.WithWorkerSubmitter(sub)
			runID, taskID := seedPendingTaskRun(t, store)
			workerNode := ownerNodeAddr
			if tc.tagged {
				workerNode = IsolationWorkerNode(ownerNodeAddr)
			}
			w := postJSON(t, h.HandleDispatch, DispatchRequest{
				RunID: runID, TaskID: taskID, OwnerGeneration: 1, Attempt: 1, WorkerNode: workerNode,
				OwnerBaseURL: "http://owner", Deadline: time.Now().Add(time.Minute), Isolation: tc.isolation,
			})
			status, claimedBy := taskStatus(t, store, runID, taskID)
			if !tc.accepted {
				require.Equal(t, http.StatusConflict, w.Code, w.Body.String())
				require.Contains(t, w.Body.String(), ReasonWrongWorker)
				require.Empty(t, sub.received())
				require.Equal(t, string(run.TaskStatusPending), status, "a refused envelope claims nothing")
				require.Empty(t, claimedBy)
				return
			}
			require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
			got := sub.received()
			require.Len(t, got, 1)
			require.Equal(t, ownerNodeAddr, got[0].WorkerNode, "completion fencing keeps the plain node id")
			require.Equal(t, tc.isolation, got[0].Isolation)
		})
	}
}

// TestLegacyWorkerRefusesTaggedEnvelope: a worker built before the context
// decodes the envelope without the field and compares worker_node with its
// node id before claiming — so it refuses an isolated task, and still accepts
// an untagged one (old workers keep working for every other task).
func TestLegacyWorkerRefusesTaggedEnvelope(t *testing.T) {
	type legacyDispatchRequest struct {
		RunID      uuid.UUID `json:"run_id"`
		WorkerNode string    `json:"worker_node"`
	}
	const node = "10.0.0.7:9001"
	for _, tc := range []struct {
		req    DispatchRequest
		refuse bool
	}{
		{DispatchRequest{RunID: uuid.New(), WorkerNode: IsolationWorkerNode(node), Isolation: marketingContext()}, true},
		{DispatchRequest{RunID: uuid.New(), WorkerNode: node, Isolation: &jobdefruntime.IsolationContext{Namespace: "finance"}}, false},
		{DispatchRequest{RunID: uuid.New(), WorkerNode: node}, false},
	} {
		body, err := json.Marshal(tc.req)
		require.NoError(t, err)
		var legacy legacyDispatchRequest
		require.NoError(t, json.Unmarshal(body, &legacy), "an old worker tolerates the new field")
		require.Equal(t, tc.refuse, legacy.WorkerNode != node, tc.req.WorkerNode)
	}
}

// TestHandleDispatch_CarriesALargeContextWithoutTruncation: a context bigger
// than the historical 64 KiB read limit (a large but valid policy) arrives
// whole; an envelope over the bound is refused with 413, never truncated, and
// the sender refuses to build one.
func TestHandleDispatch_CarriesALargeContextWithoutTruncation(t *testing.T) {
	store, _, h := setupHandler(t)
	sub := &isolationSubmitter{capable: true}
	h = h.WithWorkerSubmitter(sub)
	runID, taskID := seedPendingTaskRun(t, store)
	allow := make([]string, 0, 4000)
	for i := range 4000 {
		allow = append(allow, fmt.Sprintf("env/MARKETING_RULE_%06d_*", i))
	}
	isolation := &jobdefruntime.IsolationContext{Namespace: "marketing", Secrets: &policy.SecretRules{Allow: allow}}
	req := DispatchRequest{RunID: runID, TaskID: taskID, OwnerGeneration: 1, Attempt: 1, WorkerNode: IsolationWorkerNode(ownerNodeAddr), Deadline: time.Now().Add(time.Minute), Isolation: isolation}
	body, err := json.Marshal(req)
	require.NoError(t, err)
	require.Greater(t, len(body), 1<<16, "the envelope exceeds the old read limit")
	w := postJSON(t, h.HandleDispatch, req)
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	got := sub.received()
	require.Len(t, got, 1)
	require.Equal(t, allow, got[0].Isolation.Secrets.Allow)

	oversized := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", bytes.NewReader(make([]byte, MaxDispatchBodyBytes+1)))
	oversized.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	h.HandleDispatch(rec, oversized)
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	require.Contains(t, rec.Body.String(), ReasonTooLarge)

	huge := strings.Repeat("x", MaxDispatchBodyBytes)
	_, _, err = PostDispatch(context.Background(), "http://127.0.0.1:1/internal/dispatch", testToken, DispatchRequest{Isolation: &jobdefruntime.IsolationContext{Namespace: huge}})
	require.ErrorIs(t, err, ErrDispatchTooLarge)
}

// isolationCluster is an owner plus peers, each a real dispatch Handler on an
// httptest server over one catalog DB, with the loop's peer URLs routed to them.
type isolationCluster struct {
	store   *run.Store
	ls      *run.LeaseStore
	owner   string
	urls    map[string]string
	subs    map[string]*isolationSubmitter
	posts   map[string]*atomic.Int32
	probes  map[string]*atomic.Int32
	servers []*httptest.Server
}

// newIsolationCluster starts one handler per node; capable[i] decides whether
// node i's worker advertises isolation_v1. Node 0 is the owner.
func newIsolationCluster(t *testing.T, capable ...bool) *isolationCluster {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)
	c := &isolationCluster{store: store, ls: ls, urls: map[string]string{}, subs: map[string]*isolationSubmitter{}, posts: map[string]*atomic.Int32{}, probes: map[string]*atomic.Int32{}}
	for i, able := range capable {
		node := fmt.Sprintf("10.9.0.%d:9001", i+1)
		sub := &isolationSubmitter{capable: able}
		h := NewHandler(store, ls, node, loopToken).WithWorkerSubmitter(sub)
		posts, probes := &atomic.Int32{}, &atomic.Int32{}
		mux := http.NewServeMux()
		mux.HandleFunc("/internal/dispatch", func(w http.ResponseWriter, r *http.Request) { posts.Add(1); h.HandleDispatch(w, r) })
		mux.HandleFunc("/internal/capabilities", func(w http.ResponseWriter, r *http.Request) { probes.Add(1); h.HandleCapabilities(w, r) })
		mux.HandleFunc("/internal/isolation", h.HandleIsolation)
		server := httptest.NewServer(mux)
		t.Cleanup(server.Close)
		c.urls[node], c.subs[node], c.posts[node], c.probes[node] = server.URL, sub, posts, probes
		c.servers = append(c.servers, server)
		if i == 0 {
			c.owner = node
		}
	}
	return c
}

func (c *isolationCluster) peers() []string {
	out := make([]string, 0, len(c.urls))
	for node := range c.urls {
		if node != c.owner {
			out = append(out, node)
		}
	}
	// A stable order (capable peers are created last): a map-ordered list
	// sometimes put the capable peer first, so nothing was skipped.
	slices.Sort(out)
	return out
}

// run drives the owner's dispatch loop for d.
func (c *isolationCluster) run(t *testing.T, d time.Duration) {
	t.Helper()
	loop := NewDispatchLoop(DispatchLoopConfig{
		NodeID: c.owner, Token: loopToken, Interval: 40 * time.Millisecond, BatchSize: 8, Deadline: time.Minute,
		LeaseStore: &testOwnerReader{c.ls}, Store: c.store, Peers: &testPeerLister{peers: c.peers()},
		PeerBaseURL: func(node string) string { return c.urls[node] },
	})
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	loop.Run(ctx)
}

func (c *isolationCluster) seed(t *testing.T, namespace string, quarantine bool) (uuid.UUID, uuid.UUID) {
	t.Helper()
	runID, _, taskRunID := seedNamespacedTaskRun(t, c.store, namespace, quarantine)
	_, err := c.ls.AcquireLease(context.Background(), runID, c.owner, time.Hour)
	require.NoError(t, err)
	return runID, taskRunID
}

// TestDispatchLoop_IsolatedTaskSkipsIncapablePeers: with the owner and one peer
// unable to honour a context and one capable peer, a task under secret rules
// lands on the capable peer — tagged, with the context — every incapable peer
// is skipped (never posted to) and counted, and the row is marked so only a
// capable worker could ever pull it.
func TestDispatchLoop_IsolatedTaskSkipsIncapablePeers(t *testing.T) {
	metrics.Register()
	holder := installIsolationPolicy(t, isolationTestPolicy)
	c := newIsolationCluster(t, false, false, true)
	runID, taskRunID := c.seed(t, "marketing", false)
	before := counterValue(t, metrics.DispatchIncompatibleWorkerTotal)

	c.run(t, 600*time.Millisecond)

	var capableNode string
	for node, sub := range c.subs {
		if sub.capable {
			capableNode = node
			continue
		}
		require.Zero(t, c.posts[node].Load(), "incapable node %s is never posted an isolated task", node)
		require.Empty(t, sub.received())
	}
	got := c.subs[capableNode].received()
	require.Len(t, got, 1)
	require.Equal(t, taskRunID, got[0].Task.ID)
	require.NotNil(t, got[0].Isolation)
	require.Equal(t, "marketing", got[0].Isolation.Namespace)
	require.Equal(t, []string{"env/MARKETING_*"}, got[0].Isolation.Secrets.Allow)
	require.Equal(t, holder.Current().Hash, got[0].Isolation.PolicyRevision)
	require.Greater(t, counterValue(t, metrics.DispatchIncompatibleWorkerTotal), before, "skipped peers are counted")

	var row models.TaskRun
	require.NoError(t, c.store.DB().First(&row, "id = ?", taskRunID).Error)
	require.Equal(t, capableNode, row.ClaimedBy)
	_, want := run.RevisionIsolation(holder.Current(), "marketing")
	require.Equal(t, want, row.NodeSelector[run.IsolationSelectorKey], "the row is marked isolation-required under the owner's settings")
	_ = runID
}

// TestDispatchLoop_IsolatedTaskWaitsWithoutACapablePeer: with no capable
// peer the task is never posted and stays pending; ordinary runs count the
// skip, quarantined replays never touch the production counter.
func TestDispatchLoop_IsolatedTaskWaitsWithoutACapablePeer(t *testing.T) {
	metrics.Register()
	installIsolationPolicy(t, isolationTestPolicy)
	for _, quarantine := range []bool{false, true} {
		c := newIsolationCluster(t, false, false)
		_, taskRunID := c.seed(t, "isolated", quarantine)
		before := counterValue(t, metrics.DispatchIncompatibleWorkerTotal)
		c.run(t, 400*time.Millisecond)
		for node := range c.urls {
			require.Zero(t, c.posts[node].Load(), "quarantine=%v: nothing is posted to %s", quarantine, node)
		}
		var row models.TaskRun
		require.NoError(t, c.store.DB().First(&row, "id = ?", taskRunID).Error)
		require.Equal(t, string(run.TaskStatusPending), row.Status, "the task waits")
		after := counterValue(t, metrics.DispatchIncompatibleWorkerTotal)
		if quarantine {
			require.Equal(t, before, after, "a quarantined replay never counts")
		} else {
			require.Greater(t, after, before)
		}
	}
}

// TestDispatchLoop_PolicylessOwnerHoldsAnIsolatedRun: a node serving no
// policy file that owns a run whose rows a policy-serving owner isolated (an
// owner-memory takeover) never dispatches it — it could neither resolve the
// context nor make a worker honour it — while an unisolated run dispatches
// exactly as before.
func TestDispatchLoop_PolicylessOwnerHoldsAnIsolatedRun(t *testing.T) {
	metrics.Register()
	policy.SetProcessHolder(nil)
	c := newIsolationCluster(t, true, true)
	_, isolatedRow := c.seed(t, "marketing", false)
	require.NoError(t, c.store.DB().Model(&models.TaskRun{}).Where("id = ?", isolatedRow).
		Update("node_selector", datatypes.JSONMap{run.IsolationSelectorKey: "v1-anything"}).Error)
	c.run(t, 400*time.Millisecond)
	for node := range c.urls {
		require.Zero(t, c.posts[node].Load(), "an isolated run is never dispatched by a policyless owner (%s)", node)
	}
	var row models.TaskRun
	require.NoError(t, c.store.DB().First(&row, "id = ?", isolatedRow).Error)
	require.Equal(t, string(run.TaskStatusPending), row.Status)

	_, plainRow := c.seed(t, "marketing", false)
	c.run(t, 400*time.Millisecond)
	var total int
	for _, sub := range c.subs {
		for _, d := range sub.received() {
			require.Equal(t, plainRow, d.Task.ID, "only the unisolated run dispatches")
			require.Nil(t, d.Isolation)
			total++
		}
	}
	require.Equal(t, 1, total)
}

// TestDispatchLoop_DefaultContextNeedsNoProbe: a namespace without settings
// is dispatched round-robin exactly as before, carrying its (default) context
// untagged, and no peer is ever probed.
func TestDispatchLoop_DefaultContextNeedsNoProbe(t *testing.T) {
	metrics.Register()
	installIsolationPolicy(t, isolationTestPolicy)
	c := newIsolationCluster(t, false, false)
	_, taskRunID := c.seed(t, "finance", false)
	c.run(t, 400*time.Millisecond)
	var total int
	for node, sub := range c.subs {
		require.Zero(t, c.probes[node].Load(), "no probe for a default context")
		for _, d := range sub.received() {
			total++
			require.Equal(t, taskRunID, d.Task.ID)
			require.NotNil(t, d.Isolation, "a policy-file owner always sends its context")
			require.True(t, d.Isolation.Default())
		}
	}
	require.Equal(t, 1, total)
}

// TestDispatchLoop_NoPolicyEnvelopeIsUnchanged: an owner serving no policy
// file sends no isolation field and an untagged worker_node, and never probes.
func TestDispatchLoop_NoPolicyEnvelopeIsUnchanged(t *testing.T) {
	metrics.Register()
	policy.SetProcessHolder(nil)
	var raw atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/capabilities" {
			t.Errorf("no probe without a policy file")
		}
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(r.Body)
		raw.Store(buf.String())
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(server.Close)
	nodeID, apiPort := serverNodeID(t, server)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)
	runID, _, _ := seedNamespacedTaskRun(t, store, "marketing", false)
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, time.Hour)
	require.NoError(t, err)
	loop := NewDispatchLoop(DispatchLoopConfig{NodeID: nodeID, APIPort: apiPort, Token: loopToken, Interval: 40 * time.Millisecond, LeaseStore: &testOwnerReader{ls}, Store: store, Peers: &testPeerLister{}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	loop.Run(ctx)
	body, _ := raw.Load().(string)
	require.NotEmpty(t, body)
	require.NotContains(t, body, `"isolation"`)
	var req DispatchRequest
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	require.Equal(t, nodeID, req.WorkerNode)
}

// TestDispatchLoop_WrongWorkerEvictsTheCapabilityCache: a peer that advertised
// isolation_v1 but refuses the tagged envelope (it was downgraded at the same
// address) is re-probed rather than trusted until the positive TTL lapses.
func TestDispatchLoop_WrongWorkerEvictsTheCapabilityCache(t *testing.T) {
	metrics.Register()
	installIsolationPolicy(t, isolationTestPolicy)
	var probes, posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/capabilities" {
			probes.Add(1)
			writeJSON(w, http.StatusOK, CapabilitiesResponse{Capabilities: []string{CapabilityIsolationV1}})
			return
		}
		posts.Add(1)
		writeJSON(w, http.StatusConflict, ErrorResponse{Code: ReasonWrongWorker})
	}))
	t.Cleanup(server.Close)
	nodeID, apiPort := serverNodeID(t, server)
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	ls := run.NewLeaseStore(db)
	store := run.NewStore(db).WithLeaseStore(ls)
	runID, _, _ := seedNamespacedTaskRun(t, store, "marketing", false)
	_, err := ls.AcquireLease(context.Background(), runID, nodeID, time.Hour)
	require.NoError(t, err)
	loop := NewDispatchLoop(DispatchLoopConfig{NodeID: nodeID, APIPort: apiPort, Token: loopToken, Interval: 40 * time.Millisecond, LeaseStore: &testOwnerReader{ls}, Store: store, Peers: &testPeerLister{}})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	loop.Run(ctx)
	require.GreaterOrEqual(t, posts.Load(), int32(2))
	require.GreaterOrEqual(t, probes.Load(), posts.Load(), "every refusal evicts the cached capability")
}

// TestHandleIsolation_AnswersTheOwnersCurrentRules: the re-check endpoint
// resolves the run's context from its persisted namespace and the owner's
// policy as it is now (a reload applies), and fails closed when it cannot.
func TestHandleIsolation_AnswersTheOwnersCurrentRules(t *testing.T) {
	installIsolationPolicy(t, isolationTestPolicy)
	store, _, h := setupHandler(t)
	runID, _, _ := seedNamespacedTaskRun(t, store, "marketing", false)
	server := httptest.NewServer(http.HandlerFunc(h.HandleIsolation))
	t.Cleanup(server.Close)

	got, err := FetchIsolation(context.Background(), server.URL, testToken, runID)
	require.NoError(t, err)
	require.Equal(t, "marketing", got.Namespace)
	require.Equal(t, []string{"env/MARKETING_*"}, got.Secrets.Allow)

	installIsolationPolicy(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  marketing:\n    secrets: {allow: []}\n")
	got, err = FetchIsolation(context.Background(), server.URL, testToken, runID)
	require.NoError(t, err)
	require.NotNil(t, got.Secrets)
	require.Empty(t, got.Secrets.Allow, "a revocation is answered at once")

	_, err = FetchIsolation(context.Background(), server.URL, "wrong-token", runID)
	require.Error(t, err)
	_, err = FetchIsolation(context.Background(), server.URL, testToken, uuid.New())
	require.ErrorContains(t, err, "404")
	_, err = FetchIsolation(context.Background(), "http://127.0.0.1:1", testToken, runID)
	require.ErrorIs(t, err, ErrPeerUnreachable)

	// Registered before Setenv so it runs after the variable is restored.
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", filepath.Join(t.TempDir(), "policy.yaml"))
	require.NoError(t, env.Process())
	policy.SetProcessHolder(nil)
	_, err = FetchIsolation(context.Background(), server.URL, testToken, runID)
	require.ErrorContains(t, err, "503", "an owner whose policy is not loaded cannot vouch for any rule")

	// An owner restarted without its policy file (or serving environment
	// settings only) has no rules to vouch for: its empty context would read
	// as "unrestricted", so it refuses.
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", "")
	require.NoError(t, env.Process())
	policy.SetProcessHolder(nil)
	_, err = FetchIsolation(context.Background(), server.URL, testToken, runID)
	require.ErrorContains(t, err, "409", "a policyless owner never answers a re-check")
	view := policy.EmptyPolicy()
	view.Namespaces["marketing"] = policy.Namespace{}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
	_, err = FetchIsolation(context.Background(), server.URL, testToken, runID)
	require.ErrorContains(t, err, "409")
}
