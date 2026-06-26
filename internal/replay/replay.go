package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/internal/cache"
	"github.com/caesium-cloud/caesium/internal/event"
	jobdefruntime "github.com/caesium-cloud/caesium/internal/jobdef/runtime"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var (
	ErrDispatchRequired         = errors.New("replay: dispatcher is required")
	ErrBaselineNotTerminal      = errors.New("replay: baseline run is not terminal")
	ErrMissingDescriptor        = errors.New("replay: missing baseline execution descriptor")
	ErrUnsupportedDescriptor    = errors.New("replay: unsupported baseline execution descriptor")
	ErrReplayUnsafe             = errors.New("replay: baseline task is not replay safe")
	ErrUnavailableBaselineProof = errors.New("replay: unchanged baseline result unavailable")
	ErrSecretIdentity           = errors.New("replay: baseline secret identity cannot be verified")
	ErrQuarantinedBaseline      = errors.New("replay: baseline run is quarantined")
)

// Dispatcher is the narrow B3 seam B4/B5 use to hand a durable replay run to
// the already-running execution machinery.
type Dispatcher interface {
	DispatchReplay(ctx context.Context, runID uuid.UUID) error
}

type DispatchFunc func(context.Context, uuid.UUID) error

func (f DispatchFunc) DispatchReplay(ctx context.Context, runID uuid.UUID) error {
	return f(ctx, runID)
}

type Option func(*Constructor)

func WithSecretResolver(resolver secret.Resolver) Option {
	return func(c *Constructor) {
		c.secretResolver = resolver
	}
}

type Constructor struct {
	store          *run.Store
	dispatcher     Dispatcher
	secretResolver secret.Resolver
	now            func() time.Time
}

func New(store *run.Store, dispatcher Dispatcher, opts ...Option) *Constructor {
	c := &Constructor{
		store:      store,
		dispatcher: dispatcher,
		now:        func() time.Time { return time.Now().UTC() },
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

type Request struct {
	BaselineRunID     uuid.UUID
	Set               map[string]string
	ReplayFingerprint string
}

type Result struct {
	Run       *run.JobRun
	Decisions []TaskDecision
}

// PreparedReplay is a validated replay plan that has not yet been materialized.
type PreparedReplay struct {
	baseline    models.JobRun
	params      map[string]string
	overrides   map[string]string
	fingerprint string
	plans       []plannedTask
}

// RequiresDispatch reports whether this replay has tasks that must re-execute.
func (p *PreparedReplay) RequiresDispatch() bool {
	if p == nil {
		return false
	}
	return hasPending(p.plans)
}

type TaskDecision struct {
	TaskID       uuid.UUID
	TaskName     string
	BaselineHash string
	ReplayHash   string
	CacheHit     bool
	Reexecute    bool
}

type baselineTask struct {
	row          models.TaskRun
	descriptor   models.TaskExecutionDescriptor
	taskName     string
	output       map[string]string
	branches     []string
	computedHash string
	effective    string
}

type plannedTask struct {
	base          *baselineTask
	replayHash    string
	effectiveHash string
	cacheHit      bool
	reexecute     bool
	source        cacheSource
	outstanding   int
	descriptor    models.TaskExecutionDescriptor
}

type cacheSource struct {
	runID     uuid.UUID
	createdAt time.Time
	expiresAt *time.Time
	result    string
	output    map[string]string
	branches  []string
}

func (c *Constructor) Replay(ctx context.Context, req Request) (*Result, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("replay: run store is required")
	}
	if c.dispatcher == nil {
		return nil, ErrDispatchRequired
	}

	prepared, err := c.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.Materialize(ctx, prepared)
}

// Prepare validates the baseline and computes replay task decisions without
// inserting the replay run. Callers may inspect the plan before materialization.
func (c *Constructor) Prepare(ctx context.Context, req Request) (*PreparedReplay, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("replay: run store is required")
	}

	baseline, tasks, err := c.loadBaseline(ctx, req.BaselineRunID)
	if err != nil {
		return nil, err
	}
	if baseline.Status != string(run.StatusSucceeded) && baseline.Status != string(run.StatusFailed) {
		return nil, fmt.Errorf("%w: %s", ErrBaselineNotTerminal, baseline.Status)
	}

	baseParams, err := decodeParams(baseline.Params)
	if err != nil {
		return nil, err
	}
	replayParams := maps.Clone(baseParams)
	if replayParams == nil {
		replayParams = make(map[string]string)
	}
	paramsChanged := false
	for k, v := range req.Set {
		if replayParams[k] != v {
			paramsChanged = true
		}
		replayParams[k] = v
	}

	plans, err := c.planTasks(ctx, tasks, replayParams, paramsChanged)
	if err != nil {
		return nil, err
	}

	return &PreparedReplay{
		baseline:    baseline,
		params:      replayParams,
		overrides:   maps.Clone(req.Set),
		fingerprint: req.ReplayFingerprint,
		plans:       plans,
	}, nil
}

