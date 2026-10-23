package mcp

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wiki-go/internal/auth"
	"wiki-go/internal/comments"
	"wiki-go/internal/config"
)

const mismatchMessage = "Revision mismatch: the document changed since you read it; read it again"

// addToken adds a user with a personal access token and returns the token.
func (w *testWiki) addToken(t *testing.T, user config.User, tokenRole string) string {
	t.Helper()
	token, hash, err := auth.GeneratePersonalToken()
	if err != nil {
		t.Fatal(err)
	}
	user.Tokens = []config.APIToken{{Name: "agent", Hash: hash, Role: tokenRole}}
	w.cfg.Users = append(w.cfg.Users, user)
	auth.SetConfig(w.cfg)
	return token
}

func (w *testWiki) documentFile(path string) string {
	if path == "/" {
		return filepath.Join(w.cfg.Wiki.RootDir, "pages", "home", "document.md")
	}
	return filepath.Join(w.cfg.Wiki.RootDir, "documents", filepath.FromSlash(path), "document.md")
}

func (w *testWiki) content(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(w.documentFile(path))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// versions returns the stored versions of a document, oldest first.
func (w *testWiki) versions(t *testing.T, path string) []string {
	t.Helper()
	dir := filepath.Join(w.cfg.Wiki.RootDir, "versions", "documents", filepath.FromSlash(path))
	if path == "/" {
		dir = filepath.Join(w.cfg.Wiki.RootDir, "versions", "pages", "home")
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	var contents []string
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		contents = append(contents, string(content))
	}
	return contents
}

// revision reads a document with read_document and returns its revision.
func (w *testWiki) revision(t *testing.T, token, path string) string {
	t.Helper()
	result := w.call(t, token, "read_document", map[string]string{"path": path})
	for _, line := range strings.Split(firstText(result), "\n") {
		if revision, ok := strings.CutPrefix(line, "revision: "); ok {
			return revision
		}
	}
	t.Fatalf("read_document result = %+v, want a revision", result.Result)
	return ""
}

func assertToolError(t *testing.T, result rpcResult, want string) {
	t.Helper()
	if !result.Result.IsError || firstText(result) != want {
		t.Fatalf("result = %+v, want error %q", result.Result, want)
	}
}

func assertToolText(t *testing.T, result rpcResult, want string) {
	t.Helper()
	if result.Result.IsError || firstText(result) != want {
		t.Fatalf("result = %q (isError=%t), want %q", firstText(result), result.Result.IsError, want)
	}
}

func edit(oldText, newText string) map[string]interface{} {
	return map[string]interface{}{"old_text": oldText, "new_text": newText}
}

func TestToolsListForEditors(t *testing.T) {
	wiki := newTestWiki(t)
	cappedToken := wiki.addToken(t, config.User{Username: "dave", Role: config.RoleEditor}, config.RoleViewer)

	toolNames := func(token string) string {
		list := decodeRPC(t, wiki.post(t, token, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`))
		var names []string
		for _, tool := range list.Result.Tools {
			names = append(names, tool.Name)
		}
		return strings.Join(names, ",")
	}

	if names := toolNames(wiki.financeToken); names != "search_documents,list_documents,read_document,edit_document,write_document,add_comment" {
		t.Fatalf("editor tools = %s, want the 6 tools in registry order", names)
	}
	// A viewer token of an editor only gets the viewer tools
	if names := toolNames(cappedToken); names != "search_documents,list_documents,read_document,add_comment" {
		t.Fatalf("viewer token tools = %s, want the 4 viewer tools", names)
	}
	for _, name := range []string{"edit_document", "write_document"} {
		result := wiki.call(t, cappedToken, name, map[string]interface{}{"path": "guides/intro", "content": "# New", "revision": "x"})
		assertToolError(t, result, "Insufficient role: editor access required")
	}
	if wiki.content(t, "guides/intro") != "# Intro" {
		t.Fatalf("content = %q, want it untouched", wiki.content(t, "guides/intro"))
	}
}

func TestEditDocument(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Wiki.MaxVersions = 10
	carolToken := wiki.addToken(t, config.User{Username: "carol", Role: config.RoleEditor}, "")
	wiki.writeDocument(t, "guides/intro", "# Intro\n\nDeploy on port 80.\n")

	result := wiki.call(t, wiki.viewerToken, "edit_document", map[string]interface{}{
		"path": "guides/intro", "revision": "x", "edits": []interface{}{edit("80", "8080")},
	})
	assertToolError(t, result, "Insufficient role: editor access required")

	revision := wiki.revision(t, wiki.financeToken, "guides/intro")
	result = wiki.call(t, wiki.financeToken, "edit_document", map[string]interface{}{
		"path": "guides/intro", "revision": revision, "edits": []interface{}{edit("port 80", "port 8080")},
	})
	text := firstText(result)
	newRevision := wiki.revision(t, wiki.financeToken, "guides/intro")
	if result.Result.IsError || !strings.HasPrefix(text, "Saved guides/intro (1 edit applied).\nrevision: "+newRevision+"\nprevious version: ") {
		t.Fatalf("result = %q, want the save summary with the new revision %s", text, newRevision)
	}
	if wiki.content(t, "guides/intro") != "# Intro\n\nDeploy on port 8080.\n" {
		t.Fatalf("content = %q", wiki.content(t, "guides/intro"))
	}
	if versions := wiki.versions(t, "guides/intro"); len(versions) != 1 || versions[0] != "# Intro\n\nDeploy on port 80.\n" {
		t.Fatalf("versions = %q, want the previous content", versions)
	}

	// The revision read before the edit is now stale
	result = wiki.call(t, wiki.financeToken, "edit_document", map[string]interface{}{
		"path": "guides/intro", "revision": revision, "edits": []interface{}{edit("8080", "9090")},
	})
	assertToolError(t, result, mismatchMessage)
	if wiki.content(t, "guides/intro") != "# Intro\n\nDeploy on port 8080.\n" {
		t.Fatalf("content after mismatch = %q, want it untouched", wiki.content(t, "guides/intro"))
	}

	// Access rules apply to the edited document
	reportRevision := wiki.revision(t, wiki.financeToken, "finance/report")
	result = wiki.call(t, carolToken, "edit_document", map[string]interface{}{
		"path": "finance/report", "revision": reportRevision, "edits": []interface{}{edit("Report", "Leaked")},
	})
	assertToolError(t, result, errDocumentUnavailable.message)
	result = wiki.call(t, wiki.financeToken, "edit_document", map[string]interface{}{
		"path": "finance/report", "revision": reportRevision, "edits": []interface{}{edit("Report", "Quarterly report"), edit("# ", "## ")},
	})
	if result.Result.IsError || !strings.HasPrefix(firstText(result), "Saved finance/report (2 edits applied).\n") {
		t.Fatalf("finance result = %+v, want the edits saved", result.Result)
	}
	if wiki.content(t, "finance/report") != "## Quarterly report" {
		t.Fatalf("finance content = %q, want both edits in order", wiki.content(t, "finance/report"))
	}
}

func TestEditDocumentAppliesAllEditsOrNone(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Wiki.MaxVersions = 10
	const original = "# Intro\n\nStep one.\nStep two.\n----\n"
	wiki.writeDocument(t, "guides/intro", original)
	revision := wiki.revision(t, wiki.financeToken, "guides/intro")

	tooMany := make([]interface{}, maxEdits+1)
	for i := range tooMany {
		tooMany[i] = edit("Step", "Stage")
	}
	tests := []struct {
		name     string
		revision string
		edits    []interface{}
		path     string
		want     string
	}{
		{"missing revision", "", []interface{}{edit("one", "1")}, "guides/intro", "Invalid arguments: revision is required; read the document first"},
		{"no edits", revision, []interface{}{}, "guides/intro", "Invalid arguments: edits must have between 1 and 50 items"},
		{"too many edits", revision, tooMany, "guides/intro", "Invalid arguments: edits must have between 1 and 50 items"},
		{"empty old_text", revision, []interface{}{edit("one", "1"), edit("", "x")}, "guides/intro", "Edit 2: old_text must not be empty"},
		{"same text", revision, []interface{}{edit("one", "one")}, "guides/intro", "Edit 1: new_text equals old_text"},
		{"second edit not found", revision, []interface{}{edit("one", "1"), edit("three", "3")}, "guides/intro", "Edit 2: old_text not found"},
		{"edit sees the previous result", revision, []interface{}{edit("one", "1"), edit("one", "uno")}, "guides/intro", "Edit 2: old_text not found"},
		{"ambiguous", revision, []interface{}{edit("Step", "Stage")}, "guides/intro", "Edit 1: old_text matches 2 times; add surrounding text or set replace_all"},
		{"overlapping matches", revision, []interface{}{edit("---", "===")}, "guides/intro", "Edit 1: old_text matches 2 times; add surrounding text or set replace_all"},
		{"empty result", revision, []interface{}{edit(original, " \n")}, "guides/intro", "Edits would leave the document empty"},
		{"stale revision", "0000000000000000", []interface{}{edit("one", "1")}, "guides/intro", mismatchMessage},
		{"missing document", revision, []interface{}{edit("one", "1")}, "guides/missing", errDocumentUnavailable.message},
		{"parent traversal", revision, []interface{}{edit("secret", "x")}, "../config.yaml", "Invalid document path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := wiki.call(t, wiki.financeToken, "edit_document", map[string]interface{}{
				"path": test.path, "revision": test.revision, "edits": test.edits,
			})
			assertToolError(t, result, test.want)
			if wiki.content(t, "guides/intro") != original || len(wiki.versions(t, "guides/intro")) != 0 {
				t.Fatalf("content = %q, versions = %q; want nothing written", wiki.content(t, "guides/intro"), wiki.versions(t, "guides/intro"))
			}
		})
	}

	replaceAll := edit("Step", "Stage")
	replaceAll["replace_all"] = true
	result := wiki.call(t, wiki.financeToken, "edit_document", map[string]interface{}{
		"path": "guides/intro", "revision": revision, "edits": []interface{}{replaceAll},
	})
	if result.Result.IsError || wiki.content(t, "guides/intro") != "# Intro\n\nStage one.\nStage two.\n----\n" {
		t.Fatalf("replace_all result = %+v, content = %q", result.Result, wiki.content(t, "guides/intro"))
	}
	if secret := readFile(t, filepath.Join(wiki.cfg.Wiki.RootDir, "config.yaml")); secret != "secret: true" {
		t.Fatalf("config.yaml = %q, want it untouched", secret)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func TestWriteDocumentCreates(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Wiki.MaxVersions = 10
	carolToken := wiki.addToken(t, config.User{Username: "carol", Role: config.RoleEditor}, "")
	const content = "# Deploy\n\nRun make deploy.\n"

	result := wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "guides/deploy", "content": content})
	assertToolText(t, result, "Created guides/deploy.\nrevision: "+wiki.revision(t, wiki.financeToken, "guides/deploy")+"\n")
	if wiki.content(t, "guides/deploy") != content || len(wiki.versions(t, "guides/deploy")) != 0 {
		t.Fatalf("content = %q, versions = %q; want the exact content and no version", wiki.content(t, "guides/deploy"), wiki.versions(t, "guides/deploy"))
	}
	list := wiki.call(t, wiki.viewerToken, "list_documents", map[string]string{"prefix": "guides"})
	if !strings.Contains(firstText(list), "guides/deploy — Deploy\n") {
		t.Fatalf("list = %q, want the new document", firstText(list))
	}

	// Intermediate directories get no document.md, like the API
	result = wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "/ops/runbooks/restart/", "content": "# Restart"})
	assertToolText(t, result, "Created ops/runbooks/restart.\nrevision: "+wiki.revision(t, wiki.financeToken, "ops/runbooks/restart")+"\n")
	if _, err := os.Stat(wiki.documentFile("ops/runbooks")); !os.IsNotExist(err) {
		t.Fatalf("intermediate document stat = %v, want none", err)
	}

	tests := []struct {
		name      string
		token     string
		arguments map[string]string
		want      string
	}{
		{"viewer", wiki.viewerToken, map[string]string{"path": "guides/new", "content": "# New"}, "Insufficient role: editor access required"},
		{"empty content", wiki.financeToken, map[string]string{"path": "guides/new", "content": " \n"}, "Invalid arguments: content must not be empty"},
		{"non canonical path", wiki.financeToken, map[string]string{"path": "Guides/My Doc", "content": "# New"}, `Invalid path for a new document; use "Guides/My-Doc"`},
		{"revision for a missing document", wiki.financeToken, map[string]string{"path": "guides/new", "content": "# New", "revision": "4396674f8992f2c5"}, errDocumentUnavailable.message},
		{"restricted section", carolToken, map[string]string{"path": "finance/new-report", "content": "# New"}, errDocumentUnavailable.message},
		{"parent traversal", wiki.financeToken, map[string]string{"path": "../outside", "content": "# New"}, "Invalid document path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertToolError(t, wiki.call(t, test.token, "write_document", test.arguments), test.want)
		})
	}
	for _, dir := range []string{
		filepath.Join(wiki.cfg.Wiki.RootDir, "documents", "guides", "new"),
		filepath.Join(wiki.cfg.Wiki.RootDir, "documents", "Guides", "My Doc"),
		filepath.Join(wiki.cfg.Wiki.RootDir, "documents", "finance", "new-report"),
		filepath.Join(wiki.cfg.Wiki.RootDir, "outside"),
	} {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want nothing created", dir, err)
		}
	}
}

func TestWriteDocumentReplaces(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Wiki.MaxVersions = 10
	revision := wiki.revision(t, wiki.financeToken, "guides/intro")

	result := wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "guides/intro", "content": "# New intro"})
	assertToolError(t, result, "Document exists; pass the revision from read_document to replace it")
	result = wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "guides/intro", "content": "# New intro", "revision": "0000000000000000"})
	assertToolError(t, result, mismatchMessage)
	if wiki.content(t, "guides/intro") != "# Intro" || len(wiki.versions(t, "guides/intro")) != 0 {
		t.Fatalf("content = %q, want it untouched", wiki.content(t, "guides/intro"))
	}

	// Identical content is unchanged, with or without a revision
	for _, arguments := range []map[string]string{
		{"path": "guides/intro", "content": "# Intro"},
		{"path": "guides/intro", "content": "# Intro", "revision": revision},
	} {
		result := wiki.call(t, wiki.financeToken, "write_document", arguments)
		assertToolText(t, result, "Unchanged guides/intro: the content is identical.\nrevision: "+revision+"\n")
	}
	if len(wiki.versions(t, "guides/intro")) != 0 {
		t.Fatalf("versions = %q, want none for identical content", wiki.versions(t, "guides/intro"))
	}

	result = wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "guides/intro", "content": "# New intro", "revision": revision})
	text := firstText(result)
	if result.Result.IsError || !strings.HasPrefix(text, "Saved guides/intro.\nrevision: "+wiki.revision(t, wiki.financeToken, "guides/intro")+"\nprevious version: ") {
		t.Fatalf("result = %q, want the save summary", text)
	}
	if wiki.content(t, "guides/intro") != "# New intro" {
		t.Fatalf("content = %q", wiki.content(t, "guides/intro"))
	}
	if versions := wiki.versions(t, "guides/intro"); len(versions) != 1 || versions[0] != "# Intro" {
		t.Fatalf("versions = %q, want the previous content", versions)
	}

	// The home page is rewritten with its revision
	homeRevision := wiki.revision(t, wiki.financeToken, "/")
	result = wiki.call(t, wiki.financeToken, "write_document", map[string]string{"path": "/", "content": "# Welcome", "revision": homeRevision})
	if result.Result.IsError || !strings.HasPrefix(firstText(result), "Saved /.\n") || wiki.content(t, "/") != "# Welcome" {
		t.Fatalf("home result = %+v, content = %q", result.Result, wiki.content(t, "/"))
	}
	if versions := wiki.versions(t, "/"); len(versions) != 1 || versions[0] != "# Home" {
		t.Fatalf("home versions = %q, want the previous home page", versions)
	}
}

func TestAddComment(t *testing.T) {
	wiki := newTestWiki(t)

	result := wiki.call(t, wiki.viewerToken, "add_comment", map[string]string{"path": "guides/intro", "content": "Typo in **step 2**"})
	assertToolText(t, result, "Comment added to guides/intro.")
	read := wiki.call(t, wiki.viewerToken, "read_document", map[string]interface{}{"path": "guides/intro", "include": []string{"comments"}})
	if !strings.Contains(firstText(read), "comments (oldest first):\n- alice, ") || !strings.Contains(firstText(read), "  Typo in **step 2**\n") {
		t.Fatalf("metadata = %q, want the comment by the token owner", firstText(read))
	}

	wiki.writeDocument(t, "guides/locked", "# Locked\n\n<!-- no comments -->")
	tests := []struct {
		name      string
		arguments map[string]string
		want      string
	}{
		{"marker", map[string]string{"path": "guides/locked", "content": "Hi"}, "Comments are not allowed for this document"},
		{"empty content", map[string]string{"path": "guides/intro", "content": "  "}, "Invalid arguments: content must not be empty"},
		{"restricted document", map[string]string{"path": "finance/report", "content": "Hi"}, errDocumentUnavailable.message},
		{"missing document", map[string]string{"path": "guides/missing", "content": "Hi"}, errDocumentUnavailable.message},
		{"parent traversal", map[string]string{"path": "../config.yaml", "content": "Hi"}, "Invalid document path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertToolError(t, wiki.call(t, wiki.viewerToken, "add_comment", test.arguments), test.want)
		})
	}
	if _, err := os.Stat(filepath.Join(wiki.cfg.Wiki.RootDir, "comments", "finance")); !os.IsNotExist(err) {
		t.Fatalf("restricted comments stat = %v, want nothing created", err)
	}

	wiki.cfg.Wiki.DisableComments = true
	auth.SetConfig(wiki.cfg)
	result = wiki.call(t, wiki.viewerToken, "add_comment", map[string]string{"path": "guides/intro", "content": "Hi"})
	assertToolError(t, result, "Comments are disabled in this wiki")
}

func TestAddCommentKeepsTheFirstCommentOfASecond(t *testing.T) {
	wiki := newTestWiki(t)
	previous := comments.Now
	at := time.Date(2026, 10, 22, 20, 30, 0, 0, time.Local)
	comments.Now = func() time.Time { return at }
	t.Cleanup(func() { comments.Now = previous })

	assertToolText(t, wiki.call(t, wiki.viewerToken, "add_comment", map[string]string{"path": "/", "content": "First"}), "Comment added to /.")
	result := wiki.call(t, wiki.viewerToken, "add_comment", map[string]string{"path": "/", "content": "Second"})
	assertToolError(t, result, "You already commented on this document this second; retry")

	list, err := comments.GetComments(filepath.Join(wiki.cfg.Wiki.RootDir, "comments"), "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Content != "First" {
		t.Fatalf("comments = %+v, want only the first one", list)
	}
}

func TestMCPRejectsLargeRequests(t *testing.T) {
	wiki := newTestWiki(t)
	body := toolCall("write_document", map[string]string{"path": "guides/big", "content": strings.Repeat("a", maxRequestBytes)})

	response := wiki.post(t, wiki.financeToken, body)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"message":"Request too large (limit 1 MiB)"`) {
		t.Fatalf("body = %s, want the size limit message", response.Body.String())
	}
	if _, err := os.Stat(wiki.documentFile("guides/big")); !os.IsNotExist(err) {
		t.Fatalf("document stat = %v, want nothing written", err)
	}
}
