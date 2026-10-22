package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/dispatch"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

// The distributed form of the worker secret-scope regressions
// (secret_scope_test.go): the task arrives with its owner's isolation context,
// and this worker serves NO policy file (or a contradicting one), so every
// allow or deny below can only come from the owner. Not parallel: these tests
// install the process holder.

// completionRecorder stands in for the owner's /internal/complete.
type completionRecorder struct {
	mu   sync.Mutex
	reqs []dispatch.CompleteRequest
}

func (r *completionRecorder) post(_ context.Context, _ string, _ string, req dispatch.CompleteRequest) (*dispatch.CompleteResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reqs = append(r.reqs, req)
	return &dispatch.CompleteResponse{Accepted: true}, nil
}

func (r *completionRecorder) only(t *testing.T) dispatch.CompleteRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.Len(t, r.reqs, 1)
	return r.reqs[0]
}

// ownerRules is a fake owner answering /internal/isolation with rules that
// a test changes between checks.
type ownerRules struct {
	mu      sync.Mutex
	calls   int
	current func(call int) (*jobdefruntime.IsolationContext, error)
}

func (o *ownerRules) fetch(_ context.Context, _ string, _ string, _ uuid.UUID) (*jobdefruntime.IsolationContext, error) {
	o.mu.Lock()
	o.calls++
	call := o.calls
	o.mu.Unlock()
	return o.current(call)
}

func (o *ownerRules) count() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls
}

func isolationFor(namespace string, allow ...string) *jobdefruntime.IsolationContext {
	return &jobdefruntime.IsolationContext{Namespace: namespace, Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}, PolicyRevision: "rev"}
}

func staticOwner(ctx *jobdefruntime.IsolationContext) *ownerRules {
	return &ownerRules{current: func(int) (*jobdefruntime.IsolationContext, error) { return ctx, nil }}
}

func (f *workerSecretFixture) executeDispatched(t *testing.T, taskRun *models.TaskRun, resolver secret.Resolver, isolation *jobdefruntime.IsolationContext, owner *ownerRules, engine atom.Engine) *completionRecorder {
	t.Helper()
	rec := &completionRecorder{}
	meta := ownerMeta()
	meta.WorkerNode = taskRun.ClaimedBy
	meta.Attempt = taskRun.Attempt
	meta.Isolation = isolation
	ctx, cancel := context.WithTimeout(withDispatchMeta(t.Context(), meta), 30*time.Second)
	defer cancel()
	(&runtimeExecutor{
		store:          f.store,
		localSink:      NewLocalSink(f.store),
		completePost:   rec.post,
		isolationFetch: owner.fetch,
		secretResolver: resolver,
		engineFactory:  func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
	}).Execute(ctx, taskRun)
	return rec
}

func requireDispatchedDenial(t *testing.T, rec *completionRecorder, engine *captureCreateEngine) dispatch.CompleteRequest {
	t.Helper()
	req := rec.only(t)
	require.Equal(t, string(run.TaskStatusFailed), req.Status)
	require.Contains(t, req.Error, secret.DeniedMarker)
	require.NotContains(t, req.Error, "secret://env/C4_TOKEN")
	require.Nil(t, engine.createReq, "a denial starts no container")
	return req
}

// TestDispatchedTaskEnforcesTheOwnersRulesNotThisNodes: on a worker serving no
// policy file a denied reference is still denied, and an allowed one resolves;
// on a worker whose own (stale) file would deny, the owner's allow wins.
func TestDispatchedTaskEnforcesTheOwnersRulesNotThisNodes(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c2-owner-rules"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	resolver := staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}

	policy.SetProcessHolder(nil) // this worker serves no policy file
	denyCtx := isolationFor(namespace, "env/OTHER_*")
	engine := &captureCreateEngine{}
	rec := f.executeDispatched(t, f.newAttempt(t, 1), resolver, denyCtx, staticOwner(denyCtx), engine)
	requireDispatchedDenial(t, rec, engine)

	allowCtx := isolationFor(namespace, "env/C4_*")
	engine = &captureCreateEngine{waitResult: atom.Success}
	rec = f.executeDispatched(t, f.newAttempt(t, 1), resolver, allowCtx, staticOwner(allowCtx), engine)
	require.NotNil(t, engine.createReq)
	require.Equal(t, "resolved", engine.createReq.Spec.Env["TOKEN"])
	require.Equal(t, string(run.TaskStatusSucceeded), rec.only(t).Status, rec.only(t).Error)

	// This worker's own (stale) file would deny everything: the owner's
	// allow still governs, so the attempt passes both checks to its cache hit.
	installWorkerSecretPolicy(t, namespace)
	rec = f.executeDispatched(t, f.newAttempt(t, 1), resolver, allowCtx, staticOwner(allowCtx), &captureCreateEngine{})
	require.Equal(t, string(run.TaskStatusCached), rec.only(t).Status, rec.only(t).Error)
}