// Materialize commits a prepared replay run and dispatches pending replay work.
func (c *Constructor) Materialize(ctx context.Context, prepared *PreparedReplay) (*Result, error) {
	if c == nil || c.store == nil {
		return nil, errors.New("replay: run store is required")
	}
	if c.dispatcher == nil {
		return nil, ErrDispatchRequired
	}
	if prepared == nil {
		return nil, errors.New("replay: prepared replay is required")
	}

	runID, err := c.materialize(ctx, prepared.baseline, prepared.params, prepared.overrides, prepared.fingerprint, prepared.plans)
	if err != nil {
		return nil, err
	}

	if hasPending(prepared.plans) {
		if err := c.dispatcher.DispatchReplay(ctx, runID); err != nil {
			return nil, err
		}
	}

	created, err := c.store.Get(runID)
	if err != nil {
		return nil, err
	}
	return &Result{Run: created, Decisions: decisions(prepared.plans)}, nil
}

func (c *Constructor) loadBaseline(ctx context.Context, runID uuid.UUID) (models.JobRun, []*baselineTask, error) {
	var baseline models.JobRun
	if err := c.store.DB().WithContext(ctx).First(&baseline, "id = ?", runID).Error; err != nil {
		return models.JobRun{}, nil, err
	}
	if baseline.Quarantine {
		return models.JobRun{}, nil, fmt.Errorf("%w: run %s", ErrQuarantinedBaseline, runID)
	}

	var rows []models.TaskRun
	if err := c.store.DB().WithContext(ctx).
		Where("job_run_id = ?", runID).
		Order("created_at ASC").
		Find(&rows).Error; err != nil {
		return models.JobRun{}, nil, err
	}
	if len(rows) == 0 {
		return models.JobRun{}, nil, fmt.Errorf("%w: baseline run %s has no task runs", ErrUnavailableBaselineProof, runID)
	}

	tasks := make([]*baselineTask, 0, len(rows))
	for i := range rows {
		task, err := decodeBaselineTask(rows[i])
		if err != nil {
			return models.JobRun{}, nil, err
		}
		tasks = append(tasks, task)
	}
	ordered, err := topologicalBaselineTasks(runID, tasks)
	if err != nil {
		return models.JobRun{}, nil, err
	}
	return baseline, ordered, nil
}

func decodeBaselineTask(row models.TaskRun) (*baselineTask, error) {
	if len(row.ExecutionDescriptor) == 0 {
		return nil, fmt.Errorf("%w: step %q task %s", ErrMissingDescriptor, fallbackTaskName(row.TaskID, ""), row.TaskID)
	}
	var desc models.TaskExecutionDescriptor
	if err := json.Unmarshal(row.ExecutionDescriptor, &desc); err != nil {
		return nil, fmt.Errorf("%w: step %q task %s: %v", ErrMissingDescriptor, fallbackTaskName(row.TaskID, ""), row.TaskID, err)
	}
	if desc.SchemaVersion != models.TaskExecutionDescriptorSchemaVersion {
		return nil, fmt.Errorf("%w: step %q descriptor version %d", ErrUnsupportedDescriptor, fallbackTaskName(row.TaskID, desc.Baseline.TaskName), desc.SchemaVersion)
	}
	if desc.Baseline.TaskID != uuid.Nil && desc.Baseline.TaskID != row.TaskID {
		return nil, fmt.Errorf("%w: descriptor task id %s does not match task_run %s", ErrUnsupportedDescriptor, desc.Baseline.TaskID, row.TaskID)
	}
	if desc.Baseline.BaselineRunID != uuid.Nil && desc.Baseline.BaselineRunID != row.JobRunID {
		return nil, fmt.Errorf("%w: descriptor baseline run %s does not match task_run run %s", ErrUnsupportedDescriptor, desc.Baseline.BaselineRunID, row.JobRunID)
	}
	if desc.Baseline.ReplaySafe != row.ReplaySafe {
		return nil, fmt.Errorf("%w: descriptor replay_safe=%t does not match task_run replay_safe=%t for step %q", ErrUnsupportedDescriptor, desc.Baseline.ReplaySafe, row.ReplaySafe, fallbackTaskName(row.TaskID, desc.Baseline.TaskName))
	}

	out, err := decodeStringMap(row.Output)
	if err != nil {
		return nil, fmt.Errorf("replay: decode baseline output for step %q: %w", fallbackTaskName(row.TaskID, desc.Baseline.TaskName), err)
	}
	branches, err := decodeStringSlice(row.BranchSelections)
	if err != nil {
		return nil, fmt.Errorf("replay: decode baseline branch selections for step %q: %w", fallbackTaskName(row.TaskID, desc.Baseline.TaskName), err)
	}
	computed := firstNonEmpty(desc.Cache.ComputedHash, desc.Baseline.ComputedHash, row.Hash)
	effective := firstNonEmpty(desc.Cache.EffectiveHash, desc.Baseline.EffectiveHash, row.EffectiveHash, computed)

	return &baselineTask{
		row:          row,
		descriptor:   desc,
		taskName:     fallbackTaskName(row.TaskID, desc.Baseline.TaskName),
		output:       out,
		branches:     branches,
		computedHash: computed,
		effective:    effective,
	}, nil
}

