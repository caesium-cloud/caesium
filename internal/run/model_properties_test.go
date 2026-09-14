package run

import (
	"encoding/json"
	"fmt"
	"sort"
	"testing"

	"github.com/caesium-cloud/caesium/internal/models"
	pkgtask "github.com/caesium-cloud/caesium/pkg/task"
	"github.com/caesium-cloud/caesium/test/model"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"pgregory.net/rapid"
)

// These properties drive the REAL owner decision engine — RunState, the same
// code the run owner runs in production — against the independent reference
// model in test/model, over generated bounded DAGs and generated operation
// sequences.
//
// What that establishes and what it does not:
//
//   - It establishes that the DAG advancement decisions agree with a second
//     implementation written from the specification rather than from this code.
//     The two are structured differently on purpose (incremental predecessor
//     counters here, a declarative fixpoint there), so an agreement is evidence
//     and not a tautology.
//   - It establishes nothing about wiring. Nothing here writes a row, takes a
//     lease, or speaks to a worker. A run that advances correctly in memory and
//     never persists its decision is exactly as broken as one that decides
//     wrongly, and only the integration and multi-node lanes can tell.
//
// Fixtures are hermetic: no database, no server, no clock, no socket.

// modelBridge maps identities between the reference model, which names steps,
// and RunState, which is keyed by catalog task UUIDs.
type modelBridge struct {
	dag      model.DAG
	topo     RunTopology
	taskID   map[model.TaskID]uuid.UUID
	instance map[model.InstanceID]uuid.UUID
	back     map[uuid.UUID]model.InstanceID
}

// newUnfannedBridge builds the topology for a DAG with no fan-out: every step
// is scheduled under its own catalog id, which is how RunState keys an
// unfanned run.
func newUnfannedBridge(dag model.DAG) *modelBridge {
	b := &modelBridge{
		dag:      dag,
		taskID:   map[model.TaskID]uuid.UUID{},
		instance: map[model.InstanceID]uuid.UUID{},
		back:     map[uuid.UUID]model.InstanceID{},
	}
	adjacency := map[uuid.UUID][]uuid.UUID{}
	predecessors := map[uuid.UUID][]uuid.UUID{}
	rules := map[uuid.UUID]string{}
	order := map[uuid.UUID]int{}

	for i, step := range dag.Order {
		id := uuid.New()
		b.taskID[step] = id
		b.instance[model.Step(step)] = id
		b.back[id] = model.Step(step)
		adjacency[id] = nil
		predecessors[id] = nil
		order[id] = i
		rules[id] = string(dag.Rule(step))
	}
	for _, edge := range dag.Edges {
		from, to := b.taskID[edge[0]], b.taskID[edge[1]]
		adjacency[from] = append(adjacency[from], to)
		predecessors[to] = append(predecessors[to], from)
	}
	b.topo = RunTopology{
		Adjacency:    adjacency,
		Predecessors: predecessors,
		TriggerRule:  rules,
		Order:        order,
	}
	return b
}

// expand registers a fanned step's instances after RunState.ExpandTask has run,
// so the bridge can map instance rows back to model identities.
func (b *modelBridge) expand(step model.TaskID, instances []ExpandedInstance) {
	for i, inst := range instances {
		id := model.Instance(step, i)
		b.instance[id] = inst.TaskRunID
		b.back[inst.TaskRunID] = id
	}
	delete(b.instance, model.Step(step))
}

func (b *modelBridge) names(ids []uuid.UUID) []model.InstanceID {
	out := make([]model.InstanceID, 0, len(ids))
	for _, id := range ids {
		out = append(out, b.back[id])
	}
	return out
}

