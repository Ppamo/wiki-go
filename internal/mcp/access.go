package mcp

import (
	"context"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/documents"
)

// errDocumentUnavailable hides whether a restricted document exists.
var errDocumentUnavailable = &toolError{"Document not found or access denied"}

// displayPath is the path form tools accept and return: "/" for the home
// page, otherwise "guides/intro".
func displayPath(logicalPath string) string {
	if trimmed := strings.Trim(logicalPath, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}

// authorizeDocument resolves a tool path argument and applies the document
// access rules for the caller. Access rules apply to the document path, not
// to the /mcp request path. Nothing is created on disk before it succeeds.
func authorizeDocument(ctx context.Context, s *Server, path string) (documents.Document, error) {
	doc, err := documents.Resolve(s.cfg, path)
	if err != nil {
		return documents.Document{}, &toolError{"Invalid document path"}
	}
	if !auth.CanAccessDocument(doc.LogicalPath, SessionFromContext(ctx), s.cfg) {
		return documents.Document{}, errDocumentUnavailable
	}
	return doc, nil
}