func topologicalBaselineTasks(runID uuid.UUID, tasks []*baselineTask) ([]*baselineTask, error) {
	byID := make(map[uuid.UUID]*baselineTask, len(tasks))
	indegree := make(map[uuid.UUID]int, len(tasks))
	successors := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))
	edges := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))

	for _, task := range tasks {
		if task == nil {
			return nil, fmt.Errorf("%w: nil baseline task in run %s", ErrUnsupportedDescriptor, runID)
		}
		taskID := task.row.TaskID
		if taskID == uuid.Nil {
			return nil, fmt.Errorf("%w: step %q has empty task id", ErrUnsupportedDescriptor, task.taskName)
		}
		if _, exists := byID[taskID]; exists {
			return nil, fmt.Errorf("%w: duplicate task %s in baseline run %s", ErrUnsupportedDescriptor, taskID, runID)
		}
		byID[taskID] = task
		indegree[taskID] = 0
	}

	addEdge := func(from, to uuid.UUID, stepName, relation string) error {
		if from == uuid.Nil || to == uuid.Nil {
			return fmt.Errorf("%w: step %q has empty %s edge", ErrUnsupportedDescriptor, stepName, relation)
		}
		if _, ok := byID[from]; !ok {
			return fmt.Errorf("%w: step %q references missing %s %s", ErrUnsupportedDescriptor, stepName, relation, from)
		}
		if _, ok := byID[to]; !ok {
			return fmt.Errorf("%w: step %q references missing %s %s", ErrUnsupportedDescriptor, stepName, relation, to)
		}
		if edges[from] == nil {
			edges[from] = make(map[uuid.UUID]struct{})
		}
		if _, exists := edges[from][to]; exists {
			return nil
		}
		edges[from][to] = struct{}{}
		if successors[from] == nil {
			successors[from] = make(map[uuid.UUID]struct{})
		}
		successors[from][to] = struct{}{}
		indegree[to]++
		return nil
	}

	for _, task := range tasks {
		taskID := task.row.TaskID
		for _, pred := range task.descriptor.DAG.Predecessors {
			if err := addEdge(pred.TaskID, taskID, task.taskName, "predecessor"); err != nil {
				return nil, err
			}
		}
		for _, succ := range task.descriptor.DAG.Successors {
			if err := addEdge(taskID, succ.TaskID, task.taskName, "successor"); err != nil {
				return nil, err
			}
		}
	}

	ready := make([]*baselineTask, 0, len(tasks))
	for _, task := range tasks {
		if indegree[task.row.TaskID] == 0 {
			ready = append(ready, task)
		}
	}
	sortBaselineReady(ready)

	ordered := make([]*baselineTask, 0, len(tasks))
	for len(ready) > 0 {
		task := ready[0]
		ready = ready[1:]
		ordered = append(ordered, task)
		for successorID := range successors[task.row.TaskID] {
			indegree[successorID]--
			if indegree[successorID] == 0 {
				ready = append(ready, byID[successorID])
			}
		}
		sortBaselineReady(ready)
	}

	if len(ordered) != len(tasks) {
		return nil, fmt.Errorf("%w: descriptor DAG cycle while ordering baseline run %s", ErrUnsupportedDescriptor, runID)
	}
	return ordered, nil
}

func sortBaselineReady(tasks []*baselineTask) {
	sort.SliceStable(tasks, func(i, j int) bool {
		if tasks[i].descriptor.DAG.TaskPosition != tasks[j].descriptor.DAG.TaskPosition {
			return tasks[i].descriptor.DAG.TaskPosition < tasks[j].descriptor.DAG.TaskPosition
		}
		if !tasks[i].row.CreatedAt.Equal(tasks[j].row.CreatedAt) {
			return tasks[i].row.CreatedAt.Before(tasks[j].row.CreatedAt)
		}
		if tasks[i].taskName != tasks[j].taskName {
			return tasks[i].taskName < tasks[j].taskName
		}
		return tasks[i].row.TaskID.String() < tasks[j].row.TaskID.String()
	})
}