// sortedNames renders an identity set as sorted strings, so a mismatch is
// reported as a readable set difference rather than as two UUID slices.
func sortedNames(ids []model.InstanceID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	sort.Strings(out)
	return out
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertAgreement compares every externally observable decision the two
// implementations make about the current state.
func assertAgreement(t *rapid.T, b *modelBridge, rs *RunState, ref *model.Run, step string) {
	t.Helper()

	gotReady := sortedNames(b.names(rs.ReadyTasks()))
	wantReady := sortedNames(ref.Ready())
	if !sameSet(gotReady, wantReady) {
		t.Fatalf("%s: ready queue disagrees\n  RunState: %v\n  model:    %v", step, gotReady, wantReady)
	}

	for modelID, productID := range b.instance {
		state, known := rs.TaskState(productID)
		refStatus, refKnown := ref.StatusOf(modelID)
		if known != refKnown {
			t.Fatalf("%s: %s known to RunState=%v, model=%v", step, modelID, known, refKnown)
		}
		if !known {
			continue
		}
		if string(state.Status) != string(refStatus) {
			t.Fatalf("%s: %s is %q in RunState and %q in the model",
				step, modelID, state.Status, refStatus)
		}
	}

	if rs.IsComplete() != ref.IsComplete() {
		t.Fatalf("%s: RunState complete=%v, model complete=%v", step, rs.IsComplete(), ref.IsComplete())
	}

	refFailed := false
	for _, id := range b.dag.AllInstances() {
		if s, _ := ref.StatusOf(id); s == model.StatusFailed {
			refFailed = true
		}
	}
	if rs.HasFailures() != refFailed {
		t.Fatalf("%s: RunState failures=%v, model failures=%v", step, rs.HasFailures(), refFailed)
	}
}

// TestOwnerStateAgreesWithReferenceModel is the core differential property for
// plain DAGs: generated shapes, generated dispatch/completion orders, compared
// after every step.
func TestOwnerStateAgreesWithReferenceModel(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		rs := NewRunState(bridge.topo, 0)
		ref, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("reference model rejected a generated DAG: %v", err)
		}
		assertAgreement(t, bridge, rs, ref, "admission")

		t.Repeat(map[string]func(*rapid.T){
			"": func(t *rapid.T) { assertAgreement(t, bridge, rs, ref, "invariant") },

			"dispatch": func(t *rapid.T) {
				ready := ref.Ready()
				if len(ready) == 0 {
					return
				}
				id := rapid.SampledFrom(ready).Draw(t, "instance")
				productID := bridge.instance[id]
				state, _ := rs.TaskState(productID)
				rs.MarkDispatched(productID, "node-a", state.Attempt, 60_000)
				if !ref.Dispatch(id, "node-a", 60_000) {
					t.Fatalf("%s was ready for RunState but not for the model", id)
				}
			},

			"complete": func(t *rapid.T) {
				running := ref.Running()
				if len(running) == 0 {
					return
				}
				id := rapid.SampledFrom(running).Draw(t, "instance")
				outcome := model.OutcomeGen().Draw(t, "outcome")
				productID := bridge.instance[id]

				got := rs.ApplyCompletion(productID, TaskStatus(outcome), nil)
				want := ref.Complete(id, outcome, ref.Generation())

				if got.Applied != want.Applied {
					t.Fatalf("completing %s: RunState Applied=%v, model Applied=%v",
						id, got.Applied, want.Applied)
				}
				if (got.TerminalSequence > 0) != (want.Sequence > 0) {
					t.Fatalf("completing %s: RunState stamped %d, model stamped %d",
						id, got.TerminalSequence, want.Sequence)
				}
				if got.Durable() != want.Durable() {
					t.Fatalf("completing %s: RunState Durable=%v, model Durable=%v",
						id, got.Durable(), want.Durable())
				}
				if got.Complete != want.Complete {
					t.Fatalf("completing %s: RunState Complete=%v, model Complete=%v",
						id, got.Complete, want.Complete)
				}

				gotSkipped := make([]model.InstanceID, 0, len(got.Skipped))
				for _, s := range got.Skipped {
					gotSkipped = append(gotSkipped, bridge.back[s.TaskID])
				}
				wantSkipped := make([]model.InstanceID, 0, len(want.Decided))
				for _, d := range want.Decided {
					wantSkipped = append(wantSkipped, d.Instance)
				}
				if !sameSet(sortedNames(gotSkipped), sortedNames(wantSkipped)) {
					t.Fatalf("completing %s: RunState skipped %v, model skipped %v",
						id, sortedNames(gotSkipped), sortedNames(wantSkipped))
				}
				if !sameSet(sortedNames(bridge.names(got.Ready)), sortedNames(want.Ready)) {
					t.Fatalf("completing %s: RunState released %v, model released %v",
						id, sortedNames(bridge.names(got.Ready)), sortedNames(want.Ready))
				}
			},

			"duplicate_complete": func(t *rapid.T) {
				// The re-delivery contract, checked on the real engine: a repeat
				// completion advances nothing, but still replays the first
				// delivery's sequence. Returning zero here is what would make
				// the re-persisted row invisible to recovery's strictly-greater
				// replay filter.
				var terminal []model.InstanceID
				for _, id := range dag.AllInstances() {
					if s, _ := ref.StatusOf(id); model.Terminal(s) {
						terminal = append(terminal, id)
					}
				}
				if len(terminal) == 0 {
					return
				}
				id := rapid.SampledFrom(terminal).Draw(t, "instance")
				productID := bridge.instance[id]
				state, _ := rs.TaskState(productID)

				before := rs.Sequence()
				got := rs.ApplyCompletion(productID, state.Status, nil)
				if got.Applied {
					t.Fatalf("a repeat completion of %s advanced RunState", id)
				}
				if rs.Sequence() != before {
					t.Fatalf("a repeat completion of %s moved the sequence cursor %d -> %d",
						id, before, rs.Sequence())
				}
				if len(got.Ready) != 0 {
					t.Fatalf("a repeat completion of %s replayed live ready work %v", id, got.Ready)
				}
				if got.Durable() && got.TerminalSequence == 0 && len(got.Skipped) == 0 {
					t.Fatalf("a repeat completion of %s reported Durable with nothing to persist", id)
				}
			},

			"requeue_expired": func(t *rapid.T) {
				// Owner-side claim reaping, checked on the real engine against
				// the model's equivalent.
				running := ref.Running()
				if len(running) == 0 {
					return
				}
				id := rapid.SampledFrom(running).Draw(t, "instance")
				productID := bridge.instance[id]
				before, _ := rs.TaskState(productID)

				got := rs.RequeueExpiredRows([]models.TaskRun{{TaskID: productID}})
				ref.ExpireClaim(id, 1<<40)

				if len(got) != 1 || got[0] != productID {
					t.Fatalf("reaping %s requeued %v", id, bridge.names(got))
				}
				after, _ := rs.TaskState(productID)
				if after.Status != TaskStatusPending {
					t.Fatalf("reaping %s left it %s", id, after.Status)
				}
				if after.Attempt != before.Attempt+1 {
					t.Fatalf("reaping %s left attempt %d, want %d", id, after.Attempt, before.Attempt+1)
				}
			},
		})
	})
}

