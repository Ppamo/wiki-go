package documents

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
	"wiki-go/internal/config"
	"wiki-go/internal/goldext"
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

// writeTree creates the files, given as slash paths below root.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertExists(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); err != nil {
			t.Fatalf("stat %s = %v, want it to exist", name, err)
		}
	}
}

func assertMissing(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(name))); !os.IsNotExist(err) {
			t.Fatalf("stat %s = %v, want it missing", name, err)
		}
	}
}

func allowMove([]Moved) error    { return nil }
func allowDelete([]string) error { return nil }

// oldGuide is a document with an attachment, history and comments.
var oldGuide = map[string]string{
	"documents/guides/old/document.md":                "# Old",
	"documents/guides/old/chart.png":                  "png",
	"versions/documents/guides/old/20261022101500.md": "# Old v1",
	"comments/guides/old/20261022101500_alice.md":     "Nice",
}

func TestMoveCarriesTheSubtreeHistoryAndComments(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, oldGuide)
	writeTree(t, root, map[string]string{
		"documents/guides/old/.drafts/notes.txt":                "draft",
		"documents/guides/old/child/document.md":                "# Child",
		"versions/documents/guides/old/child/20261022101500.md": "# Child v1",
		"comments/guides/old/child/20261022101500_bob.md":       "Ok",
	})

	var moved []Moved
	err := Move(cfg, resolve(t, cfg, "guides/old"), resolve(t, cfg, "archive/new"), func(subtree []Moved) error {
		moved = subtree
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []Moved{
		{From: "/guides/old", To: "/archive/new", IsDocument: true},
		{From: "/guides/old/.drafts", To: "/archive/new/.drafts"},
		{From: "/guides/old/child", To: "/archive/new/child", IsDocument: true},
	}
	if !reflect.DeepEqual(moved, want) {
		t.Fatalf("check got %v, want every directory of the subtree %v", moved, want)
	}
	assertExists(t, root,
		"documents/archive/new/document.md",
		"documents/archive/new/chart.png",
		"documents/archive/new/.drafts/notes.txt",
		"documents/archive/new/child/document.md",
		"versions/documents/archive/new/20261022101500.md",
		"versions/documents/archive/new/child/20261022101500.md",
		"comments/archive/new/20261022101500_alice.md",
		"comments/archive/new/child/20261022101500_bob.md",
	)
	assertMissing(t, root, "documents/guides/old", "versions/documents/guides/old", "comments/guides/old")
}

func TestMoveCheckCanVetoTheMove(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, oldGuide)

	veto := errors.New("veto")
	err := Move(cfg, resolve(t, cfg, "guides/old"), resolve(t, cfg, "archive/deep/new"), func([]Moved) error { return veto })
	if !errors.Is(err, veto) {
		t.Fatalf("error = %v, want the veto", err)
	}
	assertExists(t, root, "documents/guides/old/document.md", "versions/documents/guides/old/20261022101500.md", "comments/guides/old/20261022101500_alice.md")
	assertMissing(t, root, "documents/archive", "versions/documents/archive", "comments/archive")
}

func TestMoveRollsBackWhenTheCommentsCannotMove(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, oldGuide)
	commentsRoot := filepath.Join(root, "comments")
	previous := rename
	rename = func(from, to string) error {
		if strings.HasPrefix(from, commentsRoot) {
			return errors.New("permission denied")
		}
		return previous(from, to)
	}
	t.Cleanup(func() { rename = previous })

	err := Move(cfg, resolve(t, cfg, "guides/old"), resolve(t, cfg, "archive/new"), allowMove)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("error = %v, want the comments rename failure", err)
	}
	assertExists(t, root, "documents/guides/old/document.md", "documents/guides/old/chart.png",
		"versions/documents/guides/old/20261022101500.md", "comments/guides/old/20261022101500_alice.md")
	assertMissing(t, root, "documents/archive", "versions/documents/archive", "comments/archive")
}

