package worker

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/atom"
	"github.com/caesium-cloud/caesium/internal/auth/policy"
	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	jobdeftestutil "github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/container"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// These tests install the process-wide policy holder, so none is parallel.

func installWorkerSecretPolicy(t *testing.T, namespace string, allow ...string) {
	t.Helper()
	view := policy.EmptyPolicy()
	view.Namespaces[namespace] = policy.Namespace{Secrets: &policy.SecretRules{Allow: append([]string{}, allow...)}}
	policy.SetProcessHolder(policy.NewEnvHolder(view, "", nil))
}

type workerSecretFixture struct {
	db        *gorm.DB
	store     *run.Store
	namespace string
	job       *models.Job
	trigger   *models.Trigger
	task      *models.Task
	atom      *models.Atom
}

func seedWorkerSecretTask(t *testing.T, namespace string, task models.Task) *workerSecretFixture {
	t.Helper()
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	now := time.Now().UTC()
	f := &workerSecretFixture{db: db, store: run.NewStore(db), namespace: namespace}
	f.trigger = &models.Trigger{ID: uuid.New(), Alias: "c4-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(f.trigger).Error)
	f.job = &models.Job{ID: uuid.New(), Alias: "c4-" + namespace, Namespace: namespace, TriggerID: f.trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(f.job).Error)
	spec, err := json.Marshal(container.Spec{Env: map[string]string{"TOKEN": "secret://env/C4_TOKEN", "PLAIN": "x"}})
	require.NoError(t, err)
	f.atom = &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, Spec: datatypes.JSON(spec), CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(f.atom).Error)
	task.ID, task.JobID, task.AtomID, task.Name, task.CreatedAt, task.UpdatedAt = uuid.New(), f.job.ID, f.atom.ID, "secret", now, now
	f.task = &task
	require.NoError(t, db.Create(f.task).Error)
	return f
}

// newAttempt seeds a fresh run of the same task, so the cache identity is
// unchanged across attempts — the worker equivalent of running the job again.
func (f *workerSecretFixture) newAttempt(t *testing.T, maxAttempts int) *models.TaskRun {
	t.Helper()
	now := time.Now().UTC()
	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: f.job.ID, Namespace: f.namespace, TriggerID: f.trigger.ID, TriggerType: string(f.trigger.Type),
		Status: string(run.StatusRunning), StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, f.db.Create(jobRun).Error)
	taskRun := &models.TaskRun{
		ID: uuid.New(), JobRunID: jobRun.ID, TaskID: f.task.ID, AtomID: f.atom.ID,
		Engine: f.atom.Engine, Image: f.atom.Image, Command: f.atom.Command,
		Status: string(run.TaskStatusRunning), ClaimedBy: "node-a", Attempt: 1, MaxAttempts: maxAttempts,
		CacheEnabled: true, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, f.db.Create(taskRun).Error)
	return taskRun
}

func (f *workerSecretFixture) execute(t *testing.T, taskRun *models.TaskRun, resolver secret.Resolver, sink CompletionSink, engine atom.Engine) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	(&runtimeExecutor{
		store:          f.store,
		localSink:      sink,
		secretResolver: resolver,
		engineFactory:  func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
	}).Execute(ctx, taskRun)
}

func requireWorkerDenial(t *testing.T, sink *fakeSink) {
	t.Helper()
	require.Equal(t, 1, sink.failed)
	require.Zero(t, sink.cached, "a denied reference is never served from cache")
	require.Zero(t, sink.succeeded)
	require.ErrorIs(t, sink.lastErr, secret.ErrSecretDenied)
	require.Contains(t, sink.lastErr.Error(), "env/C4_TOKEN/…")
	require.NotContains(t, sink.lastErr.Error(), "secret://env/C4_TOKEN")
}