func (c *Constructor) planTasks(ctx context.Context, tasks []*baselineTask, params map[string]string, forceReexecute bool) ([]plannedTask, error) {
	plannedByID := make(map[uuid.UUID]int, len(tasks))

	plans := make([]plannedTask, 0, len(tasks))
	for _, task := range tasks {
		predOutputsByName := make(map[string]map[string]string)
		predHashes := make([]string, 0, len(task.descriptor.DAG.Predecessors))
		pendingPredecessors := 0
		for _, pred := range task.descriptor.DAG.Predecessors {
			plannedIdx, ok := plannedByID[pred.TaskID]
			if !ok {
				return nil, fmt.Errorf("%w: step %q references missing predecessor %s", ErrUnsupportedDescriptor, task.taskName, pred.TaskID)
			}
			plannedPred := &plans[plannedIdx]
			if plannedPred.reexecute {
				pendingPredecessors++
				continue
			}
			if len(plannedPred.source.output) > 0 {
				predName := firstNonEmpty(pred.TaskName, plannedPred.base.taskName, pred.TaskID.String())
				predOutputsByName[predName] = maps.Clone(plannedPred.source.output)
			}
			if plannedPred.effectiveHash != "" {
				predHashes = append(predHashes, plannedPred.effectiveHash)
			}
		}

		replayHash := computeDescriptorHash(task.descriptor, params, predOutputsByName, predHashes)
		unchanged := !forceReexecute && hashMatchesBaseline(replayHash, task.computedHash, task.effective)
		plan := plannedTask{
			base:          task,
			replayHash:    replayHash,
			effectiveHash: replayHash,
			// The replay TaskRun stores the baseline descriptor unchanged; its
			// Baseline fields are the audit reference for what was replayed.
			descriptor: task.descriptor,
		}

		if unchanged && pendingPredecessors == 0 {
			source, err := c.cacheSourceForUnchanged(ctx, task, replayHash)
			if err != nil {
				return nil, err
			}
			plan.cacheHit = true
			plan.source = source
			plan.effectiveHash = firstNonEmpty(task.effective, replayHash)
		} else {
			if err := c.authorizeReexecution(ctx, task); err != nil {
				return nil, err
			}
			plan.reexecute = true
			plan.outstanding = pendingPredecessors
		}
		plans = append(plans, plan)
		plannedByID[task.row.TaskID] = len(plans) - 1
	}
	return plans, nil
}

func computeDescriptorHash(desc models.TaskExecutionDescriptor, params map[string]string, predOutputs map[string]map[string]string, predHashes []string) string {
	spec := desc.ContainerSpec
	env := maps.Clone(spec.Env)
	if env == nil {
		env = make(map[string]string)
	}
	for k, v := range pkgtask.BuildOutputEnv(predOutputs) {
		env[k] = v
	}
	command := append([]string(nil), desc.Runtime.Command...)
	if len(command) == 0 && strings.TrimSpace(desc.Runtime.CommandRaw) != "" {
		command = []string{desc.Runtime.CommandRaw}
	}
	workdir := spec.WorkDir
	if workdir == "" {
		workdir = desc.Runtime.WorkDir
	}
	return cache.HashInput{
		JobAlias:             desc.Baseline.JobAlias,
		TaskName:             desc.Baseline.TaskName,
		Image:                desc.Runtime.Image,
		ResolvedImageDigest:  desc.Runtime.ResolvedImageDigest,
		Command:              command,
		Env:                  env,
		WorkDir:              workdir,
		Mounts:               spec.Mounts,
		ResolvedVolumeMounts: spec.ResolvedVolumeMounts,
		Kubernetes:           spec.Kubernetes,
		PredecessorHashes:    append([]string(nil), predHashes...),
		PredecessorOutputs:   cloneNestedStringMap(predOutputs),
		RunParams:            maps.Clone(params),
		CacheVersion:         desc.Cache.Version,
	}.Compute()
}

func (c *Constructor) cacheSourceForUnchanged(ctx context.Context, task *baselineTask, replayHash string) (cacheSource, error) {
	if strings.TrimSpace(task.row.Result) == "" && len(task.output) > 0 {
		return cacheSource{}, fmt.Errorf("%w: step %q produced baseline output but recorded an empty result (corruption)", ErrUnavailableBaselineProof, task.taskName)
	}
	status := run.TaskStatus(task.row.Status)
	if status != run.TaskStatusSucceeded && status != run.TaskStatusCached {
		return cacheSource{}, fmt.Errorf("%w: step %q baseline status %q is not reusable", ErrUnavailableBaselineProof, task.taskName, task.row.Status)
	}
	if !run.IsSuccessfulTaskResult(task.row.Result) {
		return cacheSource{}, fmt.Errorf("%w: step %q baseline result %q is not successful", ErrUnavailableBaselineProof, task.taskName, task.row.Result)
	}

	cacheStore := cache.NewStore(c.store.DB())
	if entry, found, err := cacheStore.Get(replayHash); err != nil {
		return cacheSource{}, fmt.Errorf("replay: cache lookup for unchanged step %q hash %s: %w", task.taskName, shortHash(replayHash), err)
	} else if found {
		if !run.IsSuccessfulTaskResult(entry.Result) {
			return cacheSource{}, fmt.Errorf("%w: step %q hash %s cache entry has non-success result %q", ErrUnavailableBaselineProof, task.taskName, shortHash(replayHash), entry.Result)
		}
		return cacheSource{
			runID:     entry.RunID,
			createdAt: entry.CreatedAt,
			expiresAt: entry.ExpiresAt,
			result:    entry.Result,
			output:    maps.Clone(entry.Output),
			branches:  append([]string(nil), entry.BranchSelections...),
		}, nil
	}

	if strings.TrimSpace(task.row.Result) != "" {
		created := task.row.CreatedAt
		if task.row.CompletedAt != nil {
			created = *task.row.CompletedAt
		}
		return cacheSource{
			runID:     task.row.JobRunID,
			createdAt: created,
			result:    task.row.Result,
			output:    maps.Clone(task.output),
			branches:  append([]string(nil), task.branches...),
		}, nil
	}
	return cacheSource{}, fmt.Errorf("%w: step %q hash %s has neither a live TaskCache entry nor recorded baseline result/output", ErrUnavailableBaselineProof, task.taskName, shortHash(replayHash))
}