// TestAnyLeaseOverdueIsNecessaryNotSufficient pins the exact strength of the
// owner-side reap precondition, which is documented as a necessary condition
// only.
//
// The direction that matters is the one asserted: it can never report "fresh"
// about work whose lease has lapsed, because that is what would wedge a run
// forever. The other direction — reporting overdue about work that is alive —
// is harmless and deliberately not asserted, because the owner never sees the
// renewals the worker performs.
func TestAnyLeaseOverdueIsNecessaryNotSufficient(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		rs := NewRunState(bridge.topo, 0)
		ref, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("reference model rejected a generated DAG: %v", err)
		}

		now := int64(rapid.IntRange(1, 1_000_000).Draw(t, "now"))
		anyLapsed := false
		for _, id := range ref.Ready() {
			if !rapid.Bool().Draw(t, "dispatch") {
				continue
			}
			lease := int64(rapid.IntRange(0, 2_000_000).Draw(t, "lease"))
			productID := bridge.instance[id]
			state, _ := rs.TaskState(productID)
			rs.MarkDispatched(productID, "node-a", state.Attempt, lease)
			ref.Dispatch(id, "node-a", lease)
			if lease <= 0 || lease < now {
				anyLapsed = true
			}
		}

		if anyLapsed && !rs.AnyLeaseOverdue(now) {
			t.Fatalf("a lapsed dispatch lease was reported fresh at %d", now)
		}
		if rs.AnyLeaseOverdue(now) != ref.AnyClaimOverdue(now) {
			t.Fatalf("RunState overdue=%v, model overdue=%v at %d",
				rs.AnyLeaseOverdue(now), ref.AnyClaimOverdue(now), now)
		}
	})
}

