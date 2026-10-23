package documents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"wiki-go/internal/config"
)

func newTestConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{}
	cfg.Wiki.RootDir = t.TempDir()
	cfg.Wiki.DocumentsDir = "documents"
	cfg.Wiki.MaxVersions = 10
	return cfg
}

func resolve(t *testing.T, cfg *config.Config, path string) Document {
	t.Helper()
	doc, err := Resolve(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func save(t *testing.T, cfg *config.Config, doc Document, content string, opts SaveOptions) SaveResult {
	t.Helper()
	result, err := Save(cfg, doc, []byte(content), opts)
	if err != nil {
		t.Fatalf("Save(%q) error = %v", content, err)
	}
	return result
}

// stopClock makes every save happen in the same second.
func stopClock(t *testing.T) string {
	t.Helper()
	previous := now
	at := time.Date(2026, 10, 22, 20, 30, 0, 0, time.Local)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = previous })
	return at.Format("20060102150405")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func versionNames(t *testing.T, doc Document) []string {
	t.Helper()
	entries, err := os.ReadDir(doc.VersionsDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func assertNoTemporaryFiles(t *testing.T, doc Document) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(doc.Dir, ".document-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) > 0 {
		t.Fatalf("temporary files left: %v", matches)
	}
}

func TestResolve(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Wiki.DocumentsDir = "docs"
	root := cfg.Wiki.RootDir

	for _, path := range []string{"", "/"} {
		home := resolve(t, cfg, path)
		if home.LogicalPath != "/" || home.File() != filepath.Join(root, "pages", "home", "document.md") ||
			home.VersionsDir != filepath.Join(root, "versions", "pages", "home") {
			t.Fatalf("Resolve(%q) = %+v, want the home page", path, home)
		}
	}

	doc := resolve(t, cfg, "/guides/intro/")
	if doc.LogicalPath != "/guides/intro" || doc.Dir != filepath.Join(root, "docs", "guides", "intro") ||
		doc.VersionsDir != filepath.Join(root, "versions", "documents", "guides", "intro") {
		t.Fatalf("Resolve = %+v, want the contained document with versions below versions/documents", doc)
	}

	for _, path := range []string{"../config.yaml", "guides/../../config.yaml", "%2e%2e/config.yaml", "guides\x00intro"} {
		if doc, err := Resolve(cfg, path); err == nil {
			t.Errorf("Resolve(%q) = %+v, want an error", path, doc)
		}
	}
}

func TestSaveCreatesOverwritesAndKeepsTheVersion(t *testing.T) {
	cfg := newTestConfig(t)
	timestamp := stopClock(t)
	doc := resolve(t, cfg, "guides/intro")

	created := save(t, cfg, doc, "# Intro\n", SaveOptions{})
	if !created.Created || created.Unchanged || created.Version != "" || created.Revision != Revision([]byte("# Intro\n")) {
		t.Fatalf("create result = %+v", created)
	}
	if readFile(t, doc.File()) != "# Intro\n" || versionNames(t, doc) != nil {
		t.Fatalf("after create: content %q, versions %v", readFile(t, doc.File()), versionNames(t, doc))
	}

	saved := save(t, cfg, doc, "# Intro\n\nUpdated.\n", SaveOptions{ExpectedRevision: created.Revision})
	if saved.Created || saved.Version != timestamp || saved.Revision != Revision([]byte("# Intro\n\nUpdated.\n")) {
		t.Fatalf("save result = %+v, want version %s", saved, timestamp)
	}
	if readFile(t, doc.File()) != "# Intro\n\nUpdated.\n" {
		t.Fatalf("content = %q", readFile(t, doc.File()))
	}
	if version := readFile(t, filepath.Join(doc.VersionsDir, timestamp+".md")); version != "# Intro\n" {
		t.Fatalf("version = %q, want the previous content", version)
	}
	if info, err := os.Stat(doc.File()); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("document mode = %v, %v; want 0644", info.Mode().Perm(), err)
	}
	assertNoTemporaryFiles(t, doc)
}

func TestSaveRejectsAStaleRevision(t *testing.T) {
	cfg := newTestConfig(t)
	doc := resolve(t, cfg, "guides/intro")
	save(t, cfg, doc, "# Intro", SaveOptions{})

	_, err := Save(cfg, doc, []byte("# Overwritten"), SaveOptions{ExpectedRevision: Revision([]byte("# Older"))})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("error = %v, want ErrRevisionMismatch", err)
	}
	if readFile(t, doc.File()) != "# Intro" || versionNames(t, doc) != nil {
		t.Fatalf("after mismatch: content %q, versions %v", readFile(t, doc.File()), versionNames(t, doc))
	}

	missing := resolve(t, cfg, "guides/missing")
	if _, err := Save(cfg, missing, []byte("# Missing"), SaveOptions{ExpectedRevision: Revision([]byte("# Missing"))}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing document error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(missing.Dir); !os.IsNotExist(err) {
		t.Fatalf("missing document directory stat = %v, want it not created", err)
	}
}

func TestSaveLetsOneConcurrentWriterWin(t *testing.T) {
	cfg := newTestConfig(t)
	doc := resolve(t, cfg, "guides/intro")
	revision := save(t, cfg, doc, "# Intro", SaveOptions{}).Revision

	const writers = 8
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Save(cfg, doc, []byte(fmt.Sprintf("# Writer %d", i)), SaveOptions{ExpectedRevision: revision})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)

	saved := 0
	for err := range errs {
		switch {
		case err == nil:
			saved++
		case !errors.Is(err, ErrRevisionMismatch):
			t.Fatalf("error = %v, want ErrRevisionMismatch", err)
		}
	}
	if saved != 1 {
		t.Fatalf("%d writers saved with the same revision, want 1", saved)
	}
}