func (c *Constructor) authorizeReexecution(ctx context.Context, task *baselineTask) error {
	if !task.row.ReplaySafe {
		return fmt.Errorf("%w: step %q would re-execute but baseline task_run replay_safe=false", ErrReplayUnsafe, task.taskName)
	}

	expectedByKey, err := ExpectedReplaySecretRefMap(task.descriptor.SecretRefs)
	if err != nil {
		return fmt.Errorf("%w: step %q: %v", ErrSecretIdentity, task.taskName, err)
	}
	seenRuntimeRefs := make(map[string]struct{}, len(expectedByKey))
	for envKey, rawRef := range task.descriptor.ContainerSpec.Env {
		refValue := strings.TrimSpace(rawRef)
		if !strings.HasPrefix(refValue, "secret://") {
			continue
		}
		key := ReplaySecretRefKey(envKey, refValue)
		ref, ok := expectedByKey[key]
		if !ok {
			return fmt.Errorf("%w: step %q env %s secret %s has no baseline identity", ErrSecretIdentity, task.taskName, envKey, refValue)
		}
		seenRuntimeRefs[key] = struct{}{}
		if err := c.verifyReplaySecretIdentity(ctx, task.taskName, ref); err != nil {
			return err
		}
	}
	for key, ref := range expectedByKey {
		if _, ok := seenRuntimeRefs[key]; !ok {
			return fmt.Errorf("%w: step %q baseline secret %s for env %s is absent from the captured runtime spec", ErrSecretIdentity, task.taskName, ref.Ref, ref.EnvKey)
		}
	}
	return nil
}

func (c *Constructor) verifyReplaySecretIdentity(ctx context.Context, taskName string, ref models.TaskExecutionSecretRef) error {
	if strings.TrimSpace(ref.Ref) == "" {
		return nil
	}
	if !ref.Verifiable {
		return fmt.Errorf("%w: step %q secret %s is not verifiable: %s", ErrSecretIdentity, taskName, ref.Ref, ref.UnverifiableReason)
	}
	if c.secretResolver == nil {
		return fmt.Errorf("%w: step %q secret %s requires a configured resolver", ErrSecretIdentity, taskName, ref.Ref)
	}
	value, identity, err := c.secretResolver.ResolveWithIdentity(ctx, ref.Ref)
	if err != nil {
		return fmt.Errorf("%w: step %q secret %s re-resolve failed: %v", ErrSecretIdentity, taskName, ref.Ref, err)
	}
	if err := VerifyResolvedReplaySecretIdentity(ctx, c.secretResolver, ref, value, identity); err != nil {
		return fmt.Errorf("%w: step %q secret %s %v", ErrSecretIdentity, taskName, ref.Ref, err)
	}
	return nil
}

// VerifyReplaySecretIdentities verifies descriptor-captured replay secret
// identities against the identities and values resolved for a quarantined task.
func VerifyReplaySecretIdentities(ctx context.Context, resolver secret.Resolver, expected []models.TaskExecutionSecretRef, actual []jobdefruntime.ResolvedSecretIdentity, resolvedEnv map[string]string) error {
	expectedByKey, err := ExpectedReplaySecretRefMap(expected)
	if err != nil {
		return err
	}
	actualByKey := make(map[string]jobdefruntime.ResolvedSecretIdentity, len(actual))
	for _, resolved := range actual {
		key := ReplaySecretRefKey(resolved.EnvKey, resolved.Ref)
		ref, ok := expectedByKey[key]
		if !ok {
			return fmt.Errorf("replay secret %s for env %s has no baseline identity", resolved.Ref, resolved.EnvKey)
		}
		actualByKey[key] = resolved
		value, ok := resolvedEnv[resolved.EnvKey]
		if !ok {
			return fmt.Errorf("replay secret %s for env %s resolved value unavailable", ref.Ref, ref.EnvKey)
		}
		if err := VerifyResolvedReplaySecretIdentity(ctx, resolver, ref, value, resolved.Identity); err != nil {
			return fmt.Errorf("replay secret %s for env %s %v", ref.Ref, ref.EnvKey, err)
		}
	}
	for _, ref := range expected {
		if strings.TrimSpace(ref.Ref) == "" {
			continue
		}
		_, ok := actualByKey[ReplaySecretRefKey(ref.EnvKey, ref.Ref)]
		if !ok {
			return fmt.Errorf("replay secret %s for env %s was not resolved", ref.Ref, ref.EnvKey)
		}
	}
	return nil
}

// ExpectedReplaySecretRefMap keys descriptor secret refs by env key and ref.
func ExpectedReplaySecretRefMap(refs []models.TaskExecutionSecretRef) (map[string]models.TaskExecutionSecretRef, error) {
	expected := make(map[string]models.TaskExecutionSecretRef)
	for _, ref := range refs {
		if strings.TrimSpace(ref.Ref) == "" {
			continue
		}
		key := ReplaySecretRefKey(ref.EnvKey, ref.Ref)
		if _, exists := expected[key]; exists {
			return nil, fmt.Errorf("duplicate replay secret identity for env %s ref %s", ref.EnvKey, ref.Ref)
		}
		expected[key] = ref
	}
	return expected, nil
}

