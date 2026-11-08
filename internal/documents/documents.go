// Package documents resolves wiki documents inside the wiki root and holds
// the write path shared by the HTTP API and the MCP tools: versions, atomic
// writes, moves and deletes, and optimistic concurrency checks under a single
// lock.
package documents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"wiki-go/internal/config"
	"wiki-go/internal/logger"
	"wiki-go/internal/utils"
)

// Document is a wiki document resolved inside the wiki root.
type Document struct {
	LogicalPath string // "/" or "/guides/intro", used by the access rules
	Dir         string // holds document.md and the attachments
	VersionsDir string
}

// File is the path of the document Markdown source.
func (d Document) File() string {
	return filepath.Join(d.Dir, "document.md")
}

// Resolve maps a path argument to a contained document ("" or "/" is the home page).
// Leading and trailing slashes are ignored.
func Resolve(cfg *config.Config, path string) (Document, error) {
	requestPath := strings.Trim(path, "/")
	if requestPath == "" {
		return Document{
			LogicalPath: "/",
			Dir:         filepath.Join(cfg.Wiki.RootDir, "pages", "home"),
			VersionsDir: filepath.Join(cfg.Wiki.RootDir, "versions", "pages", "home"),
		}, nil
	}

	resolved, err := utils.ResolveRelativeDocumentPath(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir, requestPath)
	if err != nil {
		return Document{}, err
	}
	// Versions keep the documents/ prefix whatever the documents directory is
	versions, err := utils.ResolveRelativeDocumentPath(filepath.Join(cfg.Wiki.RootDir, "versions"), "documents", requestPath)
	if err != nil {
		return Document{}, err
	}
	return Document{
		LogicalPath: resolved.LogicalPath,
		Dir:         resolved.FilesystemPath,
		VersionsDir: versions.FilesystemPath,
	}, nil
}

// Revision identifies a content; read_document returns it and writes verify it.
func Revision(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:8])
}

type SaveOptions struct {
	ExpectedRevision string // when set, must match the current content
	CreateOnly       bool   // fail with ErrExists when the document exists
}

type SaveResult struct {
	Revision  string // revision of the content now on disk
	Created   bool
	Unchanged bool   // the content was identical; nothing was written
	Version   string // timestamp of the stored previous content, "" if none
}

var (
	ErrExists           = errors.New("document exists")
	ErrNotFound         = errors.New("document not found")
	ErrRevisionMismatch = errors.New("revision mismatch")
	ErrTargetExists     = errors.New("target exists")
	ErrInsideItself     = errors.New("target is inside the source")
	errHomePage         = errors.New("the home page cannot be moved or deleted")
)

// ChildrenError reports sub-documents when Recursive is false.
type ChildrenError struct{ Count int }

func (e *ChildrenError) Error() string {
	return fmt.Sprintf("document has %d sub-documents", e.Count)
}

var (
	// writeMu serializes the writes: they are rare and the server is a
	// single process, so one lock for the whole wiki is enough
	writeMu sync.Mutex
	// now and rename are replaced by the tests
	now    = time.Now
	rename = os.Rename
)

// Save writes content to the document. The previous content is stored as a
// version first when the wiki keeps versions, and the checks in opts run
// under the same lock as the write.
func Save(cfg *config.Config, doc Document, content []byte, opts SaveOptions) (SaveResult, error) {
	writeMu.Lock()
	defer writeMu.Unlock()

	current, err := os.ReadFile(doc.File())
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return SaveResult{}, err
	}

	if opts.CreateOnly && exists {
		return SaveResult{}, ErrExists
	}
	if exists && bytes.Equal(current, content) {
		return SaveResult{Revision: Revision(content), Unchanged: true}, nil
	}
	if opts.ExpectedRevision != "" {
		if !exists {
			return SaveResult{}, ErrNotFound
		}
		if Revision(current) != opts.ExpectedRevision {
			return SaveResult{}, ErrRevisionMismatch
		}
	}

	result := SaveResult{Revision: Revision(content), Created: !exists}
	if exists && cfg.Wiki.MaxVersions > 0 && len(current) > 0 {
		version, err := storeVersion(doc.VersionsDir, current)
		if err != nil {
			return SaveResult{}, err
		}
		result.Version = version
	}

	if err := os.MkdirAll(doc.Dir, 0755); err != nil {
		removeStoredVersion(doc, result.Version)
		return SaveResult{}, err
	}
	if err := writeFileAtomic(doc.File(), content); err != nil {
		removeStoredVersion(doc, result.Version)
		return SaveResult{}, err
	}
	if exists && cfg.Wiki.MaxVersions > 0 && len(current) > 0 {
		utils.CleanupOldVersions(doc.VersionsDir, cfg.Wiki.MaxVersions)
	}
	if result.Created {
		touchDocumentsRoot(cfg)
	}
	return result, nil
}

