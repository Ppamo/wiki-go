package mcp

import (
	"path/filepath"
	"strings"
	"testing"
	"wiki-go/internal/auth"
	"wiki-go/internal/comments"
	"wiki-go/internal/config"
)

func (w *testWiki) call(t *testing.T, token, name string, arguments interface{}) rpcResult {
	t.Helper()
	result := decodeRPC(t, w.post(t, token, toolCall(name, arguments)))
	if result.Error != nil {
		t.Fatalf("protocol error = %+v, want a tool result", result.Error)
	}
	return result
}

func (w *testWiki) writeDocument(t *testing.T, path, content string) {
	t.Helper()
	writeDocument(t, filepath.Join(w.cfg.Wiki.RootDir, "documents", filepath.FromSlash(path), "document.md"), content)
}

func firstText(result rpcResult) string {
	if len(result.Result.Content) == 0 {
		return ""
	}
	return result.Result.Content[0].Text
}

func TestSearchDocumentsAppliesAccessRules(t *testing.T) {
	wiki := newTestWiki(t)

	result := wiki.call(t, wiki.viewerToken, "search_documents", map[string]string{"query": "report"})
	if result.Result.IsError || firstText(result) != `No documents match "report".` {
		t.Fatalf("viewer result = %+v, want no visible matches", result.Result)
	}

	result = wiki.call(t, wiki.financeToken, "search_documents", map[string]string{"query": "report"})
	if result.Result.IsError || !strings.Contains(firstText(result), "- finance/report — Report") {
		t.Fatalf("finance result = %+v, want the restricted report", result.Result)
	}
}

func TestSearchDocumentsIgnoresMarkdownFilesInsideDocuments(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.AccessRules = []config.AccessRule{{
		Pattern: "/finance/report",
		Access:  "restricted",
		Groups:  []string{"finance"},
	}}
	auth.SetConfig(wiki.cfg)
	writeDocument(t, filepath.Join(wiki.cfg.Wiki.RootDir, "documents", "finance", "report", "notes.md"), "private-search-marker")

	result := wiki.call(t, wiki.viewerToken, "search_documents", map[string]string{"query": "private-search-marker"})
	if result.Result.IsError || firstText(result) != `No documents match "private-search-marker".` {
		t.Fatalf("result = %+v, want the non-document Markdown file excluded", result.Result)
	}
}

func TestSearchDocumentsLimitsResults(t *testing.T) {
	wiki := newTestWiki(t)
	for _, path := range []string{"ops/deploy-a", "ops/deploy-b", "ops/deploy-c"} {
		wiki.writeDocument(t, path, "# Deploy\n\nSet the deploy port.")
	}

	result := wiki.call(t, wiki.viewerToken, "search_documents", map[string]interface{}{"query": "deploy port", "limit": 2})
	text := firstText(result)
	if result.Result.IsError || !strings.HasPrefix(text, `Matches for "deploy port": 3 (showing the first 2`) || strings.Count(text, "\n- ") != 2 {
		t.Fatalf("result = %q, want 2 of 3 matches", text)
	}

	result = wiki.call(t, wiki.viewerToken, "search_documents", map[string]interface{}{"query": "deploy", "limit": -1})
	if !result.Result.IsError || !strings.Contains(firstText(result), "limit must be 1 or greater") {
		t.Fatalf("negative limit result = %+v, want an error", result.Result)
	}

	result = wiki.call(t, wiki.viewerToken, "search_documents", map[string]string{"query": "  "})
	if !result.Result.IsError || !strings.Contains(firstText(result), "query must not be empty") {
		t.Fatalf("empty query result = %+v, want an error", result.Result)
	}
}

func TestListDocuments(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.writeDocument(t, "guides/intro/setup", "# Setup")
	wiki.writeDocument(t, ".drafts/secret", "# Hidden directory")

	tests := []struct {
		name      string
		token     string
		arguments map[string]interface{}
		want      string
	}{
		{"default depth", wiki.viewerToken, map[string]interface{}{},
			"Wiki documents (deeper documents are counted as \"+N below\"):\n/ — Home\nguides/intro — Intro (+1 below)\n"},
		{"restricted documents with group", wiki.financeToken, map[string]interface{}{},
			"Wiki documents (deeper documents are counted as \"+N below\"):\n/ — Home\nfinance/report — Report\nguides/intro — Intro (+1 below)\n"},
		{"depth one counts on a folder", wiki.viewerToken, map[string]interface{}{"depth": 1},
			"Wiki documents (deeper documents are counted as \"+N below\"):\n/ — Home\nguides (+2 below)\n"},
		{"all levels under a prefix", wiki.viewerToken, map[string]interface{}{"prefix": "/guides/", "depth": 0},
			"Documents under \"guides\":\nguides/intro — Intro\nguides/intro/setup — Setup\n"},
		{"restricted prefix", wiki.viewerToken, map[string]interface{}{"prefix": "finance"},
			"No documents found under \"finance\"."},
		{"missing prefix", wiki.viewerToken, map[string]interface{}{"prefix": "missing"},
			"No documents found under \"missing\"."},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := wiki.call(t, test.token, "list_documents", test.arguments)
			if result.Result.IsError || firstText(result) != test.want {
				t.Fatalf("result = %q, want %q", firstText(result), test.want)
			}
		})
	}

	result := wiki.call(t, wiki.viewerToken, "list_documents", map[string]string{"prefix": "../config.yaml"})
	if !result.Result.IsError || firstText(result) != "Invalid prefix" {
		t.Fatalf("traversal result = %+v, want an invalid prefix error", result.Result)
	}
}

