package freshness

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
)

var (
	// ErrDatasetMultipleProducers is returned when more than one job declares
	// that it produces the same dataset.
	ErrDatasetMultipleProducers = errors.New("dataset produced by multiple jobs")
	// ErrDatasetUnresolvedConsumes is returned when a consumed dataset is
	// neither produced by any job nor declared as a source.
	ErrDatasetUnresolvedConsumes = errors.New("consumed dataset is not produced or declared")
	// ErrDatasetGraphCycle is returned when the producer→consumer graph contains
	// a cross-job cycle (a dataset cycle is a derivation cycle).
	ErrDatasetGraphCycle = errors.New("dataset dependency cycle detected")
)

// ValidateGraph runs the cross-job declared-graph checks over the full set of
// declarations — the applied set plus any persisted declarations from jobs not
// being replaced:
//
//   - exactly one job produces a given dataset (any number consume);
//   - every consumed dataset resolves to a produced dataset or a declared
//     source (external:true is a declared source); and
//   - the producer→consumer graph is acyclic across jobs.
//
// It is a pure function over the declaration rows so it can be unit-tested
// without a database; the caller (internal/jobdef) is responsible for gathering
// the applied + persisted declarations and excluding replaced jobs.
func ValidateGraph(decls []models.DatasetDeclaration) error {
	producers := make(map[string]map[string]struct{}) // dataset name -> set of producer job aliases
	sources := make(map[string]struct{})              // dataset names declared as a source
	type consumeEdge struct{ alias, name string }
	consumes := make([]consumeEdge, 0)

	for i := range decls {
		d := &decls[i]
		name := strings.TrimSpace(d.Name)
		alias := strings.TrimSpace(d.JobAlias)
		if name == "" {
			continue
		}
		switch d.Direction {
		case models.DatasetDirectionProduces:
			if producers[name] == nil {
				producers[name] = make(map[string]struct{})
			}
			if alias != "" {
				producers[name][alias] = struct{}{}
			}
		case models.DatasetDirectionSource:
			sources[name] = struct{}{}
		case models.DatasetDirectionConsumes:
			consumes = append(consumes, consumeEdge{alias: alias, name: name})
		}
	}

	// Single-producer: at most one job may produce a given dataset.
	for _, name := range sortedMapKeys(producers) {
		if len(producers[name]) > 1 {
			owners := sortedSetKeys(producers[name])
			return fmt.Errorf("%w: %q produced by %s", ErrDatasetMultipleProducers, name, strings.Join(owners, ", "))
		}
	}

	// Resolution: every consumed dataset must be produced or declared as a source.
	sort.Slice(consumes, func(i, j int) bool {
		if consumes[i].name != consumes[j].name {
			return consumes[i].name < consumes[j].name
		}
		return consumes[i].alias < consumes[j].alias
	})
	for _, c := range consumes {
		if _, ok := producers[c.name]; ok {
			continue
		}
		if _, ok := sources[c.name]; ok {
			continue
		}
		return fmt.Errorf("%w: job %q consumes %q which is not produced by any job or declared as a source", ErrDatasetUnresolvedConsumes, c.alias, c.name)
	}

	// Cross-job cycle detection over producer→consumer edges. An edge P→C means
	// job C consumes a dataset produced by job P.
	graph := make(map[string]map[string]struct{})
	addEdge := func(from, to string) {
		if from == "" || to == "" || from == to {
			return
		}
		if graph[from] == nil {
			graph[from] = make(map[string]struct{})
		}
		graph[from][to] = struct{}{}
	}
	for _, c := range consumes {
		for producer := range producers[c.name] {
			addEdge(producer, c.alias)
		}
	}
	if cycle := detectGraphCycle(graph); len(cycle) > 0 {
		return fmt.Errorf("%w: %s", ErrDatasetGraphCycle, strings.Join(cycle, " -> "))
	}

	return nil
}

func sortedMapKeys(m map[string]map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedSetKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// detectGraphCycle returns a readable node path if the directed graph contains a
// cycle, or nil otherwise. Iteration is over sorted nodes/edges for a
// deterministic path in the error message.
func detectGraphCycle(graph map[string]map[string]struct{}) []string {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[string]int, len(graph))
	stack := make([]string, 0, len(graph))

	nodes := make([]string, 0, len(graph))
	for node := range graph {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	var visit func(string) []string
	visit = func(node string) []string {
		state[node] = visiting
		stack = append(stack, node)
		succs := sortedSetKeys(graph[node])
		for _, next := range succs {
			switch state[next] {
			case unvisited:
				if cycle := visit(next); len(cycle) > 0 {
					return cycle
				}
			case visiting:
				return graphCyclePath(stack, next)
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = visited
		return nil
	}

	for _, node := range nodes {
		if state[node] != unvisited {
			continue
		}
		if cycle := visit(node); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

func graphCyclePath(stack []string, repeated string) []string {
	start := 0
	for idx, node := range stack {
		if node == repeated {
			start = idx
			break
		}
	}
	cycle := append([]string{}, stack[start:]...)
	cycle = append(cycle, repeated)
	return cycle
}