// ReplaySecretRefKey returns the canonical map key for an env secret ref.
func ReplaySecretRefKey(envKey, ref string) string {
	return strings.TrimSpace(envKey) + "\x00" + strings.TrimSpace(ref)
}

// VerifyResolvedReplaySecretIdentity verifies one descriptor secret identity
// against the value and identity returned by the resolver.
func VerifyResolvedReplaySecretIdentity(ctx context.Context, resolver secret.Resolver, ref models.TaskExecutionSecretRef, resolvedValue string, identity secret.Identity) error {
	if !ref.Verifiable {
		return fmt.Errorf("is not verifiable: %s", ref.UnverifiableReason)
	}
	if !identity.Verifiable {
		return fmt.Errorf("re-resolved identity is not verifiable: %s", identity.UnverifiableReason)
	}
	if ref.Provider != "" && ref.Provider != identity.Provider {
		return fmt.Errorf("provider changed from %s to %s", ref.Provider, identity.Provider)
	}
	if ReplaySecretRequiresPinnedVaultVerification(ref) {
		if ReplayDescriptorIdentityString(ref, "version") != identity.Version {
			return fmt.Errorf("version changed from %s to %s", ReplayDescriptorIdentityString(ref, "version"), identity.Version)
		}
		verifier, ok := resolver.(secret.ResolvedIdentityVerifier)
		if !ok {
			return fmt.Errorf("provider %s does not support baseline identity verification", ref.Provider)
		}
		pinned, err := verifier.VerifyResolvedIdentity(ctx, ref.Ref, ReplaySecretIdentityFromDescriptor(ref), resolvedValue)
		if err != nil {
			return fmt.Errorf("baseline identity verification failed: %w", err)
		}
		if !pinned.Verifiable {
			return fmt.Errorf("baseline identity is not verifiable: %s", pinned.UnverifiableReason)
		}
		if !ReplaySecretIdentityMatches(ref, pinned) {
			return fmt.Errorf("baseline identity changed")
		}
		return nil
	}
	if !ReplaySecretIdentityMatches(ref, identity) {
		return fmt.Errorf("identity changed")
	}
	return nil
}

// ReplaySecretRequiresPinnedVaultVerification reports whether a descriptor ref
// carries the Vault version and HMAC key needed for baseline identity pinning.
func ReplaySecretRequiresPinnedVaultVerification(ref models.TaskExecutionSecretRef) bool {
	return strings.EqualFold(ref.Provider, "vault") &&
		ReplayDescriptorIdentityString(ref, "version") != "" &&
		ReplayDescriptorIdentityString(ref, "keyId") != ""
}

// ReplaySecretIdentityFromDescriptor converts a descriptor secret ref back to
// the captured identity shape used for verification.
func ReplaySecretIdentityFromDescriptor(ref models.TaskExecutionSecretRef) secret.Identity {
	return secret.Identity{
		Provider:        ref.Provider,
		Ref:             ref.Ref,
		Version:         ReplayDescriptorIdentityString(ref, "version"),
		ResourceVersion: ReplayDescriptorIdentityString(ref, "resourceVersion"),
		Namespace:       ReplayDescriptorIdentityString(ref, "namespace"),
		Name:            ReplayDescriptorIdentityString(ref, "name"),
		Key:             ReplayDescriptorIdentityString(ref, "key"),
		KeyID:           ReplayDescriptorIdentityString(ref, "keyId"),
		HMACSHA256:      ReplayDescriptorIdentityString(ref, "hmacSha256"),
		Verifiable:      ref.Verifiable,
	}
}

