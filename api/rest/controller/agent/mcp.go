package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	agentsvc "github.com/caesium-cloud/caesium/api/rest/service/agent"
	"github.com/caesium-cloud/caesium/internal/mcp"
	"github.com/caesium-cloud/caesium/internal/models"
	runstorage "github.com/caesium-cloud/caesium/internal/run"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"gorm.io/gorm"
)

// MCP handles POST /v1/agent/incidents/:id/mcp: a synchronous JSON-RPC 2.0 MCP
// endpoint for the same incident-scoped agent tool surface as the REST routes.
// The incident id is read only from the route, so the auth middleware's
// per-incident agent-token confinement applies before JSON-RPC dispatch.
func MCP(c *echo.Context) error {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid incident id")
	}

	svc := agentsvc.New(c.Request().Context())
	inc, err := svc.Incident(id)
	if err != nil {
		if errors.Is(err, agentsvc.ErrIncidentNotFound) {
			return echo.ErrNotFound
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	allowed := authmw.GetAllowedJobAliases(c)
	dispatcher := mcp.Dispatcher{
		Tools:               mcp.AgentTools,
		ReservedParamFields: []string{"incident_id", "incidentId", "incidentID"},
		CallTool: func(name string, arguments json.RawMessage) (any, *mcp.Error) {
			return callMCPAgentTool(svc, inc, allowed, name, arguments)
		},
	}

	return c.JSON(http.StatusOK, mcp.Handle(c.Request().Body, dispatcher))
}

type getContextArgs struct {
	Kind string `json:"kind"`
	Job  string `json:"job,omitempty"`
	Task string `json:"task,omitempty"`
}

type proposeActionArgs struct {
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params,omitempty"`
}

type addNoteArgs struct {
	Text string `json:"text"`
}

func callMCPAgentTool(svc *agentsvc.Service, inc *models.Incident, allowed []string, name string, arguments json.RawMessage) (any, *mcp.Error) {
	switch name {
	case "get_bundle":
		var args struct{}
		if rpcErr := decodeMCPToolArguments(arguments, &args); rpcErr != nil {
			return nil, rpcErr
		}
		bundle, err := svc.Bundle(inc.ID)
		if err != nil {
			return nil, mapMCPAgentError(err)
		}
		return bundle, nil

	case "get_context":
		var args getContextArgs
		if rpcErr := decodeMCPToolArguments(arguments, &args); rpcErr != nil {
			return nil, rpcErr
		}
		switch strings.TrimSpace(args.Kind) {
		case "logs":
			text, ok, err := svc.FailingLog(inc)
			if err != nil {
				return nil, mapMCPAgentError(err)
			}
			return map[string]any{"log_tail": text, "available": ok, "scrubbed": true}, nil
		case "history":
			runs, err := svc.History(inc, strings.TrimSpace(args.Job), allowed)
			if err != nil {
				return nil, mapMCPAgentError(err)
			}
			return map[string]any{"runs": runs}, nil
		case "why":
			task := strings.TrimSpace(args.Task)
			if task == "" {
				return nil, mcp.InvalidParams("task is required for why context")
			}
			explanation, err := svc.Why(inc, task)
			if err != nil {
				return nil, mapMCPAgentError(err)
			}
			return explanation, nil
		default:
			return nil, mcp.InvalidParams("unknown context kind")
		}

	case "propose_action":
		var args proposeActionArgs
		if rpcErr := decodeMCPToolArguments(arguments, &args); rpcErr != nil {
			return nil, rpcErr
		}
		result, err := svc.ProposeAction(inc, agentsvc.ActionRequest{
			Type:   strings.TrimSpace(args.Type),
			Params: args.Params,
		})
		if err != nil {
			return nil, mapMCPAgentError(err)
		}
		return result, nil

	case "add_note":
		var args addNoteArgs
		if rpcErr := decodeMCPToolArguments(arguments, &args); rpcErr != nil {
			return nil, rpcErr
		}
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return nil, mcp.InvalidParams("text is required")
		}
		action, err := svc.Note(inc, text)
		if err != nil {
			return nil, mapMCPAgentError(err)
		}
		return action, nil

	default:
		return nil, mcp.NotFound("unknown tool")
	}
}

func decodeMCPToolArguments(raw json.RawMessage, target any) *mcp.Error {
	if hasIncidentIDArgument(raw) {
		return mcp.InvalidParams("incident id must be supplied in the URL path, not MCP arguments")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return mcp.InvalidParams("invalid tool arguments")
	}
	var extra any
	if err := dec.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		return mcp.InvalidParams("invalid tool arguments")
	}
	return nil
}

func hasIncidentIDArgument(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, snake := fields["incident_id"]
	_, lowerCamel := fields["incidentId"]
	_, upperCamel := fields["incidentID"]
	return snake || lowerCamel || upperCamel
}

func mapMCPAgentError(err error) *mcp.Error {
	switch {
	case errors.Is(err, agentsvc.ErrUnknownActionType):
		return mcp.InvalidParams(err.Error())
	case errors.Is(err, agentsvc.ErrForbiddenJob):
		return mcp.Forbidden(agentsvc.ErrForbiddenJob.Error())
	case errors.Is(err, agentsvc.ErrIncidentNotFound), errors.Is(err, agentsvc.ErrNoFailingRun), errors.Is(err, gorm.ErrRecordNotFound), errors.Is(err, runstorage.ErrTaskRunNotFound):
		return mcp.NotFound("not found")
	default:
		return mcp.InternalError()
	}
}
