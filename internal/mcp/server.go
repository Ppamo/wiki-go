// Package mcp serves a Model Context Protocol endpoint over the Streamable
// HTTP transport. It runs stateless: every POST carries one JSON-RPC message
// and gets a single JSON response, and the caller is authenticated on every
// request with the same credential resolver used by the HTTP API.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/logger"
	"wiki-go/internal/version"
)

// LatestProtocolVersion is offered to clients that ask for an unknown version.
const LatestProtocolVersion = "2025-06-18"

var supportedProtocolVersions = map[string]bool{
	"2025-06-18": true,
	"2025-03-26": true,
}

// maxRequestBytes bounds a single JSON-RPC message.
const maxRequestBytes = 1 << 20

// JSON-RPC error codes
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type sessionKey struct{}

// SessionFromContext returns the authenticated caller of an MCP request.
func SessionFromContext(ctx context.Context) *auth.Session {
	session, _ := ctx.Value(sessionKey{}).(*auth.Session)
	return session
}

// Server is the MCP endpoint. Tools read the configuration through cfg, which
// handlers update in place, so the pointer stays valid after settings change.
type Server struct {
	cfg *config.Config
}

// NewServer returns the MCP handler wrapped in its authentication middleware.
func NewServer(cfg *config.Config) http.Handler {
	return RequireSession(&Server{cfg: cfg})
}

// RequireSession resolves the caller with auth.GetSession (session cookie,
// bearer session token or personal access token) and stores the session in
// the request context for the tools. Unauthenticated requests get 401.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reject cross-origin browser requests (DNS rebinding, cookie reuse)
		if !sameOrigin(r) {
			writeHTTPError(w, http.StatusForbidden, "Origin not allowed")
			return
		}

		session := auth.GetSession(r)
		if session == nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="wiki-go"`)
			writeHTTPError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), sessionKey{}, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sameOrigin accepts requests without an Origin header (non-browser clients)
// and browser requests whose origin matches the requested host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && strings.EqualFold(parsed.Host, r.Host)
}

func writeHTTPError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": false,
		"message": message,
	})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// No server-initiated SSE stream and no session to delete
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPError(w, http.StatusMethodNotAllowed, "Method not allowed")
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !supportedProtocolVersions[v] {
		writeHTTPError(w, http.StatusBadRequest, "Unsupported MCP protocol version")
		return
	}

	var req rpcRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err := decoder.Decode(&req); err != nil {
		writeRPC(w, rpcResponse{ID: json.RawMessage("null"), Error: &rpcError{codeParseError, "Parse error"}})
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		writeRPC(w, rpcResponse{ID: idOrNull(req.ID), Error: &rpcError{codeInvalidRequest, "Invalid request"}})
		return
	}

	// Notifications (no id) need no answer
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.dispatch(r.Context(), req)
	writeRPC(w, rpcResponse{ID: req.ID, Result: result, Error: rpcErr})
}

func idOrNull(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func writeRPC(w http.ResponseWriter, response rpcResponse) {
	response.JSONRPC = "2.0"
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		logger.Error("Error writing MCP response: %v", err)
	}
}

func (s *Server) dispatch(ctx context.Context, req rpcRequest) (interface{}, *rpcError) {
	switch req.Method {
	case "initialize":
		return s.initialize(req.Params)
	case "ping":
		return map[string]interface{}{}, nil
	case "tools/list":
		return map[string]interface{}{"tools": toolDefinitions()}, nil
	case "tools/call":
		return s.callTool(ctx, req.Params)
	default:
		return nil, &rpcError{codeMethodNotFound, "Method not found: " + req.Method}
	}
}

func (s *Server) initialize(params json.RawMessage) (interface{}, *rpcError) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{codeInvalidParams, "Invalid params"}
		}
	}
	protocolVersion := LatestProtocolVersion
	if supportedProtocolVersions[p.ProtocolVersion] {
		protocolVersion = p.ProtocolVersion
	}

	return map[string]interface{}{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]interface{}{
			"tools": map[string]interface{}{},
		},
		"serverInfo": map[string]interface{}{
			"name":    "wiki-go",
			"title":   s.cfg.Wiki.Title,
			"version": version.Version,
		},
	}, nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (interface{}, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil || p.Name == "" {
		return nil, &rpcError{codeInvalidParams, "Invalid params"}
	}

	tool, ok := tools[p.Name]
	if !ok {
		return nil, &rpcError{codeInvalidParams, "Unknown tool: " + p.Name}
	}

	text, err := tool.run(ctx, s, p.Arguments)
	var toolErr *toolError
	switch {
	case errors.As(err, &toolErr):
		// Failures the agent can act on are tool results, not protocol errors
		return toolResult(toolErr.message, true), nil
	case err != nil:
		logger.Error("MCP tool %s failed: %v", p.Name, err)
		return nil, &rpcError{codeInternalError, "Internal error"}
	}
	return toolResult(text, false), nil
}

func toolResult(text string, isError bool) map[string]interface{} {
	result := map[string]interface{}{
		"content": []map[string]interface{}{{"type": "text", "text": text}},
	}
	if isError {
		result["isError"] = true
	}
	return result
}
