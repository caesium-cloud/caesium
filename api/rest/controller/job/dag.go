package job

import (
	"errors"
	"net/http"
	"sort"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"gorm.io/gorm"
)

type DAGResponse struct {
	JobID uuid.UUID `json:"job_id"`
	Nodes []DAGNode `json:"nodes"`
	Edges []DAGEdge `json:"edges"`
}

type DAGNode struct {
	ID         uuid.UUID   `json:"id"`
	AtomID     uuid.UUID   `json:"atom_id"`
	NextID     *uuid.UUID  `json:"next_id,omitempty"`
	Successors []uuid.UUID `json:"successors"`
}

type DAGEdge struct {
	From uuid.UUID `json:"from"`
	To   uuid.UUID `json:"to"`
}

func DAG(c echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.ErrBadRequest.SetInternal(err)
	}

	if _, err = job.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.ErrInternalServerError.SetInternal(err)
	}

	tasks, err := task.Service(ctx).List(&task.ListRequest{
		JobID:   id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	rawEdges, err := taskedge.Service(ctx).List(&taskedge.ListRequest{
		JobID:   id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		return echo.ErrInternalServerError.SetInternal(err)
	}

	edgeSet := make(map[uuid.UUID]map[uuid.UUID]struct{}, len(tasks))
	addEdge := func(from, to uuid.UUID) {
		targets, ok := edgeSet[from]
		if !ok {
			targets = make(map[uuid.UUID]struct{})
			edgeSet[from] = targets
		}
		targets[to] = struct{}{}
	}

	for _, edge := range rawEdges {
		addEdge(edge.FromTaskID, edge.ToTaskID)
	}

	if len(rawEdges) == 0 {
		for _, t := range tasks {
			if t.NextID == nil {
				continue
			}
			addEdge(t.ID, *t.NextID)
		}
	}

	nodes := make([]DAGNode, 0, len(tasks))
	edges := make([]DAGEdge, 0, len(tasks))

	for _, t := range tasks {
		successors := make([]uuid.UUID, 0)
		if targets, ok := edgeSet[t.ID]; ok {
			for to := range targets {
				successors = append(successors, to)
			}
		}

		if len(successors) > 1 {
			sort.Slice(successors, func(i, j int) bool {
				return successors[i].String() < successors[j].String()
			})
		}

		for _, to := range successors {
			edges = append(edges, DAGEdge{
				From: t.ID,
				To:   to,
			})
		}

		var nextID *uuid.UUID
		switch len(successors) {
		case 0:
			if t.NextID != nil {
				val := *t.NextID
				nextID = &val
			}
		case 1:
			val := successors[0]
			nextID = &val
		}

		nodes = append(nodes, DAGNode{
			ID:         t.ID,
			AtomID:     t.AtomID,
			NextID:     nextID,
			Successors: successors,
		})
	}

	resp := &DAGResponse{
		JobID: id,
		Nodes: nodes,
		Edges: edges,
	}

	return c.JSON(http.StatusOK, resp)
}
