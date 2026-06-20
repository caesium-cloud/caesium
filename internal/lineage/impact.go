package lineage

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ImpactNode describes a dataset (or the step that produces it) that is
// transitively downstream of a changed dataset.  Each node carries the
// producing step name, the job alias, and git provenance so callers can
// attribute "who wrote this and when."
type ImpactNode struct {
	// DatasetNamespace and DatasetName are the OpenLineage identity of the
	// downstream dataset.
	DatasetNamespace string `json:"dataset_namespace"`
	DatasetName      string `json:"dataset_name"`
	// Direction is always "output" for nodes returned by Impact — each node
	// is a dataset produced by a downstream step.
	Direction string `json:"direction"`

	// ProducingStep is the task name that emits this dataset, sourced from
	// FacetSummary.caesium_dataset.step_name when available.
	ProducingStep string `json:"producing_step,omitempty"`

	// JobID and JobAlias identify the job that contains the producing step.
	// Populated via a join from task_runs → job_runs → jobs.
	JobID    uuid.UUID `json:"job_id"`
	JobAlias string    `json:"job_alias"`

	// ProvenanceCommit and ProvenanceRepo carry git provenance from the job
	// at the time the dataset row was last written.
	ProvenanceCommit string `json:"provenance_commit,omitempty"`
	ProvenanceRepo   string `json:"provenance_repo,omitempty"`

	// LastSeen is the created_at timestamp of the lineage_dataset row — a
	// proxy for "when was this dependency last observed."
	LastSeen time.Time `json:"last_seen"`

	// Depth is the transitive hop count from the root dataset (0 = direct
	// consumer, 1 = consumer of a consumer, …).
	Depth int `json:"depth"`
}

// ImpactResult is the response shape returned by QueryImpact.
type ImpactResult struct {
	// RootNamespace and RootName are the input dataset whose downstream
	// impact is being reported.
	RootNamespace string `json:"root_namespace"`
	RootName      string `json:"root_name"`

	// Downstream lists every transitively reachable output dataset, ordered
	// breadth-first (shallowest nodes first within each depth level).
	// When the same (namespace, name) is produced by multiple distinct
	// task runs / jobs, each (namespace+name+job_id) triple appears as a
	// separate node; BFS frontier expansion is keyed only on dataset identity
	// so the graph traversal still terminates.
	Downstream []ImpactNode `json:"downstream"`
}

// QueryImpact returns all datasets transitively downstream of the dataset
// identified by (namespace, name) — i.e. datasets whose producing steps
// consume namespace/name as an input, directly or transitively, across job
// boundaries.
//
// maxDepth controls how many hops to traverse:
//   - 0 (or unset): use the server default of 10.
//   - 1–20: traverse that many hops.
//   - >20: capped at 20.
//
// The lineage_dataset table records (task_run_id, namespace, name, direction)
// rows for every observed input and output.  Two rows form an edge when an
// output dataset in one task run shares its (namespace, name) with an input
// dataset in another task run: the consumer's task run is the next hop.
//
// This is a pure read-side query over the existing dataset graph populated by
// C1.  It does not touch mapper.go or any write path.
func QueryImpact(ctx context.Context, db *gorm.DB, namespace, name string, maxDepth int) (*ImpactResult, error) {
	const defaultMaxDepth = 10
	const absoluteMaxDepth = 20

	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	if maxDepth > absoluteMaxDepth {
		maxDepth = absoluteMaxDepth
	}

	result := &ImpactResult{
		RootNamespace: namespace,
		RootName:      name,
		Downstream:    []ImpactNode{},
	}

	// visitedDatasets tracks (namespace, name) pairs we have already
	// enqueued into the BFS frontier, preventing cycles and redundant
	// traversal waves.  It is intentionally separate from the reported-nodes
	// dedup: a dataset can be produced by multiple jobs and all of those
	// (dataset, job) pairs are reported — but the dataset name itself is only
	// added to the frontier once so the BFS terminates.
	visitedDatasets := make(map[string]bool)
	rootKey := namespace + "\x00" + name
	visitedDatasets[rootKey] = true

	// frontier holds the (namespace, name) pairs whose direct consumers we
	// must find in the current BFS wave.
	frontier := []datasetRef{{namespace: namespace, name: name}}

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		consumers, err := findConsumers(ctx, db, frontier)
		if err != nil {
			return nil, err
		}
		if len(consumers) == 0 {
			break
		}

		var nextFrontier []datasetRef
		for _, c := range consumers {
			c.Depth = depth
			result.Downstream = append(result.Downstream, c)

			// Only add to the next frontier if we have not yet traversed
			// this dataset name — prevents cycles while still reporting
			// multiple producers of the same dataset name.
			key := c.DatasetNamespace + "\x00" + c.DatasetName
			if !visitedDatasets[key] {
				visitedDatasets[key] = true
				nextFrontier = append(nextFrontier, datasetRef{
					namespace: c.DatasetNamespace,
					name:      c.DatasetName,
				})
			}
		}
		frontier = nextFrontier
	}

	return result, nil
}

