package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

// toolError is a failure reported to the agent as a tool result with isError,
// such as a missing document or a permission problem.
type toolError struct {
	message string
}

func (e *toolError) Error() string { return e.message }

type tool struct {
	definition map[string]interface{}
	run        func(ctx context.Context, s *Server, arguments json.RawMessage) (string, error)
}

var tools = map[string]tool{
	"read_document": {
		definition: map[string]interface{}{
			"name":  "read_document",
			"title": "Read document",
			"description": "Returns the Markdown source of a wiki document. " +
				"The path is the document URL path without a leading slash, " +
				"for example \"finance/finance-report\"; use \"\" or \"/\" for the home page.",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Document path, e.g. \"finance/finance-report\"",
					},
				},
				"required": []string{"path"},
			},
			"annotations": map[string]interface{}{
				"readOnlyHint": true,
			},
		},
		run: readDocument,
	},
}

func toolDefinitions() []map[string]interface{} {
	definitions := make([]map[string]interface{}, 0, len(tools))
	for _, t := range tools {
		definitions = append(definitions, t.definition)
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

// errDocumentUnavailable hides whether a restricted document exists.
var errDocumentUnavailable = &toolError{"Document not found or access denied"}

func readDocument(ctx context.Context, s *Server, arguments json.RawMessage) (string, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(arguments, &args); err != nil {
		return "", &toolError{"Invalid arguments: path must be a string"}
	}

	session := SessionFromContext(ctx)
	if err := requireRole(session, roles.RoleViewer); err != nil {
		return "", err
	}

	// Resolve the document like the API does: home page first, otherwise a
	// contained path below the documents root.
	logicalPath := "/"
	var docFile string
	requestPath := strings.Trim(args.Path, "/")
	if requestPath == "" {
		docFile = filepath.Join(s.cfg.Wiki.RootDir, "pages", "home", "document.md")
	} else {
		resolved, err := utils.ResolveRelativeDocumentPath(s.cfg.Wiki.RootDir, s.cfg.Wiki.DocumentsDir, requestPath)
		if err != nil {
			return "", &toolError{"Invalid document path"}
		}
		logicalPath = resolved.LogicalPath
		docFile = filepath.Join(resolved.FilesystemPath, "document.md")
	}

	// Access rules apply to the document path, not to the /mcp request path
	if !auth.CanAccessDocument(logicalPath, session, s.cfg) {
		return "", errDocumentUnavailable
	}

	content, err := os.ReadFile(docFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errDocumentUnavailable
		}
		return "", err
	}

	logger.Debug("MCP read_document %s by %s (token %q)", logicalPath, session.Username, session.TokenName)
	return string(content), nil
}
