package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"wiki-go/internal/auth"
	"wiki-go/internal/comments"
	"wiki-go/internal/config"
	"wiki-go/internal/documents"
)

// restrict puts an access rule for the finance group before the others.
func (w *testWiki) restrict(pattern string) {
	w.cfg.AccessRules = append([]config.AccessRule{{Pattern: pattern, Access: "restricted", Groups: []string{"finance"}}}, w.cfg.AccessRules...)
	auth.SetConfig(w.cfg)
}

func (w *testWiki) assertExists(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(w.cfg.Wiki.RootDir, filepath.FromSlash(name))); err != nil {
			t.Fatalf("stat %s = %v, want it to exist", name, err)
		}
	}
}

func (w *testWiki) assertMissing(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(w.cfg.Wiki.RootDir, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want it missing", name, err)
		}
	}
}

func TestMoveDocument(t *testing.T) {
	wiki := newTestWiki(t)
	root := wiki.cfg.Wiki.RootDir
	carolToken := wiki.addToken(t, config.User{Username: "carol", Role: config.RoleEditor}, "")
	writeDocument(t, filepath.Join(root, "documents", "guides", "intro", "chart.png"), "png")
	writeDocument(t, filepath.Join(root, "versions", "documents", "guides", "intro", "20261022101500.md"), "# Intro v1")
	if err := comments.AddComment(filepath.Join(root, "comments"), "/guides/intro", "Nice", "alice"); err != nil {
		t.Fatal(err)
	}
	wiki.writeDocument(t, "guides/index", "Start with [the intro](/guides/intro).")
	wiki.writeDocument(t, "finance/links", "[[guides/intro]]")

	result := wiki.call(t, wiki.viewerToken, "move_document", map[string]string{"path": "guides/intro", "new_path": "guides/moved"})
	assertToolError(t, result, "Insufficient role: editor access required")

	// A rename takes the attachments, history and comments along; the
	// restricted finance/links also links to it but carol cannot see it
	result = wiki.call(t, carolToken, "move_document", map[string]string{"path": "guides/intro", "new_path": "guides/introduction"})
	assertToolText(t, result, "Moved guides/intro → guides/introduction (1 document).\n"+
		"documents linking to the old path (update them with edit_document):\n- guides/index\n")
	if references, _ := documents.References(wiki.cfg, "/guides/intro", "/guides/introduction"); !reflect.DeepEqual(references, []string{"/finance/links", "/guides/index"}) {
		t.Fatalf("references = %v, want the restricted one too", references)
	}
	read := wiki.call(t, wiki.financeToken, "read_document", map[string]interface{}{"path": "guides/introduction", "include": []string{"attachments", "history", "comments"}})
	for _, want := range []string{"- chart.png", "history (newest first):\n- 20261022101500\n", "comments (oldest first):\n- alice, "} {
		if !strings.Contains(firstText(read), want) {
			t.Fatalf("metadata = %q, want it to contain %q", firstText(read), want)
		}
	}
	assertToolError(t, wiki.call(t, carolToken, "read_document", map[string]string{"path": "guides/intro"}), errDocumentUnavailable.message)

	// To another section, then to the root
	result = wiki.call(t, carolToken, "move_document", map[string]string{"path": "guides/introduction", "new_path": "archive/intro"})
	assertToolText(t, result, "Moved guides/introduction → archive/intro (1 document).\ndocuments linking to the old path: none\n")
	result = wiki.call(t, carolToken, "move_document", map[string]string{"path": "/archive/intro/", "new_path": "intro"})
	assertToolText(t, result, "Moved archive/intro → intro (1 document).\ndocuments linking to the old path: none\n")
	wiki.assertExists(t, "documents/intro/document.md", "documents/intro/chart.png", "versions/documents/intro/20261022101500.md")

	// Moving a section needs access to all of it
	wiki.restrict("/guides/secret/**")
	wiki.writeDocument(t, "guides/secret", "# Secret")
	result = wiki.call(t, carolToken, "move_document", map[string]string{"path": "guides", "new_path": "manuals"})
	assertToolError(t, result, "Cannot move: the section contains documents you cannot access")
	wiki.assertExists(t, "documents/guides/secret/document.md", "documents/guides/index/document.md")
	wiki.assertMissing(t, "documents/manuals")

	// A change of access rule needs confirmation, also with access to both
	// Auxiliary directories move and require access, but are not documents
	// and therefore do not inflate the reported impact.
	writeDocument(t, filepath.Join(root, "documents", "guides", "index", "assets", "logo.svg"), "svg")
	result = wiki.call(t, wiki.financeToken, "move_document", map[string]string{"path": "guides/index", "new_path": "finance/index"})
	assertToolError(t, result, "Moving changes the access rules of 1 document, e.g. guides/index: public → restricted (/finance/**); "+
		"ask the user and pass allow_access_change: true to confirm")
	wiki.assertExists(t, "documents/guides/index/document.md")
	result = wiki.call(t, wiki.financeToken, "move_document", map[string]interface{}{"path": "guides/index", "new_path": "finance/index", "allow_access_change": true})
	assertToolText(t, result, "Moved guides/index → finance/index (1 document).\ndocuments linking to the old path: none\n")

	wiki.writeDocument(t, "guides/taken", "# Taken")
	tests := []struct {
		name      string
		arguments map[string]string
		want      string
	}{
		{"missing new_path", map[string]string{"path": "guides/taken"}, "Invalid arguments: new_path is required"},
		{"home source", map[string]string{"path": "/", "new_path": "home-copy"}, "The home page cannot be moved"},
		{"home target", map[string]string{"path": "guides/taken", "new_path": "/"}, "Cannot move a document to the home page"},
		{"non canonical", map[string]string{"path": "guides/taken", "new_path": "Archive/My Doc"}, `Invalid new_path; use "Archive/My-Doc"`},
		{"same path", map[string]string{"path": "guides/taken", "new_path": "/guides/taken/"}, "Invalid arguments: new_path equals path"},
		{"inside itself", map[string]string{"path": "guides", "new_path": "guides/sub/guides"}, "Cannot move a document inside itself"},
		{"missing source", map[string]string{"path": "guides/missing", "new_path": "guides/found"}, errDocumentUnavailable.message},
		{"restricted source", map[string]string{"path": "finance/report", "new_path": "guides/report"}, errDocumentUnavailable.message},
		{"restricted target", map[string]string{"path": "guides/taken", "new_path": "finance/taken"}, "Access denied at new_path"},
		{"restricted existing target", map[string]string{"path": "guides/taken", "new_path": "finance/report"}, "Access denied at new_path"},
		{"occupied target", map[string]string{"path": "guides/taken", "new_path": "intro"}, "A document already exists at new_path"},
		{"parent traversal", map[string]string{"path": "../config.yaml", "new_path": "config"}, "Invalid document path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertToolError(t, wiki.call(t, carolToken, "move_document", test.arguments), test.want)
		})
	}
	if wiki.content(t, "guides/taken") != "# Taken" || wiki.content(t, "intro") != "# Intro" {
		t.Fatalf("contents changed after the rejected moves")
	}
	wiki.assertMissing(t, "documents/finance/taken", "documents/guides/found", "documents/home-copy")
}

