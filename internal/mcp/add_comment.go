package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"wiki-go/internal/comments"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
)

var addCommentTool = tool{
	name:  "add_comment",
	title: "Add comment",
	description: "Adds a Markdown comment to a wiki document as the token owner. " +
		"Use it to explain a change or to ask the document owners a question.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page",
			},
			"content": map[string]interface{}{
				"type":        "string",
				"description": "Comment in Markdown",
			},
		},
		"required": []string{"path", "content"},
	},
	annotations: writeAnnotations(false, false),
	// Like /api/comments/add, any authenticated role may comment
	role: roles.RoleViewer,
	run:  addComment,
}

// addComment runs the checks of AddCommentHandler in the same order.
func addComment(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	if s.cfg.Wiki.DisableComments {
		return nil, &toolError{"Comments are disabled in this wiki"}
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}

	doc, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	content, err := os.ReadFile(doc.File())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errDocumentUnavailable
		}
		return nil, err
	}
	if !comments.AreCommentsAllowed(string(content)) {
		return nil, &toolError{"Comments are not allowed for this document"}
	}
	if strings.TrimSpace(args.Content) == "" {
		return nil, &toolError{"Invalid arguments: content must not be empty"}
	}

	session := SessionFromContext(ctx)
	err = comments.AddComment(filepath.Join(s.cfg.Wiki.RootDir, "comments"), doc.LogicalPath, args.Content, session.Username)
	if errors.Is(err, comments.ErrCommentExists) {
		return nil, &toolError{"You already commented on this document this second; retry"}
	}
	if err != nil {
		return nil, err
	}

	logger.Info("MCP add_comment %s by %s (token %q)", doc.LogicalPath, session.Username, session.TokenName)
	return []string{fmt.Sprintf("Comment added to %s.", displayPath(doc.LogicalPath))}, nil
}