// TestCloneIsolatesStagedDecisions checks the property the clone exists for:
// the owner stages a DAG transition in a copy and swaps it in only after the
// durable write commits, so a failed transaction can never leave the published
// state ahead of the rows.
//
// A shallow copy passes a casual test and fails this one: the mutation lands in
// a map or a slice backing array both copies share.
func TestCloneIsolatesStagedDecisions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := model.DAGGen(model.UnfannedConfig()).Draw(t, "dag")
		bridge := newUnfannedBridge(dag)
		rs := NewRunState(bridge.topo, 0)
		ref, err := model.New("run-1", "job-1", dag)
		if err != nil {
			t.Fatalf("reference model rejected a generated DAG: %v", err)
		}

		// Advance a little so the clone has real state to protect.
		for range rapid.IntRange(0, 6).Draw(t, "warmup") {
			ready := ref.Ready()
			if len(ready) == 0 {
				break
			}
			id := ready[0]
			productID := bridge.instance[id]
			state, _ := rs.TaskState(productID)
			rs.MarkDispatched(productID, "node-a", state.Attempt, 60_000)
			ref.Dispatch(id, "node-a", 60_000)
			outcome := model.OutcomeGen().Draw(t, "outcome")
			rs.ApplyCompletion(productID, TaskStatus(outcome), nil)
			ref.Complete(id, outcome, ref.Generation())
		}

		beforeSeq := rs.Sequence()
		beforeReady := sortedNames(bridge.names(rs.ReadyTasks()))
		beforeStatus := map[uuid.UUID]TaskStatus{}
		for _, productID := range bridge.instance {
			s, _ := rs.TaskState(productID)
			beforeStatus[productID] = s.Status
		}

		staged := rs.Clone()
		// Abandon the staged transition, as a failed commit would.
		for _, id := range staged.ReadyTasks() {
			staged.MarkDispatched(id, "node-b", 9, 1)
			staged.ApplyCompletion(id, TaskStatusFailed, nil)
		}

		if rs.Sequence() != beforeSeq {
			t.Fatalf("an abandoned staged transition moved the original cursor %d -> %d",
				beforeSeq, rs.Sequence())
		}
		if !sameSet(sortedNames(bridge.names(rs.ReadyTasks())), beforeReady) {
			t.Fatalf("an abandoned staged transition changed the original ready queue")
		}
		for productID, want := range beforeStatus {
			if got, _ := rs.TaskState(productID); got.Status != want {
				t.Fatalf("an abandoned staged transition moved %s from %s to %s",
					bridge.back[productID], want, got.Status)
			}
		}
		assertAgreement(t, bridge, rs, ref, "after abandoning a clone")
	})
}

// modelFanOutFixture builds a hermetic single-group fan-out run: one fanned step,
// its partitions, its in-group edges, its parallelism cap and its failure
// policy, expanded into RunState exactly as the owner's expansion path does.
type modelFanOutFixture struct {
	bridge   *modelBridge
	rs       *RunState
	ref      *model.Run
	step     model.TaskID
	catalog  []models.Task
	rows     []models.TaskRun
	dag      model.DAG
	instance []ExpandedInstance
}

