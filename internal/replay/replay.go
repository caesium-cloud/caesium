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
	// ErrFannedBaseline is the fail-closed refusal of a baseline whose fan-out
	// groups cannot be reconstructed. It no longer means "the baseline is
	// fanned": replay re-expands a group from the partition list frozen on its
	// producer's descriptor, so this fires only when that list is absent (a
	// baseline recorded before descriptors captured it), disagrees with the
	// instances the baseline actually materialized, or is not a valid group.
	ErrFannedBaseline = errors.New("replay: fan-out group cannot be re-expanded from the recorded baseline")
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

// TaskDecision is one planned row's outcome. A fanned group contributes ONE
// decision per instance, all sharing a TaskID — Partition is what tells them
// apart, and it is empty for an ordinary task.
type TaskDecision struct {
	TaskID       uuid.UUID
	TaskName     string
	Partition    string
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

	// fanned reports that this baseline row is one instance of a fan-out group.
	fanned bool
	// partition is the instance's identity, taken from the list RECORDED on the
	// producer's descriptor rather than re-read off the row, because that
	// recorded list is what the replay group is re-expanded from. The two are
	// cross-checked in groupBaselineTasks before either is trusted.
	partition      pkgtask.Partition
	partitionIndex int
	partitionCount int
}

// baselineGroup is one catalog task's baseline rows: exactly one row for an
// ordinary task, N (ordered by partition index) for a fan-out group.
//
// The planner works in groups rather than rows because that is the unit the DAG
// is wired in: a fanned predecessor presents ONE aggregate output and ONE
// aggregate identity downstream, and contributes exactly one outstanding
// predecessor — never N. Only within a group does planning descend to rows.
type baselineGroup struct {
	taskID   uuid.UUID
	taskName string
	tasks    []*baselineTask
	fanned   bool

	// partitions is the list recorded on the PRODUCER's descriptor, which this
	// group is re-expanded from. nil for an unfanned group.
	partitions []pkgtask.Partition
	// dependents is the in-group reverse adjacency (key → keys that wait on it),
	// derived from partitions by the same pkgtask.ValidatePartitionGraph the live
	// expansion uses.
	dependents map[string][]string
	// order lists indices into tasks in in-group TOPOLOGICAL order, so a
	// sibling's cache-hit-or-re-execute decision is known before the decision of
	// anything that waits on it.
	order []int
	// producerFanOut is this group's own producer record — set when this task is
	// a fan-out producer, nil otherwise.
	producerFanOut *models.TaskExecutionFanOut
}

func (g *baselineGroup) descriptor() models.TaskExecutionDescriptor {
	return g.tasks[0].descriptor
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
	// producerPartitions is the list this task emitted as a fan-out PRODUCER,
	// mirrored onto the replay row's partitions column exactly as the live
	// completion and cache-hit paths do. Nil for every non-producer.
	producerPartitions []pkgtask.Partition
}

