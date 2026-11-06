// Package blame attributes current DAG elements to the snapshot that introduced
// their persisted topology descriptor.
package blame

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/caesium-cloud/caesium/internal/authz"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// CoverageTopologyImageCommand is the coverage of snapshots written before
	// the descriptor carried behavior. Blame no longer reports it; it is kept so
	// callers comparing against the historical value still compile.
	CoverageTopologyImageCommand = "topology+image+command"
	// CoverageStepSpec is the coverage Blame reports: topology, image, command
	// and every other step-level behavior field (env digests, nodeSelector,
	// retries, triggerRule, cache, input/output schemas, volumes, ...).
	// Job-level settings (taskTimeout, runTimeout, sla) are not step fields and
	// are not covered.
	CoverageStepSpec = "topology+image+command+step-spec"
)

var (
	ErrCommitNotFound = errors.New("blame: commit not found")
	ErrInvalidRange   = errors.New("blame: invalid commit range")
)

type Options struct {
	Task       string
	FromCommit string
	ToCommit   string
}

type Query struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Query {
	return &Query{db: db}
}

type Result struct {
	JobID      uuid.UUID         `json:"job_id"`
	Coverage   string            `json:"coverage"`
	FromCommit string            `json:"from_commit,omitempty"`
	ToCommit   string            `json:"to_commit,omitempty"`
	Tasks      []TaskAttribution `json:"tasks"`
	Edges      []EdgeAttribution `json:"edges"`
}