// TestDispatchedRecheckBeforeTheCacheConsultsTheOwner: a rule the owner
// revoked after the dispatch is enforced by the re-check right before the
// cache lookup, so the cached result is not served (the distributed form of
// TestWorkerSecretRulesAreRecheckedRightBeforeTheCacheLookup).
func TestDispatchedRecheckBeforeTheCacheConsultsTheOwner(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c2-recheck"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	resolver := staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}
	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	f.execute(t, f.newAttempt(t, 1), resolver, NewLocalSink(f.store), &captureCreateEngine{})
	policy.SetProcessHolder(nil)

	allowCtx := isolationFor(namespace, "env/C4_*")
	control := f.executeDispatched(t, f.newAttempt(t, 1), resolver, allowCtx, staticOwner(allowCtx), &captureCreateEngine{})
	require.Equal(t, string(run.TaskStatusCached), control.only(t).Status, "while the owner allows, the attempt is a cache hit")

	revoked := staticOwner(isolationFor(namespace))
	engine := &captureCreateEngine{}
	rec := f.executeDispatched(t, f.newAttempt(t, 1), resolver, allowCtx, revoked, engine)
	requireDispatchedDenial(t, rec, engine)
	require.Equal(t, 1, revoked.count(), "the dispatched rules passed the first check; the owner's current rules denied the re-check")

	// A context dispatched with no rules at all is re-checked the same way:
	// rules the owner added since, or an owner that cannot be asked, deny.
	unrestricted := &jobdefruntime.IsolationContext{Namespace: namespace}
	engine = &captureCreateEngine{}
	rec = f.executeDispatched(t, f.newAttempt(t, 1), resolver, unrestricted, staticOwner(isolationFor(namespace)), engine)
	requireDispatchedDenial(t, rec, engine)
	engine = &captureCreateEngine{}
	unreachable := &ownerRules{current: func(int) (*jobdefruntime.IsolationContext, error) { return nil, errors.New("owner down") }}
	rec = f.executeDispatched(t, f.newAttempt(t, 1), resolver, unrestricted, unreachable, engine)
	requireDispatchedDenial(t, rec, engine)
}