type cacheSource struct {
	runID     uuid.UUID
	createdAt time.Time
	expiresAt *time.Time
	result    string
	output    map[string]string
	branches  []string
	// partitions is the list recorded on the reused cache ENTRY (F7). nil and
	// partitionsRecorded=false when the source is not a cache entry, or is one
	// written before the column existed; see cache.Entry.Partitions for why the
	// two states must stay distinguishable.
	partitions         []pkgtask.Partition
	partitionsRecorded bool
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

	baseline, groups, err := c.loadBaseline(ctx, req.BaselineRunID)
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

	plans, err := c.planTasks(ctx, groups, replayParams, paramsChanged)
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

func (c *Constructor) loadBaseline(ctx context.Context, runID uuid.UUID) (models.JobRun, []*baselineGroup, error) {
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
		Order("created_at ASC, partition_index ASC").
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
	groups, err := groupBaselineTasks(runID, tasks)
	if err != nil {
		return models.JobRun{}, nil, err
	}
	ordered, err := topologicalBaselineGroups(runID, groups)
	if err != nil {
		return models.JobRun{}, nil, err
	}
	return baseline, ordered, nil
}

// groupBaselineTasks collapses baseline rows into one group per catalog task and
// re-expands every fan-out group from the partition list recorded on its
// PRODUCER's descriptor.
//
// The recorded list — not the instance rows — is the authority, because that is
// the artifact replay is contractually built from: an instance row is live state
// that a retry or a per-partition reset can rewrite, while the descriptor is
// frozen at the moment the producer determined the group. The rows are still
// read, but only to CHECK the recorded list against what the baseline actually
// materialized; any disagreement is a refusal, never a merge.
//
// Everything here fails closed onto ErrFannedBaseline (409). A baseline whose
// descriptors predate fan-out capture is refused exactly as it was before this
// existed, which is what keeps the change backward-compatible for descriptors
// already on disk.
func groupBaselineTasks(runID uuid.UUID, tasks []*baselineTask) ([]*baselineGroup, error) {
	groups := make([]*baselineGroup, 0, len(tasks))
	byTaskID := make(map[uuid.UUID]*baselineGroup, len(tasks))
	for _, task := range tasks {
		taskID := task.row.TaskID
		if taskID == uuid.Nil {
			return nil, fmt.Errorf("%w: step %q has empty task id", ErrUnsupportedDescriptor, task.taskName)
		}
		g, ok := byTaskID[taskID]
		if !ok {
			g = &baselineGroup{taskID: taskID, taskName: task.taskName}
			byTaskID[taskID] = g
			groups = append(groups, g)
		}
		g.tasks = append(g.tasks, task)
		if run.IsFanOutInstance(&task.row) {
			g.fanned = true
		}
	}

	// Producer records first: a group can only be re-expanded once the producer
	// that emitted its list has been seen, and emission order says nothing about
	// which of the two rows the query returned first.
	producerOf := make(map[uuid.UUID]*baselineGroup, len(groups))
	for _, g := range groups {
		fanOut := g.descriptor().FanOut
		if fanOut == nil || !fanOut.PartitionsRecorded {
			continue
		}
		if len(g.tasks) != 1 {
			// Chained fan-out is rejected at job validation
			// (pkg/jobdef/definition.go), so a producer is always exactly one row.
			// N rows here means the descriptor and the materialized run disagree
			// about what this task is.
			return nil, fmt.Errorf("%w: run %s: step %q records a fan-out partition list but materialized %d instances",
				ErrFannedBaseline, runID, g.taskName, len(g.tasks))
		}
		g.producerFanOut = fanOut
		for _, ref := range fanOut.Groups {
			if ref.Skipped {
				continue
			}
			if prior, exists := producerOf[ref.TaskID]; exists {
				return nil, fmt.Errorf("%w: run %s: step %q is claimed as a fan-out group by both %q and %q",
					ErrFannedBaseline, runID, ref.TaskName, prior.taskName, g.taskName)
			}
			producerOf[ref.TaskID] = g
		}
	}

	for _, g := range groups {
		if !g.fanned {
			if len(g.tasks) != 1 {
				return nil, fmt.Errorf("%w: duplicate task %s in baseline run %s", ErrUnsupportedDescriptor, g.taskID, runID)
			}
			g.order = []int{0}
			continue
		}
		producer, ok := producerOf[g.taskID]
		if !ok {
			return nil, fmt.Errorf("%w: run %s: step %q is a fan-out group with no partition list recorded on its producer's descriptor "+
				"(the baseline predates descriptor fan-out capture)", ErrFannedBaseline, runID, g.taskName)
		}
		if err := g.adoptRecordedPartitions(runID, producer); err != nil {
			return nil, err
		}
	}
	return groups, nil
}

// adoptRecordedPartitions binds one fanned group to its producer's recorded
// list, refusing unless the two describe the same group instance for instance.
func (g *baselineGroup) adoptRecordedPartitions(runID uuid.UUID, producer *baselineGroup) error {
	partitions := producer.producerFanOut.Partitions
	if len(partitions) != len(g.tasks) {
		return fmt.Errorf("%w: run %s: step %q materialized %d instances but its producer %q recorded %d partitions",
			ErrFannedBaseline, runID, g.taskName, len(g.tasks), producer.taskName, len(partitions))
	}
	sort.SliceStable(g.tasks, func(i, j int) bool {
		return g.tasks[i].row.PartitionIndex < g.tasks[j].row.PartitionIndex
	})
	for i, task := range g.tasks {
		part := partitions[i]
		switch {
		case task.row.PartitionIndex != i:
			return fmt.Errorf("%w: run %s: step %q instance %d has partition index %d",
				ErrFannedBaseline, runID, g.taskName, i, task.row.PartitionIndex)
		case task.row.PartitionValue != part.Key:
			return fmt.Errorf("%w: run %s: step %q instance %d is partition %q but the recorded list has %q",
				ErrFannedBaseline, runID, g.taskName, i, task.row.PartitionValue, part.Key)
		case task.row.PartitionFingerprint != part.Fingerprint:
			return fmt.Errorf("%w: run %s: step %q partition %q has fingerprint %q but the recorded list has %q",
				ErrFannedBaseline, runID, g.taskName, part.Key, task.row.PartitionFingerprint, part.Fingerprint)
		case task.row.PartitionCount != len(partitions):
			return fmt.Errorf("%w: run %s: step %q partition %q records partition count %d in a group of %d",
				ErrFannedBaseline, runID, g.taskName, part.Key, task.row.PartitionCount, len(partitions))
		}
		task.fanned = true
		task.partition = part
		task.partitionIndex = i
		task.partitionCount = len(partitions)
	}

	// The same graph the live expansion builds, from the same normalized list, so
	// the replayed group carries the baseline's in-group ordering rather than a
	// re-derivation of it.
	graph, err := pkgtask.ValidatePartitionGraph(partitions)
	if err != nil {
		return fmt.Errorf("%w: run %s: step %q recorded partition list is not a valid group: %v",
			ErrFannedBaseline, runID, g.taskName, err)
	}
	g.partitions = partitions
	if graph != nil {
		g.dependents = graph.Dependents
		g.order, err = inGroupPlanOrder(runID, g, graph.Indegree)
		if err != nil {
			return err
		}
		return nil
	}
	g.order = make([]int, len(g.tasks))
	for i := range g.order {
		g.order[i] = i
	}
	return nil
}

// inGroupPlanOrder returns instance indices in in-group topological order,
// breaking ties by partition index so the plan is deterministic.
func inGroupPlanOrder(runID uuid.UUID, g *baselineGroup, indegree map[string]int) ([]int, error) {
	remaining := make(map[string]int, len(indegree))
	maps.Copy(remaining, indegree)
	indexByKey := make(map[string]int, len(g.tasks))
	for i, task := range g.tasks {
		indexByKey[task.partition.Key] = i
	}

	ready := make([]int, 0, len(g.tasks))
	for i, task := range g.tasks {
		if remaining[task.partition.Key] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Ints(ready)

	order := make([]int, 0, len(g.tasks))
	for len(ready) > 0 {
		idx := ready[0]
		ready = ready[1:]
		order = append(order, idx)
		for _, dep := range g.dependents[g.tasks[idx].partition.Key] {
			depIdx, ok := indexByKey[dep]
			if !ok {
				continue
			}
			remaining[dep]--
			if remaining[dep] == 0 {
				ready = append(ready, depIdx)
			}
		}
		sort.Ints(ready)
	}
	if len(order) != len(g.tasks) {
		// ValidatePartitionGraph rejects cycles, so this is unreachable unless the
		// recorded list and the graph disagree; refuse rather than plan a
		// partial group.
		return nil, fmt.Errorf("%w: run %s: step %q recorded partition list has an unresolvable in-group ordering",
			ErrFannedBaseline, runID, g.taskName)
	}
	return order, nil
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

func topologicalBaselineGroups(runID uuid.UUID, groups []*baselineGroup) ([]*baselineGroup, error) {
	byID := make(map[uuid.UUID]*baselineGroup, len(groups))
	indegree := make(map[uuid.UUID]int, len(groups))
	successors := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(groups))
	edges := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(groups))

	for _, group := range groups {
		if group == nil || len(group.tasks) == 0 {
			return nil, fmt.Errorf("%w: nil baseline task in run %s", ErrUnsupportedDescriptor, runID)
		}
		taskID := group.taskID
		if taskID == uuid.Nil {
			return nil, fmt.Errorf("%w: step %q has empty task id", ErrUnsupportedDescriptor, group.taskName)
		}
		if _, exists := byID[taskID]; exists {
			return nil, fmt.Errorf("%w: duplicate task %s in baseline run %s", ErrUnsupportedDescriptor, taskID, runID)
		}
		byID[taskID] = group
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

	for _, group := range groups {
		taskID := group.taskID
		desc := group.descriptor()
		for _, pred := range desc.DAG.Predecessors {
			if err := addEdge(pred.TaskID, taskID, group.taskName, "predecessor"); err != nil {
				return nil, err
			}
		}
		for _, succ := range desc.DAG.Successors {
			if err := addEdge(taskID, succ.TaskID, group.taskName, "successor"); err != nil {
				return nil, err
			}
		}
	}

	ready := make([]*baselineGroup, 0, len(groups))
	for _, group := range groups {
		if indegree[group.taskID] == 0 {
			ready = append(ready, group)
		}
	}
	sortBaselineReady(ready)

	ordered := make([]*baselineGroup, 0, len(groups))
	for len(ready) > 0 {
		group := ready[0]
		ready = ready[1:]
		ordered = append(ordered, group)
		for successorID := range successors[group.taskID] {
			indegree[successorID]--
			if indegree[successorID] == 0 {
				ready = append(ready, byID[successorID])
			}
		}
		sortBaselineReady(ready)
	}

	if len(ordered) != len(groups) {
		return nil, fmt.Errorf("%w: descriptor DAG cycle while ordering baseline run %s", ErrUnsupportedDescriptor, runID)
	}
	return ordered, nil
}

func sortBaselineReady(groups []*baselineGroup) {
	sort.SliceStable(groups, func(i, j int) bool {
		li, lj := groups[i].tasks[0], groups[j].tasks[0]
		if li.descriptor.DAG.TaskPosition != lj.descriptor.DAG.TaskPosition {
			return li.descriptor.DAG.TaskPosition < lj.descriptor.DAG.TaskPosition
		}
		if !li.row.CreatedAt.Equal(lj.row.CreatedAt) {
			return li.row.CreatedAt.Before(lj.row.CreatedAt)
		}
		if groups[i].taskName != groups[j].taskName {
			return groups[i].taskName < groups[j].taskName
		}
		return groups[i].taskID.String() < groups[j].taskID.String()
	})
}

func (c *Constructor) planTasks(ctx context.Context, groups []*baselineGroup, params map[string]string, forceReexecute bool) ([]plannedTask, error) {
	// One entry per catalog task, listing every plan for it in partition-index
	// order. A fanned predecessor is resolved from the whole slice, never from
	// one sibling: the DAG wires the GROUP, so it presents one aggregate output
	// and one aggregate identity downstream, and contributes exactly one
	// outstanding predecessor.
	plannedByID := make(map[uuid.UUID][]int, len(groups))

	plans := make([]plannedTask, 0, len(groups))
	for _, group := range groups {
		predOutputsByName := make(map[string]map[string]string)
		desc := group.descriptor()
		predHashes := make([]string, 0, len(desc.DAG.Predecessors))
		pendingPredecessors := 0
		for _, pred := range desc.DAG.Predecessors {
			plannedIdxs, ok := plannedByID[pred.TaskID]
			if !ok {
				return nil, fmt.Errorf("%w: step %q references missing predecessor %s", ErrUnsupportedDescriptor, group.taskName, pred.TaskID)
			}
			// Copied by value, not pointed into `plans`: the instance loop below
			// appends to that slice, and a re-allocation would leave any retained
			// pointer aliasing the old backing array.
			predPlans := make([]plannedTask, 0, len(plannedIdxs))
			pending := false
			for _, idx := range plannedIdxs {
				predPlans = append(predPlans, plans[idx])
				if plans[idx].reexecute {
					pending = true
				}
			}
			if pending {
				// Group-level fan-in: the whole predecessor group is one edge, so
				// one pending instance makes the edge pending and contributes one,
				// not N. The live run releases it once, on the transition that makes
				// every instance terminal (run.Store's group-terminal gate).
				pendingPredecessors++
				continue
			}
			predName := firstNonEmpty(pred.TaskName, predPlans[0].base.taskName, pred.TaskID.String())
			output, err := plannedGroupOutput(predName, predPlans)
			if err != nil {
				return nil, err
			}
			if len(output) > 0 {
				predOutputsByName[predName] = output
			}
			if hash := plannedGroupHash(predPlans); hash != "" {
				predHashes = append(predHashes, hash)
			}
		}

		// pendingSiblings counts, per partition key, how many of the in-group
		// dependencies it waits on will actually re-execute. A sibling resolved
		// from cache is written terminal at materialization and never completes,
		// so it never decrements anything downstream of it — counting it would
		// strand its dependents forever.
		pendingSiblings := make(map[string]int, len(group.tasks))
		groupIdxs := make([]int, len(group.tasks))
		for _, i := range group.order {
			task := group.tasks[i]
			pending := pendingPredecessors + pendingSiblings[task.partition.Key]
			replayHash, err := computeDescriptorInstanceHash(task.descriptor, params, predOutputsByName, predHashes, task.partition)
			if err != nil {
				return nil, err
			}
			unchanged := !forceReexecute && hashMatchesBaseline(replayHash, task.computedHash, task.effective)
			plan := plannedTask{
				base:          task,
				replayHash:    replayHash,
				effectiveHash: replayHash,
				// The replay TaskRun stores the baseline descriptor unchanged; its
				// Baseline fields are the audit reference for what was replayed.
				descriptor: task.descriptor,
			}

			if unchanged && pending == 0 {
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
				plan.outstanding = pending
				// An in-group edge exists because the units genuinely affect each
				// other's external state (a dimension before a fact, a VPC before the
				// stacks in it). If this one re-runs, everything ordered after it
				// waits for it — and, further down, re-runs too, since a dependent
				// whose dependency is pending cannot take the cache path above.
				for _, dep := range group.dependents[task.partition.Key] {
					pendingSiblings[dep]++
				}
			}
			plans = append(plans, plan)
			groupIdxs[i] = len(plans) - 1
		}
		plannedByID[group.taskID] = groupIdxs
	}

	// The producer's own replay row records the list its group was expanded from,
	// mirroring what the live completion and cache-hit paths write onto a
	// producer's partitions column.
	for _, group := range groups {
		if group.producerFanOut == nil {
			continue
		}
		idxs := plannedByID[group.taskID]
		if len(idxs) != 1 {
			continue
		}
		plan := &plans[idxs[0]]
		if err := assertReusedProducerListMatches(group, plan); err != nil {
			return nil, err
		}
		plan.producerPartitions = group.producerFanOut.Partitions
	}
	return plans, nil
}

// assertReusedProducerListMatches refuses a producer whose reused cache entry
// recorded a DIFFERENT partition list than the baseline's descriptor.
//
// Same hash, different list means the producer is nondeterministic: the entry
// being reused proves some other execution of these exact inputs discovered
// other work. Expanding the baseline's group against that result would attach a
// group to a result that never produced it — the same "arbitrary sibling"
// mistake the reproduce surface's assertUnfanned exists to prevent, one level
// up. An entry with no recorded list (nil, not empty) is a pre-fan-out entry and
// makes no claim, so it is not evidence of disagreement.
func assertReusedProducerListMatches(group *baselineGroup, plan *plannedTask) error {
	if !plan.cacheHit || !plan.source.partitionsRecorded {
		return nil
	}
	recorded := group.producerFanOut.Partitions
	reused := plan.source.partitions
	mismatch := len(recorded) != len(reused)
	if !mismatch {
		for i := range recorded {
			if !recorded[i].EqualPayload(reused[i]) {
				mismatch = true
				break
			}
		}
	}
	if !mismatch {
		return nil
	}
	return fmt.Errorf("%w: step %q reuses a cache entry from run %s whose recorded partition list (%d) differs from the baseline's (%d); the producer is not deterministic",
		ErrFannedBaseline, group.taskName, plan.source.runID, len(reused), len(recorded))
}

// plannedGroupOutput is the ONE output map a resolved predecessor presents
// downstream: the single row's output when unfanned, and otherwise the same
// pkgtask.AggregateFanInOutputs shape both execution lanes build
// (run.Store's predecessorGroupOutput). Replay must agree with them or a
// fan-in consumer's replayed hash misses every entry it should hit.
//
// Every plan here is a resolved cache hit — the caller returns early when any
// instance is pending — and cacheSourceForUnchanged admits only successful
// results, so the aggregate's counts are all-succeeded, none-failed.
func plannedGroupOutput(producer string, predPlans []plannedTask) (map[string]string, error) {
	if len(predPlans) == 1 && !predPlans[0].base.fanned {
		return maps.Clone(predPlans[0].source.output), nil
	}
	byPartition := make(map[string]map[string]string, len(predPlans))
	for _, plan := range predPlans {
		if len(plan.source.output) > 0 {
			byPartition[plan.base.partition.Key] = plan.source.output
		}
	}
	return pkgtask.AggregateFanInOutputs(producer, byPartition, len(predPlans), 0)
}

// plannedGroupHash is run.Store's predecessorGroupHash over planned instances:
// the single effective hash when unfanned, and otherwise the group identity over
// every instance's effective hash in partition order.
func plannedGroupHash(predPlans []plannedTask) string {
	if len(predPlans) == 1 && !predPlans[0].base.fanned {
		return predPlans[0].effectiveHash
	}
	hashes := make([]string, 0, len(predPlans))
	for _, plan := range predPlans {
		hashes = append(hashes, plan.effectiveHash)
	}
	return run.GroupIdentityHash(hashes)
}

// computeDescriptorHash rebuilds an UNFANNED task's identity from its
// descriptor. A fanned instance goes through computeDescriptorInstanceHash,
// which folds in the partition the baseline instance actually ran.
func computeDescriptorHash(desc models.TaskExecutionDescriptor, params map[string]string, predOutputs map[string]map[string]string, predHashes []string) (string, error) {
	return computeDescriptorInstanceHash(desc, params, predOutputs, predHashes, pkgtask.Partition{})
}

// computeDescriptorInstanceHash is computeDescriptorHash with the instance's
// partition identity folded in exactly as both execution lanes fold it
// (internal/job's buildTaskHashInput and internal/worker's runtime executor):
// key, fingerprint and attributes, never dependsOn — in-group ordering is a
// scheduling instruction, not a data input. A zero Partition leaves the digest
// byte-identical to the unfanned form, which is what keeps every existing
// baseline's replay hash unchanged.
func computeDescriptorInstanceHash(
	desc models.TaskExecutionDescriptor,
	params map[string]string,
	predOutputs map[string]map[string]string,
	predHashes []string,
	partition pkgtask.Partition,
) (string, error) {
	spec := desc.ContainerSpec
	env := maps.Clone(spec.Env)
	if env == nil {
		env = make(map[string]string)
	}
	outputEnv, err := pkgtask.BuildOutputEnv(predOutputs)
	if err != nil {
		return "", err
	}
	for k, v := range outputEnv {
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
		// The chain mode is read from the descriptor, not re-resolved from the
		// live job definition: replay must rebuild the baseline's key, and a
		// values-mode step re-hashed as transitive would miss every entry it
		// should hit. An absent field (a descriptor written before this) is
		// transitive, which is what those baselines were.
		Chain:                desc.Cache.Chain,
		Partition:            partition.Key,
		PartitionFingerprint: partition.Fingerprint,
		PartitionAttributes:  partition.Attributes,
		CacheVersion:         desc.Cache.Version,
	}.Compute(), nil
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

	// entry.Partitions (F7, see cache.Entry's own doc comment) is threaded onto
	// cacheSource rather than dropped, because replay now re-materializes fanned
	// groups and a producer resolved here is exactly the case that gate exists
	// for. Replay's use of it is narrower than internal/job's and
	// internal/worker's, though, and deliberately so: those two must decide
	// whether a hit is usable at all, because a hit with no recorded list leaves
	// them with nothing to expand. Replay always expands from the list frozen on
	// the BASELINE producer's descriptor — reproducing this run, not whichever
	// run happened to write the entry — so the entry's copy is used to CHECK
	// that authority, not to replace it.
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
			// nil vs a non-nil empty slice is meaningful and is preserved end to
			// end by cache.Put/Get: nil means no list was ever recorded on this
			// entry, which is not the same claim as "recorded, and empty".
			partitions:         entry.Partitions,
			partitionsRecorded: entry.Partitions != nil,
		}, nil
	}

	if task.descriptor.Cache.Enabled {
		return cacheSource{}, fmt.Errorf("%w: step %q hash %s cache entry is unavailable or expired", ErrUnavailableBaselineProof, task.taskName, shortHash(replayHash))
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
	priority := baseline.Priority
	if priority <= 0 {
		priority = run.PriorityNormalValue
	}
	for _, plan := range plans {
		record, err := taskRunRecord(replayID, plan, now, priority)
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
			Priority:          priority,
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

func taskRunRecord(replayID uuid.UUID, plan plannedTask, now time.Time, priority int) (models.TaskRun, error) {
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
		Priority:                priority,
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
		CacheChain:              desc.Cache.Chain,
		CacheTTLNever:           desc.Cache.TTLNever,
		ResolvedImageDigest:     desc.Runtime.ResolvedImageDigest,
		OutputSchema:            append(datatypes.JSON(nil), desc.Schema.OutputSchema...),
		SchemaValidation:        desc.Schema.ValidationMode,
		ExecutionDescriptor:     datatypes.JSON(encodedDescriptor),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	// The re-expanded group. Written from the partition RECORDED on the
	// baseline producer's descriptor, in exactly the shape
	// run.Store.expandFanOutSuccessors writes at live expansion — including
	// marshalling a nil DependsOn to JSON `null`, which every reader guards with
	// len() > 0 — so the two kinds of fanned instance row are indistinguishable
	// downstream. The worker's partition env injection, the per-instance cache
	// identity, `run partitions`, `why --partition` and receipts all read these
	// columns and nothing else.
	if plan.base.fanned {
		attrs, err := encodePartitionAttributes(plan.base.partition.Attributes)
		if err != nil {
			return models.TaskRun{}, fmt.Errorf("replay: encode partition attributes for step %q partition %q: %w", plan.base.taskName, plan.base.partition.Key, err)
		}
		deps, err := json.Marshal(plan.base.partition.DependsOn)
		if err != nil {
			return models.TaskRun{}, fmt.Errorf("replay: encode partition dependsOn for step %q partition %q: %w", plan.base.taskName, plan.base.partition.Key, err)
		}
		record.PartitionValue = plan.base.partition.Key
		record.PartitionIndex = plan.base.partitionIndex
		record.PartitionCount = plan.base.partitionCount
		record.PartitionFingerprint = plan.base.partition.Fingerprint
		record.PartitionAttributes = attrs
		record.PartitionDependsOn = datatypes.JSON(deps)
	}
	if len(plan.producerPartitions) > 0 {
		encoded, err := pkgtask.EncodePartitions(plan.producerPartitions)
		if err != nil {
			return models.TaskRun{}, fmt.Errorf("replay: encode producer partitions for step %q: %w", plan.base.taskName, err)
		}
		record.Partitions = datatypes.JSON(encoded)
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
			Partition:    plan.base.partition.Key,
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

// encodePartitionAttributes mirrors run.encodePartitionMap: an empty map leaves
// the column NULL rather than writing `{}`.
func encodePartitionAttributes(attrs map[string]string) (datatypes.JSON, error) {
	if len(attrs) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(attrs)
	if err != nil {
		return nil, err
	}
	return datatypes.JSON(encoded), nil
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