func TestSaveCreateOnly(t *testing.T) {
	cfg := newTestConfig(t)
	doc := resolve(t, cfg, "guides/intro")
	save(t, cfg, doc, "# Intro", SaveOptions{CreateOnly: true})

	for _, content := range []string{"# Intro", "# Other"} {
		if _, err := Save(cfg, doc, []byte(content), SaveOptions{CreateOnly: true}); !errors.Is(err, ErrExists) {
			t.Fatalf("CreateOnly(%q) error = %v, want ErrExists", content, err)
		}
	}
	if readFile(t, doc.File()) != "# Intro" {
		t.Fatalf("content = %q, want it untouched", readFile(t, doc.File()))
	}
}

func TestSaveSkipsIdenticalContent(t *testing.T) {
	cfg := newTestConfig(t)
	doc := resolve(t, cfg, "/")
	save(t, cfg, doc, "# Home", SaveOptions{})

	result := save(t, cfg, doc, "# Home", SaveOptions{})
	if !result.Unchanged || result.Created || result.Version != "" || result.Revision != Revision([]byte("# Home")) {
		t.Fatalf("result = %+v, want unchanged", result)
	}
	if versionNames(t, doc) != nil {
		t.Fatalf("versions = %v, want none", versionNames(t, doc))
	}
}

func TestSavePreservesExistingDocumentPermissions(t *testing.T) {
	cfg := newTestConfig(t)
	doc := resolve(t, cfg, "guides/private")
	created := save(t, cfg, doc, "# Private", SaveOptions{})
	if err := os.Chmod(doc.File(), 0600); err != nil {
		t.Fatal(err)
	}

	save(t, cfg, doc, "# Updated", SaveOptions{ExpectedRevision: created.Revision})
	info, err := os.Stat(doc.File())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("document mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestSaveKeepsTheFirstVersionOfASecond(t *testing.T) {
	cfg := newTestConfig(t)
	timestamp := stopClock(t)
	doc := resolve(t, cfg, "guides/intro")
	save(t, cfg, doc, "# v1", SaveOptions{})

	if first := save(t, cfg, doc, "# v2", SaveOptions{}); first.Version != timestamp {
		t.Fatalf("first save version = %q, want %q", first.Version, timestamp)
	}
	if second := save(t, cfg, doc, "# v3", SaveOptions{}); second.Version != "" {
		t.Fatalf("second save version = %q, want none", second.Version)
	}
	if names := versionNames(t, doc); len(names) != 1 || readFile(t, filepath.Join(doc.VersionsDir, timestamp+".md")) != "# v1" {
		t.Fatalf("versions = %v, want only the content before the burst", names)
	}
	if readFile(t, doc.File()) != "# v3" {
		t.Fatalf("content = %q, want the last save", readFile(t, doc.File()))
	}
}

func TestSaveWithoutVersions(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Wiki.MaxVersions = 0
	doc := resolve(t, cfg, "guides/intro")
	save(t, cfg, doc, "# v1", SaveOptions{})

	if result := save(t, cfg, doc, "# v2", SaveOptions{}); result.Version != "" {
		t.Fatalf("version = %q, want none", result.Version)
	}
	if _, err := os.Stat(doc.VersionsDir); !os.IsNotExist(err) {
		t.Fatalf("versions directory stat = %v, want it not created", err)
	}
}

func TestSaveRemovesTheTemporaryFileOnFailure(t *testing.T) {
	cfg := newTestConfig(t)
	cfg.Wiki.MaxVersions = 1
	timestamp := stopClock(t)
	doc := resolve(t, cfg, "guides/intro")
	save(t, cfg, doc, "# Intro", SaveOptions{})
	oldVersion := filepath.Join(doc.VersionsDir, "20200101000000.md")
	if err := os.MkdirAll(doc.VersionsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(oldVersion, []byte("# Older"), 0644); err != nil {
		t.Fatal(err)
	}

	previous := rename
	rename = func(string, string) error { return errors.New("disk full") }
	t.Cleanup(func() { rename = previous })

	if _, err := Save(cfg, doc, []byte("# Lost"), SaveOptions{}); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("error = %v, want the rename failure", err)
	}
	if readFile(t, doc.File()) != "# Intro" {
		t.Fatalf("content = %q, want it untouched", readFile(t, doc.File()))
	}
	if readFile(t, oldVersion) != "# Older" {
		t.Fatalf("old version was changed after the failed save")
	}
	if _, err := os.Stat(filepath.Join(doc.VersionsDir, timestamp+".md")); !os.IsNotExist(err) {
		t.Fatalf("failed save version stat = %v, want it rolled back", err)
	}
	assertNoTemporaryFiles(t, doc)
}

func TestRevisionMatchesTheStageOneValues(t *testing.T) {
	// Values of read_document's revision before the write path existed
	for content, want := range map[string]string{
		"# Intro": "4396674f8992f2c5",
		"# Home":  "1d30f572201fed5f",
		"":        "e3b0c44298fc1c14",
	} {
		if got := Revision([]byte(content)); got != want {
			t.Errorf("Revision(%q) = %s, want %s", content, got, want)
		}
	}
}
