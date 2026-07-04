package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const protocolVersion = "2024-11-05"

// ToolHandler executes one named MCP tool and returns the structured payload for
// the MCP tools/call result.
type ToolHandler func(name string, arguments json.RawMessage) (any, *Error)

// Dispatcher handles MCP's JSON-RPC methods over one synchronous HTTP POST.
type Dispatcher struct {
	Tools               []Tool
	CallTool            ToolHandler
	ReservedParamFields []string
}

// Dispatch handles initialize, tools/list, and tools/call.
func (d Dispatcher) Dispatch(req Request) Response {
	if field := findReservedField(req.Params, d.ReservedParamFields); field != "" {
		return ErrorResponse(req.ID, InvalidParams(field+" is not accepted in MCP params"))
	}

	switch req.Method {
	case "initialize":
		return SuccessResponse(req.ID, InitializeResult{
			ProtocolVersion: protocolVersion,
			Capabilities:    Capabilities{Tools: map[string]any{}},
			ServerInfo:      ServerInfo{Name: "caesium-agent-tools", Version: "0.1.0"},
		})
	case "tools/list":
		return SuccessResponse(req.ID, ListToolsResult{Tools: d.Tools})
	case "tools/call":
		result, rpcErr := d.dispatchToolCall(req.Params)
		if rpcErr != nil {
			return ErrorResponse(req.ID, rpcErr)
		}
		return SuccessResponse(req.ID, result)
	default:
		return ErrorResponse(req.ID, &Error{Code: CodeMethodNotFound, Message: "method not found"})
	}
}

func (d Dispatcher) dispatchToolCall(params json.RawMessage) (ToolCallResult, *Error) {
	if d.CallTool == nil {
		return ToolCallResult{}, &Error{Code: CodeMethodNotFound, Message: "method not found"}
	}
	if field := findReservedField(params, d.ReservedParamFields); field != "" {
		return ToolCallResult{}, InvalidParams(field + " is not accepted in MCP params")
	}

	var call ToolCallParams
	if err := decodeObject(params, &call); err != nil {
		return ToolCallResult{}, InvalidParams("invalid tools/call params")
	}
	if call.Name == "" {
		return ToolCallResult{}, InvalidParams("tool name is required")
	}
	if len(call.Arguments) == 0 {
		call.Arguments = json.RawMessage("{}")
	}

	payload, rpcErr := d.CallTool(call.Name, call.Arguments)
	if rpcErr != nil {
		return ToolCallResult{}, rpcErr
	}
	result, err := NewToolCallResult(payload)
	if err != nil {
		return ToolCallResult{}, InternalError()
	}
	return result, nil
}

func findReservedField(raw json.RawMessage, fields []string) string {
	if len(fields) == 0 || len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	for _, field := range fields {
		if _, ok := object[field]; ok {
			return field
		}
	}
	return ""
}

func decodeObject(raw json.RawMessage, target any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
