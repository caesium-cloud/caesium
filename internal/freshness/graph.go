package freshness

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
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

// GraphError is a declared-graph violation. Error() is the historical message;
// the fields name the identities in it so a response boundary can redact the
// ones a namespace-confined caller may not see (identity-and-access B2).
type GraphError struct {
	// Err is ErrDatasetMultipleProducers, ErrDatasetUnresolvedConsumes or
	// ErrDatasetGraphCycle.
	Err error
	// Dataset is the dataset concerned; empty for a cycle.
	Dataset string
	// Jobs are the producer aliases (multiple producers), the consumer alias
	// (unresolved consume) or the cycle path (cycle), in message order.
	Jobs []string
	msg  string
}

func (e *GraphError) Error() string { return e.msg }

func (e *GraphError) Unwrap() error { return e.Err }

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
	producers := make(map[string]map[graphJob]struct{}) // dataset name -> set of producer jobs
	sources := make(map[string]struct{})                // dataset names declared as a source
	type consumeEdge struct {
		job  graphJob
		name string
	}
	consumes := make([]consumeEdge, 0)

	for i := range decls {
		d := &decls[i]
		name := strings.TrimSpace(d.Name)
		job := graphJob{alias: strings.TrimSpace(d.JobAlias), id: d.JobID}
		if name == "" {
			continue
		}
		switch d.Direction {
		case models.DatasetDirectionProduces:
			if producers[name] == nil {
				producers[name] = make(map[graphJob]struct{})
			}
			if job.alias != "" {
				producers[name][job] = struct{}{}
			}
		case models.DatasetDirectionSource:
			sources[name] = struct{}{}
		case models.DatasetDirectionConsumes:
			consumes = append(consumes, consumeEdge{job: job, name: name})
		}
	}

	// Single-producer: at most one job may produce a given dataset.
	for _, name := range slices.Sorted(maps.Keys(producers)) {
		if len(producers[name]) > 1 {
			owners := graphJobAliases(sortedGraphJobs(producers[name]))
			return &GraphError{Err: ErrDatasetMultipleProducers, Dataset: name, Jobs: owners,
				msg: fmt.Sprintf("%s: %q produced by %s", ErrDatasetMultipleProducers, name, strings.Join(owners, ", "))}
		}
	}

	// Resolution: every consumed dataset must be produced or declared as a source.
	sort.Slice(consumes, func(i, j int) bool {
		if consumes[i].name != consumes[j].name {
			return consumes[i].name < consumes[j].name
		}
		return consumes[i].job.less(consumes[j].job)
	})
	for _, c := range consumes {
		if _, ok := producers[c.name]; ok {
			continue
		}
		if _, ok := sources[c.name]; ok {
			continue
		}
		return &GraphError{Err: ErrDatasetUnresolvedConsumes, Dataset: c.name, Jobs: []string{c.job.alias},
			msg: fmt.Sprintf("%s: job %q consumes %q which is not produced by any job or declared as a source", ErrDatasetUnresolvedConsumes, c.job.alias, c.name)}
	}

	// Cross-job cycle detection over producer→consumer edges. An edge P→C means
	// job C consumes a dataset produced by job P.
	graph := make(map[graphJob]map[graphJob]struct{})
	addEdge := func(from, to graphJob) {
		if from.alias == "" || to.alias == "" || from == to {
			return
		}
		if graph[from] == nil {
			graph[from] = make(map[graphJob]struct{})
		}
		graph[from][to] = struct{}{}
	}
	for _, c := range consumes {
		for producer := range producers[c.name] {
			addEdge(producer, c.job)
		}
	}
	if cycle := graphJobAliases(detectGraphCycle(graph)); len(cycle) > 0 {
		return &GraphError{Err: ErrDatasetGraphCycle, Jobs: cycle,
			msg: fmt.Sprintf("%s: %s", ErrDatasetGraphCycle, strings.Join(cycle, " -> "))}
	}

	return nil
}

// graphJob identifies a job in the declared graph. Persisted declarations
// carry their job's id; the definitions being validated have none yet
// (uuid.Nil) and are identified by alias, which ValidateAlias keeps
// canonical. The alias stored on a declaration is trimmed, so a legacy job
// whose alias carries surrounding whitespace reads exactly like its canonical
// twin: keying on the alias alone merged the two into one producer
// (identity-and-access W7-ε).
type graphJob struct {
	alias string
	id    uuid.UUID
}

// less orders jobs by alias, the order the diagnostics have always used, with
// the job id as the tie-breaker between same-alias jobs.
func (j graphJob) less(o graphJob) bool {
	if j.alias != o.alias {
		return j.alias < o.alias
	}
	return bytes.Compare(j.id[:], o.id[:]) < 0
}

func sortedGraphJobs(m map[graphJob]struct{}) []graphJob {
	jobs := slices.Collect(maps.Keys(m))
	slices.SortFunc(jobs, func(a, b graphJob) int {
		switch {
		case a.less(b):
			return -1
		case b.less(a):
			return 1
		}
		return 0
	})
	return jobs
}

func graphJobAliases(jobs []graphJob) []string {
	if jobs == nil {
		return nil
	}
	out := make([]string, len(jobs))
	for i, job := range jobs {
		out[i] = job.alias
	}
	return out
}

// detectGraphCycle returns a readable node path if the directed graph contains a
// cycle, or nil otherwise. Iteration is over sorted nodes/edges for a
// deterministic path in the error message.
func detectGraphCycle(graph map[graphJob]map[graphJob]struct{}) []graphJob {
	const (
		unvisited = iota
		visiting
		visited
	)
	state := make(map[graphJob]int, len(graph))
	stack := make([]graphJob, 0, len(graph))

	nodes := make(map[graphJob]struct{}, len(graph))
	for node := range graph {
		nodes[node] = struct{}{}
	}

	var visit func(graphJob) []graphJob
	visit = func(node graphJob) []graphJob {
		state[node] = visiting
		stack = append(stack, node)
		for _, next := range sortedGraphJobs(graph[node]) {
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

	for _, node := range sortedGraphJobs(nodes) {
		if state[node] != unvisited {
			continue
		}
		if cycle := visit(node); len(cycle) > 0 {
			return cycle
		}
	}
	return nil
}

func graphCyclePath(stack []graphJob, repeated graphJob) []graphJob {
	start := 0
	for idx, node := range stack {
		if node == repeated {
			start = idx
			break
		}
	}
	cycle := append([]graphJob{}, stack[start:]...)
	cycle = append(cycle, repeated)
	return cycle
}
