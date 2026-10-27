package wpgo

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Error codes reported by the host in failed responses.
const (
	// CodePermissionDenied means the plugin lacks the permission the call needs.
	CodePermissionDenied = "permission_denied"
	// CodeInvalidRequest means the request JSON was malformed or incomplete.
	CodeInvalidRequest = "invalid_request"
	// CodeNotFound means the requested item does not exist.
	CodeNotFound = "not_found"
	// CodeHostError means the host failed while serving the request.
	CodeHostError = "host_error"
)

// ErrNotInHost is returned by host functions when the code is not running
// inside the wpgo plugin runtime (i.e. not compiled for wasip1/wasm).
var ErrNotInHost = errors.New("wpgo: not running inside the wpgo plugin host")

// Error is a failure reported by the host for a host function call.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"error"`
}

func (e *Error) Error() string { return "wpgo: " + e.Code + ": " + e.Message }

// IsPermissionDenied reports whether err is a host permission error.
func IsPermissionDenied(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == CodePermissionDenied
}

// hostResponse is the envelope of every host import response.
type hostResponse struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Code   string          `json:"code,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// handleRequest is what the host sends to the wpgo_handle export.
type handleRequest struct {
	Plugin   string            `json:"plugin"`
	Callback string            `json:"callback"`
	Hook     string            `json:"hook"`
	Type     string            `json:"type"`
	Args     []json.RawMessage `json:"args"`
	Value    json.RawMessage   `json:"value,omitempty"`
}

// handleResponse is what wpgo_handle returns to the host.
type handleResponse struct {
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// decodeResponse turns a raw host envelope into its result or an *Error.
func decodeResponse(raw []byte) (json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, &Error{Code: CodeHostError, Message: "empty host response"}
	}
	var resp hostResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, &Error{Code: CodeHostError, Message: "malformed host response: " + err.Error()}
	}
	if !resp.OK {
		return nil, &Error{Code: resp.Code, Message: resp.Error}
	}
	return resp.Result, nil
}

// Dispatch runs the registered callback named in a JSON hook request and
// returns the JSON response, exactly as the wpgo_handle export does. It is
// exported so plugin authors can unit test their callbacks on a normal Go
// toolchain; the host never calls it directly.
func Dispatch(request []byte) []byte {
	resp := dispatch(request)
	out, err := json.Marshal(resp)
	if err != nil {
		out, _ = json.Marshal(handleResponse{Error: "marshal response: " + err.Error()})
	}
	return out
}

func dispatch(request []byte) (resp handleResponse) {
	defer func() {
		if r := recover(); r != nil {
			resp = handleResponse{Error: fmt.Sprintf("panic in plugin callback: %v", r)}
		}
	}()
	var req handleRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return handleResponse{Error: "malformed request: " + err.Error()}
	}
	cb, ok := lookupCallback(req.Callback)
	if !ok {
		return handleResponse{Error: "unknown callback " + req.Callback}
	}
	args := make([]any, len(req.Args))
	for i, raw := range req.Args {
		if err := json.Unmarshal(raw, &args[i]); err != nil {
			return handleResponse{Error: fmt.Sprintf("decode arg %d: %v", i, err)}
		}
	}
	ctx := Context{Plugin: req.Plugin, Hook: req.Hook, Callback: req.Callback}
	value, err := cb(ctx, req.Value, args, req.Args)
	if err != nil {
		return handleResponse{Error: err.Error()}
	}
	return handleResponse{Value: value}
}