func newModelFanOutFixture(t *rapid.T, dag model.DAG, step model.TaskID) *modelFanOutFixture {
	t.Helper()
	fan := dag.Fan[step]
	bridge := newUnfannedBridge(dag)
	rs := NewRunState(bridge.topo, 0)
	ref, err := model.New("run-1", "job-1", dag)
	if err != nil {
		t.Fatalf("reference model rejected a generated DAG: %v", err)
	}

	catalogID := bridge.taskID[step]
	base := len(dag.Pred(step))
	instances := make([]ExpandedInstance, 0, len(fan.Partitions))
	rows := make([]models.TaskRun, 0, len(fan.Partitions))
	for i, key := range fan.Partitions {
		deps := fan.DependsOn[key]
		depKeys := make([]string, 0, len(deps))
		for _, d := range deps {
			depKeys = append(depKeys, string(d))
		}
		rowID := uuid.New()
		instances = append(instances, ExpandedInstance{
			TaskRunID:               rowID,
			TaskID:                  catalogID,
			PartitionIndex:          i,
			Partition:               pkgtask.Partition{Key: string(key), DependsOn: depKeys},
			OutstandingPredecessors: base + len(depKeys),
		})
		dependsOn, err := json.Marshal(depKeys)
		if err != nil {
			t.Fatalf("marshal partition dependencies: %v", err)
		}
		rows = append(rows, models.TaskRun{
			ID:                 rowID,
			TaskID:             catalogID,
			PartitionIndex:     i,
			PartitionValue:     string(key),
			PartitionCount:     len(fan.Partitions),
			PartitionDependsOn: datatypes.JSON(dependsOn),
			Status:             string(TaskStatusPending),
		})
	}

	fanConfig, err := json.Marshal(map[string]any{
		"from":          "producer",
		"maxParallel":   fan.MaxParallel,
		"failurePolicy": string(fan.Policy.Normalize()),
	})
	if err != nil {
		t.Fatalf("marshal fan-out config: %v", err)
	}
	catalog := []models.Task{{
		ID:           catalogID,
		Name:         string(step),
		FanOutConfig: datatypes.JSON(fanConfig),
	}}

	expansion := &FanOutExpansion{
		ProducerTaskID: catalogID,
		Groups: []ExpandedGroup{{
			TaskID:      catalogID,
			TaskName:    string(step),
			MaxParallel: fan.MaxParallel,
			Instances:   instances,
			Dependents:  dependentsOf(fan),
		}},
	}
	rs.SetGroupFailurePolicy(catalogID, string(fan.Policy.Normalize()))
	rs.ApplyExpansion(expansion)
	bridge.expand(step, instances)

	return &modelFanOutFixture{
		bridge: bridge, rs: rs, ref: ref, step: step,
		catalog: catalog, rows: rows, dag: dag, instance: instances,
	}
}

// dependentsOf inverts a partition's DependsOn into the from -> dependents map
// the expansion payload carries.
func dependentsOf(fan model.FanOut) map[string][]string {
	out := map[string][]string{}
	for _, key := range fan.Partitions {
		for _, dep := range fan.DependsOn[key] {
			out[string(dep)] = append(out[string(dep)], string(key))
		}
	}
	return out
}

// fannedDAGGen generates a DAG with exactly one fanned step so the fixture has
// a single, unambiguous group to account for.
func fannedDAGGen() *rapid.Generator[model.DAG] {
	return rapid.Custom(func(t *rapid.T) model.DAG {
		steps := rapid.IntRange(1, 3).Draw(t, "steps")
		fannedAt := rapid.IntRange(0, steps-1).Draw(t, "fanned_at")
		dag := model.DAG{Rules: map[model.TaskID]model.TriggerRule{}, Fan: map[model.TaskID]model.FanOut{}}
		for i := range steps {
			dag.Order = append(dag.Order, model.TaskID(fmt.Sprintf("s%d", i)))
		}
		for i := range steps {
			dag.Rules[dag.Order[i]] = rapid.SampledFrom(model.TriggerRules()).Draw(t, "rule")
			if i > 0 && rapid.Bool().Draw(t, "edge") {
				dag.Edges = append(dag.Edges, [2]model.TaskID{dag.Order[i-1], dag.Order[i]})
			}
		}
		partitions := rapid.IntRange(1, 3).Draw(t, "partitions")
		fan := model.FanOut{
			Policy:    rapid.SampledFrom([]model.FailurePolicy{model.FailFast, model.Continue}).Draw(t, "policy"),
			DependsOn: map[model.PartitionKey][]model.PartitionKey{},
		}
		for p := range partitions {
			fan.Partitions = append(fan.Partitions, model.PartitionKey(fmt.Sprintf("p%d", p)))
		}
		for p := 1; p < partitions; p++ {
			if rapid.Bool().Draw(t, "in_group_edge") {
				fan.DependsOn[fan.Partitions[p]] = []model.PartitionKey{fan.Partitions[p-1]}
			}
		}
		fan.MaxParallel = rapid.IntRange(0, partitions).Draw(t, "max_parallel")
		dag.Fan[dag.Order[fannedAt]] = fan
		if err := dag.Validate(); err != nil {
			t.Fatalf("generated an invalid DAG: %v", err)
		}
		return dag
	})
}

