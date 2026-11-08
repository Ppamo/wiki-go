package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"wiki-go/internal/documents"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

var restoreVersionTool = tool{
	name:  "restore_version",
	title: "Restore version",
	description: "Restores a document to a version from its history. Pass the version " +
		"timestamp and the current revision from read_document (include history lists the " +
		"versions). The current content is kept in the history, so a restore can be undone.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path; \"/\" for the home page",
			},
			"version": map[string]interface{}{
				"type":        "string",
				"description": "Version timestamp (yyyymmddhhmmss) from the history",
				"pattern":     "^[0-9]{14}$",
			},
			"revision": map[string]interface{}{
				"type":        "string",
				"description": "Current revision returned by read_document",
			},
		},
		"required": []string{"path", "version", "revision"},
	},
	annotations: writeAnnotations(true, false),
	// Like /api/versions; the role also covers reading the history
	role: roles.RoleEditor,
	run:  restoreVersion,
}

func restoreVersion(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path     string `json:"path"`
		Version  string `json:"version"`
		Revision string `json:"revision"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if len(args.Version) != 14 || !utils.IsNumeric(args.Version) {
		return nil, &toolError{"Invalid arguments: version must be a yyyymmddhhmmss timestamp from the history"}
	}
	if args.Revision == "" {
		return nil, errRevisionRequired
	}

	doc, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(doc.File()); err != nil {
		if os.IsNotExist(err) {
			return nil, errDocumentUnavailable
		}
		return nil, err
	}
	content, err := os.ReadFile(filepath.Join(doc.VersionsDir, args.Version+".md"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &toolError{"Version not found; include history in read_document to list them"}
		}
		return nil, err
	}

	// Save compares the content before the revision, so restoring the same
	// version again is Unchanged even with a newer revision
	result, err := saveDocument(ctx, s, "restore_version", doc, content, documents.SaveOptions{ExpectedRevision: args.Revision})
	if err != nil {
		return nil, err
	}
	summary := saveSummary(doc, result, "")
	if !result.Unchanged {
		// Replace the "Saved <path>." line
		_, details, _ := strings.Cut(summary, "\n")
		summary = fmt.Sprintf("Restored %s to version %s.\n", displayPath(doc.LogicalPath), args.Version) + details
	}
	return []string{summary}, nil
}