func TestMoveRejectsTargetsItCannotUse(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, map[string]string{
		"documents/guides/old/document.md":   "# Old",
		"documents/guides/taken/document.md": "# Taken",
	})
	if err := os.MkdirAll(filepath.Join(root, "documents", "guides", "empty"), 0755); err != nil {
		t.Fatal(err)
	}
	old := resolve(t, cfg, "guides/old")

	tests := []struct {
		src, dst string
		want     error
	}{
		{"guides/old", "guides/old/sub", ErrInsideItself},
		{"guides/old", "guides/taken", ErrTargetExists},
		{"guides/missing", "guides/new", ErrNotFound},
	}
	for _, test := range tests {
		if err := Move(cfg, resolve(t, cfg, test.src), resolve(t, cfg, test.dst), allowMove); !errors.Is(err, test.want) {
			t.Fatalf("Move(%s, %s) error = %v, want %v", test.src, test.dst, err, test.want)
		}
	}
	assertExists(t, root, "documents/guides/old/document.md", "documents/guides/taken/document.md")

	// An empty directory is replaced, as in the API
	if err := Move(cfg, old, resolve(t, cfg, "guides/empty"), allowMove); err != nil {
		t.Fatalf("move onto an empty directory error = %v", err)
	}
	assertExists(t, root, "documents/guides/empty/document.md")

	// Also where names are case-insensitive, where guides/Empty exists already
	if err := Move(cfg, resolve(t, cfg, "guides/empty"), resolve(t, cfg, "guides/Empty"), allowMove); err != nil {
		t.Fatalf("case-only rename error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "documents", "guides"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if strings.Join(names, ",") != "Empty,taken" {
		t.Fatalf("guides = %v, want the renamed Empty", names)
	}
}

func TestDeleteRequiresRecursiveForSubDocuments(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, oldGuide)
	writeTree(t, root, map[string]string{
		"documents/guides/old/child/document.md":                "# Child",
		"documents/guides/old/child/data.csv":                   "a,b",
		"versions/documents/guides/old/child/20261022101500.md": "# Child v1",
	})
	doc := resolve(t, cfg, "guides/old")

	_, err := Delete(cfg, doc, DeleteOptions{}, allowDelete)
	var children *ChildrenError
	if !errors.As(err, &children) || children.Count != 1 {
		t.Fatalf("error = %v, want ChildrenError{1}", err)
	}
	assertExists(t, root, "documents/guides/old/child/document.md")

	var checked []string
	result, err := Delete(cfg, doc, DeleteOptions{ExpectedRevision: Revision([]byte("# Old")), Recursive: true}, func(dirs []string) error {
		checked = dirs
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/guides/old", "/guides/old/child"}
	if !reflect.DeepEqual(checked, want) || !reflect.DeepEqual(result.Documents, want) {
		t.Fatalf("checked %v, deleted %v; want %v", checked, result.Documents, want)
	}
	if result.Attachments != 2 || result.Warnings != nil {
		t.Fatalf("result = %+v, want 2 attachments and no warnings", result)
	}
	assertMissing(t, root, "documents/guides/old", "versions/documents/guides/old", "comments/guides/old")
	assertExists(t, root, "documents/guides")
}

func TestDeleteKeepsTheDocumentOnAStaleRevisionOrAVeto(t *testing.T) {
	cfg := newTestConfig(t)
	root := cfg.Wiki.RootDir
	writeTree(t, root, oldGuide)
	doc := resolve(t, cfg, "guides/old")

	if _, err := Delete(cfg, doc, DeleteOptions{ExpectedRevision: Revision([]byte("# Older"))}, allowDelete); !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("stale revision error = %v, want ErrRevisionMismatch", err)
	}
	veto := errors.New("veto")
	if _, err := Delete(cfg, doc, DeleteOptions{}, func([]string) error { return veto }); !errors.Is(err, veto) {
		t.Fatalf("veto error = %v, want the veto", err)
	}
	if _, err := Delete(cfg, resolve(t, cfg, "guides/missing"), DeleteOptions{}, allowDelete); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing document error = %v, want ErrNotFound", err)
	}
	assertExists(t, root, "documents/guides/old/document.md", "versions/documents/guides/old/20261022101500.md", "comments/guides/old/20261022101500_alice.md")
}