// TestFanOutPartitionAccounting checks the accounting a fanned step introduces
// on the real engine: one scheduled identity per partition, a parallelism cap
// that holds under any dispatch order, and a group that contributes exactly one
// cross-step fan-in edge rather than one per partition.
func TestFanOutPartitionAccounting(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		dag := fannedDAGGen().Draw(t, "dag")
		var fanned model.TaskID
		for step := range dag.Fan {
			fanned = step
		}
		fx := newModelFanOutFixture(t, dag, fanned)
		fan := dag.Fan[fanned]
		catalogID := fx.bridge.taskID[fanned]

		// One identity per declared partition, and the template node is gone.
		if _, templateStillPresent := fx.rs.TaskState(catalogID); templateStillPresent && len(fan.Partitions) > 0 {
			t.Fatalf("expansion left the template node for %s in the state", fanned)
		}
		for i := range fan.Partitions {
			id := fx.bridge.instance[model.Instance(fanned, i)]
			if _, ok := fx.rs.TaskState(id); !ok {
				t.Fatalf("partition %d of %s was not installed", i, fanned)
			}
			got, isInstance := fx.rs.CatalogTaskID(id)
			if !isInstance || got != catalogID {
				t.Fatalf("partition %d of %s maps to catalog %v (instance=%v)", i, fanned, got, isInstance)
			}
		}

		// The cap holds under every dispatch order the engine itself offers.
		inFlight := 0
		for range rapid.IntRange(0, 8).Draw(t, "dispatches") {
			ready := fx.rs.ReadyTasks()
			if len(ready) == 0 {
				break
			}
			pick := ready[rapid.IntRange(0, len(ready)-1).Draw(t, "pick")]
			if _, isInstance := fx.rs.CatalogTaskID(pick); isInstance {
				if fan.MaxParallel > 0 && inFlight >= fan.MaxParallel {
					t.Fatalf("%s offered a %d-th instance of a group capped at %d",
						fanned, inFlight+1, fan.MaxParallel)
				}
				inFlight++
			}
			state, _ := fx.rs.TaskState(pick)
			fx.rs.MarkDispatched(pick, "node-a", state.Attempt, 60_000)
		}

		// A successor of the fanned step may not be released until EVERY
		// partition is terminal. Complete all but one and check it is held.
		if len(fan.Partitions) > 1 && len(dag.Succ(fanned)) > 0 {
			for i := 0; i < len(fan.Partitions)-1; i++ {
				id := fx.bridge.instance[model.Instance(fanned, i)]
				if s, _ := fx.rs.TaskState(id); IsTerminal(s.Status) {
					continue
				}
				fx.rs.MarkDispatched(id, "node-a", 1, 60_000)
				fx.rs.ApplyCompletion(id, TaskStatusSucceeded, nil)
			}
			for _, succ := range dag.Succ(fanned) {
				succID := fx.bridge.taskID[succ]
				state, ok := fx.rs.TaskState(succID)
				if !ok {
					continue
				}
				if state.Status == TaskStatusRunning {
					t.Fatalf("%s started while a partition of %s was still outstanding", succ, fanned)
				}
				for _, ready := range fx.rs.ReadyTasks() {
					if ready == succID {
						t.Fatalf("%s was offered for dispatch with a partition of %s outstanding",
							succ, fanned)
					}
				}
			}
		}
	})
}

