package tools

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ParthSareen/o/agent"
)

func TestEditReplacesUniqueText(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "note.txt",
		"old_text": "hello",
		"new_text": "hi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "Updated note.txt") {
		t.Fatalf("result = %q", result.Content)
	}
	// Single edits also append a unified diff of the change.
	for _, want := range []string{
		"Updated note.txt (1 edit, 1 replacement).\n--- a/note.txt\n+++ b/note.txt\n",
		"@@ -1 +1 @@\n-hello world\n+hi world\n",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("result missing %q, got %q", want, result.Content)
		}
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hi world\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditRequiresUniqueMatchByDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("same same\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "note.txt",
		"old_text": "same",
		"new_text": "other",
	})
	if err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}
	if !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditAppliesMultipleEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("alpha beta gamma delta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "beta", "new_text": "BETA"},
			map[string]any{"old_text": "delta", "new_text": "DELTA"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Content, "2 edits, 2 replacements") {
		t.Fatalf("result = %q", result.Content)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "alpha BETA gamma DELTA\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditMatchesEditsAgainstOriginalContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("abc def\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// edits[1] must target the original "def", not the one introduced by edits[0].
	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "abc", "new_text": "def"},
			map[string]any{"old_text": "def", "new_text": "ghi"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "def ghi\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditRejectsOverlappingEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "ab", "new_text": "x"},
			map[string]any{"old_text": "bc", "new_text": "y"},
		},
	})
	if err == nil {
		t.Fatal("expected overlapping edits to fail")
	}
	if !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditMultipleEditsNotFoundIndexed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "hello", "new_text": "hi"},
			map[string]any{"old_text": "missing", "new_text": "x"},
		},
	})
	if err == nil {
		t.Fatal("expected missing edit to fail")
	}
	if !strings.Contains(err.Error(), "edits[1]") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditMultipleEditsAmbiguousIndexed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello same same\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "hello", "new_text": "hi"},
			map[string]any{"old_text": "same", "new_text": "x"},
		},
	})
	if err == nil {
		t.Fatal("expected ambiguous edit to fail")
	}
	if !strings.Contains(err.Error(), "edits[1]") || !strings.Contains(err.Error(), "matched 2 times") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditRejectsEmptyEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for name, args := range map[string]map[string]any{
		"missing edits": {"path": "note.txt"},
		"empty edits":   {"path": "note.txt", "edits": []any{}},
	} {
		if _, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, args); err == nil {
			t.Fatalf("%s: expected error", name)
		} else if !strings.Contains(err.Error(), "edits parameter is required") {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestEditRejectsEmptyOldTextInArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "hello", "new_text": "hi"},
			map[string]any{"old_text": "", "new_text": "x"},
		},
	})
	if err == nil {
		t.Fatal("expected empty old_text to fail")
	}
	if !strings.Contains(err.Error(), "edits[1].old_text must not be empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditAcceptsJSONStringEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Some models serialize array arguments as a JSON string.
	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":  "note.txt",
		"edits": `[{"oldText": "hello", "newText": "hi"}, {"oldText": "world", "newText": "earth"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hi earth\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestEditRejectsReplaceAllWithMultipleEdits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("a b c\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":        "note.txt",
		"replace_all": true,
		"edits": []any{
			map[string]any{"old_text": "a", "new_text": "x"},
			map[string]any{"old_text": "b", "new_text": "y"},
		},
	})
	if err == nil {
		t.Fatal("expected replace_all with multiple edits to fail")
	}
	if !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditRejectsNoChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "note.txt",
		"old_text": "hello",
		"new_text": "hello",
	})
	if err == nil {
		t.Fatal("expected no-change edit to fail")
	}
	if !strings.Contains(err.Error(), "no changes") {
		t.Fatalf("err = %v", err)
	}
	// The no-change path fails before any diff is built, so the error stays bare.
	if strings.Contains(err.Error(), "--- a/") || strings.Contains(err.Error(), "diff lines elided") {
		t.Fatalf("no-change error should not carry diff info: %v", err)
	}
}

func TestEditAppendsUnifiedDiffForMultiEdit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("one\ntwo\nthree\nfour\nfive\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "two", "new_text": "TWO"},
			map[string]any{"old_text": "four", "new_text": "FOUR"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Updated note.txt (2 edits, 2 replacements).\n" +
		"--- a/note.txt\n+++ b/note.txt\n" +
		"@@ -1,5 +1,5 @@\n one\n-two\n+TWO\n three\n-four\n+FOUR\n five\n"
	if result.Content != want {
		t.Fatalf("result = %q", result.Content)
	}
}

func TestEditAppendsUnifiedDiffForReplaceAll(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("aa bb aa\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":        "note.txt",
		"replace_all": true,
		"edits": []any{
			map[string]any{"old_text": "aa", "new_text": "xx"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "Updated note.txt (1 edit, 2 replacements).\n" +
		"--- a/note.txt\n+++ b/note.txt\n" +
		"@@ -1 +1 @@\n-aa bb aa\n+xx bb xx\n"
	if result.Content != want {
		t.Fatalf("result = %q", result.Content)
	}
}

// TestEditTruncatesLargeDiff builds three hunks around a giant middle hunk
// and checks the result stays capped, keeping the outer hunks and eliding the
// middle one behind a single note.
func TestEditTruncatesLargeDiff(t *testing.T) {
	dir := t.TempDir()

	var content strings.Builder
	content.WriteString("alpha\n")
	for i := 0; i < 38; i++ {
		fmt.Fprintf(&content, "fill-a-%02d\n", i)
	}
	content.WriteString("charlie\n")
	for i := 0; i < 38; i++ {
		fmt.Fprintf(&content, "fill-b-%02d\n", i)
	}
	content.WriteString("echo\n")
	for i := 0; i < 20; i++ {
		fmt.Fprintf(&content, "fill-c-%02d\n", i)
	}
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	bulk := strings.Repeat(strings.Repeat("x", 50)+"\n", 250)
	updated := strings.Replace(content.String(), "alpha", "ALPHA", 1)
	updated = strings.Replace(updated, "charlie", bulk, 1)
	updated = strings.Replace(updated, "echo", "ECHO", 1)

	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"edits": []any{
			map[string]any{"old_text": "alpha", "new_text": "ALPHA"},
			map[string]any{"old_text": "charlie", "new_text": bulk},
			map[string]any{"old_text": "echo", "new_text": "ECHO"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Content, "Updated note.txt (3 edits, 3 replacements).\n--- a/note.txt\n+++ b/note.txt\n") {
		t.Fatalf("missing message and diff header: %q", result.Content)
	}

	lines := strings.Split(strings.TrimSuffix(result.Content, "\n"), "\n")
	if len(lines) > 1+editDiffMaxLines {
		t.Fatalf("result = %d lines, want at most %d", len(lines), 1+editDiffMaxLines)
	}
	if strings.Count(result.Content, "@@ -") != 2 {
		t.Fatalf("expected 2 kept hunks, result = %q", result.Content)
	}
	for _, want := range []string{"-alpha\n+ALPHA\n", "-echo\n+ECHO\n"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("missing kept hunk %q, result = %q", want, result.Content)
		}
	}
	if strings.Contains(result.Content, strings.Repeat("x", 50)) {
		t.Fatalf("middle hunk should be elided, result = %q", result.Content)
	}

	elided := -1
	for _, line := range lines {
		var n int
		if _, err := fmt.Sscanf(line, "... [%d diff lines elided] ...", &n); err == nil {
			elided = n
		}
	}
	if elided <= 0 {
		t.Fatalf("expected an elision note with a positive count, result = %q", result.Content)
	}

	got, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != updated {
		t.Fatal("file content does not match the edited version")
	}
}

// TestEditTrimsOversizedSingleHunk covers a rewrite too large to keep even as
// one hunk: its opening lines survive with the range header recomputed.
func TestEditTrimsOversizedSingleHunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("alpha\ncharlie\ndelta\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	bulk := strings.Repeat("bulk\n", 500)
	result, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "note.txt",
		"old_text": "charlie",
		"new_text": bulk,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Content, "Updated note.txt (1 edit, 1 replacement).\n") {
		t.Fatalf("missing message: %q", result.Content)
	}

	lines := strings.Split(strings.TrimSuffix(result.Content, "\n"), "\n")
	if len(lines) > 1+editDiffMaxLines {
		t.Fatalf("result = %d lines, want at most %d", len(lines), 1+editDiffMaxLines)
	}
	if !strings.Contains(result.Content, "... ") || !strings.Contains(result.Content, "diff lines elided") {
		t.Fatalf("missing elision note, result = %q", result.Content)
	}
	if strings.Count(result.Content, "+bulk\n") == 0 {
		t.Fatal("expected trimmed hunk to keep some added lines")
	}

	if !strings.HasPrefix(result.Content, "Updated note.txt (1 edit, 1 replacement).\n--- a/note.txt\n+++ b/note.txt\n") {
		t.Fatalf("missing message and diff header, result = %q", result.Content)
	}

	// The trimmed hunk's range header must match the lines it shows.
	header := lines[3]
	var oldStart, oldCount, newStart, newCount int
	if n, err := fmt.Sscanf(header, "@@ -%d,%d +%d,%d @@", &oldStart, &oldCount, &newStart, &newCount); err != nil || n != 4 {
		t.Fatalf("hunk header %q does not list counts", header)
	}
	oldShown, newShown := 0, 0
	for _, line := range lines[4:] { // body starts after message, ---/+++, and the @@ line
		if strings.HasPrefix(line, "... ") {
			break
		}
		switch line[0] {
		case ' ':
			oldShown++
			newShown++
		case '-':
			oldShown++
		case '+':
			newShown++
		}
	}
	if oldShown != oldCount || newShown != newCount {
		t.Fatalf("hunk header %s does not match shown lines (old=%d new=%d)", header, oldShown, newShown)
	}

	got, err := os.ReadFile(filepath.Join(dir, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// The original line's trailing newline survives the replacement.
	if string(got) != "alpha\n"+bulk+"\ndelta\n" {
		t.Fatal("file content does not match the edited version")
	}
}

func TestEditUnifiedDiff(t *testing.T) {
	// Identical content yields no diff.
	if diff := editUnifiedDiff("x.txt", "same\n", "same\n"); diff != "" {
		t.Fatalf("diff = %q, want empty", diff)
	}
	// Content beyond the byte cap yields no diff on either side.
	big := strings.Repeat("a", editDiffMaxBytes+1)
	if diff := editUnifiedDiff("x.txt", big, "same\n"); diff != "" {
		t.Fatalf("oversized original gave diff %q", diff)
	}
	if diff := editUnifiedDiff("x.txt", "same\n", big); diff != "" {
		t.Fatalf("oversized updated gave diff %q", diff)
	}

	diff := editUnifiedDiff("x.txt", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	want := "--- a/x.txt\n+++ b/x.txt\n@@ -1,3 +1,3 @@\n one\n-two\n+TWO\n three\n"
	if diff != want {
		t.Fatalf("diff = %q, want %q", diff, want)
	}
}

func TestEditRejectsEscapingPath(t *testing.T) {
	dir := t.TempDir()
	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "../outside.txt",
		"old_text": "old",
		"new_text": "new",
	})
	if err == nil {
		t.Fatal("expected escaping path to fail")
	}
	if !strings.Contains(err.Error(), "path escapes working directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestEditRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "note.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     filepath.Join("link", "note.txt"),
		"old_text": "old",
		"new_text": "new",
	})
	if err == nil {
		t.Fatal("expected symlink escape to fail")
	}
	if !strings.Contains(err.Error(), "path escapes working directory") {
		t.Fatalf("err = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(outside, "note.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old\n" {
		t.Fatalf("outside content changed to %q", content)
	}
}

func TestEditRejectsFinalSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":     "link.txt",
		"old_text": "old",
		"new_text": "new",
	})
	if err == nil {
		t.Fatal("expected final symlink edit to fail")
	}
	if !strings.Contains(err.Error(), "is a symlink") {
		t.Fatalf("err = %v", err)
	}
	content, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "old\n" {
		t.Fatalf("target content changed to %q", content)
	}
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link mode = %v, want symlink", info.Mode())
	}
}

func TestReadRejectsParentOutsideCurrentWorkingDir(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "sub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: subdir}, map[string]any{
		"path": "../note.txt",
	})
	if err == nil {
		t.Fatal("expected parent path to fail")
	}
	if !strings.Contains(err.Error(), "path escapes working directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRequiresApproval(t *testing.T) {
	if !agent.ToolRequiresApproval((&Read{}), map[string]any{"path": "note.txt"}) {
		t.Fatal("read should require approval")
	}
}

func TestReadDefaultsToEntireFile(t *testing.T) {
	dir := t.TempDir()
	content := "one\ntwo\nthree\n"
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != content {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadAllowsAbsolutePathInsideWorkingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	content := "one\ntwo\nthree\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != content {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadRejectsAbsolutePathOutsideWorkingDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: t.TempDir()}, map[string]any{
		"path": path,
	})
	if err == nil {
		t.Fatal("expected absolute path outside the working directory to fail")
	}
	if !strings.Contains(err.Error(), "path escapes working directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAllowsAbsolutePathThroughSymlinkedWorkingDir(t *testing.T) {
	realDir := t.TempDir()
	path := filepath.Join(realDir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: alias}, map[string]any{
		"path": path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello\n" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadRejectsAbsolutePathThroughParentSymlink(t *testing.T) {
	workDir := t.TempDir()
	outsideDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideDir, "secret.txt"), []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(workDir, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: workDir}, map[string]any{
		"path": filepath.Join(link, "secret.txt"),
	})
	if err == nil {
		t.Fatal("expected a path through an escaping symlink to fail")
	}
	if !strings.Contains(err.Error(), "path escapes working directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAllowsAbsolutePathWithInnerDotDot(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": filepath.Join(dir, "sub", "..", "note.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "hello\n" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadRejectsAbsoluteDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": filepath.Join(dir, "sub"),
	})
	if err == nil {
		t.Fatal("expected absolute directory to fail")
	}
	if !strings.Contains(err.Error(), "is a directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsAbsoluteSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": link,
	})
	if err == nil {
		t.Fatal("expected absolute symlink to be rejected")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want symlink rejection", err)
	}
}

func TestEditRejectsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "note.txt")
	if err := os.WriteFile(path, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Edit{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": path,
		"edits": []any{map[string]any{
			"old_text": "hello",
			"new_text": "goodbye",
		}},
	})
	if err == nil {
		t.Fatal("expected edit to reject an absolute path")
	}
	if !strings.Contains(err.Error(), "absolute paths are not allowed") {
		t.Fatalf("err = %v", err)
	}
}

func TestFileApprovalScopesUseResolvedTargets(t *testing.T) {
	dir := t.TempDir()
	root, err := workingDirAbs(dir)
	if err != nil {
		t.Fatal(err)
	}
	read := &Read{}
	edit := &Edit{}
	target := "read\x00" + filepath.Join(root, "notes.txt")

	if got := read.ApprovalScopeIn(map[string]any{"path": "notes.txt"}, dir); got != target {
		t.Fatalf("relative read scope = %q, want %q", got, target)
	}
	if got := read.ApprovalScopeIn(map[string]any{"path": filepath.Join(root, "notes.txt")}, dir); got != target {
		t.Fatalf("absolute read scope = %q, want the same scope as the relative read", got)
	}
	if got := read.ApprovalScopeIn(map[string]any{"path": "other.txt"}, dir); got == target {
		t.Fatalf("scopes collapsed across paths: %q", got)
	}
	if got, want := edit.ApprovalScopeIn(map[string]any{"path": "notes.txt"}, dir), "edit\x00"+filepath.Join(root, "notes.txt"); got != want {
		t.Fatalf("edit scope = %q, want %q", got, want)
	}
	if got := edit.ApprovalScopeIn(map[string]any{"path": filepath.Join(root, "notes.txt")}, dir); got == edit.ApprovalScopeIn(map[string]any{"path": "notes.txt"}, dir) {
		t.Fatalf("absolute edit path collapsed into the relative scope: %q", got)
	}

	escapeA := read.ApprovalScopeIn(map[string]any{"path": "../a"}, dir)
	escapeB := read.ApprovalScopeIn(map[string]any{"path": "../b"}, dir)
	if escapeA == escapeB {
		t.Fatalf("escaping paths collapsed to the same scope: %q", escapeA)
	}
	if want := "read\x00" + root + "\x00../a"; escapeA != want {
		t.Fatalf("escaping scope = %q, want %q", escapeA, want)
	}
	if got := read.ApprovalScopeIn(map[string]any{}, dir); got != "read" {
		t.Fatalf("empty path scope = %q, want read", got)
	}
	if got := edit.ApprovalScopeIn(map[string]any{}, dir); got != "edit" {
		t.Fatalf("empty path scope = %q, want edit", got)
	}
}

func TestReadStartEnd(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":  "note.txt",
		"start": 2,
		"end":   3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "two\nthree\n" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadStartOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":  "note.txt",
		"start": 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "three\nfour\n" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadEndOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "note.txt",
		"end":  2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "one\ntwo\n" {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadSelectionRejectsHugeSingleLine(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte(strings.Repeat("x", maxReadBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":  "note.txt",
		"start": 1,
		"end":   1,
	})
	if err == nil {
		t.Fatal("expected huge selected line to fail")
	}
	if !strings.Contains(err.Error(), "selected content is too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadAllWithinLimitRejectsGrowingRead(t *testing.T) {
	reader := io.MultiReader(
		strings.NewReader(strings.Repeat("x", maxReadBytes)),
		strings.NewReader("x"),
	)

	_, err := readAllWithinLimit(reader, maxReadBytes)
	if err == nil {
		t.Fatal("expected over-limit read to fail")
	}
	if !strings.Contains(err.Error(), "content is too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsInvalidRange(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path":  "note.txt",
		"start": 4,
		"end":   2,
	})
	if err == nil {
		t.Fatal("expected invalid range to fail")
	}
	if !strings.Contains(err.Error(), "end must") {
		t.Fatalf("err = %v", err)
	}
}

func writeTestPNG(t *testing.T, dir, name string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 0xff, A: 0xff})
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestReadReturnsImageDataWhenModelSupportsImages(t *testing.T) {
	dir := t.TempDir()
	data := writeTestPNG(t, dir, "photo.png")

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir, SupportsImages: true}, map[string]any{
		"path": "photo.png",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Images) != 1 {
		t.Fatalf("images = %d, want 1", len(result.Images))
	}
	if string(result.Images[0]) != string(data) {
		t.Fatal("image data mismatch")
	}
	if !strings.Contains(result.Content, "PNG image") {
		t.Fatalf("content = %q", result.Content)
	}
}

func TestReadRejectsImageWhenModelLacksImageSupport(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, dir, "photo.png")

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir}, map[string]any{
		"path": "photo.png",
	})
	if err == nil || !strings.Contains(err.Error(), "does not support image input") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadImageRejectsLineRanges(t *testing.T) {
	dir := t.TempDir()
	writeTestPNG(t, dir, "photo.png")

	_, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir, SupportsImages: true}, map[string]any{
		"path":  "photo.png",
		"start": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "line ranges are not supported for image files") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadTextFileCarriesNoImages(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "note.txt"), []byte("plain text\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := (&Read{}).Execute(context.Background(), agent.ToolContext{WorkingDir: dir, SupportsImages: true}, map[string]any{
		"path": "note.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != "plain text\n" || len(result.Images) != 0 {
		t.Fatalf("result = %#v", result)
	}
}
