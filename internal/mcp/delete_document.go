package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/documents"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
)

var deleteDocumentTool = tool{
	name:  "delete_document",
	title: "Delete document",
	description: "Permanently deletes a wiki document with its attachments, history " +
		"and comments; this cannot be undone. Pass the revision from read_document. A document " +
		"with sub-documents is only deleted with recursive: true; check them first with " +
		"list_documents. Lists the documents that link to it.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path, e.g. \"guides/obsolete\"",
			},
			"revision": map[string]interface{}{
				"type":        "string",
				"description": "Revision from read_document; required when the path has a document",
			},
			"recursive": map[string]interface{}{
				"type":        "boolean",
				"description": "Also delete the sub-documents",
				"default":     false,
			},
		},
		"required": []string{"path"},
	},
	annotations: writeAnnotations(true, true),
	role:        roles.RoleEditor,
	run:         deleteDocument,
}

func deleteDocument(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path      string `json:"path"`
		Revision  string `json:"revision"`
		Recursive bool   `json:"recursive"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if strings.Trim(args.Path, "/") == "" {
		return nil, &toolError{"The home page cannot be deleted"}
	}

	doc, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(doc.Dir); err != nil || !info.IsDir() {
		return nil, errDocumentUnavailable
	}
	// A folder without document.md has no revision
	if args.Revision == "" && fileExists(doc.File()) {
		return nil, errRevisionRequired
	}

	session := SessionFromContext(ctx)
	check := func(dirs []string) error {
		// Under the lock: the document may have been created since
		if args.Revision == "" && fileExists(doc.File()) {
			return errRevisionRequired
		}
		for _, dir := range dirs {
			if !auth.CanAccessDocument(dir, session, s.cfg) {
				return &toolError{"Cannot delete: the section contains documents you cannot access"}
			}
		}
		return nil
	}
	result, err := documents.Delete(s.cfg, doc, documents.DeleteOptions{ExpectedRevision: args.Revision, Recursive: args.Recursive}, check)
	var children *documents.ChildrenError
	switch {
	case errors.As(err, &children):
		return nil, &toolError{fmt.Sprintf("The document has %s; check them with list_documents and pass recursive: true to delete them too", plural(children.Count, "sub-document"))}
	case errors.Is(err, documents.ErrRevisionMismatch):
		return nil, errRevisionMismatch
	case errors.Is(err, documents.ErrNotFound):
		return nil, errDocumentUnavailable
	case err != nil:
		return nil, err
	}
	logger.Info("MCP delete_document %s (%d documents) by %s (token %q)", doc.LogicalPath, len(result.Documents), session.Username, session.TokenName)

	subDocuments := len(result.Documents)
	if subDocuments > 0 && result.Documents[0] == doc.LogicalPath {
		subDocuments--
	}
	var out strings.Builder
	fmt.Fprintf(&out, "Deleted %s (%s, %s). This cannot be undone.\n",
		displayPath(doc.LogicalPath), plural(subDocuments, "sub-document"), plural(result.Attachments, "attachment"))
	out.WriteString(referencesSection(ctx, s, "documents linking to it", doc.LogicalPath, doc.LogicalPath))
	if len(result.Warnings) > 0 {
		out.WriteString("warnings:\n- " + strings.Join(result.Warnings, "\n- ") + "\n")
	}
	return []string{out.String()}, nil
}
