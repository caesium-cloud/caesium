package job

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/env"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// installSecretPolicy serves namespace's secret rules from the process policy
// holder. An env-translated holder (not file-backed) keeps every other
// namespace allow-all, so tests running alongside are unaffected.
func installSecretPolicy(t *testing.T, namespace string, allow ...string) {
	t.Helper()
	view := policy.EmptyPolicy()
	view.Namespaces[namespace] = policy.Namespace{Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
}

// revokingSecretResolver installs revoke right after the first reference is
// canonicalised: the before-cache allow-check passes on the old rules and the
// attempt's fresh scope then reads the new ones.
type revokingSecretResolver struct {
	localStaticSecretResolver
	once   sync.Once
	revoke func()
}

func (r *revokingSecretResolver) CanonicalTarget(ref string) (string, error) {
	target, err := secret.NewEnvResolver().CanonicalTarget(ref)
	r.once.Do(r.revoke)
	return target, err
}

type secretScopeFixture struct {
	db      *gorm.DB
	store   *run.Store
	engine  *fakeEngine
	jobID   uuid.UUID
	taskID  uuid.UUID
	job     *models.Job
	taskSvc *fakeTaskService
	atomSvc *fakeAtomService
}

func newSecretScopeFixture(t *testing.T, namespace string, task models.Task) *secretScopeFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	f := &secretScopeFixture{db: db, store: run.NewStore(db), engine: newFakeEngine(), jobID: uuid.New(), taskID: uuid.New()}
	f.job = &models.Job{ID: f.jobID, Alias: "c4-" + namespace, Namespace: namespace}
	require.NoError(t, db.Create(&models.Job{ID: f.jobID, Alias: f.job.Alias, Namespace: namespace}).Error)
	atomID := uuid.New()
	task.ID, task.JobID, task.AtomID, task.Name = f.taskID, f.jobID, atomID, "secret"
	tasks := models.Tasks{&task}
	f.taskSvc = &fakeTaskService{tasks: tasks}
	f.atomSvc = &fakeAtomService{atoms: map[uuid.UUID]*models.Atom{
		atomID: fakeModelAtomWithEnv(t, atomID, map[string]string{"TOKEN": "secret://env/C4_TOKEN", "PLAIN": "x"}),
	}}
	persistGraph(t, db, tasks, nil)
	return f
}

func (f *secretScopeFixture) run(t *testing.T, resolver secret.Resolver, extra ...JobOption) error {
	t.Helper()
	opts := append(withTestDeps(f.store, env.Environment{
		MaxParallelTasks: 1, TaskFailurePolicy: taskFailurePolicyHalt, ExecutionMode: executionModeLocal,
	}, f.taskSvc, f.atomSvc, &fakeTaskEdgeService{}, f.engine), WithSecretResolver(resolver))
	opts = append(opts, extra...)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	return New(f.job, opts...).Run(ctx)
}

func requireDenied(t *testing.T, tr *run.TaskRun) {
	t.Helper()
	require.NotNil(t, tr)
	require.Equal(t, run.TaskStatusFailed, tr.Status)
	require.False(t, tr.CacheHit, "a denied reference is never served from cache")
	require.Contains(t, tr.Error, secret.DeniedMarker)
	require.Contains(t, tr.Error, "env/C4_TOKEN/…")
	require.NotContains(t, tr.Error, "secret://env/C4_TOKEN", "the raw reference never reaches the task error")
	require.LessOrEqual(t, tr.Attempt, 1, "a denial is never retried")
}

