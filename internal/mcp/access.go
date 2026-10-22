package mcp

import (
	"context"
	"path/filepath"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/utils"
)

// errDocumentUnavailable hides whether a restricted document exists.
var errDocumentUnavailable = &toolError{"Document not found or access denied"}

// document is a wiki document resolved from a tool path argument.
type document struct {
	// logicalPath is the URL path the access rules apply to: "/" for the
	// home page, otherwise "/guides/intro"
	logicalPath string
	// dir holds document.md and the document attachments
	dir         string
	versionsDir string
}

// displayPath is the path form tools accept and return: "/" for the home
// page, otherwise "guides/intro".
func (d document) displayPath() string {
	return displayPath(d.logicalPath)
}

func (d document) file() string {
	return filepath.Join(d.dir, "document.md")
}

func displayPath(logicalPath string) string {
	if trimmed := strings.Trim(logicalPath, "/"); trimmed != "" {
		return trimmed
	}
	return "/"
}

// resolveDocument resolves a tool path argument like the API does: "" or "/"
// is the home page, anything else a contained path below the documents root.
func resolveDocument(cfg *config.Config, path string) (document, error) {
	requestPath := strings.Trim(path, "/")
	if requestPath == "" {
		return document{
			logicalPath: "/",
			dir:         filepath.Join(cfg.Wiki.RootDir, "pages", "home"),
			versionsDir: filepath.Join(cfg.Wiki.RootDir, "versions", "pages", "home"),
		}, nil
	}

	resolved, err := utils.ResolveRelativeDocumentPath(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir, requestPath)
	if err != nil {
		return document{}, err
	}
	// Versions keep the documents/ prefix whatever the documents directory is
	versions, err := utils.ResolveRelativeDocumentPath(filepath.Join(cfg.Wiki.RootDir, "versions"), "documents", requestPath)
	if err != nil {
		return document{}, err
	}
	return document{
		logicalPath: resolved.LogicalPath,
		dir:         resolved.FilesystemPath,
		versionsDir: versions.FilesystemPath,
	}, nil
}

// authorizeDocument resolves a tool path argument and applies the document
// access rules for the caller. Access rules apply to the document path, not
// to the /mcp request path.
func authorizeDocument(ctx context.Context, s *Server, path string) (document, error) {
	doc, err := resolveDocument(s.cfg, path)
	if err != nil {
		return document{}, &toolError{"Invalid document path"}
	}
	if !auth.CanAccessDocument(doc.logicalPath, SessionFromContext(ctx), s.cfg) {
		return document{}, errDocumentUnavailable
	}
	return doc, nil
}
