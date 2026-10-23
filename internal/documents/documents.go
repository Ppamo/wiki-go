// Package documents resolves wiki documents inside the wiki root and holds
// the write path shared by the HTTP API and the MCP tools: versions, atomic
// writes and optimistic concurrency checks under a single lock.
package documents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
)

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