// datasetRef is a lightweight (namespace, name) pair used as BFS frontier state.
type datasetRef struct {
	namespace string
	name      string
}

// findConsumers runs a single SQL pass for one BFS level.  For each dataset
// in frontier it finds task runs that consume it (direction='input') via a
// correlated subquery, then returns those task runs' output datasets
// (direction='output') joined with job provenance.
//
// Using a subquery avoids loading intermediate task_run_id values into Go
// memory and sidesteps the SQLite/dqlite 999-host-parameter limit that would
// be hit by a large IN (?,?,…,?) list.
//
// Results are ordered by (ld.created_at DESC, ld.id DESC) so that when the
// same (namespace, name) output was produced by multiple task runs the most
// recent run is returned first, making attribution deterministic ("latest
// producer wins").
func findConsumers(ctx context.Context, db *gorm.DB, frontier []datasetRef) ([]ImpactNode, error) {
	if len(frontier) == 0 {
		return nil, nil
	}

	// Build the frontier (namespace = ? AND name = ?) OR arms.
	// The number of arms is bounded by the BFS frontier width, which is
	// small in practice (one arm per distinct dataset at this depth level).
	orArms := make([]string, len(frontier))
	// subArgs holds the placeholder values for the subquery's WHERE clause.
	subArgs := make([]interface{}, 0, len(frontier)*2)
	for i, f := range frontier {
		orArms[i] = "(namespace = ? AND name = ?)"
		subArgs = append(subArgs, f.namespace, f.name)
	}

	// The subquery selects task_run_ids that consumed any frontier dataset as
	// an input.  The ? placeholders are filled by subArgs in the Scan call.
	subquery := "SELECT task_run_id FROM lineage_datasets" +
		" WHERE direction = 'input' AND (" + strings.Join(orArms, " OR ") + ")"

	// The outer WHERE clause matches output rows whose task_run_id is in the
	// subquery.  Args: 'output' for ld.direction, then subArgs for the
	// embedded subquery placeholders.
	outerWhere := "ld.direction = ? AND ld.task_run_id IN (" + subquery + ")"
	queryArgs := append([]interface{}{"output"}, subArgs...)

	type outputRow struct {
		Namespace        string
		Name             string
		FacetSummary     []byte
		JobID            string
		JobAlias         string
		ProvenanceCommit string
		ProvenanceRepo   string
		CreatedAt        time.Time
	}
	var outputRows []outputRow

	err := db.WithContext(ctx).
		Table("lineage_datasets ld").
		Select(
			"ld.namespace, ld.name, ld.facet_summary, ld.created_at,"+
				" j.id as job_id, j.alias as job_alias,"+
				" j.provenance_commit, j.provenance_repo",
		).
		Joins("JOIN task_runs tr ON tr.id = ld.task_run_id").
		Joins("JOIN job_runs jr ON jr.id = tr.job_run_id").
		Joins("JOIN jobs j ON j.id = jr.job_id").
		Where(outerWhere, queryArgs...).
		Order("ld.created_at DESC, ld.id DESC").
		Scan(&outputRows).Error
	if err != nil {
		return nil, err
	}

	nodes := make([]ImpactNode, 0, len(outputRows))
	for _, row := range outputRows {
		jobID, err := uuid.Parse(row.JobID)
		if err != nil {
			// A zero UUID signals to callers that provenance attribution
			// failed for this row (e.g. a schema mismatch during migration).
			// We still include the node so the dataset graph is complete.
			jobID = uuid.Nil
		}
		nodes = append(nodes, ImpactNode{
			DatasetNamespace: row.Namespace,
			DatasetName:      row.Name,
			Direction:        "output",
			ProducingStep:    stepNameFromFacet(row.FacetSummary),
			JobID:            jobID,
			JobAlias:         row.JobAlias,
			ProvenanceCommit: row.ProvenanceCommit,
			ProvenanceRepo:   row.ProvenanceRepo,
			LastSeen:         row.CreatedAt,
		})
	}
	return nodes, nil
}

// stepNameFromFacet extracts the step_name field from the FacetSummary JSON
// blob stored on the lineage_dataset row.  Returns "" on any parse error so
// the caller degrades gracefully.
func stepNameFromFacet(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var summary struct {
		CaesiumDataset struct {
			StepName string `json:"step_name"`
		} `json:"caesium_dataset"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		return ""
	}
	return summary.CaesiumDataset.StepName
}