// removeStoredVersion rolls back a version created for a write that did not
// complete. An empty version means a file for that second already existed and
// must not be removed.
func removeStoredVersion(doc Document, version string) {
	if version != "" {
		_ = os.Remove(filepath.Join(doc.VersionsDir, version+".md"))
	}
}

// storeVersion keeps content as <yyyymmddhhmmss>.md. Versions are named by
// second, so an existing version of the same second is kept: it holds the
// content from before a burst of saves. It then returns "".
func storeVersion(versionsDir string, content []byte) (string, error) {
	if err := os.MkdirAll(versionsDir, 0755); err != nil {
		return "", err
	}
	timestamp := now().Format("20060102150405")
	versionPath := filepath.Join(versionsDir, timestamp+".md")
	file, err := os.OpenFile(versionPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if os.IsExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if _, err := file.Write(content); err != nil {
		file.Close()
		os.Remove(versionPath)
		return "", fmt.Errorf("write version: %w", err)
	}
	if err := file.Close(); err != nil {
		os.Remove(versionPath)
		return "", fmt.Errorf("write version: %w", err)
	}
	logger.Info("Created version: %s", versionPath)
	return timestamp, nil
}

// writeFileAtomic writes content to a hidden temporary file in the same
// directory and renames it over path, so readers never see a partial file.
func writeFileAtomic(path string, content []byte) error {
	mode := os.FileMode(0644)
	info, err := os.Stat(path)
	if err == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(err) {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".document-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return err
	}
	// CreateTemp uses 0600; new documents use 0644 and replacements keep the
	// permissions of the existing document.
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := rename(tmpName, path); err != nil {
		return err
	}
	tmpName = ""
	return nil
}

// Moved pairs a directory of a moved subtree with its new location.
type Moved struct {
	From, To   string // logical paths, e.g. "/guides/intro" → "/archive/intro"
	IsDocument bool   // the directory contains a document.md
}

// Move renames src to dst together with its versions and comments. It holds
// the write lock; check runs under it with every directory of the subtree
// (src first) and can veto the move by returning an error.
func Move(cfg *config.Config, src, dst Document, check func([]Moved) error) error {
	if src.LogicalPath == "/" || dst.LogicalPath == "/" {
		return errHomePage
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	if !isDir(src.Dir) {
		return ErrNotFound
	}
	if strings.HasPrefix(dst.LogicalPath, src.LogicalPath+"/") {
		return ErrInsideItself
	}
	// The document directory always moves; its versions and comments only
	// when they exist
	type step struct {
		from, to string
		// os.Rename does not replace a directory, so an empty one at to is
		// removed first
		replace bool
	}
	steps := []step{{from: src.Dir, to: dst.Dir}}
	for _, pair := range [][2]string{{src.VersionsDir, dst.VersionsDir}, {commentsDir(cfg, src), commentsDir(cfg, dst)}} {
		if isDir(pair[0]) {
			steps = append(steps, step{from: pair[0], to: pair[1]})
		}
	}
	for i := range steps {
		replace, err := vacant(steps[i].from, steps[i].to)
		if err != nil {
			return err
		}
		steps[i].replace = replace
	}

	tree, err := scanSubtree(src.Dir)
	if err != nil {
		return err
	}
	moved := make([]Moved, len(tree.dirs))
	documents := make(map[string]bool, len(tree.documents))
	for _, rel := range tree.documents {
		documents[rel] = true
	}
	for i, rel := range tree.dirs {
		moved[i] = Moved{
			From:       joinLogical(src.LogicalPath, rel),
			To:         joinLogical(dst.LogicalPath, rel),
			IsDocument: documents[rel],
		}
	}
	if err := check(moved); err != nil {
		return err
	}

	// A failed step undoes the ones already done, in reverse order, and
	// removes the parents created for them
	var done []step
	var created []string
	for _, s := range steps {
		parents, err := mkdirParents(s.to)
		created = append(created, parents...)
		if err == nil && s.replace {
			err = os.Remove(s.to)
		}
		if err == nil {
			err = rename(s.from, s.to)
		}
		if err != nil {
			if s.replace {
				os.Mkdir(s.to, 0755)
			}
			for i := len(done) - 1; i >= 0; i-- {
				if undoErr := rename(done[i].to, done[i].from); undoErr != nil {
					logger.Error("Could not undo the move of %s to %s: %v", done[i].from, done[i].to, undoErr)
				} else if done[i].replace {
					os.Mkdir(done[i].to, 0755)
				}
			}
			removeEmptyDirs(created)
			return err
		}
		done = append(done, s)
	}
	touchDocumentsRoot(cfg)
	return nil
}

type DeleteOptions struct {
	ExpectedRevision string // when set, must match doc's document.md
	Recursive        bool   // allow deleting sub-documents
}

type DeleteResult struct {
	Documents   []string // logical paths of the deleted documents, doc first
	Attachments int      // files other than document.md that were removed
	Warnings    []string // leftovers that could not be removed
}

// Delete removes doc with its subtree, versions and comments. check runs under
// the write lock with every directory of the subtree and can veto it.
func Delete(cfg *config.Config, doc Document, opts DeleteOptions, check func([]string) error) (DeleteResult, error) {
	if doc.LogicalPath == "/" {
		return DeleteResult{}, errHomePage
	}
	writeMu.Lock()
	defer writeMu.Unlock()

	if !isDir(doc.Dir) {
		return DeleteResult{}, ErrNotFound
	}
	if opts.ExpectedRevision != "" {
		current, err := os.ReadFile(doc.File())
		if err != nil && !os.IsNotExist(err) {
			return DeleteResult{}, err
		}
		if err != nil || Revision(current) != opts.ExpectedRevision {
			return DeleteResult{}, ErrRevisionMismatch
		}
	}

	tree, err := scanSubtree(doc.Dir)
	if err != nil {
		return DeleteResult{}, err
	}
	dirs := make([]string, len(tree.dirs))
	for i, rel := range tree.dirs {
		dirs[i] = joinLogical(doc.LogicalPath, rel)
	}
	// check runs before the sub-documents are counted, so that the count
	// never includes documents the caller cannot access
	if err := check(dirs); err != nil {
		return DeleteResult{}, err
	}
	result := DeleteResult{Attachments: tree.attachments}
	for _, rel := range tree.documents {
		result.Documents = append(result.Documents, joinLogical(doc.LogicalPath, rel))
	}
	children := len(tree.documents)
	if children > 0 && tree.documents[0] == "" {
		children--
	}
	if children > 0 && !opts.Recursive {
		return DeleteResult{}, &ChildrenError{Count: children}
	}

	defer touchDocumentsRoot(cfg)
	if err := os.RemoveAll(doc.Dir); err != nil {
		return DeleteResult{}, err
	}
	// The document is gone, so leftovers of its history are reported
	// instead of failing the delete
	for _, leftover := range []struct{ name, dir string }{
		{"versions", doc.VersionsDir},
		{"comments", commentsDir(cfg, doc)},
	} {
		if err := os.RemoveAll(leftover.dir); err != nil {
			logger.Warn("Failed to delete the %s directory %s: %v", leftover.name, leftover.dir, err)
			result.Warnings = append(result.Warnings, "the "+leftover.name+" directory could not be removed")
		}
	}
	return result, nil
}

// codeRe matches fenced code blocks and inline code, where links are text.
var codeRe = regexp.MustCompile("(?s)```.*?```|~~~.*?~~~|`[^`]*`")

// References returns the documents, other than those under exclude, whose
// Markdown links to logicalPath: absolute Markdown links, wikilinks with the
// path, attachment URLs and wikilinks by name. exclude is the new path after
// a move, or logicalPath itself after a delete. Relative links are not
// detected.
func References(cfg *config.Config, logicalPath, exclude string) ([]string, error) {
	target := strings.Trim(logicalPath, "/")
	if target == "" {
		return nil, nil
	}
	pattern := `(?:\]\(/|\[\[\s*/?|/api/files/)` + regexp.QuoteMeta(target) + `(?:[)/#|\]\s"']|$)`
	// A wikilink by name still resolves after a move that keeps the name
	if name := path.Base(target); exclude == logicalPath || path.Base(exclude) != name {
		pattern += `|\[\[\s*` + regexp.QuoteMeta(name) + `\s*[\]|#]`
	}
	linkRe := regexp.MustCompile(pattern)

	var references []string
	visit := func(logical, file string) error {
		if logical == exclude || strings.HasPrefix(logical, exclude+"/") {
			return nil
		}
		content, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if linkRe.MatchString(codeRe.ReplaceAllString(string(content), " ")) {
			references = append(references, logical)
		}
		return nil
	}

	if home := filepath.Join(cfg.Wiki.RootDir, "pages", "home", "document.md"); isFile(home) {
		if err := visit("/", home); err != nil {
			return nil, err
		}
	}
	// WalkDir visits the documents in tree order
	documentsRoot := filepath.Join(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir)
	err := filepath.WalkDir(documentsRoot, func(p string, d fs.DirEntry, err error) error {
		// Unreadable directories are skipped, like list_documents does
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != documentsRoot && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		file := filepath.Join(p, "document.md")
		if p == documentsRoot || !isFile(file) {
			return nil
		}
		rel, err := filepath.Rel(documentsRoot, p)
		if err != nil {
			return err
		}
		return visit("/"+filepath.ToSlash(rel), file)
	})
	return references, err
}

// subtree describes a document directory and everything below it, with
// slash paths relative to it ("" is the directory itself).
type subtree struct {
	dirs        []string // every directory, hidden or not, parents first
	documents   []string // the directories with a document.md
	attachments int      // files other than document.md
}

func scanSubtree(dir string) (subtree, error) {
	var tree subtree
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		// A directory that cannot be read cannot be checked either
		if err != nil {
			return err
		}
		if !d.IsDir() {
			if d.Name() != "document.md" {
				tree.attachments++
			}
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); rel == "." {
			rel = ""
		}
		tree.dirs = append(tree.dirs, rel)
		if isFile(filepath.Join(p, "document.md")) {
			tree.documents = append(tree.documents, rel)
		}
		return nil
	})
	return tree, err
}

