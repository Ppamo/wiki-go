package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"wiki-go/internal/comments"
	"wiki-go/internal/config"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

// Sections read_document adds to the metadata block on request
const (
	includeAttachments = "attachments"
	includeHistory     = "history"
	includeComments    = "comments"
)

var readDocumentTool = tool{
	name:  "read_document",
	title: "Read document",
	description: "Reads a wiki document. The result has two text blocks: first the path, " +
		"the revision (a hash of the current content) and the sections requested with include; " +
		"then the Markdown source. Use \"/\" for the home page. Pass version, a timestamp " +
		"from the history, to read an old version instead. The history and old versions " +
		"require the editor role.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path, e.g. \"finance/finance-report\"",
			},
			"version": map[string]interface{}{
				"type":        "string",
				"description": "Version timestamp (yyyymmddhhmmss) from the history",
				"pattern":     "^[0-9]{14}$",
			},
			"include": map[string]interface{}{
				"type":        "array",
				"description": "Extra sections to return with the document",
				"items": map[string]interface{}{
					"type": "string",
					"enum": []string{includeAttachments, includeHistory, includeComments},
				},
				"uniqueItems": true,
			},
		},
		"required": []string{"path"},
	},
	annotations: readOnlyAnnotations,
	role:        roles.RoleViewer,
	run:         readDocument,
}

func readDocument(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path    string   `json:"path"`
		Version string   `json:"version"`
		Include []string `json:"include"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	include := map[string]bool{}
	for _, section := range args.Include {
		switch section {
		case includeAttachments, includeHistory, includeComments:
			include[section] = true
		default:
			return nil, &toolError{"Invalid arguments: include accepts attachments, history and comments"}
		}
	}

	// The API serves versions to editors only
	session := SessionFromContext(ctx)
	if args.Version != "" || include[includeHistory] {
		if err := requireRole(session, roles.RoleEditor); err != nil {
			return nil, err
		}
	}
	if args.Version != "" && (len(args.Version) != 14 || !utils.IsNumeric(args.Version)) {
		return nil, &toolError{"Invalid arguments: version must be a yyyymmddhhmmss timestamp from the history"}
	}

	doc, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(doc.file())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errDocumentUnavailable
		}
		return nil, err
	}

	var metadata strings.Builder
	fmt.Fprintf(&metadata, "path: %s\n", doc.displayPath())
	if args.Version == "" {
		fmt.Fprintf(&metadata, "revision: %s\n", contentRevision(content))
	} else {
		content, err = os.ReadFile(filepath.Join(doc.versionsDir, args.Version+".md"))
		if err != nil {
			if os.IsNotExist(err) {
				return nil, &toolError{"Version not found; include history to list the versions"}
			}
			return nil, err
		}
		fmt.Fprintf(&metadata, "version: %s\n", args.Version)
	}

	if include[includeAttachments] {
		section, err := attachmentsSection(s.cfg, doc)
		if err != nil {
			return nil, err
		}
		metadata.WriteString("\n" + section)
	}
	if include[includeHistory] {
		section, err := historySection(doc)
		if err != nil {
			return nil, err
		}
		metadata.WriteString("\n" + section)
	}
	if include[includeComments] {
		section, err := commentsSection(s.cfg, doc)
		if err != nil {
			return nil, err
		}
		metadata.WriteString("\n" + section)
	}

	logger.Debug("MCP read_document %s by %s (token %q)", doc.logicalPath, session.Username, session.TokenName)
	return []string{metadata.String(), string(content)}, nil
}

// contentRevision identifies the content of a document, so that a later
// change can detect that someone else edited it in between.
func contentRevision(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8])
}

// attachmentsSection lists the files the API would list for the document.
func attachmentsSection(cfg *config.Config, doc document) (string, error) {
	files, err := os.ReadDir(doc.dir)
	if err != nil {
		return "", err
	}
	var lines []string
	for _, file := range files {
		if file.IsDir() || file.Name() == "document.md" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(file.Name()))
		if !cfg.Wiki.DisableFileUploadChecking && !config.IsAllowedExtension(ext) {
			continue
		}
		info, err := file.Info()
		if err != nil {
			continue
		}
		lines = append(lines, fmt.Sprintf("- %s (%s, %d bytes)", file.Name(), config.GetMimeTypeForExtension(ext), info.Size()))
	}
	if len(lines) == 0 {
		return "attachments: none\n", nil
	}
	return "attachments:\n" + strings.Join(lines, "\n") + "\n", nil
}

// historySection lists the stored versions, newest first.
func historySection(doc document) (string, error) {
	files, err := os.ReadDir(doc.versionsDir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	var timestamps []string
	for _, file := range files {
		timestamp := strings.TrimSuffix(file.Name(), ".md")
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".md") && len(timestamp) == 14 && utils.IsNumeric(timestamp) {
			timestamps = append(timestamps, timestamp)
		}
	}
	if len(timestamps) == 0 {
		return "history: none\n", nil
	}
	sort.Sort(sort.Reverse(sort.StringSlice(timestamps)))
	return "history (newest first):\n- " + strings.Join(timestamps, "\n- ") + "\n", nil
}

// commentsSection returns the raw Markdown of the comments, oldest first.
func commentsSection(cfg *config.Config, doc document) (string, error) {
	if cfg.Wiki.DisableComments {
		return "comments: disabled in this wiki\n", nil
	}
	list, err := comments.GetComments(filepath.Join(cfg.Wiki.RootDir, "comments"), doc.logicalPath)
	if err != nil {
		return "", err
	}
	if len(list) == 0 {
		return "comments: none\n", nil
	}
	var out strings.Builder
	out.WriteString("comments (oldest first):\n")
	for _, comment := range list {
		fmt.Fprintf(&out, "- %s, %s:\n", comment.Author, comments.FormatCommentTime(comment.Timestamp))
		for _, line := range strings.Split(strings.TrimSpace(comment.Content), "\n") {
			out.WriteString("  " + line + "\n")
		}
	}
	return out.String(), nil
}
