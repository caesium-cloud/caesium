package mcp

import (
	"encoding/json"
	"errors"
	"io"
)

const (
	// JSON-RPC 2.0 well-known error codes.
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	// Application error codes live in the JSON-RPC server error range.
	CodeForbidden = -32003
	CodeNotFound  = -32004
)

var nullID = json.RawMessage("null")

// Request is the JSON-RPC 2.0 envelope accepted by the MCP endpoint.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is the JSON-RPC 2.0 envelope returned by the MCP endpoint.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is the JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Handle decodes one JSON-RPC request from r and dispatches it.
func Handle(r io.Reader, d Dispatcher) Response {
	req, rpcErr := decodeRequest(r)
	if rpcErr != nil {
		return ErrorResponse(nil, rpcErr)
	}
	return d.Dispatch(req)
}

// SuccessResponse builds a JSON-RPC success envelope.
func SuccessResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: responseID(id), Result: result}
}

// ErrorResponse builds a JSON-RPC error envelope.
func ErrorResponse(id json.RawMessage, err *Error) Response {
	return Response{JSONRPC: "2.0", ID: responseID(id), Error: err}
}

// InvalidParams returns a JSON-RPC invalid-params error.
func InvalidParams(message string) *Error {
	return &Error{Code: CodeInvalidParams, Message: message}
}

// Forbidden returns a JSON-RPC forbidden application error.
func Forbidden(message string) *Error {
	return &Error{Code: CodeForbidden, Message: message}
}

// NotFound returns a JSON-RPC not-found application error.
func NotFound(message string) *Error {
	return &Error{Code: CodeNotFound, Message: message}
}

// InternalError returns a JSON-RPC internal-error response with a stable message.
func InternalError() *Error {
	return &Error{Code: CodeInternalError, Message: "internal server error"}
}

func decodeRequest(r io.Reader) (Request, *Error) {
	dec := json.NewDecoder(r)

	var req Request
	if err := dec.Decode(&req); err != nil {
		return Request{}, &Error{Code: CodeParseError, Message: "parse error"}
	}

	var extra any
	if err := dec.Decode(&extra); err == nil || !errors.Is(err, io.EOF) {
		return Request{}, &Error{Code: CodeParseError, Message: "parse error"}
	}

	if req.JSONRPC != "2.0" || req.Method == "" {
		return req, &Error{Code: CodeInvalidRequest, Message: "invalid request"}
	}
	return req, nil
}

func responseID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return nullID
	}
	return id
}