func (c *Constructor) materialize(ctx context.Context, baseline models.JobRun, params, overrides map[string]string, fingerprint string, plans []plannedTask) (uuid.UUID, error) {
	now := c.now()
	replayID := uuid.New()
	var fingerprintPtr *string
	if strings.TrimSpace(fingerprint) != "" {
		value := strings.TrimSpace(fingerprint)
		fingerprintPtr = &value
	}

	encodedParams, err := json.Marshal(params)
	if err != nil {
		return uuid.Nil, fmt.Errorf("replay: encode replay params: %w", err)
	}
	encodedOverrides, err := json.Marshal(overrides)
	if err != nil {
		return uuid.Nil, fmt.Errorf("replay: encode replay overrides: %w", err)
	}

	records := make([]models.TaskRun, 0, len(plans))
	allCached := true
	for _, plan := range plans {
		record, err := taskRunRecord(replayID, plan, now)
		if err != nil {
			return uuid.Nil, err
		}
		if plan.reexecute {
			allCached = false
		} else if plan.cacheHit && !run.IsSuccessfulTaskResult(plan.source.result) {
			return uuid.Nil, fmt.Errorf("%w: step %q cache hit result %q is not successful", ErrUnavailableBaselineProof, plan.base.taskName, plan.source.result)
		}
		records = append(records, record)
	}

	var pendingEvents []event.Event
	err = c.store.DB().WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		status := string(run.StatusRunning)
		var completedAt *time.Time
		if allCached {
			status = string(run.StatusSucceeded)
			completedAt = &now
		}
		model := models.JobRun{
			ID:                replayID,
			JobID:             baseline.JobID,
			Status:            status,
			Params:            datatypes.JSON(encodedParams),
			Quarantine:        true,
			ReplayFingerprint: fingerprintPtr,
			ReplayOverrides:   datatypes.JSON(encodedOverrides),
			TriggerType:       "replay",
			TriggerAlias:      "quarantined-replay",
			StartedAt:         now,
			CompletedAt:       completedAt,
			CreatedAt:         now,
			UpdatedAt:         now,
		}
		if err := tx.Create(&model).Error; err != nil {
			return err
		}
		if len(records) > 0 {
			if err := tx.Create(&records).Error; err != nil {
				return err
			}
		}
		if es := c.store.EventStore(); es != nil {
			events := replayEvents(baseline.JobID, replayID, records, allCached, now)
			for i := range events {
				if err := es.AppendTx(tx, &events[i]); err != nil {
					return err
				}
			}
			pendingEvents = events
		}
		return nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	c.store.PublishEvents(pendingEvents...)
	return replayID, nil
}

func taskRunRecord(replayID uuid.UUID, plan plannedTask, now time.Time) (models.TaskRun, error) {
	desc := plan.descriptor
	encodedDescriptor, err := json.Marshal(&desc)
	if err != nil {
		return models.TaskRun{}, fmt.Errorf("replay: encode descriptor for step %q: %w", plan.base.taskName, err)
	}
	command, err := encodeCommand(desc.Runtime.Command, desc.Runtime.CommandRaw)
	if err != nil {
		return models.TaskRun{}, fmt.Errorf("replay: encode command for step %q: %w", plan.base.taskName, err)
	}

	maxAttempts := desc.Runtime.RetryCount + 1
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	record := models.TaskRun{
		ID:                      uuid.New(),
		JobRunID:                replayID,
		TaskID:                  plan.base.row.TaskID,
		AtomID:                  desc.Baseline.AtomID,
		Engine:                  desc.Runtime.Engine,
		Image:                   desc.Runtime.Image,
		Command:                 command,
		Status:                  string(run.TaskStatusPending),
		NodeSelector:            datatypes.JSONMap(stringMapToAny(desc.Runtime.NodeSelector)),
		Attempt:                 1,
		MaxAttempts:             maxAttempts,
		Hash:                    plan.replayHash,
		OutstandingPredecessors: plan.outstanding,
		Quarantine:              true,
		CacheEnabled:            desc.Cache.Enabled,
		CacheTTL:                desc.Cache.TTL,
		CacheVersion:            desc.Cache.Version,
		ReplaySafe:              plan.base.row.ReplaySafe,
		CachePinDigests:         desc.Cache.PinDigests,
		CacheDigestTTL:          desc.Cache.DigestTTL,
		ResolvedImageDigest:     desc.Runtime.ResolvedImageDigest,
		OutputSchema:            append(datatypes.JSON(nil), desc.Schema.OutputSchema...),
		SchemaValidation:        desc.Schema.ValidationMode,
		ExecutionDescriptor:     datatypes.JSON(encodedDescriptor),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if plan.cacheHit {
		if !run.IsSuccessfulTaskResult(plan.source.result) {
			return models.TaskRun{}, fmt.Errorf("%w: step %q cache hit result %q is not successful", ErrUnavailableBaselineProof, plan.base.taskName, plan.source.result)
		}
		record.Status = string(run.TaskStatusCached)
		record.CompletedAt = &now
		record.Result = plan.source.result
		record.CacheHit = true
		origin := plan.source.runID
		record.CacheOriginRunID = &origin
		record.CacheCreatedAt = &plan.source.createdAt
		record.CacheExpiresAt = plan.source.expiresAt
		if len(plan.source.output) > 0 {
			encoded, err := json.Marshal(plan.source.output)
			if err != nil {
				return models.TaskRun{}, fmt.Errorf("replay: encode cache output for step %q: %w", plan.base.taskName, err)
			}
			record.Output = datatypes.JSON(encoded)
		}
		if len(plan.source.branches) > 0 {
			encoded, err := json.Marshal(plan.source.branches)
			if err != nil {
				return models.TaskRun{}, fmt.Errorf("replay: encode branch selections for step %q: %w", plan.base.taskName, err)
			}
			record.BranchSelections = datatypes.JSON(encoded)
		}
	}
	return record, nil
}

func replayEvents(jobID, runID uuid.UUID, records []models.TaskRun, allCached bool, now time.Time) []event.Event {
	events := []event.Event{{
		Type:       event.TypeRunStarted,
		JobID:      jobID,
		RunID:      runID,
		Timestamp:  now,
		Quarantine: true,
	}}
	for _, record := range records {
		switch run.TaskStatus(record.Status) {
		case run.TaskStatusCached:
			events = append(events, event.Event{
				Type:       event.TypeTaskCached,
				JobID:      jobID,
				RunID:      runID,
				TaskID:     record.TaskID,
				Timestamp:  now,
				Quarantine: true,
			})
		case run.TaskStatusPending:
			if record.OutstandingPredecessors == 0 {
				events = append(events, event.Event{
					Type:       event.TypeTaskReady,
					JobID:      jobID,
					RunID:      runID,
					TaskID:     record.TaskID,
					Timestamp:  now,
					Quarantine: true,
				})
			}
		}
	}
	if allCached {
		events = append(events, event.Event{
			Type:       event.TypeRunCompleted,
			JobID:      jobID,
			RunID:      runID,
			Timestamp:  now,
			Quarantine: true,
		})
	}
	return events
}

func decisions(plans []plannedTask) []TaskDecision {
	out := make([]TaskDecision, 0, len(plans))
	for _, plan := range plans {
		out = append(out, TaskDecision{
			TaskID:       plan.base.row.TaskID,
			TaskName:     plan.base.taskName,
			BaselineHash: plan.base.computedHash,
			ReplayHash:   plan.replayHash,
			CacheHit:     plan.cacheHit,
			Reexecute:    plan.reexecute,
		})
	}
	return out
}

func hasPending(plans []plannedTask) bool {
	for _, plan := range plans {
		if plan.reexecute {
			return true
		}
	}
	return false
}

func hashMatchesBaseline(replayHash, computed, effective string) bool {
	return replayHash != "" && (replayHash == computed || (effective != "" && replayHash == effective))
}

// ReplaySecretIdentityMatches compares a descriptor ref's recorded identity
// fields with a resolved or verified identity.
func ReplaySecretIdentityMatches(ref models.TaskExecutionSecretRef, identity secret.Identity) bool {
	if ref.Provider != "" && ref.Provider != identity.Provider {
		return false
	}
	if !ReplaySecretHasRequiredDiscriminator(ref) {
		return false
	}
	expected := ref.Identity
	actual := ReplaySecretIdentityMap(identity)
	for key, expectedValue := range expected {
		if fmt.Sprint(expectedValue) != fmt.Sprint(actual[key]) {
			return false
		}
	}
	return true
}

// ReplaySecretHasRequiredDiscriminator confirms a descriptor ref has enough
// provider-specific identity material to fail closed on mismatch.
func ReplaySecretHasRequiredDiscriminator(ref models.TaskExecutionSecretRef) bool {
	if len(ref.Identity) == 0 {
		return false
	}
	if strings.EqualFold(ref.Provider, "vault") {
		return ReplayDescriptorIdentityString(ref, "version") != "" &&
			ReplayDescriptorIdentityString(ref, "keyId") != "" &&
			ReplayDescriptorIdentityString(ref, "hmacSha256") != ""
	}
	for _, value := range ref.Identity {
		if strings.TrimSpace(fmt.Sprint(value)) != "" {
			return true
		}
	}
	return false
}

// ReplayDescriptorIdentityString returns a trimmed string identity field from a
// descriptor ref.
func ReplayDescriptorIdentityString(ref models.TaskExecutionSecretRef, key string) string {
	if len(ref.Identity) == 0 {
		return ""
	}
	value, ok := ref.Identity[key]
	if !ok || value == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(value))
}