// TestWorkerSecretRulesAreCheckedBeforeTheCache: with a cached result for the
// task, a run whose namespace no longer allows the reference fails without a
// cache hit and without a container.
func TestWorkerSecretRulesAreCheckedBeforeTheCache(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-worker-cache"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	resolver := staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}

	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	cold := &captureCreateEngine{}
	first := f.newAttempt(t, 1)
	f.execute(t, first, resolver, NewLocalSink(f.store), cold)
	require.NotNil(t, cold.createReq)
	require.Equal(t, "resolved", cold.createReq.Spec.Env["TOKEN"])
	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", first.ID).Error)
	require.NotEmpty(t, persisted.Hash)
	_, found, err := cache.NewStore(f.db).Get(persisted.Hash)
	require.NoError(t, err)
	require.True(t, found, "the allowed run publishes a cache entry")

	// While allowed, the next attempt is a cache hit.
	hit := &fakeSink{}
	f.execute(t, f.newAttempt(t, 1), resolver, hit, &captureCreateEngine{})
	require.Equal(t, 1, hit.cached)

	installWorkerSecretPolicy(t, namespace) // allow: [] denies everything
	denied := &fakeSink{}
	engine := &captureCreateEngine{}
	f.execute(t, f.newAttempt(t, 1), resolver, denied, engine)
	requireWorkerDenial(t, denied)
	require.Nil(t, engine.createReq, "a denial starts no container")
}

// revokingStaticResolver revokes the rule while the revokeAt-th reference is
// canonicalised (the first when revokeAt is 0): that check still passes on the
// rules its scope was built with, and every later scope reads the new ones.
type revokingStaticResolver struct {
	staticSecretResolver
	mu       sync.Mutex
	calls    int
	revokeAt int
	revoke   func()
}

func (r *revokingStaticResolver) CanonicalTarget(ref string) (string, error) {
	target, err := secret.NewEnvResolver().CanonicalTarget(ref)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls == max(r.revokeAt, 1) {
		r.revoke()
	}
	return target, err
}

// TestWorkerSecretRulesAreRecheckedRightBeforeTheCacheLookup: a rule revoked
// after the first allow-check (while input acquisition and digest resolution
// run) is enforced by the recheck right before the lookup, so the cached result
// is not served.
func TestWorkerSecretRulesAreRecheckedRightBeforeTheCacheLookup(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-worker-recheck"
	f := seedWorkerSecretTask(t, namespace, models.Task{})
	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	resolver := staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}
	f.execute(t, f.newAttempt(t, 1), resolver, NewLocalSink(f.store), &captureCreateEngine{})
	hit := &fakeSink{}
	f.execute(t, f.newAttempt(t, 1), resolver, hit, &captureCreateEngine{})
	require.Equal(t, 1, hit.cached, "while allowed, the next attempt is a cache hit")

	revoking := &revokingStaticResolver{
		staticSecretResolver: resolver,
		revoke:               func() { installWorkerSecretPolicy(t, namespace) },
	}
	denied := &fakeSink{}
	engine := &captureCreateEngine{}
	f.execute(t, f.newAttempt(t, 1), revoking, denied, engine)
	requireWorkerDenial(t, denied)
	require.Nil(t, engine.createReq, "a denial starts no container")
	require.Equal(t, 2, revoking.calls, "the first check passed; the recheck denied")
}

// TestWorkerSecretDenialAtResolutionIsTerminal: a rule revoked after the
// before-cache check is enforced by the attempt's fresh scope, and the denial
// is terminal — the retry budget and its hour-long delay are never used.
func TestWorkerSecretDenialAtResolutionIsTerminal(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-worker-revoke"
	f := seedWorkerSecretTask(t, namespace, models.Task{Retries: 3, RetryDelay: time.Hour})
	installWorkerSecretPolicy(t, namespace, "env/C4_*")
	// Revoked during the second check (the recheck right before the cache
	// lookup), so both before-cache checks pass and the attempt's own scope
	// is the one that denies.
	resolver := &revokingStaticResolver{
		staticSecretResolver: staticSecretResolver{"secret://env/C4_TOKEN": "resolved"},
		revokeAt:             2,
		revoke:               func() { installWorkerSecretPolicy(t, namespace) },
	}
	sink := &fakeSink{}
	engine := &captureCreateEngine{}
	taskRun := f.newAttempt(t, 4)
	started := time.Now()
	f.execute(t, taskRun, resolver, sink, engine)
	require.Less(t, time.Since(started), 20*time.Second, "no retry backoff")
	requireWorkerDenial(t, sink)
	require.Nil(t, engine.createReq)
	var persisted models.TaskRun
	require.NoError(t, f.db.First(&persisted, "id = ?", taskRun.ID).Error)
	require.Equal(t, 1, persisted.Attempt, "the denial never advanced the attempt")
	require.GreaterOrEqual(t, resolver.calls, 3, "denied by the attempt's resolution, after both checks passed")
}