// TestLocalSecretRulesAreCheckedBeforeTheCache pins the before-cache contract:
// a task whose result is cached is still refused, without a container and
// without retries, once its namespace stops allowing the reference.
func TestLocalSecretRulesAreCheckedBeforeTheCache(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-local-cache"
	f := newSecretScopeFixture(t, namespace, models.Task{Retries: 3, CacheConfig: datatypes.JSON("true")})
	resolver := localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"}

	installSecretPolicy(t, namespace, "env/C4_*")
	require.NoError(t, f.run(t, resolver))
	first := latestRunSnapshot(t, f.store, f.jobID)
	require.Equal(t, namespace, first.Namespace)
	require.Equal(t, run.TaskStatusSucceeded, taskRunByID(first, f.taskID).Status)
	require.Len(t, f.engine.createRequestsForTask(f.taskID), 1)

	require.NoError(t, f.run(t, resolver))
	second := latestRunSnapshot(t, f.store, f.jobID)
	require.Equal(t, run.TaskStatusCached, taskRunByID(second, f.taskID).Status, "while allowed, the cache serves the task")

	installSecretPolicy(t, namespace) // allow: [] denies everything
	runErr := f.run(t, resolver)
	require.Error(t, runErr)
	require.ErrorContains(t, runErr, secret.DeniedMarker)
	third := latestRunSnapshot(t, f.store, f.jobID)
	requireDenied(t, taskRunByID(third, f.taskID))
	require.Len(t, f.engine.createRequestsForTask(f.taskID), 1, "a denial starts no container")
}

// TestLocalSecretRulesAreRecheckedRightBeforeTheCacheLookup: a rule revoked
// after the first allow-check (while cache-identity resolution, which can
// block on registry I/O, runs) is enforced by the recheck right before the
// lookup, so the cached result is not served.
func TestLocalSecretRulesAreRecheckedRightBeforeTheCacheLookup(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-local-recheck"
	f := newSecretScopeFixture(t, namespace, models.Task{Retries: 3, CacheConfig: datatypes.JSON("true")})
	resolver := localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"}
	installSecretPolicy(t, namespace, "env/C4_*")
	require.NoError(t, f.run(t, resolver))
	require.NoError(t, f.run(t, resolver))
	require.Equal(t, run.TaskStatusCached, taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID).Status,
		"while allowed, the cache serves the task")

	revoking := &revokingSecretResolver{
		localStaticSecretResolver: resolver,
		revoke:                    func() { installSecretPolicy(t, namespace) },
	}
	require.ErrorContains(t, f.run(t, revoking), secret.DeniedMarker)
	requireDenied(t, taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID))
	require.Len(t, f.engine.createRequestsForTask(f.taskID), 1, "a denial starts no container")
}

// TestLocalSecretDenialAtResolutionIsTerminal revokes the rule after the
// before-cache check passed: the attempt's fresh scope denies the reference,
// and the configured retries (with a long delay) are not consumed.
func TestLocalSecretDenialAtResolutionIsTerminal(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-local-revoke"
	f := newSecretScopeFixture(t, namespace, models.Task{Retries: 3, RetryDelay: time.Hour})
	installSecretPolicy(t, namespace, "env/C4_*")
	resolver := &revokingSecretResolver{
		localStaticSecretResolver: localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"},
		revoke:                    func() { installSecretPolicy(t, namespace) },
	}

	started := time.Now()
	require.ErrorContains(t, f.run(t, resolver), secret.DeniedMarker)
	require.Less(t, time.Since(started), 20*time.Second, "no retry backoff")
	requireDenied(t, taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID))
	require.Empty(t, f.engine.createRequestsForTask(f.taskID))
}

