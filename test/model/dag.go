package model

import (
	"fmt"
	"sort"
	"strings"
)

// Unfanned is the partition index of an identity that is a plain catalog step
// rather than one instance of a fanned one.
const Unfanned = -1

// TaskID names one catalog step of a modelled DAG.
type TaskID string

// PartitionKey names one unit of work inside a fanned step.
type PartitionKey string

// InstanceID is the identity a modelled run actually schedules. A plain step is
// scheduled under its own task id (Partition == Unfanned); a fanned step is
// replaced by one identity per partition.
//
// The product draws the same distinction with two different UUIDs (a catalog
// Task id and a TaskRun id), which is where a whole class of its bugs lived.
// Making the distinction structural here means the model cannot accidentally
// conflate them.
type InstanceID struct {
	Task      TaskID
	Partition int
}

// Step is the identity of an unfanned catalog step.
func Step(t TaskID) InstanceID { return InstanceID{Task: t, Partition: Unfanned} }

// Instance is the identity of one partition of a fanned catalog step.
func Instance(t TaskID, partition int) InstanceID {
	return InstanceID{Task: t, Partition: partition}
}

// IsInstance reports whether the identity belongs to a fanned group.
func (i InstanceID) IsInstance() bool { return i.Partition != Unfanned }

func (i InstanceID) String() string {
	if !i.IsInstance() {
		return string(i.Task)
	}
	return fmt.Sprintf("%s[%d]", i.Task, i.Partition)
}

// FailurePolicy is a fanned step's fanOut.failurePolicy: what one instance
// failing does to the rest of its group.
type FailurePolicy string

const (
	// FailFast stops the whole group at the first instance failure. It is the
	// product's default when the key is omitted.
	FailFast FailurePolicy = "fail_fast"
	// Continue lets the surviving instances run to their own outcomes.
	Continue FailurePolicy = "continue"
)

// Normalize resolves an unset or unknown policy to the product default.
func (p FailurePolicy) Normalize() FailurePolicy {
	if p == Continue {
		return Continue
	}
	return FailFast
}

// FanOut is a fanned step's shape: its partitions, the dependency edges between
// them, its parallelism cap and its failure policy.
type FanOut struct {
	Partitions  []PartitionKey
	MaxParallel int
	// DependsOn maps a partition to the partitions it must follow WITHIN the
	// group. It is the in-group edge set; cross-step edges live on the DAG.
	DependsOn map[PartitionKey][]PartitionKey
	Policy    FailurePolicy
}

// DAG is a bounded, frozen run shape: steps in dispatch order, explicit edges,
// per-step trigger rules, and per-step fan-out.
type DAG struct {
	// Order is every catalog step, in dispatch order. It is the authoritative
	// node set.
	Order []TaskID
	// Edges are explicit cross-step edges, from -> to.
	Edges [][2]TaskID
	// Rules is the per-step trigger rule; an absent entry means all_success.
	Rules map[TaskID]TriggerRule
	// Fan is the per-step fan-out; an absent entry means the step is unfanned.
	Fan map[TaskID]FanOut
}

// Rule returns a step's trigger rule, defaulting an absent or empty entry to
// all_success exactly as the product's schema does.
func (d DAG) Rule(t TaskID) TriggerRule {
	if r, ok := d.Rules[t]; ok && r != "" {
		return r
	}
	return AllSuccess
}

// Succ returns a step's direct successors in edge order.
func (d DAG) Succ(t TaskID) []TaskID {
	var out []TaskID
	for _, e := range d.Edges {
		if e[0] == t {
			out = append(out, e[1])
		}
	}
	return out
}

// Pred returns a step's direct predecessors in edge order.
func (d DAG) Pred(t TaskID) []TaskID {
	var out []TaskID
	for _, e := range d.Edges {
		if e[1] == t {
			out = append(out, e[0])
		}
	}
	return out
}

// Position is a step's index in dispatch order, or -1 when it is not a node.
func (d DAG) Position(t TaskID) int {
	for i, id := range d.Order {
		if id == t {
			return i
		}
	}
	return -1
}

// Instances enumerates the identities a step is scheduled under: itself when
// unfanned, one per partition when fanned.
func (d DAG) Instances(t TaskID) []InstanceID {
	fan, fanned := d.Fan[t]
	if !fanned || len(fan.Partitions) == 0 {
		return []InstanceID{Step(t)}
	}
	out := make([]InstanceID, 0, len(fan.Partitions))
	for i := range fan.Partitions {
		out = append(out, Instance(t, i))
	}
	return out
}

// AllInstances enumerates every scheduled identity in dispatch order.
func (d DAG) AllInstances() []InstanceID {
	var out []InstanceID
	for _, t := range d.Order {
		out = append(out, d.Instances(t)...)
	}
	return out
}

// PartitionKeyOf returns the partition key an instance carries.
func (d DAG) PartitionKeyOf(id InstanceID) PartitionKey {
	if !id.IsInstance() {
		return ""
	}
	fan := d.Fan[id.Task]
	if id.Partition < 0 || id.Partition >= len(fan.Partitions) {
		return ""
	}
	return fan.Partitions[id.Partition]
}