type TaskAttribution struct {
	Element           models.DagSnapshotTask `json:"element"`
	IntroducingCommit string                 `json:"introducing_commit"`
	SnapshotID        uuid.UUID              `json:"snapshot_id"`
	// Author and Ref are the introducing snapshot's recorded author and git
	// ref. Empty for snapshots written before they were captured.
	Author string `json:"author,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

type EdgeElement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type EdgeAttribution struct {
	Element           EdgeElement `json:"element"`
	IntroducingCommit string      `json:"introducing_commit"`
	SnapshotID        uuid.UUID   `json:"snapshot_id"`
	Author            string      `json:"author,omitempty"`
	Ref               string      `json:"ref,omitempty"`
	// ProvenanceCommit is the edge's provenance_commit as recorded at its
	// *introducing* snapshot. provenance_commit is excluded from the snapshot
	// ContentHash, so a later apply can change an edge's provenance without
	// writing a new snapshot row; when that happens this reflects the
	// introducing snapshot's value, not the current one — the same honest
	// limitation as the step-spec coverage caveat.
	ProvenanceCommit string `json:"provenance_commit,omitempty"`
}

func (q *Query) Blame(ctx context.Context, jobID uuid.UUID, opts Options) (*Result, error) {
	result := &Result{
		JobID:      jobID,
		Coverage:   CoverageStepSpec,
		FromCommit: opts.FromCommit,
		ToCommit:   opts.ToCommit,
		Tasks:      []TaskAttribution{},
		Edges:      []EdgeAttribution{},
	}

	snaps, err := jobdef.NewSnapshotQuery(ctx, q.db).List(jobID)
	if err != nil {
		return nil, fmt.Errorf("blame: list snapshots: %w", err)
	}
	if len(snaps) == 0 {
		// A confined caller (identity-and-access B7) with no readable
		// snapshot names a commit none of its snapshots carries, however
		// many unreadable ones exist: not found, as for any other missing
		// commit. An unrestricted caller of a job with no snapshots keeps the
		// empty result.
		if authz.Restricted(ctx) && (opts.FromCommit != "" || opts.ToCommit != "") {
			commit := opts.FromCommit
			if commit == "" {
				commit = opts.ToCommit
			}
			return nil, fmt.Errorf("%w: commit %q", ErrCommitNotFound, commit)
		}
		return result, nil
	}

	sortSnapshotsAscending(snaps)

	fromIdx, toIdx, err := rangeIndexes(snaps, opts)
	if err != nil {
		return nil, err
	}

	taskIntroductions := make(map[string]introduction)
	edgeIntroductions := make(map[edgeKey]introduction)
	prevTasks := map[string]models.DagSnapshotTask{}
	prevEdges := map[edgeKey]models.DagSnapshotEdge{}

	var currentTasks []models.DagSnapshotTask
	var currentEdges []models.DagSnapshotEdge
	for i := 0; i <= toIdx; i++ {
		// Honor the request deadline: a job with a long snapshot history
		// should not keep walking after the caller's context is cancelled.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tasks, edges, err := parseSnapshot(snaps[i])
		if err != nil {
			return nil, err
		}

		taskSet := make(map[string]models.DagSnapshotTask, len(tasks))
		for _, task := range tasks {
			taskSet[task.Name] = task
			if i >= fromIdx {
				if prev, ok := prevTasks[task.Name]; !ok || !sameTaskDescriptor(prev, task) {
					taskIntroductions[task.Name] = newIntroduction(snaps[i], "")
				}
			}
		}

		edgeSet := make(map[edgeKey]models.DagSnapshotEdge, len(edges))
		for _, edge := range edges {
			key := newEdgeKey(edge)
			edgeSet[key] = edge
			if i >= fromIdx {
				if _, ok := prevEdges[key]; !ok {
					edgeIntroductions[key] = newIntroduction(snaps[i], edge.ProvenanceCommit)
				}
			}
		}

		prevTasks = taskSet
		prevEdges = edgeSet
		if i == toIdx {
			currentTasks = tasks
			currentEdges = edges
		}
	}

	for _, task := range currentTasks {
		if opts.Task != "" && task.Name != opts.Task {
			continue
		}
		intro, ok := taskIntroductions[task.Name]
		if !ok {
			continue
		}
		result.Tasks = append(result.Tasks, TaskAttribution{
			Element:           task,
			IntroducingCommit: intro.commit,
			SnapshotID:        intro.snapshotID,
			Author:            intro.author,
			Ref:               intro.ref,
		})
	}

	for _, edge := range currentEdges {
		if opts.Task != "" && edge.From != opts.Task && edge.To != opts.Task {
			continue
		}
		intro, ok := edgeIntroductions[newEdgeKey(edge)]
		if !ok {
			continue
		}
		result.Edges = append(result.Edges, EdgeAttribution{
			Element: EdgeElement{
				From: edge.From,
				To:   edge.To,
			},
			IntroducingCommit: intro.commit,
			SnapshotID:        intro.snapshotID,
			Author:            intro.author,
			Ref:               intro.ref,
			ProvenanceCommit:  intro.provenanceCommit,
		})
	}

	return result, nil
}

type introduction struct {
	snapshotID       uuid.UUID
	commit           string
	author           string
	ref              string
	provenanceCommit string
}

func newIntroduction(snap models.DagSnapshot, provenanceCommit string) introduction {
	return introduction{
		snapshotID:       snap.ID,
		commit:           snap.GitCommit,
		author:           derefString(snap.Author),
		ref:              derefString(snap.Ref),
		provenanceCommit: provenanceCommit,
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sameTaskDescriptor reports whether two descriptors of the same-named task are
// the same for attribution. Name, image and command always compare. Behavior
// compares only when BOTH sides recorded it: a descriptor written before the
// snapshot was broadened (nil Behavior) has unknown, not empty, behavior, so
// the legacy-to-broadened boundary (the first apply after upgrade writes one
// new snapshot per job with identical topology) is never reported as a change,
// and attribution stays with the snapshot that introduced the unchanged
// fields. Behavior changes are attributed from the first snapshot that records
// behavior onwards.
func sameTaskDescriptor(a, b models.DagSnapshotTask) bool {
	if a.Name != b.Name || a.Image != b.Image || !slices.Equal(a.Command, b.Command) {
		return false
	}
	if len(a.Behavior) == 0 || len(b.Behavior) == 0 {
		return true
	}
	return canonicalJSON(a.Behavior) == canonicalJSON(b.Behavior)
}

// canonicalJSON re-encodes raw with sorted keys and no insignificant
// whitespace, so a store that reformats JSON cannot fake a change.
func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(out)
}

type edgeKey struct {
	from string
	to   string
}

func newEdgeKey(edge models.DagSnapshotEdge) edgeKey {
	return edgeKey{from: edge.From, to: edge.To}
}

// sortSnapshotsAscending orders snapshots oldest-first so the walk attributes
// each element to the first snapshot that contains its descriptor. The key is
// the per-job insertion Sequence, then CreatedAt, then ID: Sequence is assigned
// in the apply transaction (max+1) so it is insertion-faithful even when two
// snapshots share a CreatedAt or the wall clock stepped backwards. Rows written
// before the column existed are backfilled in (created_at, id) order; an
// unassigned row (Sequence 0, e.g. the backfill has not run) sorts before every
// assigned one and falls back to CreatedAt then ID among its peers.
func sortSnapshotsAscending(snaps []models.DagSnapshot) {
	sort.SliceStable(snaps, func(i, j int) bool {
		if snaps[i].Sequence != snaps[j].Sequence {
			return snaps[i].Sequence < snaps[j].Sequence
		}
		if !snaps[i].CreatedAt.Equal(snaps[j].CreatedAt) {
			return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
		}
		return snaps[i].ID.String() < snaps[j].ID.String()
	})
}

func rangeIndexes(snaps []models.DagSnapshot, opts Options) (int, int, error) {
	fromIdx := 0
	toIdx := len(snaps) - 1

	if opts.FromCommit != "" {
		idx, ok := firstCommitIndex(snaps, opts.FromCommit)
		if !ok {
			return 0, 0, fmt.Errorf("%w: from commit %q", ErrCommitNotFound, opts.FromCommit)
		}
		fromIdx = idx
	}
	if opts.ToCommit != "" {
		idx, ok := lastCommitIndex(snaps, opts.ToCommit)
		if !ok {
			return 0, 0, fmt.Errorf("%w: to commit %q", ErrCommitNotFound, opts.ToCommit)
		}
		toIdx = idx
	}
	if fromIdx > toIdx {
		return 0, 0, fmt.Errorf("%w: from commit %q is after to commit %q", ErrInvalidRange, opts.FromCommit, opts.ToCommit)
	}
	return fromIdx, toIdx, nil
}

func firstCommitIndex(snaps []models.DagSnapshot, commit string) (int, bool) {
	for i, snap := range snaps {
		if snap.GitCommit == commit {
			return i, true
		}
	}
	return 0, false
}

func lastCommitIndex(snaps []models.DagSnapshot, commit string) (int, bool) {
	for i, snap := range slices.Backward(snaps) {
		if snap.GitCommit == commit {
			return i, true
		}
	}
	return 0, false
}

func parseSnapshot(snap models.DagSnapshot) ([]models.DagSnapshotTask, []models.DagSnapshotEdge, error) {
	var tasks []models.DagSnapshotTask
	if err := json.Unmarshal(snap.Tasks, &tasks); err != nil {
		return nil, nil, fmt.Errorf("blame: parse snapshot %s tasks: %w", snap.ID, err)
	}
	var edges []models.DagSnapshotEdge
	if err := json.Unmarshal(snap.Edges, &edges); err != nil {
		return nil, nil, fmt.Errorf("blame: parse snapshot %s edges: %w", snap.ID, err)
	}
	return tasks, edges, nil
}
