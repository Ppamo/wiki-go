package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"wiki-go/internal/auth"
	"wiki-go/internal/roles"
	"wiki-go/internal/utils"
)

const (
	defaultListDepth = 2
	// maxListedDocuments bounds the result for agents on large wikis
	maxListedDocuments = 500
)

var listDocumentsTool = tool{
	name:  "list_documents",
	title: "List documents",
	description: "Lists the wiki documents as \"path — title\" lines in tree order. " +
		"Use prefix to list one section and depth to limit how many levels below it are shown; " +
		"an entry marked \"+N below\" has N deeper documents, listed by passing its path as prefix.",
	inputSchema: map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"prefix": map[string]interface{}{
				"type":        "string",
				"description": "Section to list, e.g. \"guides\"; empty lists the whole wiki including the home page",
			},
			"depth": map[string]interface{}{
				"type":        "integer",
				"description": fmt.Sprintf("Levels below the prefix to show (default %d, 0 for all)", defaultListDepth),
				"minimum":     0,
			},
		},
	},
	annotations: readOnlyAnnotations,
	role:        roles.RoleViewer,
	run:         listDocuments,
}

type listEntry struct {
	title      string
	isDocument bool
	// hidden counts the accessible documents below the depth limit
	hidden int
}

func listDocuments(ctx context.Context, s *Server, arguments json.RawMessage) ([]string, error) {
	var args struct {
		Prefix string `json:"prefix"`
		Depth  *int   `json:"depth"`
	}
	if err := decodeArguments(arguments, &args); err != nil {
		return nil, err
	}
	depth := defaultListDepth
	if args.Depth != nil {
		if *args.Depth < 0 {
			return nil, &toolError{"Invalid arguments: depth must be 0 or greater"}
		}
		depth = *args.Depth
	}

	session := SessionFromContext(ctx)
	documentsRoot := filepath.Join(s.cfg.Wiki.RootDir, s.cfg.Wiki.DocumentsDir)
	walkRoot := documentsRoot
	prefix := strings.Trim(args.Prefix, "/")
	prefixLevels := 0
	if prefix != "" {
		resolved, err := utils.ResolveRelativeDocumentPath(s.cfg.Wiki.RootDir, s.cfg.Wiki.DocumentsDir, prefix)
		if err != nil {
			return nil, &toolError{"Invalid prefix"}
		}
		walkRoot = resolved.FilesystemPath
		prefix = strings.TrimPrefix(resolved.LogicalPath, "/")
		prefixLevels = strings.Count(prefix, "/") + 1
	}

	// Entries are keyed by path without the leading slash; "" is the home page
	entries := map[string]*listEntry{}
	entryFor := func(key string) *listEntry {
		if entries[key] == nil {
			entries[key] = &listEntry{}
		}
		return entries[key]
	}

	homeDir := filepath.Join(s.cfg.Wiki.RootDir, "pages", "home")
	if prefix == "" && fileExists(filepath.Join(homeDir, "document.md")) && auth.CanAccessDocument("/", session, s.cfg) {
		home := entryFor("")
		home.title = utils.GetDocumentTitle(homeDir)
		home.isDocument = true
	}

	filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
		// Unreadable directories are skipped, like the search does
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != documentsRoot && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if !fileExists(filepath.Join(path, "document.md")) {
			return nil
		}
		relative, err := filepath.Rel(documentsRoot, path)
		if err != nil || relative == "." {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if !auth.CanAccessDocument("/"+relative, session, s.cfg) {
			return nil
		}

		levels := strings.Split(relative, "/")
		if depth == 0 || len(levels)-prefixLevels <= depth {
			entry := entryFor(relative)
			entry.title = utils.GetDocumentTitle(path)
			entry.isDocument = true
		} else {
			// Count it on its ancestor at the depth limit
			entryFor(strings.Join(levels[:prefixLevels+depth], "/")).hidden++
		}
		return nil
	})

	if len(entries) == 0 {
		if prefix == "" {
			return []string{"No documents found."}, nil
		}
		return []string{fmt.Sprintf("No documents found under %q.", prefix)}, nil
	}

	keys := make([]string, 0, len(entries))
	countsHidden := false
	for key, entry := range entries {
		keys = append(keys, key)
		countsHidden = countsHidden || entry.hidden > 0
	}
	// Sorting on segments keeps children right after their parent
	sort.Slice(keys, func(i, j int) bool {
		return strings.ReplaceAll(keys[i], "/", "\x00") < strings.ReplaceAll(keys[j], "/", "\x00")
	})

	var notes []string
	if countsHidden {
		notes = append(notes, "deeper documents are counted as \"+N below\"")
	}
	var out strings.Builder
	if prefix == "" {
		out.WriteString("Wiki documents")
	} else {
		fmt.Fprintf(&out, "Documents under %q", prefix)
	}
	if len(keys) > maxListedDocuments {
		notes = append(notes, fmt.Sprintf("first %d of %d entries; narrow the prefix or the depth", maxListedDocuments, len(keys)))
		keys = keys[:maxListedDocuments]
	}
	if len(notes) > 0 {
		out.WriteString(" (" + strings.Join(notes, "; ") + ")")
	}
	out.WriteString(":\n")

	for _, key := range keys {
		entry := entries[key]
		out.WriteString(displayPath("/" + key))
		if entry.isDocument {
			out.WriteString(" — " + entry.title)
		}
		if entry.hidden > 0 {
			fmt.Fprintf(&out, " (+%d below)", entry.hidden)
		}
		out.WriteString("\n")
	}
	return []string{out.String()}, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