// InGroupSucc returns the in-group dependents of an instance: the siblings that
// declared it in their DependsOn.
func (d DAG) InGroupSucc(id InstanceID) []InstanceID {
	if !id.IsInstance() {
		return nil
	}
	fan, ok := d.Fan[id.Task]
	if !ok {
		return nil
	}
	key := d.PartitionKeyOf(id)
	var out []InstanceID
	for i, p := range fan.Partitions {
		for _, dep := range fan.DependsOn[p] {
			if dep == key {
				out = append(out, Instance(id.Task, i))
				break
			}
		}
	}
	return out
}

// InGroupPred returns the in-group predecessors of an instance.
func (d DAG) InGroupPred(id InstanceID) []InstanceID {
	if !id.IsInstance() {
		return nil
	}
	fan, ok := d.Fan[id.Task]
	if !ok {
		return nil
	}
	key := d.PartitionKeyOf(id)
	index := make(map[PartitionKey]int, len(fan.Partitions))
	for i, p := range fan.Partitions {
		index[p] = i
	}
	var out []InstanceID
	for _, dep := range fan.DependsOn[key] {
		if i, ok := index[dep]; ok {
			out = append(out, Instance(id.Task, i))
		}
	}
	return out
}

// Policy returns a fanned step's normalized failure policy. An unfanned step
// has no policy and reports the default, which is inert for it.
func (d DAG) Policy(t TaskID) FailurePolicy {
	return d.Fan[t].Policy.Normalize()
}

// Validate rejects a DAG the product could not have produced: an unknown node
// on an edge, a duplicate step, a cycle in the cross-step edges, a duplicate or
// unknown partition key, or a cycle in the in-group edges.
//
// The generators only build valid DAGs; this exists so a hand-written
// regression fixture cannot silently encode an impossible shape and "pass".
func (d DAG) Validate() error {
	seen := make(map[TaskID]bool, len(d.Order))
	for _, t := range d.Order {
		if t == "" {
			return fmt.Errorf("model: empty task id in order")
		}
		if seen[t] {
			return fmt.Errorf("model: duplicate task %q in order", t)
		}
		seen[t] = true
	}
	for _, e := range d.Edges {
		if !seen[e[0]] || !seen[e[1]] {
			return fmt.Errorf("model: edge %s -> %s references an unknown step", e[0], e[1])
		}
		if e[0] == e[1] {
			return fmt.Errorf("model: self edge on %s", e[0])
		}
	}
	if cycle := findCycle(d.Order, func(t TaskID) []TaskID { return d.Succ(t) }); cycle != "" {
		return fmt.Errorf("model: cycle in cross-step edges: %s", cycle)
	}
	for t, fan := range d.Fan {
		if !seen[t] {
			return fmt.Errorf("model: fan-out on unknown step %q", t)
		}
		keys := make(map[PartitionKey]bool, len(fan.Partitions))
		for _, p := range fan.Partitions {
			if p == "" {
				return fmt.Errorf("model: empty partition key on %q", t)
			}
			if keys[p] {
				return fmt.Errorf("model: duplicate partition %q on %q", p, t)
			}
			keys[p] = true
		}
		for p, deps := range fan.DependsOn {
			if !keys[p] {
				return fmt.Errorf("model: dependsOn for unknown partition %q on %q", p, t)
			}
			for _, dep := range deps {
				if !keys[dep] {
					return fmt.Errorf("model: partition %q on %q depends on unknown %q", p, t, dep)
				}
				if dep == p {
					return fmt.Errorf("model: partition %q on %q depends on itself", p, t)
				}
			}
		}
		order := append([]PartitionKey(nil), fan.Partitions...)
		if cycle := findCycle(order, func(p PartitionKey) []PartitionKey {
			var out []PartitionKey
			for _, q := range fan.Partitions {
				for _, dep := range fan.DependsOn[q] {
					if dep == p {
						out = append(out, q)
					}
				}
			}
			return out
		}); cycle != "" {
			return fmt.Errorf("model: cycle in in-group edges of %q: %s", t, cycle)
		}
		if fan.MaxParallel < 0 {
			return fmt.Errorf("model: negative maxParallel on %q", t)
		}
	}
	return nil
}

// findCycle reports a cycle in a directed graph as a readable path, or "" when
// the graph is acyclic. Generic so the cross-step and in-group graphs share it.
func findCycle[T comparable](nodes []T, succ func(T) []T) string {
	const (
		white = 0
		grey  = 1
		black = 2
	)
	color := make(map[T]int, len(nodes))
	var stack []T
	var found string

	var visit func(T) bool
	visit = func(n T) bool {
		color[n] = grey
		stack = append(stack, n)
		for _, s := range succ(n) {
			switch color[s] {
			case grey:
				parts := make([]string, 0, len(stack)+1)
				for _, x := range stack {
					parts = append(parts, fmt.Sprint(x))
				}
				parts = append(parts, fmt.Sprint(s))
				found = strings.Join(parts, " -> ")
				return true
			case white:
				if visit(s) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[n] = black
		return false
	}

	for _, n := range nodes {
		if color[n] == white && visit(n) {
			return found
		}
	}
	return ""
}

// SortInstances orders identities the way a dispatcher would: by the catalog
// step's position, then by partition index. Deterministic ordering is what lets
// two implementations be compared element by element instead of as sets.
func (d DAG) SortInstances(ids []InstanceID) {
	sort.SliceStable(ids, func(i, j int) bool {
		pi, pj := d.Position(ids[i].Task), d.Position(ids[j].Task)
		if pi != pj {
			return pi < pj
		}
		return ids[i].Partition < ids[j].Partition
	})
}
