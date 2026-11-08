package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/config"
	"wiki-go/internal/documents"
	"wiki-go/internal/logger"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

const (
	// maxReferences bounds the documents listed as linking to a moved or
	// deleted path
	maxReferences = 20
	// maxAccessChanges bounds the examples of a rejected access rule change
	maxAccessChanges = 3
)

var errMoveInsideItself = &toolError{"Cannot move a document inside itself"}

var moveDocumentTool = tool{
	name:  "move_document",
	title: "Move document",
	description: "Moves or renames a wiki document together with its sub-documents, " +
		"attachments, history and comments. new_path is the full new path, e.g. " +
		"\"archive/old-guide\". Fails if the move would change the access rules of any moved " +
		"document unless allow_access_change is true: ask the user before setting it. Lists " +
		"the documents that link to the old path so you can fix them with edit_document.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Current document path, e.g. \"guides/old-name\"",
			},
			"new_path": map[string]interface{}{
				"type":        "string",
				"description": "Full new path, e.g. \"archive/new-name\"; letters, digits, '-', '_' and '/'",
			},
			"allow_access_change": map[string]interface{}{
				"type":        "boolean",
				"description": "Confirm a move that changes which access rule applies to the moved documents",
				"default":     false,
			},
		},
		"required": []string{"path", "new_path"},
	},
	annotations: writeAnnotations(false, false),
	role:        roles.RoleEditor,
	run:         moveDocument,
}

func moveDocument(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path              string `json:"path"`
		NewPath           string `json:"new_path"`
		AllowAccessChange bool   `json:"allow_access_change"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	switch {
	case args.NewPath == "":
		return nil, &toolError{"Invalid arguments: new_path is required"}
	case strings.Trim(args.Path, "/") == "":
		return nil, &toolError{"The home page cannot be moved"}
	case strings.Trim(args.NewPath, "/") == "":
		return nil, &toolError{"Cannot move a document to the home page"}
	}
	if suggested := utils.SanitizePath(args.NewPath); suggested != strings.Trim(args.NewPath, "/") {
		return nil, &toolError{fmt.Sprintf("Invalid new_path; use %q", suggested)}
	}

	src, err := authorizeDocument(ctx, s, args.Path)
	if err != nil {
		return nil, err
	}
	dst, err := documents.Resolve(s.cfg, args.NewPath)
	if err != nil {
		return nil, &toolError{"Invalid new_path"}
	}
	switch {
	case dst.LogicalPath == src.LogicalPath:
		return nil, &toolError{"Invalid arguments: new_path equals path"}
	case strings.HasPrefix(dst.LogicalPath, src.LogicalPath+"/"):
		return nil, errMoveInsideItself
	}
	if info, err := os.Stat(src.Dir); err != nil || !info.IsDir() {
		return nil, errDocumentUnavailable
	}
	// Like the API, the target and its parent must be accessible; checked
	// before Move looks at the target, so a restricted one is not revealed
	session := SessionFromContext(ctx)
	if !auth.CanAccessDocument(dst.LogicalPath, session, s.cfg) || !auth.CanAccessDocument(path.Dir(dst.LogicalPath), session, s.cfg) {
		return nil, &toolError{"Access denied at new_path"}
	}

	moved := 0
	check := func(subtree []documents.Moved) error {
		var changes []string
		for _, m := range subtree {
			if !auth.CanAccessDocument(m.From, session, s.cfg) || !auth.CanAccessDocument(m.To, session, s.cfg) {
				return &toolError{"Cannot move: the section contains documents you cannot access"}
			}
			if !m.IsDocument {
				continue
			}
			moved++
			from := auth.MatchingRuleIndex(m.From, s.cfg.AccessRules)
			to := auth.MatchingRuleIndex(m.To, s.cfg.AccessRules)
			if from != to {
				changes = append(changes, fmt.Sprintf("%s: %s → %s", displayPath(m.From), describeRule(s.cfg, from), describeRule(s.cfg, to)))
			}
		}
		// Admins are asked too: this guards against exposing documents by
		// mistake, not against missing permissions
		if len(changes) > 0 && !args.AllowAccessChange {
			return &toolError{fmt.Sprintf("Moving changes the access rules of %s, e.g. %s; ask the user and pass allow_access_change: true to confirm",
				plural(len(changes), "document"), strings.Join(changes[:min(len(changes), maxAccessChanges)], ", "))}
		}
		return nil
	}
	err = documents.Move(s.cfg, src, dst, check)
	switch {
	case errors.Is(err, documents.ErrNotFound):
		return nil, errDocumentUnavailable
	case errors.Is(err, documents.ErrTargetExists):
		return nil, &toolError{"A document already exists at new_path"}
	case errors.Is(err, documents.ErrInsideItself):
		return nil, errMoveInsideItself
	case err != nil:
		return nil, err
	}
	logger.Info("MCP move_document %s -> %s (%d documents) by %s (token %q)", src.LogicalPath, dst.LogicalPath, moved, session.Username, session.TokenName)

	var out strings.Builder
	fmt.Fprintf(&out, "Moved %s → %s (%s).\n", displayPath(src.LogicalPath), displayPath(dst.LogicalPath), plural(moved, "document"))
	out.WriteString(referencesSection(ctx, s, "documents linking to the old path", src.LogicalPath, dst.LogicalPath))
	return []string{out.String()}, nil
}

// describeRule names the access rule at index, e.g. "restricted (/finance/**)";
// -1 is the wiki default, "public" or "private".
func describeRule(cfg *config.Config, index int) string {
	if index < 0 {
		if cfg.Wiki.Private {
			return "private"
		}
		return "public"
	}
	rule := cfg.AccessRules[index]
	return fmt.Sprintf("%s (%s)", rule.Access, rule.Pattern)
}

// referencesSection lists the documents the caller may access that link to
// logicalPath, other than those under exclude.
func referencesSection(ctx context.Context, s *Server, heading, logicalPath, exclude string) string {
	references, err := documents.References(s.cfg, logicalPath, exclude)
	if err != nil {
		// The change is done; report it without the references
		logger.Error("MCP references to %s: %v", logicalPath, err)
		return heading + ": could not be checked\n"
	}
	session := SessionFromContext(ctx)
	var lines []string
	for _, reference := range references {
		if auth.CanAccessDocument(reference, session, s.cfg) {
			lines = append(lines, "- "+displayPath(reference))
		}
	}
	if len(lines) == 0 {
		return heading + ": none\n"
	}
	if len(lines) > maxReferences {
		lines = append(lines[:maxReferences], fmt.Sprintf("and %d more", len(lines)-maxReferences))
	}
	return heading + " (update them with edit_document):\n" + strings.Join(lines, "\n") + "\n"
}