// TestDispatchedRecheckDeniesWhenTheOwnerLostItsPolicy: the task was
// dispatched under the owner's policy-backed context, then the owner restarted
// without CAESIUM_AUTH_POLICY_FILE before the re-check. Its real
// /internal/isolation endpoint refuses (it cannot vouch for any rule), so the
// re-check denies instead of reading an empty context as "no rules" and
// serving the cached result.
func TestDispatchedRecheckDeniesWhenTheOwnerLostItsPolicy(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c2-policy-lost"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	resolver := staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}
	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	f.execute(t, f.newAttempt(t, 1), resolver, NewLocalSink(f.store), &captureCreateEngine{})

	const token = "c2-owner-token"
	ownerServer := httptest.NewServer(http.HandlerFunc(dispatch.NewHandler(f.store, nil, "owner:9001", token).HandleIsolation))
	t.Cleanup(ownerServer.Close)
	viaOwner := func(runID uuid.UUID) *ownerRules {
		return &ownerRules{current: func(int) (*jobdefruntime.IsolationContext, error) {
			return dispatch.FetchIsolation(context.Background(), ownerServer.URL, token, runID)
		}}
	}
	allowCtx := isolationFor(namespace, "env/C4_*")

	// Control: the owner still serves its policy file, so the re-check passes
	// and the attempt is a cache hit.
	policy.SetProcessHolder(loadWorkerPolicyFile(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  "+namespace+":\n    secrets: {allow: [\"env/C4_*\"]}\n"))
	taskRun := f.newAttempt(t, 1)
	control := f.executeDispatched(t, taskRun, resolver, allowCtx, viaOwner(taskRun.JobRunID), &captureCreateEngine{})
	require.Equal(t, string(run.TaskStatusCached), control.only(t).Status, control.only(t).Error)

	// The owner lost its policy file: no holder, no file configured.
	policy.SetProcessHolder(nil)
	taskRun = f.newAttempt(t, 1)
	owner := viaOwner(taskRun.JobRunID)
	engine := &captureCreateEngine{}
	rec := f.executeDispatched(t, taskRun, resolver, allowCtx, owner, engine)
	req := requireDispatchedDenial(t, rec, engine)
	require.Contains(t, req.Error, "isolation context is unavailable")
	require.Equal(t, 1, owner.count(), "the re-check asked the owner and was refused")

	// The same for an owner serving environment-derived settings only.
	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	taskRun = f.newAttempt(t, 1)
	engine = &captureCreateEngine{}
	rec = f.executeDispatched(t, taskRun, resolver, allowCtx, viaOwner(taskRun.JobRunID), engine)
	requireDispatchedDenial(t, rec, engine)
}

// TestDispatchedDenialAtResolutionIsTerminal: the owner revoking between the
// before-cache checks and the attempt's resolution stops the attempt, and the
// denial is terminal (the distributed form of
// TestWorkerSecretDenialAtResolutionIsTerminal).
func TestDispatchedDenialAtResolutionIsTerminal(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	policy.SetProcessHolder(nil)
	const namespace = "c2-revoke"
	f := seedWorkerSecretTask(t, namespace, models.Task{Retries: 3, RetryDelay: time.Hour})
	allowCtx := isolationFor(namespace, "env/C4_*")
	owner := &ownerRules{current: func(call int) (*jobdefruntime.IsolationContext, error) {
		if call == 1 {
			return allowCtx, nil // the re-check right before the cache lookup
		}
		return isolationFor(namespace), nil
	}}
	engine := &captureCreateEngine{}
	taskRun := f.newAttempt(t, 4)
	started := time.Now()
	rec := f.executeDispatched(t, taskRun, staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}, allowCtx, owner, engine)
	require.Less(t, time.Since(started), 20*time.Second, "no retry backoff")
	requireDispatchedDenial(t, rec, engine)
	require.GreaterOrEqual(t, owner.count(), 2)
	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", taskRun.ID).Error)
	require.Equal(t, 1, persisted.Attempt, "the denial never advanced the attempt")
}

// TestDispatchedReplayVerificationConsultsTheOwner: a quarantined replay's
// pinned identity is re-verified against the owner's rules as they are after
// the read (the distributed form of TestWorkerReplayVerificationRereadsSecretRules).
func TestDispatchedReplayVerificationConsultsTheOwner(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c2-replay"
	for _, revoke := range []bool{false, true} {
		policy.SetProcessHolder(nil)
		_, store, taskRun := seedQuarantinedVaultTask(t, namespace)
		allowCtx := isolationFor(namespace, "vault/secret/data/c4/**")
		var mu sync.Mutex
		revoked := false
		owner := &ownerRules{current: func(int) (*jobdefruntime.IsolationContext, error) {
			mu.Lock()
			defer mu.Unlock()
			if revoked {
				return isolationFor(namespace), nil
			}
			return allowCtx, nil
		}}
		inner := &pinnedVaultResolver{}
		if revoke {
			inner.revoke = func() { mu.Lock(); revoked = true; mu.Unlock() }
		}
		engine := &captureCreateEngine{waitResult: atom.Success}
		rec := &completionRecorder{}
		meta := ownerMeta()
		meta.WorkerNode = taskRun.ClaimedBy
		meta.Attempt = taskRun.Attempt
		meta.Isolation = allowCtx
		ctx, cancel := context.WithTimeout(withDispatchMeta(t.Context(), meta), 30*time.Second)
		(&runtimeExecutor{
			store: store, localSink: NewLocalSink(store), completePost: rec.post, isolationFetch: owner.fetch,
			secretResolver: inner,
			engineFactory:  func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
		}).Execute(ctx, taskRun)
		cancel()
		if !revoke {
			require.Equal(t, 1, inner.verified, "the control run re-verifies the pinned identity")
			require.NotNil(t, engine.createReq, "the control run executes")
			continue
		}
		require.Zero(t, inner.verified, "a revoked reference never reaches the inner verifier")
		require.Nil(t, engine.createReq)
		req := rec.only(t)
		require.Equal(t, string(run.TaskStatusFailed), req.Status)
		require.Contains(t, req.Error, secret.DeniedMarker)
	}
}

// TestDispatchedTaskFailsClosedWithoutItsOwner: an owner that cannot be asked
// at a re-check denies every reference.
func TestDispatchedTaskFailsClosedWithoutItsOwner(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	policy.SetProcessHolder(nil)
	const namespace = "c2-unreachable"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	allowCtx := isolationFor(namespace, "env/C4_*")
	owner := &ownerRules{current: func(int) (*jobdefruntime.IsolationContext, error) {
		return nil, errors.New("dial tcp: connection refused")
	}}
	engine := &captureCreateEngine{}
	rec := f.executeDispatched(t, f.newAttempt(t, 1), staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}, allowCtx, owner, engine)
	req := requireDispatchedDenial(t, rec, engine)
	require.Contains(t, req.Error, "isolation context is unavailable")
}