func joinLogical(base, rel string) string {
	if rel == "" {
		return base
	}
	return base + "/" + rel
}

// vacant checks that dst can receive src: it does not exist, it is src itself
// under a name that only differs in case, or it is an empty directory that
// the rename must replace.
func vacant(src, dst string) (replace bool, err error) {
	info, err := os.Stat(dst)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if dst != src && strings.EqualFold(dst, src) {
		if srcInfo, err := os.Stat(src); err == nil && os.SameFile(srcInfo, info) {
			return false, nil
		}
	}
	if info.IsDir() {
		entries, err := os.ReadDir(dst)
		if err != nil {
			return false, err
		}
		if len(entries) == 0 {
			return true, nil
		}
	}
	return false, ErrTargetExists
}

// mkdirParents creates the missing parents of target and returns them, so
// that a rollback can remove them.
func mkdirParents(target string) ([]string, error) {
	var missing []string
	for dir := filepath.Dir(target); dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			break
		}
		missing = append(missing, dir)
	}
	return missing, os.MkdirAll(filepath.Dir(target), 0755)
}

// removeEmptyDirs removes the directories that are still empty, deepest first.
func removeEmptyDirs(dirs []string) {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, dir := range dirs {
		os.Remove(dir)
	}
}

// commentsDir holds the comments of doc and, nested, of its sub-documents.
func commentsDir(cfg *config.Config, doc Document) string {
	return filepath.Join(cfg.Wiki.RootDir, "comments", filepath.FromSlash(strings.TrimPrefix(doc.LogicalPath, "/")))
}

// touchDocumentsRoot updates the mtime of the documents directory, which
// invalidates the wikilink name index (goldext.cachedSlugIndex) after changes
// below its top level. It uses the real clock: the tests stop now.
func touchDocumentsRoot(cfg *config.Config) {
	root := filepath.Join(cfg.Wiki.RootDir, cfg.Wiki.DocumentsDir)
	at := time.Now()
	if err := os.Chtimes(root, at, at); err != nil && !os.IsNotExist(err) {
		logger.Warn("Failed to update the mtime of %s: %v", root, err)
	}
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
