package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"wiki-go/internal/auth"
	"wiki-go/internal/roles"
)

// toolError is a failure reported to the agent as a tool result with isError,
// such as a missing document or a permission problem.
type toolError struct {
	message string
}

func (e *toolError) Error() string { return e.message }

type tool struct {
	name        string
	title       string
	description string
	inputSchema map[string]interface{}
	annotations map[string]interface{}
	// role is the minimum role that sees the tool in tools/list and may call it
	role string
	// run returns the text blocks of the result, in order
	run func(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error)
}

func (t tool) definition() map[string]interface{} {
	return map[string]interface{}{
		"name":        t.name,
		"title":       t.title,
		"description": t.description,
		"inputSchema": t.inputSchema,
		"annotations": t.annotations,
	}
}

// readOnlyAnnotations marks tools that only read the wiki.
var readOnlyAnnotations = map[string]interface{}{
	"readOnlyHint":  true,
	"openWorldHint": false,
}

// writeAnnotations marks tools that change the wiki.
func writeAnnotations(idempotent, destructive bool) map[string]interface{} {
	return map[string]interface{}{
		"readOnlyHint":    false,
		"destructiveHint": destructive,
		"idempotentHint":  idempotent,
		"openWorldHint":   false,
	}
}

// registry lists the tools in the order tools/list returns them.
var registry = []tool{
	searchDocumentsTool,
	listDocumentsTool,
	readDocumentTool,
	editDocumentTool,
	writeDocumentTool,
	addCommentTool,
}

func findTool(name string) (tool, bool) {
	for _, t := range registry {
		if t.name == name {
			return t, true
		}
	}
	return tool{}, false
}

// toolDefinitions returns the tools the session's role may call.
func toolDefinitions(session *auth.Session) []map[string]interface{} {
	definitions := []map[string]interface{}{}
	for _, t := range registry {
		if requireRole(session, t.role) == nil {
			definitions = append(definitions, t.definition())
		}
	}
	return definitions
}

// requireRole checks the caller's role with the same hierarchy as the API.
func requireRole(session *auth.Session, role string) error {
	if session == nil || roles.Rank(session.Role) < roles.Rank(role) {
		return &toolError{"Insufficient role: " + role + " access required"}
	}
	return nil
}

// decodeArguments unmarshals the tool arguments into args. Missing arguments
// leave args at its zero value.
func decodeArguments(arguments json.RawMessage, args interface{}) error {
	if len(bytes.TrimSpace(arguments)) == 0 || bytes.Equal(bytes.TrimSpace(arguments), []byte("null")) {
		return nil
	}
	if err := json.Unmarshal(arguments, args); err != nil {
		return &toolError{"Invalid arguments: " + err.Error()}
	}
	return nil
}