// TestDispatchedContextForAnotherNamespaceIsRefused: a context resolved for a
// namespace other than the run's is refused as a config failure.
func TestDispatchedContextForAnotherNamespaceIsRefused(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	policy.SetProcessHolder(nil)
	f := seedWorkerSecretTask(t, "c2-mine", models.Task{})
	wrong := isolationFor("c2-theirs", "env/C4_*")
	engine := &captureCreateEngine{}
	rec := f.executeDispatched(t, f.newAttempt(t, 1), staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}, wrong, staticOwner(wrong), engine)
	req := requireDispatchedDenial(t, rec, engine)
	require.Contains(t, req.Error, "c2-theirs")
}

// TestDispatchedKubernetesTargetComesFromTheOwner: the mapping, a refusal for
// an undeclared namespace, and an unmapped pod's namespace all come from the
// owner's context, whatever this worker's own policy and environment say.
func TestDispatchedKubernetesTargetComesFromTheOwner(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_KUBERNETES_NAMESPACE", "worker-pods")
	require.NoError(t, env.Process())
	policy.SetProcessHolder(nil)
	e := &runtimeExecutor{}
	k8s := models.AtomEngineKubernetes
	dispatched := func(iso *jobdefruntime.IsolationContext) context.Context {
		meta := ownerMeta()
		meta.Isolation = iso
		return withDispatchMeta(t.Context(), meta)
	}

	mapped := &jobdefruntime.IsolationContext{Namespace: "isolated", Kubernetes: &atom.KubeTarget{Namespace: "caesium-isolated", ServiceAccountName: "runner"}, DefaultKubernetesNamespace: "owner-pods"}
	got, err := e.kubernetesMapping(dispatched(mapped), "isolated", k8s)
	require.NoError(t, err)
	require.Equal(t, atom.KubeTarget{Namespace: "caesium-isolated", ServiceAccountName: "runner"}, got)
	require.Equal(t, got, e.podTarget(dispatched(mapped), k8s, got))

	unmapped := &jobdefruntime.IsolationContext{Namespace: "finance", DefaultKubernetesNamespace: "owner-pods"}
	got, err = e.kubernetesMapping(dispatched(unmapped), "finance", k8s)
	require.NoError(t, err)
	require.True(t, got.IsZero(), "the descriptor and cache identity stay unmapped")
	require.Equal(t, atom.KubeTarget{Namespace: "owner-pods"}, e.podTarget(dispatched(unmapped), k8s, got), "the pod runs in the owner's default, not worker-pods")
	require.True(t, e.podTarget(t.Context(), k8s, got).IsZero(), "a pulled task keeps the engine's own default")

	_, err = e.kubernetesMapping(dispatched(&jobdefruntime.IsolationContext{Namespace: "ghost", Undeclared: true}), "ghost", k8s)
	require.ErrorIs(t, err, jobdefruntime.ErrKubernetesTargetDenied)
	_, err = e.kubernetesMapping(dispatched(mapped), "finance", k8s)
	require.ErrorContains(t, err, secret.DeniedMarker)
}