// TestFanOutFailurePolicyDecisions checks the two failure policies on the real
// engine, and that each records the reason an operator will read.
func TestFanOutFailurePolicyDecisions(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		partitions := rapid.IntRange(2, 3).Draw(t, "partitions")
		policy := rapid.SampledFrom([]model.FailurePolicy{model.FailFast, model.Continue}).Draw(t, "policy")
		chained := rapid.Bool().Draw(t, "chained")

		fan := model.FanOut{Policy: policy, DependsOn: map[model.PartitionKey][]model.PartitionKey{}}
		for p := range partitions {
			fan.Partitions = append(fan.Partitions, model.PartitionKey(fmt.Sprintf("p%d", p)))
		}
		if chained {
			for p := 1; p < partitions; p++ {
				fan.DependsOn[fan.Partitions[p]] = []model.PartitionKey{fan.Partitions[p-1]}
			}
		}
		dag := model.DAG{
			Order: []model.TaskID{"fanned"},
			Fan:   map[model.TaskID]model.FanOut{"fanned": fan},
		}
		fx := newModelFanOutFixture(t, dag, "fanned")

		// Dispatch the first partition and fail it.
		first := fx.bridge.instance[model.Instance("fanned", 0)]
		fx.rs.MarkDispatched(first, "node-a", 1, 60_000)
		res := fx.rs.ApplyCompletion(first, TaskStatusFailed, nil)

		switch {
		case policy == model.FailFast:
			// Every sibling that had not started is resolved, so the group is
			// fully terminal and the run can settle.
			for i := 1; i < partitions; i++ {
				id := fx.bridge.instance[model.Instance("fanned", i)]
				state, _ := fx.rs.TaskState(id)
				if state.Status != TaskStatusSkipped {
					t.Fatalf("fail_fast left partition %d as %s", i, state.Status)
				}
			}
			if !fx.rs.IsComplete() {
				t.Fatalf("fail_fast left the run incomplete: ready=%v", fx.rs.ReadyTasks())
			}
			for _, s := range res.Skipped {
				if s.Reason != "fan-out group failed fast" {
					t.Fatalf("fail_fast recorded the reason %q", s.Reason)
				}
			}
		case chained:
			// continue + in-group edges: the failure cascades to the dependents
			// that can never run, naming the failed partition's key.
			for i := 1; i < partitions; i++ {
				id := fx.bridge.instance[model.Instance("fanned", i)]
				state, _ := fx.rs.TaskState(id)
				if state.Status != TaskStatusSkipped {
					t.Fatalf("the in-group cascade left partition %d as %s", i, state.Status)
				}
			}
			for _, s := range res.Skipped {
				if s.Reason != "fan-out dependency p0 failed" {
					t.Fatalf("the in-group cascade recorded the reason %q", s.Reason)
				}
			}
		default:
			// continue with independent partitions: the survivors still run.
			for i := 1; i < partitions; i++ {
				id := fx.bridge.instance[model.Instance("fanned", i)]
				state, _ := fx.rs.TaskState(id)
				if IsTerminal(state.Status) {
					t.Fatalf("continue resolved independent partition %d as %s", i, state.Status)
				}
			}
			if fx.rs.IsComplete() {
				t.Fatalf("continue completed the run with partitions outstanding")
			}
		}

		// Whatever the policy decided, it must be durable: a decision that is
		// not persisted is re-derived differently after a takeover.
		if len(res.Skipped) > 0 && !res.Durable() {
			t.Fatalf("owner decisions %v were reported non-durable", res.Skipped)
		}
		for _, s := range res.Skipped {
			if s.TerminalSequence <= 0 {
				t.Fatalf("skip of %v carries sequence %d", s.TaskID, s.TerminalSequence)
			}
		}
	})
}
