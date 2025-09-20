package job

import (
	"errors"
	"net/http"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
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
	ID     uuid.UUID  `json:"id"`
	AtomID uuid.UUID  `json:"atom_id"`
	NextID *uuid.UUID `json:"next_id,omitempty"`
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

	nodes := make([]DAGNode, 0, len(tasks))
	edges := make([]DAGEdge, 0, len(tasks))

	for _, t := range tasks {
		nodes = append(nodes, DAGNode{
			ID:     t.ID,
			AtomID: t.AtomID,
			NextID: t.NextID,
		})

		if t.NextID != nil {
			edges = append(edges, DAGEdge{
				From: t.ID,
				To:   *t.NextID,
			})
		}
	}

	resp := &DAGResponse{
		JobID: id,
		Nodes: nodes,
		Edges: edges,
	}

	return c.JSON(http.StatusOK, resp)
}
