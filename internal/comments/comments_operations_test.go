package comments

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCommentOperationsUseConfiguredContainedRoot(t *testing.T) {
	commentsRoot := filepath.Join(t.TempDir(), "custom-comments")
	if err := AddComment(commentsRoot, "/guides/start", "nested comment", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := AddComment(commentsRoot, "/", "homepage comment", "admin"); err != nil {
		t.Fatal(err)
	}

	nested, err := GetComments(commentsRoot, "/guides/start")
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 1 || nested[0].Content != "nested comment" || nested[0].Author != "editor" {
		t.Fatalf("nested comments = %#v, want one editor comment", nested)
	}
	homepage, err := GetComments(commentsRoot, "/")
	if err != nil {
		t.Fatal(err)
	}
	if len(homepage) != 1 || homepage[0].Content != "homepage comment" || homepage[0].Author != "admin" {
		t.Fatalf("homepage comments = %#v, want one admin comment", homepage)
	}

	if _, err := os.Stat(filepath.Join(commentsRoot, "guides", "start", nested[0].ID)); err != nil {
		t.Fatalf("nested comment was not stored below configured root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(commentsRoot, homepage[0].ID)); err != nil {
		t.Fatalf("homepage comment was not stored below configured root: %v", err)
	}

	if err := DeleteComment(commentsRoot, nested[0].ID, "/guides/start", true); err != nil {
		t.Fatal(err)
	}
	nested, err = GetComments(commentsRoot, "/guides/start")
	if err != nil {
		t.Fatal(err)
	}
	if len(nested) != 0 {
		t.Fatalf("nested comments after delete = %#v, want none", nested)
	}
}

func TestCommentOperationsRejectUnsafePaths(t *testing.T) {
	commentsRoot := filepath.Join(t.TempDir(), "comments")
	const commentID = "20260921123456_admin.md"
	for _, documentPath := range []string{"../outside", `..\outside`, "%2e%2e/outside"} {
		if err := AddComment(commentsRoot, documentPath, "blocked", "admin"); err == nil {
			t.Errorf("AddComment(%q) succeeded, want error", documentPath)
		}
		if result, err := GetComments(commentsRoot, documentPath); err == nil {
			t.Errorf("GetComments(%q) = %#v, want error", documentPath, result)
		}
		if err := DeleteComment(commentsRoot, commentID, documentPath, true); err == nil {
			t.Errorf("DeleteComment(%q) succeeded, want error", documentPath)
		}
	}
}

func TestAddCommentNeverOverwritesAComment(t *testing.T) {
	commentsRoot := filepath.Join(t.TempDir(), "comments")
	previousNow := Now
	at := time.Date(2026, 10, 22, 20, 30, 0, 0, time.Local)
	Now = func() time.Time { return at }
	t.Cleanup(func() { Now = previousNow })

	if err := AddComment(commentsRoot, "/guides/start", "first", "editor"); err != nil {
		t.Fatal(err)
	}
	if err := AddComment(commentsRoot, "/guides/start", "second", "editor"); !errors.Is(err, ErrCommentExists) {
		t.Fatalf("second AddComment error = %v, want ErrCommentExists", err)
	}
	// Another user may comment in the same second
	if err := AddComment(commentsRoot, "/guides/start", "other", "admin"); err != nil {
		t.Fatal(err)
	}

	list, err := GetComments(commentsRoot, "/guides/start")
	if err != nil {
		t.Fatal(err)
	}
	contents := map[string]string{}
	for _, comment := range list {
		contents[comment.Author] = comment.Content
	}
	if len(list) != 2 || contents["editor"] != "first" || contents["admin"] != "other" {
		t.Fatalf("comments = %#v, want the first editor comment and the admin comment", list)
	}
}