func TestRestoreVersion(t *testing.T) {
	wiki := newTestWiki(t)
	wiki.cfg.Wiki.MaxVersions = 10
	root := wiki.cfg.Wiki.RootDir
	carolToken := wiki.addToken(t, config.User{Username: "carol", Role: config.RoleEditor}, "")
	for name, content := range map[string]string{
		"guides/intro/20261022101500.md":   "# Intro v1",
		"guides/intro/20261022111500.md":   "# Intro v2",
		"finance/report/20261022101500.md": "# Report v1",
	} {
		writeDocument(t, filepath.Join(root, "versions", "documents", filepath.FromSlash(name)), content)
	}
	revision := wiki.revision(t, wiki.financeToken, "guides/intro")

	result := wiki.call(t, wiki.viewerToken, "restore_version", map[string]string{"path": "guides/intro", "version": "20261022101500", "revision": revision})
	assertToolError(t, result, "Insufficient role: editor access required")

	result = wiki.call(t, wiki.financeToken, "restore_version", map[string]string{"path": "guides/intro", "version": "20261022101500", "revision": revision})
	restored := wiki.revision(t, wiki.financeToken, "guides/intro")
	if text := firstText(result); result.Result.IsError || !strings.HasPrefix(text, "Restored guides/intro to version 20261022101500.\nrevision: "+restored+"\nprevious version: ") {
		t.Fatalf("result = %q, want the restore summary with revision %s", text, restored)
	}
	if wiki.content(t, "guides/intro") != "# Intro v1" {
		t.Fatalf("content = %q, want the exact version", wiki.content(t, "guides/intro"))
	}
	if versions := wiki.versions(t, "guides/intro"); len(versions) != 3 || !slices.Contains(versions, "# Intro") {
		t.Fatalf("versions = %q, want the replaced content kept", versions)
	}

	// The revision read before the restore is stale now
	result = wiki.call(t, wiki.financeToken, "restore_version", map[string]string{"path": "guides/intro", "version": "20261022111500", "revision": revision})
	assertToolError(t, result, mismatchMessage)
	if wiki.content(t, "guides/intro") != "# Intro v1" {
		t.Fatalf("content after mismatch = %q, want it untouched", wiki.content(t, "guides/intro"))
	}

	// A retry is unchanged, even with that stale revision
	result = wiki.call(t, wiki.financeToken, "restore_version", map[string]string{"path": "guides/intro", "version": "20261022101500", "revision": revision})
	assertToolText(t, result, "Unchanged guides/intro: the content is identical.\nrevision: "+restored+"\n")
	if versions := wiki.versions(t, "guides/intro"); len(versions) != 3 {
		t.Fatalf("versions = %q, want no new version", versions)
	}

	tests := []struct {
		name      string
		token     string
		arguments map[string]string
		want      string
	}{
		{"invalid version", wiki.financeToken, map[string]string{"path": "guides/intro", "version": "latest", "revision": restored},
			"Invalid arguments: version must be a yyyymmddhhmmss timestamp from the history"},
		{"missing revision", wiki.financeToken, map[string]string{"path": "guides/intro", "version": "20261022111500"}, errRevisionRequired.message},
		{"missing version", wiki.financeToken, map[string]string{"path": "guides/intro", "version": "20990101000000", "revision": restored},
			"Version not found; include history in read_document to list them"},
		{"missing document", wiki.financeToken, map[string]string{"path": "guides/missing", "version": "20261022101500", "revision": restored}, errDocumentUnavailable.message},
		{"restricted document", carolToken, map[string]string{"path": "finance/report", "version": "20261022101500", "revision": wiki.revision(t, wiki.financeToken, "finance/report")},
			errDocumentUnavailable.message},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assertToolError(t, wiki.call(t, test.token, "restore_version", test.arguments), test.want)
		})
	}
	if wiki.content(t, "finance/report") != "# Report" {
		t.Fatalf("restricted content = %q, want it untouched", wiki.content(t, "finance/report"))
	}
}

