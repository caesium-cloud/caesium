// Package blame attributes current DAG elements to the snapshot that introduced
// their persisted topology descriptor.
package blame

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const CoverageTopologyImageCommand = "topology+image+command"

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
}

type EdgeElement struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type EdgeAttribution struct {
	Element           EdgeElement `json:"element"`
	IntroducingCommit string      `json:"introducing_commit"`
	SnapshotID        uuid.UUID   `json:"snapshot_id"`
	ProvenanceCommit  string      `json:"provenance_commit,omitempty"`
}

func (q *Query) Blame(ctx context.Context, jobID uuid.UUID, opts Options) (*Result, error) {
	result := &Result{
		JobID:      jobID,
		Coverage:   CoverageTopologyImageCommand,
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
		return result, nil
	}

	sortSnapshotsAscending(snaps)

	fromIdx, toIdx, err := rangeIndexes(snaps, opts)
	if err != nil {
		return nil, err
	}

	taskIntroductions := make(map[taskKey]introduction)
	edgeIntroductions := make(map[edgeKey]introduction)
	prevTasks := map[taskKey]struct{}{}
	prevEdges := map[edgeKey]models.DagSnapshotEdge{}

	var currentTasks []models.DagSnapshotTask
	var currentEdges []models.DagSnapshotEdge
	for i := 0; i <= toIdx; i++ {
		tasks, edges, err := parseSnapshot(snaps[i])
		if err != nil {
			return nil, err
		}

		taskSet := make(map[taskKey]struct{}, len(tasks))
		for _, task := range tasks {
			key := newTaskKey(task)
			taskSet[key] = struct{}{}
			if i >= fromIdx {
				if _, ok := prevTasks[key]; !ok {
					taskIntroductions[key] = newIntroduction(snaps[i], "")
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
		intro, ok := taskIntroductions[newTaskKey(task)]
		if !ok {
			continue
		}
		result.Tasks = append(result.Tasks, TaskAttribution{
			Element:           task,
			IntroducingCommit: intro.commit,
			SnapshotID:        intro.snapshotID,
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
			ProvenanceCommit:  intro.provenanceCommit,
		})
	}

	return result, nil
}

type introduction struct {
	snapshotID       uuid.UUID
	commit           string
	provenanceCommit string
}

func newIntroduction(snap models.DagSnapshot, provenanceCommit string) introduction {
	return introduction{
		snapshotID:       snap.ID,
		commit:           snap.GitCommit,
		provenanceCommit: provenanceCommit,
	}
}

type taskKey struct {
	name    string
	image   string
	command string
}

func newTaskKey(task models.DagSnapshotTask) taskKey {
	command, _ := json.Marshal(task.Command)
	return taskKey{
		name:    task.Name,
		image:   task.Image,
		command: string(command),
	}
}

type edgeKey struct {
	from string
	to   string
}

func newEdgeKey(edge models.DagSnapshotEdge) edgeKey {
	return edgeKey{from: edge.From, to: edge.To}
}

// sortSnapshotsAscending orders snapshots oldest-first so the walk attributes
// each element to the first snapshot that contains its descriptor. DagSnapshot
// has no monotonic sequence column, so CreatedAt is the only temporal key; on
// an exact CreatedAt collision the tiebreak is the (stable but not
// insertion-faithful) snapshot ID. Two transition-bearing snapshots written at
// the same wall-clock instant could therefore be ordered arbitrarily — a rare
// edge case for a read-only diagnostic. An insertion-faithful tiebreak would
// require an additive monotonic column on dag_snapshot (deferred substrate
// enhancement, tracked with the broader full-descriptor blame work).
func sortSnapshotsAscending(snaps []models.DagSnapshot) {
	sort.Slice(snaps, func(i, j int) bool {
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
	for i := len(snaps) - 1; i >= 0; i-- {
		if snaps[i].GitCommit == commit {
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
