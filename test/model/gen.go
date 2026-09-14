package model

import (
	"fmt"

	"pgregory.net/rapid"
)

// GenConfig bounds the shape of generated DAGs.
//
// Everything here is a bound, not a target. Property tests get their value from
// running many small cases rather than a few large ones: a counterexample in a
// three-step DAG is a bug report, a counterexample in a thirty-step DAG is a
// puzzle. The bounds also keep the suite inside `just unit-test`'s budget,
// which matters because this package runs on every PR under -race.
type GenConfig struct {
	MinSteps      int
	MaxSteps      int
	MaxEdges      int
	FanOut        bool
	MaxPartitions int
	InGroupEdges  bool
	MaxParallel   bool
}

// UnfannedConfig generates plain DAGs with no fan-out. It is what the strict
// step-for-step differential test against the product uses, because comparing
// two implementations is only meaningful where both are fully determined.
func UnfannedConfig() GenConfig {
	return GenConfig{MinSteps: 1, MaxSteps: 5, MaxEdges: 8}
}

// FannedConfig generates DAGs with fan-out, in-group edges and parallelism
// caps, for the partition-accounting properties.
func FannedConfig() GenConfig {
	return GenConfig{
		MinSteps: 2, MaxSteps: 4, MaxEdges: 5,
		FanOut: true, MaxPartitions: 3, InGroupEdges: true, MaxParallel: true,
	}
}

// DAGGen builds a generator of valid bounded DAGs.
//
// Acyclicity is structural rather than filtered: edges only ever point from a
// lower dispatch position to a higher one, and in-group edges only ever form a
// chain. Generating freely and rejecting cycles would work, but it wastes the
// shrinker's budget on invalid candidates and makes minimized counterexamples
// less stable.
func DAGGen(cfg GenConfig) *rapid.Generator[DAG] {
	if cfg.MinSteps < 1 {
		cfg.MinSteps = 1
	}
	if cfg.MaxSteps < cfg.MinSteps {
		cfg.MaxSteps = cfg.MinSteps
	}
	return rapid.Custom(func(t *rapid.T) DAG {
		n := rapid.IntRange(cfg.MinSteps, cfg.MaxSteps).Draw(t, "steps")
		d := DAG{Rules: map[TaskID]TriggerRule{}, Fan: map[TaskID]FanOut{}}
		for i := range n {
			d.Order = append(d.Order, TaskID(fmt.Sprintf("s%d", i)))
		}
		for _, id := range d.Order {
			d.Rules[id] = rapid.SampledFrom(TriggerRules()).Draw(t, "rule")
		}
		for i := range n {
			for j := i + 1; j < n; j++ {
				if len(d.Edges) >= cfg.MaxEdges {
					break
				}
				if rapid.Bool().Draw(t, "edge") {
					d.Edges = append(d.Edges, [2]TaskID{d.Order[i], d.Order[j]})
				}
			}
		}
		if cfg.FanOut {
			for _, id := range d.Order {
				if !rapid.Bool().Draw(t, "fanned") {
					continue
				}
				parts := rapid.IntRange(1, max(cfg.MaxPartitions, 1)).Draw(t, "partitions")
				fan := FanOut{
					Policy:    rapid.SampledFrom([]FailurePolicy{FailFast, Continue}).Draw(t, "policy"),
					DependsOn: map[PartitionKey][]PartitionKey{},
				}
				for p := range parts {
					fan.Partitions = append(fan.Partitions, PartitionKey(fmt.Sprintf("p%d", p)))
				}
				if cfg.InGroupEdges {
					for p := 1; p < parts; p++ {
						if rapid.Bool().Draw(t, "in_group_edge") {
							fan.DependsOn[fan.Partitions[p]] = []PartitionKey{fan.Partitions[p-1]}
						}
					}
				}
				if cfg.MaxParallel {
					fan.MaxParallel = rapid.IntRange(0, parts).Draw(t, "max_parallel")
				}
				d.Fan[id] = fan
			}
		}
		if err := d.Validate(); err != nil {
			// Unreachable by construction; failing loudly beats generating a
			// shape the product could not produce and calling the result a bug.
			t.Fatalf("generated an invalid DAG: %v", err)
		}
		return d
	})
}

// OutcomeGen draws a terminal outcome a worker can report.
func OutcomeGen() *rapid.Generator[TaskStatus] {
	return rapid.SampledFrom(WorkerOutcomes())
}
