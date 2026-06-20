package lineage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
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
	Downstream []ImpactNode `json:"downstream"`
}

// QueryImpact returns all datasets transitively downstream of the dataset
// identified by (namespace, name) — i.e. datasets whose producing steps
// consume namespace/name as an input, directly or transitively, across job
// boundaries.  The traversal is bounded by maxDepth (≤ 20; pass 0 to use the
// default of 10).
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

	// visited tracks (namespace, name) pairs we have already added to the
	// result set, preventing cycles in the graph.
	visited := make(map[string]bool)
	rootKey := namespace + "\x00" + name
	visited[rootKey] = true

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
			key := c.DatasetNamespace + "\x00" + c.DatasetName
			if visited[key] {
				continue
			}
			visited[key] = true
			c.Depth = depth
			result.Downstream = append(result.Downstream, c)
			nextFrontier = append(nextFrontier, datasetRef{
				namespace: c.DatasetNamespace,
				name:      c.DatasetName,
			})
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

// findConsumers runs a two-step SQL pass for a single BFS level:
//
//  1. Find task_run_ids that have any frontier dataset as an input
//     (direction='input').
//  2. Return those task runs' output datasets (direction='output') joined
//     with job provenance so the caller has attribution in one query.
func findConsumers(ctx context.Context, db *gorm.DB, frontier []datasetRef) ([]ImpactNode, error) {
	if len(frontier) == 0 {
		return nil, nil
	}

	// Step 1: collect consumer task_run_ids.
	type taskRunIDRow struct{ TaskRunID string }
	var inputRows []taskRunIDRow

	q := db.WithContext(ctx).
		Model(&models.LineageDataset{}).
		Select("task_run_id").
		Where("direction = ?", "input")

	orCond := db.Where("namespace = ? AND name = ?", frontier[0].namespace, frontier[0].name)
	for _, f := range frontier[1:] {
		orCond = orCond.Or("namespace = ? AND name = ?", f.namespace, f.name)
	}
	q = q.Where(orCond)

	if err := q.Scan(&inputRows).Error; err != nil {
		return nil, err
	}
	if len(inputRows) == 0 {
		return nil, nil
	}

	taskRunIDs := make([]string, len(inputRows))
	for i, r := range inputRows {
		taskRunIDs[i] = r.TaskRunID
	}

	// Step 2: fetch the output datasets for those task_run_ids joined to
	// job provenance via task_runs → job_runs → jobs.
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
		Where("ld.direction = ? AND ld.task_run_id IN ?", "output", taskRunIDs).
		Scan(&outputRows).Error
	if err != nil {
		return nil, err
	}

	nodes := make([]ImpactNode, 0, len(outputRows))
	for _, row := range outputRows {
		jobID, _ := uuid.Parse(row.JobID)
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
