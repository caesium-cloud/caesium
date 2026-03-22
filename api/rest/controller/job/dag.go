package job

import (
	"cmp"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/api/rest/service/task"
	"github.com/caesium-cloud/caesium/api/rest/service/taskedge"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

type DAGResponse struct {
	JobID uuid.UUID `json:"job_id"`
	Nodes []DAGNode `json:"nodes"`
	Edges []DAGEdge `json:"edges"`
}

type DAGNode struct {
	ID           uuid.UUID       `json:"id"`
	AtomID       uuid.UUID       `json:"atom_id"`
	Type         string          `json:"type,omitempty"`
	Successors   []uuid.UUID     `json:"successors"`
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
}

type DAGEdge struct {
	From            uuid.UUID `json:"from"`
	To              uuid.UUID `json:"to"`
	ContractDefined bool      `json:"contract_defined,omitempty"`
}

func DAG(c *echo.Context) error {
	ctx := c.Request().Context()

	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "bad request").Wrap(err)
	}

	if _, err = job.Service(ctx).Get(id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return echo.ErrNotFound
		}

		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	tasks, err := task.Service(ctx).List(&task.ListRequest{
		JobID:   id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	rawEdges, err := taskedge.Service(ctx).List(&taskedge.ListRequest{
		JobID:   id.String(),
		OrderBy: []string{"created_at"},
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
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

	// contractEdges: set of (from,to) edges where the consumer has inputSchema referencing the producer.
	type edgeKey struct{ from, to uuid.UUID }
	contractEdges := make(map[edgeKey]struct{})
	for _, t := range tasks {
		if len(t.InputSchema) == 0 {
			continue
		}
		var inputSchema map[string]any
		if err := json.Unmarshal(t.InputSchema, &inputSchema); err != nil {
			continue
		}
		// Find predecessor tasks by name and mark their edges as contract-bearing.
		for producerName := range inputSchema {
			for _, candidate := range tasks {
				if candidate.Name == producerName {
					contractEdges[edgeKey{from: candidate.ID, to: t.ID}] = struct{}{}
					break
				}
			}
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
			slices.SortFunc(successors, func(a, b uuid.UUID) int {
				return cmp.Compare(a.String(), b.String())
			})
		}

		for _, to := range successors {
			_, hasContract := contractEdges[edgeKey{from: t.ID, to: to}]
			edges = append(edges, DAGEdge{
				From:            t.ID,
				To:              to,
				ContractDefined: hasContract,
			})
		}

		nodeType := t.Type
		if nodeType == "task" {
			nodeType = "" // omit default type to keep response compact
		}

		node := DAGNode{
			ID:         t.ID,
			AtomID:     t.AtomID,
			Type:       nodeType,
			Successors: successors,
		}
		if len(t.OutputSchema) > 0 {
			node.OutputSchema = json.RawMessage(t.OutputSchema)
		}
		if len(t.InputSchema) > 0 {
			node.InputSchema = json.RawMessage(t.InputSchema)
		}
		nodes = append(nodes, node)
	}

	resp := &DAGResponse{
		JobID: id,
		Nodes: nodes,
		Edges: edges,
	}

	return c.JSON(http.StatusOK, resp)
}