// pinnedVaultResolver resolves a pinned Vault identity, optionally revoking
// the namespace rule while the read is in flight, and verifies identities.
type pinnedVaultResolver struct {
	revoke   func()
	verified int
}

func (r *pinnedVaultResolver) Resolve(ctx context.Context, ref string) (string, error) {
	value, _, err := r.ResolveWithIdentity(ctx, ref)
	return value, err
}

func (r *pinnedVaultResolver) ResolveWithIdentity(_ context.Context, ref string) (string, secret.Identity, error) {
	if r.revoke != nil {
		r.revoke()
	}
	return "vault-value", secret.Identity{Provider: "vault", Ref: ref, Version: "3", KeyID: "k1", HMACSHA256: "proof", Verifiable: true}, nil
}

func (r *pinnedVaultResolver) CanonicalTarget(ref string) (string, error) {
	return secret.NewVaultResolverWithLogical(nil).CanonicalTarget(ref)
}

func (r *pinnedVaultResolver) VerifyResolvedIdentity(_ context.Context, _ string, expected secret.Identity, _ string) (secret.Identity, error) {
	r.verified++
	return expected, nil
}

// seedQuarantinedVaultTask registers a quarantined replay task whose frozen
// descriptor carries a pinned Vault secret identity, so the worker re-verifies
// it through the resolver after resolution.
func seedQuarantinedVaultTask(t *testing.T, namespace string) (*gorm.DB, *run.Store, *models.TaskRun) {
	t.Helper()
	const ref = "secret://vault/secret/data/c4/token/password"
	db := jobdeftestutil.OpenTestDB(t)
	t.Cleanup(func() { jobdeftestutil.CloseDB(db) })
	store := run.NewStore(db)
	now := time.Now().UTC()
	trigger := &models.Trigger{ID: uuid.New(), Alias: "c4-replay-trig-" + uuid.NewString()[:8], Type: models.TriggerTypeCron, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(trigger).Error)
	job := &models.Job{ID: uuid.New(), Alias: "c4-replay-" + uuid.NewString()[:8], Namespace: namespace, TriggerID: trigger.ID, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(job).Error)
	spec, err := json.Marshal(container.Spec{Env: map[string]string{"TOKEN": ref}})
	require.NoError(t, err)
	atomModel := &models.Atom{ID: uuid.New(), Engine: models.AtomEngineDocker, Image: "alpine:3.23", Command: `["sh","-c","true"]`, Spec: datatypes.JSON(spec), CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(atomModel).Error)
	task := &models.Task{ID: uuid.New(), JobID: job.ID, AtomID: atomModel.ID, Name: "deploy", ReplaySafe: true, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, db.Create(task).Error)
	jobRun := &models.JobRun{
		ID: uuid.New(), JobID: job.ID, Namespace: namespace, TriggerID: trigger.ID, TriggerType: string(trigger.Type),
		Status: string(run.StatusRunning), Quarantine: true, StartedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, db.Create(jobRun).Error)
	require.NoError(t, store.RegisterTasks(jobRun.ID, []run.RegisterTaskInput{{Task: task, Atom: atomModel}}))

	var registered models.TaskRun
	require.NoError(t, db.First(&registered, "job_run_id = ? AND task_id = ?", jobRun.ID, task.ID).Error)
	var descriptor models.TaskExecutionDescriptor
	require.NoError(t, json.Unmarshal(registered.ExecutionDescriptor, &descriptor))
	descriptor.SecretRefs = []models.TaskExecutionSecretRef{{
		Ref: ref, EnvKey: "TOKEN", Provider: "vault", Verifiable: true,
		Identity: datatypes.JSONMap{"version": "3", "keyId": "k1", "hmacSha256": "proof"},
	}}
	frozen, err := json.Marshal(&descriptor)
	require.NoError(t, err)
	require.NoError(t, db.Model(&models.TaskRun{}).Where("id = ?", registered.ID).Updates(map[string]any{
		"execution_descriptor": datatypes.JSON(frozen),
		"status":               string(run.TaskStatusRunning),
		"claimed_by":           "node-a",
	}).Error)
	var taskRun models.TaskRun
	require.NoError(t, db.First(&taskRun, "id = ?", registered.ID).Error)
	require.True(t, taskRun.Quarantine, "the replay task must take the quarantined path")
	return db, store, &taskRun
}

