package handlers

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wiki-go/internal/config"
)

func writeStructureFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertStructurePaths(t *testing.T, root string, exist bool, names ...string) {
	t.Helper()
	for _, name := range names {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(name)))
		if exist && err != nil {
			t.Fatalf("stat %s = %v, want it to exist", name, err)
		}
		if !exist && !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want it missing", name, err)
		}
	}
}

func moveRequest(t *testing.T, testConfig *config.Config, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/document/move", strings.NewReader(body))
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	MoveDocumentHandler(response, request, testConfig)
	return response
}

func TestMoveAndDeleteRequireAccessToTheWholeSubtree(t *testing.T) {
	testConfig := installUserSessionTestConfig(t, nil)
	testConfig.Wiki.DocumentsDir = "documents"
	testConfig.AccessRules = []config.AccessRule{{
		Pattern: "/guides/secret/**",
		Access:  "restricted",
		Groups:  []string{"finance"},
	}}
	root := testConfig.Wiki.RootDir
	writeStructureFiles(t, root, map[string]string{
		"documents/guides/document.md":        "# Guides",
		"documents/guides/secret/document.md": "# Secret",
	})
	editorCookie := sessionCookieForUser(t, testConfig, "editor", config.RoleEditor)

	if response := moveRequest(t, testConfig, editorCookie, `{"sourcePath":"guides","targetPath":"archive"}`); response.Code != http.StatusForbidden {
		t.Fatalf("move status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest(http.MethodDelete, "/api/document/guides", nil)
	request.AddCookie(editorCookie)
	response := httptest.NewRecorder()
	DeleteDocumentHandler(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("delete status = %d, want 403; body: %s", response.Code, response.Body.String())
	}
	assertStructurePaths(t, root, true, "documents/guides/document.md", "documents/guides/secret/document.md")
	assertStructurePaths(t, root, false, "documents/archive")

	// Admins access every document
	adminCookie := sessionCookieForUser(t, testConfig, "admin", config.RoleAdmin)
	if response := moveRequest(t, testConfig, adminCookie, `{"sourcePath":"guides","targetPath":"guides/secret"}`); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "Cannot move a document inside itself") {
		t.Fatalf("move inside itself status = %d, want 400; body: %s", response.Code, response.Body.String())
	}
	if response := moveRequest(t, testConfig, adminCookie, `{"sourcePath":"guides","targetPath":"archive"}`); response.Code != http.StatusOK {
		t.Fatalf("admin move status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	assertStructurePaths(t, root, true, "documents/archive/guides/secret/document.md")
	request = httptest.NewRequest(http.MethodDelete, "/api/document/archive/guides", nil)
	request.AddCookie(adminCookie)
	response = httptest.NewRecorder()
	DeleteDocumentHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("admin delete status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	assertStructurePaths(t, root, false, "documents/archive/guides")
}

func TestMoveDocumentLeavesEverythingInPlaceWhenTheCommentsCannotMove(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not apply to root")
	}
	testConfig := installUserSessionTestConfig(t, nil)
	testConfig.Wiki.DocumentsDir = "documents"
	root := testConfig.Wiki.RootDir
	writeStructureFiles(t, root, map[string]string{
		"documents/alpha/document.md":                "# Alpha",
		"versions/documents/alpha/20261022101500.md": "# Alpha v1",
		"comments/alpha/20261022101500_editor.md":    "Note",
	})
	commentsRoot := filepath.Join(root, "comments")
	if err := os.Chmod(commentsRoot, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(commentsRoot, 0o755) })
	editorCookie := sessionCookieForUser(t, testConfig, "editor", config.RoleEditor)

	response := moveRequest(t, testConfig, editorCookie, `{"sourcePath":"alpha","targetPath":"archive","newSlug":"renamed"}`)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("move status = %d, want 500; body: %s", response.Code, response.Body.String())
	}
	assertStructurePaths(t, root, true,
		"documents/alpha/document.md", "versions/documents/alpha/20261022101500.md", "comments/alpha/20261022101500_editor.md")
	assertStructurePaths(t, root, false, "documents/archive", "versions/documents/archive", "comments/archive")
}
