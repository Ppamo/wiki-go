package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"wiki-go/internal/documents"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

var (
	// errRevisionMismatch leaves out the current revision so that the agent
	// reads the document again before retrying
	errRevisionMismatch = &toolError{"Revision mismatch: the document changed since you read it; read it again"}
	errRevisionRequired = &toolError{"Invalid arguments: revision is required; read the document first"}
	errDocumentExists   = &toolError{"Document exists; pass the revision from read_document to replace it"}
)

var writeDocumentTool = tool{
	name:  "write_document",
	title: "Write document",
	description: "Creates a wiki document or replaces its whole content. Without revision " +
		"it only creates: it fails if the document exists. To replace an existing document " +
		"pass the revision from read_document; prefer edit_document for partial changes. " +
		"New paths use letters, digits, '-', '_' and '/'. Start the content with front matter " +
		"(---\\nlayout: kanban\\n---) for a kanban or links page. Requests are limited to 1 MiB.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path, e.g. \"guides/deploy\"; \"/\" for the home page",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Full Markdown content, including the # title",
			},
			"revision": map[string]interface{}{
				"type":        "string",
				"description": "Revision from read_document; required to replace an existing document",
			},
		},
		"required": []string{"path", "content"},
	},
	annotations: writeAnnotations(true, true),
	role:        roles.RoleEditor,
	run:         writeDocumentContent,
}

func writeDocumentContent(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Revision string `json:"revision"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Content) == "" {
		return nil, &toolError{"Invalid arguments: content must not be empty"}
	}

	doc, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	current, err := os.ReadFile(doc.File())
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	content := []byte(args.Content)
	var opts documents.SaveOptions
	switch {
	case !exists:
		if args.Revision != "" {
			// The document the agent read is gone
			return nil, errDocumentUnavailable
		}
		if doc.LogicalPath != "/" {
			if suggested := utils.SanitizePath(args.Path); suggested != strings.Trim(args.Path, "/") {
				return nil, &toolError{fmt.Sprintf("Invalid path for a new document; use %q", suggested)}
			}
		}
		opts.CreateOnly = true
	case bytes.Equal(current, content):
		// A retry after a success is idempotent, with or without revision
		return []string{saveSummary(doc, documents.SaveResult{Revision: documents.Revision(content), Unchanged: true}, "")}, nil
	case args.Revision == "":
		return nil, errDocumentExists
	default:
		opts.ExpectedRevision = args.Revision
	}

	result, err := saveDocument(ctx, s, "write_document", doc, content, opts)
	if err != nil {
		return nil, err
	}
	return []string{saveSummary(doc, result, "")}, nil
}

// saveDocument writes through the write path shared with the HTTP API, maps
// its conflicts to tool errors and logs the write for auditing.
func saveDocument(ctx context.Context, s *Server, toolName string, doc documents.Document, content []byte, opts documents.SaveOptions) (documents.SaveResult, error) {
	result, err := documents.Save(s.cfg, doc, content, opts)
	switch {
	case errors.Is(err, documents.ErrRevisionMismatch):
		return result, errRevisionMismatch
	case errors.Is(err, documents.ErrExists):
		return result, errDocumentExists
	case errors.Is(err, documents.ErrNotFound):
		return result, errDocumentUnavailable
	case err != nil:
		return result, err
	}
	if !result.Unchanged {
		session := SessionFromContext(ctx)
		logger.Info("MCP %s %s by %s (token %q)", toolName, doc.LogicalPath, session.Username, session.TokenName)
	}
	return result, nil
}

// saveSummary describes a save in the "key: value" style of read_document.
// detail follows the path on the first line of a save, e.g. " (2 edits applied)".
func saveSummary(doc documents.Document, result documents.SaveResult, detail string) string {
	path := displayPath(doc.LogicalPath)
	var out strings.Builder
	switch {
	case result.Unchanged:
		fmt.Fprintf(&out, "Unchanged %s: the content is identical.\n", path)
	case result.Created:
		fmt.Fprintf(&out, "Created %s.\n", path)
	default:
		fmt.Fprintf(&out, "Saved %s%s.\n", path, detail)
	}
	fmt.Fprintf(&out, "revision: %s\n", result.Revision)
	if result.Version != "" {
		fmt.Fprintf(&out, "previous version: %s\n", result.Version)
	}
	return out.String()
}