// TestLocalSecretScopeWithoutRulesIsUnchanged: a namespace without rules (and
// the default namespace) resolves exactly as before, error text included.
func TestLocalSecretScopeWithoutRulesIsUnchanged(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	installSecretPolicy(t, "c4-other", "env/NOTHING")
	f := newSecretScopeFixture(t, "c4-unruled", models.Task{})
	require.NoError(t, f.run(t, localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"}))
	tr := taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID)
	require.Equal(t, run.TaskStatusSucceeded, tr.Status)
	reqs := f.engine.createRequestsForTask(f.taskID)
	require.Len(t, reqs, 1)
	require.Equal(t, "resolved", reqs[0].Spec.Env["TOKEN"])

	// A provider failure keeps its pre-C4 wording (the raw ref included).
	g := newSecretScopeFixture(t, "c4-unruled-missing", models.Task{})
	require.Error(t, g.run(t, secret.NewMultiResolver(map[string]secret.Resolver{"env": secret.NewEnvResolver()})))
	failed := taskRunByID(latestRunSnapshot(t, g.store, g.jobID), g.taskID)
	require.Contains(t, failed.Error, "resolve env TOKEN from secret://env/C4_TOKEN: environment variable C4_TOKEN not set")
}

// TestClientProcessRunNeverWaitsForAServerPolicy: a client process (`caesium
// dev`) inherits CAESIUM_AUTH_POLICY_FILE but never installs a policy holder,
// so its run resolves the secret at once instead of waiting for the server's
// policy and failing denied. Not parallel: it sets the process environment.
func TestClientProcessRunNeverWaitsForAServerPolicy(t *testing.T) {
	t.Cleanup(func() {
		policy.SetProcessHolder(nil)
		require.NoError(t, env.Process())
	})
	t.Setenv("CAESIUM_AUTH_POLICY_FILE", filepath.Join(t.TempDir(), "policy.yaml"))
	t.Setenv("CAESIUM_AUTH_ROLE_MAPPING", "")
	require.NoError(t, env.Process())
	policy.SetProcessHolder(nil)
	require.True(t, secret.ScopingActive(), "a configured policy file makes the server's scope pending")

	f := newSecretScopeFixture(t, "c4-client", models.Task{})
	started := time.Now()
	require.NoError(t, f.run(t, localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"}, WithClientProcess()))
	require.Less(t, time.Since(started), 10*time.Second, "a client run never waits for a server policy")
	tr := taskRunByID(latestRunSnapshot(t, f.store, f.jobID), f.taskID)
	require.Equal(t, run.TaskStatusSucceeded, tr.Status)
	reqs := f.engine.createRequestsForTask(f.taskID)
	require.Len(t, reqs, 1)
	require.Equal(t, "resolved", reqs[0].Spec.Env["TOKEN"])
}

// hookEngine runs onCreate before delegating each container create.
type hookEngine struct {
	*fakeEngine
	onCreate func(req *atom.EngineCreateRequest)
}

func (e *hookEngine) Create(req *atom.EngineCreateRequest) (atom.Atom, error) {
	if e.onCreate != nil {
		e.onCreate(req)
	}
	return e.fakeEngine.Create(req)
}

// TestFannedInstancesCheckSecretRulesBeforeTheirOwnCacheLookup revokes the
// rule while partition a holds the group open (its container is running and
// MaxParallel is 1). Partitions b and c, whose results are cached, wait and
// then run their own allow-check: they fail as denied instead of being served
// from cache.
func TestFannedInstancesCheckSecretRulesBeforeTheirOwnCacheLookup(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-fanned"
	f := newFanOutFixture(t, `["a","b","c"]`, &schema.FanOut{
		From: "list", MaxPartitions: 16, MaxParallel: 1, FailurePolicy: schema.FanOutFailureContinue,
	}, 2)
	require.NoError(t, f.db.Create(&models.Job{ID: f.jobID, Alias: "c4-fanned", Namespace: namespace}).Error)
	fannedAtom := f.taskSvc.tasks[1].AtomID
	f.atomSvc.atoms[fannedAtom] = fakeModelAtomWithEnv(t, fannedAtom, map[string]string{"TOKEN": "secret://env/C4_TOKEN"})
	f.enableStepCache(t)
	resolver := localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"}
	engine := &hookEngine{fakeEngine: f.engine}
	runJob := func() error {
		opts := append(withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, engine), WithSecretResolver(resolver))
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		return New(&models.Job{ID: f.jobID, Alias: "c4-fanned", Namespace: namespace}, opts...).Run(ctx)
	}

	installSecretPolicy(t, namespace, "env/C4_*")
	require.NoError(t, runJob())
	cached := f.instanceRows(t)
	require.Len(t, cached, 3)
	for _, row := range cached {
		require.Equal(t, string(run.TaskStatusSucceeded), row.Status, row.PartitionValue)
	}
	// Partition a misses the cache next time; b and c would hit it.
	require.NoError(t, f.db.Where("hash = ?", cached[0].Hash).Delete(&models.TaskCache{}).Error)

	engine.onCreate = func(req *atom.EngineCreateRequest) {
		if req.Spec.Env[schema.DefaultFanOutEnv] == "a" {
			installSecretPolicy(t, namespace) // revoke while a runs
		}
	}
	require.Error(t, runJob())
	rows := f.instanceRows(t)
	require.Len(t, rows, 3)
	byPartition := map[string]models.TaskRun{}
	for _, row := range rows {
		byPartition[row.PartitionValue] = row
	}
	require.Equal(t, string(run.TaskStatusSucceeded), byPartition["a"].Status, "a ran before the revocation")
	for _, p := range []string{"b", "c"} {
		row := byPartition[p]
		require.Equal(t, string(run.TaskStatusFailed), row.Status, p)
		require.False(t, row.CacheHit, p)
		require.Contains(t, row.Error, secret.DeniedMarker, p)
		require.NotContains(t, row.Error, "secret://env/C4_TOKEN", p)
		require.LessOrEqual(t, row.Attempt, 1, "%s: a denial is never retried", p)
	}
	require.Equal(t, 1, f.engine.createCallsByPartition["b"], "b ran a container only in the first run")
	require.Equal(t, 1, f.engine.createCallsByPartition["c"], "c ran a container only in the first run")
}

// TestFannedInstanceDenialAtResolutionIsTerminal revokes after the first
// instance's before-cache check: that instance is denied at resolution and
// later instances at their own check; the retry budget is never used.
func TestFannedInstanceDenialAtResolutionIsTerminal(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-fanned-revoke"
	f := newFanOutFixture(t, `["a","b"]`, &schema.FanOut{
		From: "list", MaxPartitions: 16, MaxParallel: 1, FailurePolicy: schema.FanOutFailureContinue,
	}, 3)
	require.NoError(t, f.db.Create(&models.Job{ID: f.jobID, Alias: "c4-fanned-revoke", Namespace: namespace}).Error)
	fannedAtom := f.taskSvc.tasks[1].AtomID
	f.atomSvc.atoms[fannedAtom] = fakeModelAtomWithEnv(t, fannedAtom, map[string]string{"TOKEN": "secret://env/C4_TOKEN"})
	installSecretPolicy(t, namespace, "env/C4_*")
	resolver := &revokingSecretResolver{
		localStaticSecretResolver: localStaticSecretResolver{"secret://env/C4_TOKEN": "resolved"},
		revoke:                    func() { installSecretPolicy(t, namespace) },
	}
	opts := append(withTestDeps(f.store, defaultFanOutVars(), f.taskSvc, f.atomSvc, f.edgeSvc, f.engine), WithSecretResolver(resolver))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	require.Error(t, New(&models.Job{ID: f.jobID, Alias: "c4-fanned-revoke", Namespace: namespace}, opts...).Run(ctx))

	rows := f.instanceRows(t)
	require.Len(t, rows, 2)
	for _, row := range rows {
		require.Equal(t, string(run.TaskStatusFailed), row.Status, row.PartitionValue)
		require.Contains(t, row.Error, secret.DeniedMarker, row.PartitionValue)
		require.LessOrEqual(t, row.Attempt, 1, row.PartitionValue)
		require.Zero(t, f.engine.createCallsByPartition[row.PartitionValue], row.PartitionValue)
	}
}