func TestReferences(t *testing.T) {
	cfg := newTestConfig(t)
	writeTree(t, cfg.Wiki.RootDir, map[string]string{
		"pages/home/document.md":                  "# Home\n\n[Old](/guides/old)",
		"documents/guides/index/document.md":      "See [setup](/guides/old#setup).",
		"documents/guides/old/document.md":        "# Old",
		"documents/guides/old/child/document.md":  "[parent](/guides/old)",
		"documents/ops/attachment/document.md":    "![chart](/api/files/guides/old/chart.png)",
		"documents/ops/by-name/document.md":       "Read [[old]] first.",
		"documents/ops/child-link/document.md":    "[child](/guides/old/child \"Child\")",
		"documents/ops/code/document.md":          "```\n[x](/guides/old)\n```\n\nUse `[[guides/old]]`.",
		"documents/ops/prefix/document.md":        "[x](/guides/older) and [[guides/older]] and [[older]]",
		"documents/ops/wikilink/document.md":      "[[guides/old|Old guide]]",
		"documents/.drafts/hidden/document.md":    "[x](/guides/old)",
		"documents/ops/no-document/attachment.md": "[x](/guides/old)",
	})

	references, err := References(cfg, "/guides/old", "/guides/old")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/", "/guides/index", "/ops/attachment", "/ops/by-name", "/ops/child-link", "/ops/wikilink"}
	if !reflect.DeepEqual(references, want) {
		t.Fatalf("references after a delete = %v, want %v", references, want)
	}

	// A move that keeps the name leaves [[old]] working
	if err := Move(cfg, resolve(t, cfg, "guides/old"), resolve(t, cfg, "archive/old"), allowMove); err != nil {
		t.Fatal(err)
	}
	references, err = References(cfg, "/guides/old", "/archive/old")
	if err != nil {
		t.Fatal(err)
	}
	want = []string{"/", "/guides/index", "/ops/attachment", "/ops/child-link", "/ops/wikilink"}
	if !reflect.DeepEqual(references, want) {
		t.Fatalf("references after a move = %v, want %v without the moved subtree", references, want)
	}

	// A rename breaks it
	if err := Move(cfg, resolve(t, cfg, "archive/old"), resolve(t, cfg, "archive/renamed"), allowMove); err != nil {
		t.Fatal(err)
	}
	references, err = References(cfg, "/archive/old", "/archive/renamed")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(references, []string{"/ops/by-name"}) {
		t.Fatalf("references after a rename = %v, want the link by name", references)
	}
}

func TestStructureChangesInvalidateTheWikilinkIndex(t *testing.T) {
	cfg := newTestConfig(t)
	goldext.ConfigureContentPaths(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir)
	t.Cleanup(func() { goldext.ConfigureContentPaths("data", "documents") })
	documentsRoot := filepath.Join(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir)
	save(t, cfg, resolve(t, cfg, "guides/intro"), "# Intro", SaveOptions{})

	// Every change happens below guides/, which leaves the mtime of
	// documents/ as it was unless the change touches it
	steps := []struct {
		name   string
		change func() error
		link   string
		want   string
	}{
		{"save that creates", func() error {
			_, err := Save(cfg, resolve(t, cfg, "guides/sub/setup"), []byte("# Setup"), SaveOptions{})
			return err
		}, "[[setup]]", "[setup](/guides/sub/setup)"},
		{"move", func() error {
			return Move(cfg, resolve(t, cfg, "guides/intro"), resolve(t, cfg, "guides/sub/intro"), allowMove)
		}, "[[intro]]", "[intro](/guides/sub/intro)"},
		{"delete", func() error {
			_, err := Delete(cfg, resolve(t, cfg, "guides/sub/intro"), DeleteOptions{}, allowDelete)
			return err
		}, "[[intro]]", "[intro](/intro)"},
	}
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, step := range steps {
		if err := os.Chtimes(documentsRoot, past, past); err != nil {
			t.Fatal(err)
		}
		// Builds the index for the current mtime
		before := goldext.WikiLinkPreprocessor(step.link, "/")
		if err := step.change(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		info, err := os.Stat(documentsRoot)
		if err != nil {
			t.Fatal(err)
		}
		if info.ModTime().Equal(past) {
			t.Fatalf("%s: the mtime of documents/ did not change", step.name)
		}
		if got := goldext.WikiLinkPreprocessor(step.link, "/"); got != step.want {
			t.Fatalf("%s: %s renders %q (before: %q), want %q", step.name, step.link, got, before, step.want)
		}
	}
}
