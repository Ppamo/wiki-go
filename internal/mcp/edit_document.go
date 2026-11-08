package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"wiki-go/internal/documents"
	"wiki-go/internal/roles"
)

const maxEdits = 50

var editDocumentTool = tool{
	name:  "edit_document",
	title: "Edit document",
	description: "Replaces exact text in a wiki document. Pass the revision from read_document; " +
		"each edit's old_text must match exactly once in the current content unless replace_all " +
		"is set. Edits apply in order and all or none are saved. The previous content is kept " +
		"in the history. Requests are limited to 1 MiB.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"path": map[string]interface{}{
				"type":        "string",
				"description": "Document path, e.g. \"guides/intro\"; \"/\" for the home page",
			},
			"revision": map[string]interface{}{
				"type":        "string",
				"description": "Revision returned by read_document",
			},
			"edits": map[string]interface{}{
				"type":     "array",
				"minItems": 1,
				"maxItems": maxEdits,
				"items": map[string]interface{}{
					"type": "object",
					"properties": map[string]interface{}{
						"old_text": map[string]interface{}{
							"type":        "string",
							"description": "Exact text to replace, copied from the Markdown block of read_document",
						},
						"new_text": map[string]interface{}{
							"type":        "string",
							"description": "Replacement text; may be empty to delete old_text",
						},
						"replace_all": map[string]interface{}{
							"type":        "boolean",
							"description": "Replace every occurrence instead of requiring a single match",
							"default":     false,
						},
					},
					"required": []string{"old_text", "new_text"},
				},
			},
		},
		"required": []string{"path", "revision", "edits"},
	},
	annotations: writeAnnotations(false, true),
	role:        roles.RoleEditor,
	run:         editDocument,
}

type documentEdit struct {
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all"`
}

func editDocument(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Path     string         `json:"path"`
		Revision string         `json:"revision"`
		Edits    []documentEdit `json:"edits"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	if args.Revision == "" {
		return nil, errRevisionRequired
	}
	if len(args.Edits) < 1 || len(args.Edits) > maxEdits {
		return nil, &toolError{fmt.Sprintf("Invalid arguments: edits must have between 1 and %d items", maxEdits)}
	}
	for i, edit := range args.Edits {
		if edit.OldText == "" {
			return nil, &toolError{fmt.Sprintf("Edit %d: old_text must not be empty", i+1)}
		}
		if edit.OldText == edit.NewText {
			return nil, &toolError{fmt.Sprintf("Edit %d: new_text equals old_text", i+1)}
		}
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
	if documents.Revision(content) != args.Revision {
		return nil, errRevisionMismatch
	}

	// Each edit applies to the result of the previous one; nothing is saved
	// unless all of them match
	edited := string(content)
	for i, edit := range args.Edits {
		matches := countMatches(edited, edit.OldText)
		switch {
		case matches == 0:
			return nil, &toolError{fmt.Sprintf("Edit %d: old_text not found", i+1)}
		case matches > 1 && !edit.ReplaceAll:
			return nil, &toolError{fmt.Sprintf("Edit %d: old_text matches %d times; add surrounding text or set replace_all", i+1, matches)}
		}
		edited = strings.ReplaceAll(edited, edit.OldText, edit.NewText)
	}
	if strings.TrimSpace(edited) == "" {
		return nil, &toolError{"Edits would leave the document empty"}
	}

	result, err := saveDocument(ctx, s, "edit_document", doc, []byte(edited), documents.SaveOptions{ExpectedRevision: args.Revision})
	if err != nil {
		return nil, err
	}
	detail := fmt.Sprintf(" (%d edits applied)", len(args.Edits))
	if len(args.Edits) == 1 {
		detail = " (1 edit applied)"
	}
	return []string{saveSummary(doc, result, detail)}, nil
}

// countMatches counts the positions where text occurs in content, including
// overlapping ones: "---" occurs twice in "----", so it does not identify a
// single place to edit.
func countMatches(content, text string) int {
	count := 0
	for start := 0; ; {
		index := strings.Index(content[start:], text)
		if index < 0 {
			return count
		}
		count++
		start += index + 1
	}
}