func TestDeleteDocument(t *testing.T) {
	wiki := newTestWiki(t)
	root := wiki.cfg.Wiki.RootDir
	carolToken := wiki.addToken(t, config.User{Username: "carol", Role: config.RoleEditor}, "")
	wiki.writeDocument(t, "guides/obsolete", "# Obsolete")
	writeDocument(t, filepath.Join(root, "documents", "guides", "obsolete", "chart.png"), "png")
	writeDocument(t, filepath.Join(root, "versions", "documents", "guides", "obsolete", "20261022101500.md"), "# Obsolete v1")
	if err := comments.AddComment(filepath.Join(root, "comments"), "/guides/obsolete", "Outdated", "alice"); err != nil {
		t.Fatal(err)
	}
	wiki.writeDocument(t, "guides/index", "See [[obsolete]].")
	wiki.writeDocument(t, "finance/links", "[old](/guides/obsolete)")
	revision := wiki.revision(t, carolToken, "guides/obsolete")

	result := wiki.call(t, wiki.viewerToken, "delete_document", map[string]string{"path": "guides/obsolete", "revision": revision})
	assertToolError(t, result, "Insufficient role: editor access required")
	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "guides/obsolete"}), errRevisionRequired.message)
	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "guides/obsolete", "revision": "0000000000000000"}), mismatchMessage)
	wiki.assertExists(t, "documents/guides/obsolete/document.md")

	result = wiki.call(t, carolToken, "delete_document", map[string]string{"path": "guides/obsolete", "revision": revision})
	assertToolText(t, result, "Deleted guides/obsolete (0 sub-documents, 1 attachment). This cannot be undone.\n"+
		"documents linking to it (update them with edit_document):\n- guides/index\n")
	wiki.assertMissing(t, "documents/guides/obsolete", "versions/documents/guides/obsolete", "comments/guides/obsolete")
	// A retry finds nothing to delete
	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "guides/obsolete", "revision": revision}), errDocumentUnavailable.message)

	// Sub-documents need recursive
	wiki.writeDocument(t, "guides/parent", "# Parent")
	wiki.writeDocument(t, "guides/parent/a", "# A")
	wiki.writeDocument(t, "guides/parent/b", "# B")
	writeDocument(t, filepath.Join(root, "documents", "guides", "parent", "a", "data.csv"), "a,b")
	parentRevision := wiki.revision(t, carolToken, "guides/parent")
	result = wiki.call(t, carolToken, "delete_document", map[string]string{"path": "guides/parent", "revision": parentRevision})
	assertToolError(t, result, "The document has 2 sub-documents; check them with list_documents and pass recursive: true to delete them too")
	wiki.assertExists(t, "documents/guides/parent/a/document.md")
	result = wiki.call(t, carolToken, "delete_document", map[string]interface{}{"path": "guides/parent", "revision": parentRevision, "recursive": true})
	assertToolText(t, result, "Deleted guides/parent (2 sub-documents, 1 attachment). This cannot be undone.\ndocuments linking to it: none\n")
	wiki.assertMissing(t, "documents/guides/parent")

	// A section with documents carol cannot access stays, and the error does
	// not count them
	wiki.restrict("/guides/section/secret/**")
	wiki.writeDocument(t, "guides/section", "# Section")
	wiki.writeDocument(t, "guides/section/secret", "# Secret")
	sectionRevision := wiki.revision(t, carolToken, "guides/section")
	for _, recursive := range []bool{true, false} {
		result = wiki.call(t, carolToken, "delete_document", map[string]interface{}{"path": "guides/section", "revision": sectionRevision, "recursive": recursive})
		assertToolError(t, result, "Cannot delete: the section contains documents you cannot access")
	}
	wiki.assertExists(t, "documents/guides/section/document.md", "documents/guides/section/secret/document.md")

	// A folder without document.md has no revision
	wiki.writeDocument(t, "ops/runbook", "# Runbook")
	result = wiki.call(t, carolToken, "delete_document", map[string]string{"path": "ops"})
	assertToolError(t, result, "The document has 1 sub-document; check them with list_documents and pass recursive: true to delete them too")
	result = wiki.call(t, carolToken, "delete_document", map[string]interface{}{"path": "ops", "recursive": true})
	assertToolText(t, result, "Deleted ops (1 sub-document, 0 attachments). This cannot be undone.\ndocuments linking to it: none\n")

	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "/"}), "The home page cannot be deleted")
	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "finance/report", "revision": "x"}), errDocumentUnavailable.message)
	assertToolError(t, wiki.call(t, carolToken, "delete_document", map[string]string{"path": "../outside"}), "Invalid document path")
	wiki.assertExists(t, "pages/home/document.md", "documents/finance/report/document.md", "config.yaml")
}
