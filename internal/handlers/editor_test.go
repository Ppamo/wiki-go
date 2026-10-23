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

func TestSaveHandlerSkipsIdenticalContent(t *testing.T) {
	testConfig := installUserSessionTestConfig(t, nil)
	testConfig.Wiki.DocumentsDir = "documents"
	testConfig.Wiki.MaxVersions = 10
	editorCookie := sessionCookieForUser(t, testConfig, "editor", config.RoleEditor)
	documentFile := filepath.Join(testConfig.Wiki.RootDir, "documents", "alpha", "document.md")
	if err := os.MkdirAll(filepath.Dir(documentFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(documentFile, []byte("# Alpha"), 0o644); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		request := httptest.NewRequest(http.MethodPost, "/api/save/alpha", strings.NewReader("# Alpha"))
		request.AddCookie(editorCookie)
		response := httptest.NewRecorder()
		SaveHandler(response, request)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Document saved successfully") {
			t.Fatalf("save status = %d, want 200; body: %s", response.Code, response.Body.String())
		}
	}

	if _, err := os.Stat(filepath.Join(testConfig.Wiki.RootDir, "versions", "documents", "alpha")); !os.IsNotExist(err) {
		t.Fatalf("versions stat = %v, want no version for identical content", err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(documentFile), ".document-*.tmp"))
	if err != nil || len(matches) > 0 {
		t.Fatalf("temporary files = %v (%v), want none", matches, err)
	}
}

func TestSaveHandlerCreatesTheHomePage(t *testing.T) {
	testConfig := installUserSessionTestConfig(t, nil)
	testConfig.Wiki.DocumentsDir = "documents"
	editorCookie := sessionCookieForUser(t, testConfig, "editor", config.RoleEditor)

	request := httptest.NewRequest(http.MethodPost, "/api/save/", strings.NewReader("# Home"))
	request.AddCookie(editorCookie)
	response := httptest.NewRecorder()
	SaveHandler(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("save status = %d, want 200; body: %s", response.Code, response.Body.String())
	}
	content, err := os.ReadFile(filepath.Join(testConfig.Wiki.RootDir, "pages", "home", "document.md"))
	if err != nil || string(content) != "# Home" {
		t.Fatalf("home page = %q (%v), want the saved content", content, err)
	}
}