// TestWorkerReplayVerificationRereadsSecretRules: a quarantined replay's
// pinned Vault identity is re-verified with the namespace rules read after
// the resolution, so a rule revoked while the read was in flight stops the
// task before its container starts. The control run proves the same harness
// reaches verification and executes when nothing is revoked.
func TestWorkerReplayVerificationRereadsSecretRules(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	const namespace = "c4-replay"
	for _, revoke := range []bool{false, true} {
		installWorkerSecretPolicy(t, namespace, "vault/secret/data/c4/**")
		_, store, taskRun := seedQuarantinedVaultTask(t, namespace)
		inner := &pinnedVaultResolver{}
		if revoke {
			inner.revoke = func() { installWorkerSecretPolicy(t, namespace) }
		}
		sink := &fakeSink{}
		engine := &captureCreateEngine{}
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		(&runtimeExecutor{
			store:          store,
			localSink:      sink,
			secretResolver: inner,
			engineFactory:  func(context.Context, models.AtomEngine) (atom.Engine, error) { return engine, nil },
		}).Execute(ctx, taskRun)
		cancel()
		if !revoke {
			require.Equal(t, 1, inner.verified, "the control run re-verifies the pinned identity")
			require.NotNil(t, engine.createReq, "the control run executes")
			require.Zero(t, sink.failed, "control: %v", sink.lastErr)
			continue
		}
		require.Zero(t, inner.verified, "a revoked reference never reaches the inner verifier")
		require.Nil(t, engine.createReq, "a reference revoked during resolution starts no container")
		require.Equal(t, 1, sink.failed)
		require.ErrorIs(t, sink.lastErr, secret.ErrSecretDenied)
		require.Contains(t, sink.lastErr.Error(), "replay secret for env TOKEN:")
		require.NotContains(t, sink.lastErr.Error(), "secret://vault/secret/data/c4/token/password")
	}
}

// TestWorkerSecretScopeWithoutRulesIsUnchanged: the default namespace and a
// namespace without rules resolve exactly as before.
func TestWorkerSecretScopeWithoutRulesIsUnchanged(t *testing.T) {
	t.Cleanup(func() { policy.SetProcessHolder(nil) })
	installWorkerSecretPolicy(t, "c4-other", "env/NOTHING")
	for _, namespace := range []string{"default", "c4-unruled"} {
		f := seedWorkerSecretTask(t, namespace, models.Task{})
		engine := &captureCreateEngine{}
		f.execute(t, f.newAttempt(t, 1), staticSecretResolver{"secret://env/C4_TOKEN": "resolved"}, NewLocalSink(f.store), engine)
		require.NotNil(t, engine.createReq, namespace)
		require.Equal(t, "resolved", engine.createReq.Spec.Env["TOKEN"], namespace)
	}
}
