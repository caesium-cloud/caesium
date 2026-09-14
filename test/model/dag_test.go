package model_test

import (
	"strings"
	"testing"

	"github.com/caesium-cloud/caesium/test/model"
)

// TestTriggerRuleSpecification pins the trigger-rule truth table as a table.
//
// It is the specification both implementations answer to, so it is written out
// in full rather than derived: if the product and the model ever agree on a
// wrong answer, this is the only thing that catches it.
func TestTriggerRuleSpecification(t *testing.T) {
	ok := model.StatusSucceeded
	bad := model.StatusFailed
	skip := model.StatusSkipped
	cached := model.StatusCached
	run := model.StatusRunning

	cases := []struct {
		name string
		rule model.TriggerRule
		pred []model.TaskStatus
		want bool
	}{
		{"no predecessors runs under all_success", model.AllSuccess, nil, true},
		{"no predecessors runs under all_failed too", model.AllFailed, nil, true},
		{"all_success with every predecessor succeeded", model.AllSuccess, []model.TaskStatus{ok, ok}, true},
		{"all_success counts a cache hit as success", model.AllSuccess, []model.TaskStatus{ok, cached}, true},
		{"all_success blocked by a failure", model.AllSuccess, []model.TaskStatus{ok, bad}, false},
		{"all_success blocked by a skip", model.AllSuccess, []model.TaskStatus{ok, skip}, false},
		{"all_done accepts any terminal mix", model.AllDone, []model.TaskStatus{ok, bad, skip}, true},
		{"all_done waits on non-terminal work", model.AllDone, []model.TaskStatus{ok, run}, false},
		{"always behaves as all_done", model.Always, []model.TaskStatus{bad, skip}, true},
		{"always still waits on non-terminal work", model.Always, []model.TaskStatus{run}, false},
		{"all_failed needs every predecessor failed", model.AllFailed, []model.TaskStatus{bad, bad}, true},
		{"all_failed rejects a skip", model.AllFailed, []model.TaskStatus{bad, skip}, false},
		{"one_success needs one", model.OneSuccess, []model.TaskStatus{bad, ok}, true},
		{"one_success accepts a cache hit", model.OneSuccess, []model.TaskStatus{bad, cached}, true},
		{"one_success rejects all failed", model.OneSuccess, []model.TaskStatus{bad, bad}, false},
		{"an unknown rule degrades to all_success, not to always",
			model.TriggerRule("any_terminal"), []model.TaskStatus{bad}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rule.Satisfied(tc.pred); got != tc.want {
				t.Fatalf("%s over %v = %v, want %v", tc.rule, tc.pred, got, tc.want)
			}
		})
	}
}

// TestGroupStatusSpecification pins how a fanned step is collapsed into the one
// status its successors evaluate.
func TestGroupStatusSpecification(t *testing.T) {
	cases := []struct {
		name    string
		members []model.TaskStatus
		want    model.TaskStatus
	}{
		{"empty group has no status", nil, ""},
		{"in-flight work outranks a failure",
			[]model.TaskStatus{model.StatusFailed, model.StatusRunning}, model.StatusRunning},
		{"any failure fails the group",
			[]model.TaskStatus{model.StatusSucceeded, model.StatusFailed}, model.StatusFailed},
		{"all skipped is skipped",
			[]model.TaskStatus{model.StatusSkipped, model.StatusSkipped}, model.StatusSkipped},
		{"all succeeded is succeeded",
			[]model.TaskStatus{model.StatusSucceeded, model.StatusCached}, model.StatusSucceeded},
		{"a partly-skipped group is NOT a success",
			[]model.TaskStatus{model.StatusSucceeded, model.StatusSkipped}, model.StatusFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := model.GroupStatus(tc.members); got != tc.want {
				t.Fatalf("GroupStatus(%v) = %q, want %q", tc.members, got, tc.want)
			}
		})
	}
}

// TestDAGValidateRejectsImpossibleShapes checks the guard that stops a
// hand-written fixture encoding a DAG the product could never produce. A model
// that happily runs an impossible shape will happily report an impossible bug.
func TestDAGValidateRejectsImpossibleShapes(t *testing.T) {
	cases := []struct {
		name string
		dag  model.DAG
		want string
	}{
		{
			name: "cycle",
			dag: model.DAG{
				Order: []model.TaskID{"a", "b"},
				Edges: [][2]model.TaskID{{"a", "b"}, {"b", "a"}},
			},
			want: "cycle in cross-step edges",
		},
		{
			name: "edge to an unknown step",
			dag: model.DAG{
				Order: []model.TaskID{"a"},
				Edges: [][2]model.TaskID{{"a", "ghost"}},
			},
			want: "unknown step",
		},
		{
			name: "duplicate step",
			dag:  model.DAG{Order: []model.TaskID{"a", "a"}},
			want: "duplicate task",
		},
		{
			name: "self edge",
			dag: model.DAG{
				Order: []model.TaskID{"a"},
				Edges: [][2]model.TaskID{{"a", "a"}},
			},
			want: "self edge",
		},
		{
			name: "duplicate partition key",
			dag: model.DAG{
				Order: []model.TaskID{"a"},
				Fan:   map[model.TaskID]model.FanOut{"a": {Partitions: []model.PartitionKey{"p", "p"}}},
			},
			want: "duplicate partition",
		},
		{
			name: "in-group cycle",
			dag: model.DAG{
				Order: []model.TaskID{"a"},
				Fan: map[model.TaskID]model.FanOut{"a": {
					Partitions: []model.PartitionKey{"p0", "p1"},
					DependsOn: map[model.PartitionKey][]model.PartitionKey{
						"p0": {"p1"},
						"p1": {"p0"},
					},
				}},
			},
			want: "cycle in in-group edges",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.dag.Validate()
			if err == nil {
				t.Fatalf("Validate accepted an impossible DAG")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate said %q, expected it to mention %q", err, tc.want)
			}
		})
	}
}

// TestFanOutEdgesAreGroupScoped checks the accounting rule that a fanned
// predecessor is ONE fan-in edge rather than one per partition, and that
// in-group edges are separate from cross-step ones.
func TestFanOutEdgesAreGroupScoped(t *testing.T) {
	dag := model.DAG{
		Order: []model.TaskID{"producer", "consumer"},
		Edges: [][2]model.TaskID{{"producer", "consumer"}},
		Fan: map[model.TaskID]model.FanOut{
			"producer": {
				Partitions: []model.PartitionKey{"p0", "p1", "p2"},
				DependsOn:  map[model.PartitionKey][]model.PartitionKey{"p1": {"p0"}},
			},
		},
	}
	if err := dag.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got := len(dag.Instances("producer")); got != 3 {
		t.Fatalf("producer schedules %d identities, want 3", got)
	}
	if got := dag.Pred("consumer"); len(got) != 1 || got[0] != "producer" {
		t.Fatalf("consumer has predecessors %v, want exactly [producer]", got)
	}
	if got := dag.InGroupSucc(model.Instance("producer", 0)); len(got) != 1 || got[0] != model.Instance("producer", 1) {
		t.Fatalf("p0's in-group dependents are %v, want [producer[1]]", got)
	}
	if got := dag.InGroupPred(model.Instance("producer", 2)); len(got) != 0 {
		t.Fatalf("p2 has in-group predecessors %v, want none", got)
	}
}
