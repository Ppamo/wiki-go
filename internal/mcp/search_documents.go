package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"wiki-go/internal/handlers"
	"wiki-go/internal/roles"
)

const (
	defaultSearchLimit = 10
	maxSearchLimit     = 50
)

var searchDocumentsTool = tool{
	name:  "search_documents",
	title: "Search documents",
	description: "Full-text search over the wiki documents. Words are ANDed and matched " +
		"case-insensitively, \"exact phrase\" matches a phrase and \"not word\" excludes a word. " +
		"Returns the path, title and an excerpt of each match; pass the path to read_document.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"query": map[string]interface{}{
				"type":        "string",
				"description": "Search query, e.g. \"deploy \\\"port number\\\" not draft\"",
			},
			"limit": map[string]interface{}{
				"type":        "integer",
				"description": fmt.Sprintf("Maximum number of results (default %d, max %d)", defaultSearchLimit, maxSearchLimit),
				"minimum":     1,
				"maximum":     maxSearchLimit,
			},
		},
		"required": []string{"query"},
	},
	annotations: readOnlyAnnotations,
	role:        roles.RoleViewer,
	run:         searchDocuments,
}

func searchDocuments(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Query string `json:"query"`
		Limit *int   `json:"limit"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		// An empty query matches every document; browsing is list_documents' job
		return nil, &toolError{"Invalid arguments: query must not be empty; use list_documents to browse"}
	}
	limit := defaultSearchLimit
	if args.Limit != nil {
		if *args.Limit < 1 {
			return nil, &toolError{"Invalid arguments: limit must be 1 or greater"}
		}
		limit = *args.Limit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}

	// The search applies the document access rules of the session
	results := handlers.PerformSearch(query, SessionFromContext(ctx), s.cfg)
	if len(results) == 0 {
		return []string{fmt.Sprintf("No documents match %q.", query)}, nil
	}

	var out strings.Builder
	shown := results
	if len(shown) > limit {
		shown = shown[:limit]
		fmt.Fprintf(&out, "Matches for %q: %d (showing the first %d; refine the query to narrow them).\n", query, len(results), limit)
	} else {
		fmt.Fprintf(&out, "Matches for %q: %d.\n", query, len(results))
	}
	for _, result := range shown {
		fmt.Fprintf(&out, "\n- %s — %s\n", displayPath(result.Path), result.Title)
		if excerpt := strings.Join(strings.Fields(result.Excerpt), " "); excerpt != "" {
			fmt.Fprintf(&out, "  %s\n", excerpt)
		}
	}
	return []string{out.String()}, nil
}
