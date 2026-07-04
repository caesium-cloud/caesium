package mcp

import "encoding/json"

// Tool is an MCP tool descriptor.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema any    `json:"inputSchema"`
}

// InitializeResult is the MCP initialize response payload.
type InitializeResult struct {
	ProtocolVersion string       `json:"protocolVersion"`
	Capabilities    Capabilities `json:"capabilities"`
	ServerInfo      ServerInfo   `json:"serverInfo"`
}

// Capabilities advertises supported MCP server capabilities.
type Capabilities struct {
	Tools map[string]any `json:"tools"`
}

// ServerInfo identifies this MCP server surface.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ListToolsResult is the tools/list response payload.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
}

// ToolCallParams is the tools/call params payload.
type ToolCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// ToolCallResult is the MCP tools/call result payload.
type ToolCallResult struct {
	Content           []ToolContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
}

// ToolContent is one MCP content item.
type ToolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// NewToolCallResult returns both structured JSON and a text JSON representation
// for MCP clients that only consume content[].
func NewToolCallResult(payload any) (ToolCallResult, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	text, err := json.Marshal(payload)
	if err != nil {
		return ToolCallResult{}, err
	}
	return ToolCallResult{
		Content: []ToolContent{
			{Type: "text", Text: string(text)},
		},
		StructuredContent: payload,
	}, nil
}

// AgentTools are the four Caesium agent tools exposed over MCP. The incident id
// is intentionally absent: it comes only from /v1/agent/incidents/:id/mcp.
var AgentTools = []Tool{
	{
		Name:        "get_bundle",
		Description: "Fetch the incident triage bundle for this agent session.",
		InputSchema: objectSchema(nil, nil),
	},
	{
		Name:        "get_context",
		Description: "Fetch scoped incident context: failing logs, run history, or why output.",
		InputSchema: objectSchema(map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"description": "Context kind to fetch.",
				"enum":        []string{"logs", "history", "why"},
			},
			"job": map[string]any{
				"type":        "string",
				"description": "Optional job alias for history; must be in the incident allowlist unless it is the incident job.",
			},
			"task": map[string]any{
				"type":        "string",
				"description": "Task name for why context.",
			},
		}, []string{"kind"}),
	},
	{
		Name:        "propose_action",
		Description: "Propose or execute a typed remediation action through Caesium's server-side executor.",
		InputSchema: objectSchema(map[string]any{
			"type": map[string]any{
				"type":        "string",
				"description": "Typed remediation action name.",
			},
			"params": map[string]any{
				"type":        "object",
				"description": "Action-specific parameters.",
			},
		}, []string{"type"}),
	},
	{
		Name:        "add_note",
		Description: "Append a free-text finding to the incident timeline.",
		InputSchema: objectSchema(map[string]any{
			"text": map[string]any{
				"type":        "string",
				"description": "Timeline note text.",
			},
		}, []string{"text"}),
	},
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