func TestReadDocumentSections(t *testing.T) {
	wiki := newTestWiki(t)
	root := wiki.cfg.Wiki.RootDir
	writeDocument(t, filepath.Join(root, "documents", "guides", "intro", "chart.png"), "png")
	writeDocument(t, filepath.Join(root, "documents", "guides", "intro", "tool.exe"), "exe")
	writeDocument(t, filepath.Join(root, "versions", "documents", "guides", "intro", "20261022101500.md"), "# Intro v1")
	if err := comments.AddComment(filepath.Join(root, "comments"), "/guides/intro", "Looks good\nShip it", "alice"); err != nil {
		t.Fatal(err)
	}

	result := wiki.call(t, wiki.viewerToken, "read_document", map[string]interface{}{
		"path":    "guides/intro",
		"include": []string{"comments", "attachments"},
	})
	metadata := firstText(result)
	for _, want := range []string{
		"path: guides/intro\nrevision: " + contentRevision([]byte("# Intro")) + "\n",
		"attachments:\n- chart.png (image/png, 3 bytes)\n",
		"comments (oldest first):\n- alice, ",
		"  Looks good\n  Ship it\n",
	} {
		if !strings.Contains(metadata, want) {
			t.Fatalf("metadata = %q, want it to contain %q", metadata, want)
		}
	}
	if strings.Contains(metadata, "tool.exe") || documentText(result) != "# Intro" {
		t.Fatalf("result = %+v, want allowed attachments and the document", result.Result)
	}

	for _, arguments := range []map[string]interface{}{
		{"path": "guides/intro", "include": []string{"history"}},
		{"path": "guides/intro", "version": "20261022101500"},
	} {
		result := wiki.call(t, wiki.viewerToken, "read_document", arguments)
		if !result.Result.IsError || firstText(result) != "Insufficient role: editor access required" {
			t.Fatalf("viewer result for %v = %+v, want an editor role error", arguments, result.Result)
		}
	}

	result = wiki.call(t, wiki.financeToken, "read_document", map[string]interface{}{"path": "guides/intro", "include": []string{"history"}})
	if !strings.Contains(firstText(result), "history (newest first):\n- 20261022101500\n") {
		t.Fatalf("history = %q, want the stored version", firstText(result))
	}

	result = wiki.call(t, wiki.financeToken, "read_document", map[string]interface{}{"path": "guides/intro", "version": "20261022101500"})
	if firstText(result) != "path: guides/intro\nversion: 20261022101500\n" || documentText(result) != "# Intro v1" {
		t.Fatalf("version result = %+v, want the old content without a revision", result.Result)
	}

	result = wiki.call(t, wiki.financeToken, "read_document", map[string]interface{}{"path": "/", "include": []string{"history", "attachments"}})
	if !strings.Contains(firstText(result), "attachments: none\n\nhistory: none\n") {
		t.Fatalf("home sections = %q, want empty sections", firstText(result))
	}

	errors := []struct {
		arguments map[string]interface{}
		want      string
	}{
		{map[string]interface{}{"path": "guides/intro", "version": "20990101000000"}, "Version not found"},
		{map[string]interface{}{"path": "guides/intro", "version": "latest"}, "version must be a yyyymmddhhmmss timestamp"},
		{map[string]interface{}{"path": "guides/intro", "include": []string{"secrets"}}, "include accepts"},
		{map[string]interface{}{"path": "finance/report", "version": "20261022101500"}, "Version not found"},
	}
	for _, test := range errors {
		result := wiki.call(t, wiki.financeToken, "read_document", test.arguments)
		if !result.Result.IsError || !strings.Contains(firstText(result), test.want) {
			t.Fatalf("result for %v = %+v, want an error containing %q", test.arguments, result.Result, test.want)
		}
	}

	wiki.cfg.Wiki.DisableComments = true
	auth.SetConfig(wiki.cfg)
	result = wiki.call(t, wiki.viewerToken, "read_document", map[string]interface{}{"path": "guides/intro", "include": []string{"comments"}})
	if !strings.Contains(firstText(result), "comments: disabled in this wiki\n") {
		t.Fatalf("comments = %q, want them disabled", firstText(result))
	}
}

func TestReadDocumentHidesRestrictedHistory(t *testing.T) {
	wiki := newTestWiki(t)
	writeDocument(t, filepath.Join(wiki.cfg.Wiki.RootDir, "versions", "documents", "finance", "report", "20261022101500.md"), "# Old report")
	wiki.cfg.Users[0].Role = "editor"
	auth.SetConfig(wiki.cfg)

	result := wiki.call(t, wiki.viewerToken, "read_document", map[string]interface{}{"path": "finance/report", "version": "20261022101500"})
	if !result.Result.IsError || firstText(result) != errDocumentUnavailable.message {
		t.Fatalf("result = %+v, want the restricted document hidden", result.Result)
	}
}