// TestWorkerAdvertisesIsolationV1: a real worker behind a real dispatch
// handler advertises the capability, and CAESIUM_WORKER_ISOLATION_V1_DISABLED
// withdraws it (owners then never send it an isolated task).
func TestWorkerAdvertisesIsolationV1(t *testing.T) {
	t.Cleanup(func() { _ = env.Process() })
	w := NewWorker(&noopClaimer{}, NewPool(1), time.Second, func(context.Context, *models.TaskRun) {})
	h := dispatch.NewHandler(nil, nil, "10.0.0.9:9001", "tok").WithWorkerSubmitter(w)
	probe := func() dispatch.CapabilitiesResponse {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/internal/capabilities", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		h.HandleCapabilities(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		var got dispatch.CapabilitiesResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}
	require.NoError(t, env.Process())
	got := probe()
	require.True(t, got.Supports(dispatch.CapabilityIsolationV1))

	t.Setenv("CAESIUM_WORKER_ISOLATION_V1_DISABLED", "true")
	require.NoError(t, env.Process())
	got = probe()
	require.False(t, got.Supports(dispatch.CapabilityIsolationV1))
}

func loadWorkerPolicyFile(t *testing.T, body string) *policy.Holder {
	t.Helper()
	path := t.TempDir() + "/policy.yaml"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	holder, err := policy.LoadFile(context.Background(), policy.HolderOptions{Path: path})
	require.NoError(t, err)
	return holder
}

// TestClaimerPullsIsolatedRowsOnlyWhenItCanHonourThem: a row marked
// isolation-required is pulled only by a worker that advertises isolation_v1
// and serves a policy file giving the row's namespace the settings the row was
// stamped with (a comment-only edit still matches, a diverging file does not,
// whatever the worker's own run-owner setting). A worker without a file, with the
// capability withdrawn, or whose operator set the reserved label itself never
// pulls it. An unmarked row is unaffected.
func TestClaimerPullsIsolatedRowsOnlyWhenItCanHonourThem(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	t.Cleanup(func() { _ = env.Process() })
	t.Setenv("CAESIUM_RUN_OWNER_ENABLED", "true")
	require.NoError(t, env.Process())
	f := seedWorkerSecretTask(t, "c2-pull", models.Task{})
	pending := func(selector map[string]any) *models.TaskRun {
		taskRun := f.newAttempt(t, 1)
		require.NoError(t, f.db.Model(&models.TaskRun{}).Where("id = ?", taskRun.ID).Updates(map[string]any{
			"status": string(run.TaskStatusPending), "claimed_by": "", "node_selector": datatypes.JSONMap(selector),
		}).Error)
		return taskRun
	}
	const servedBody = "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  c2-pull:\n    secrets: {allow: []}\n"
	served := loadWorkerPolicyFile(t, servedBody)
	commented := loadWorkerPolicyFile(t, "# reworded\n"+servedBody)
	diverging := loadWorkerPolicyFile(t, "apiVersion: v1\nkind: AccessPolicy\nnamespaces:\n  c2-pull:\n    secrets: {allow: [\"env/C4_*\"]}\n")
	_, stamp := run.RevisionIsolation(served.Current(), "c2-pull")
	claim := func(labels map[string]string) *models.TaskRun {
		got, err := NewClaimer("node-b", f.store, time.Minute, labels).ClaimNext(t.Context())
		require.NoError(t, err)
		return got
	}

	first := pending(map[string]any{run.IsolationSelectorKey: stamp})
	policy.SetProcessHolder(nil)
	require.Nil(t, claim(nil), "a worker without a policy file never pulls an isolated row")
	require.Nil(t, claim(map[string]string{run.IsolationSelectorKey: stamp}), "the reserved label cannot be set by an operator")

	policy.SetProcessHolder(diverging)
	require.Nil(t, claim(nil), "a worker whose file gives the namespace other settings never pulls it")

	policy.SetProcessHolder(served)
	t.Setenv("CAESIUM_WORKER_ISOLATION_V1_DISABLED", "true")
	require.NoError(t, env.Process())
	require.Nil(t, claim(nil), "a worker with the capability withdrawn never pulls an isolated row")

	t.Setenv("CAESIUM_WORKER_ISOLATION_V1_DISABLED", "false")
	require.NoError(t, env.Process())
	got := claim(map[string]string{"zone": "a"})
	require.NotNil(t, got, "a capable worker serving the same settings pulls it")
	require.Equal(t, first.ID, got.ID)

	second := pending(map[string]any{run.IsolationSelectorKey: stamp})
	policy.SetProcessHolder(commented)
	got = claim(nil)
	require.NotNil(t, got, "an edit that keeps the namespace's settings keeps the row pullable")
	require.Equal(t, second.ID, got.ID)

	// This node's own run-owner setting changes nothing: another node may own
	// the run, so a diverging file never pulls the row.
	third := pending(map[string]any{run.IsolationSelectorKey: stamp})
	t.Setenv("CAESIUM_RUN_OWNER_ENABLED", "false")
	require.NoError(t, env.Process())
	policy.SetProcessHolder(diverging)
	require.Nil(t, claim(nil))
	policy.SetProcessHolder(served)
	got = claim(nil)
	require.NotNil(t, got)
	require.Equal(t, third.ID, got.ID)

	policy.SetProcessHolder(nil)
	plain := pending(nil)
	got = claim(nil)
	require.NotNil(t, got, "an unmarked row is pulled exactly as before")
	require.Equal(t, plain.ID, got.ID)
}

type noopClaimer struct{}

func (noopClaimer) ClaimNext(context.Context) (*models.TaskRun, error) { return nil, nil }
func (noopClaimer) ReclaimExpired(context.Context) error               { return nil }