// ReplaySecretIdentityMap returns the descriptor-comparable fields for an
// identity.
func ReplaySecretIdentityMap(identity secret.Identity) datatypes.JSONMap {
	out := datatypes.JSONMap{}
	if identity.Version != "" {
		out["version"] = identity.Version
	}
	if identity.ResourceVersion != "" {
		out["resourceVersion"] = identity.ResourceVersion
	}
	if identity.Namespace != "" {
		out["namespace"] = identity.Namespace
	}
	if identity.Name != "" {
		out["name"] = identity.Name
	}
	if identity.Key != "" {
		out["key"] = identity.Key
	}
	if identity.KeyID != "" {
		out["keyId"] = identity.KeyID
	}
	if identity.HMACSHA256 != "" {
		out["hmacSha256"] = identity.HMACSHA256
	}
	for k, v := range identity.Metadata {
		out[k] = v
	}
	return out
}

func encodeCommand(command []string, raw string) (string, error) {
	if len(command) > 0 {
		encoded, err := json.Marshal(command)
		if err != nil {
			return "", err
		}
		return string(encoded), nil
	}
	return raw, nil
}

func decodeParams(raw datatypes.JSON) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var params map[string]string
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("replay: decode baseline params: %w", err)
	}
	return params, nil
}

func decodeStringMap(raw datatypes.JSON) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeStringSlice(raw datatypes.JSON) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func cloneNestedStringMap(in map[string]map[string]string) map[string]map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]map[string]string, len(in))
	for k, v := range in {
		out[k] = maps.Clone(v)
	}
	return out
}

func stringMapToAny(in map[string]string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func fallbackTaskName(id uuid.UUID, name string) string {
	if strings.TrimSpace(name) != "" {
		return name
	}
	return id.String()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
