package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// toolText runs a tool and returns its first text block, marked when the call
// reported an error. It differs from callText in not failing the test on an
// error result: several of these tests are about what the error says.
func toolText(t *testing.T, s *Server, name string, args map[string]any) string {
	t.Helper()
	got := call(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	body, _ := got["content"].([]any)
	if len(body) == 0 {
		return ""
	}
	block, _ := body[0].(map[string]any)
	text, _ := block["text"].(string)
	if got["isError"] == true {
		return "ERROR: " + text
	}
	return text
}

// TestMultiEditOneFileTwoSpellings: two edits naming the same file different
// ways used to be staged as two files, so the second write - taken from disk -
// silently dropped the first edit while reporting success.
func TestMultiEditOneFileTwoSpellings(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "sub/a.txt", "one\ntwo\n")

	text := toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "sub/a.txt", "old_string": "one", "new_string": "ONE"},
		map[string]any{"path": "./sub/a.txt", "old_string": "two", "new_string": "TWO"},
	}})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("multi_edit failed: %s", text)
	}
	if got := mustRead(t, root, "sub/a.txt"); got != "ONE\nTWO\n" {
		t.Errorf("a.txt = %q, want both edits applied", got)
	}
	if !strings.Contains(text, "2 replacement(s)") {
		t.Errorf("both edits should be reported against one file: %s", text)
	}
}

// TestMultiEditRootedPathIsTheSameFile is the same problem in the spelling a
// model is most likely to use: "/a.txt" is anchored to the workspace root.
func TestMultiEditRootedPathIsTheSameFile(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\n")

	toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "a.txt", "old_string": "one", "new_string": "ONE"},
		map[string]any{"path": "/a.txt", "old_string": "two", "new_string": "TWO"},
	}})
	if got := mustRead(t, root, "a.txt"); got != "ONE\nTWO\n" {
		t.Errorf("a.txt = %q, want both edits applied", got)
	}
}

// TestReadFileCountsRealLines: the trailing empty string a final newline
// leaves behind is not a line, and must not be reported or returned as one.
func TestReadFileCountsRealLines(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	if got := toolText(t, s, "read_file", map[string]any{"path": "a.txt", "start_line": 2}); got != "two\nthree\n" {
		t.Errorf("tail read = %q, want %q", got, "two\nthree\n")
	}
	if got := toolText(t, s, "read_file", map[string]any{"path": "a.txt", "start_line": 1, "end_line": 2}); got != "one\ntwo" {
		t.Errorf("range read = %q, want %q", got, "one\ntwo")
	}
	text := toolText(t, s, "read_file", map[string]any{"path": "a.txt", "start_line": 9})
	if !strings.Contains(text, "has 3 lines") {
		t.Errorf("error should say the file has 3 lines: %s", text)
	}
}

// TestReadFileWithoutFinalNewline keeps the other case honest: a file that
// does not end in a newline does not gain one on the way back.
func TestReadFileWithoutFinalNewline(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo")

	if got := toolText(t, s, "read_file", map[string]any{"path": "a.txt", "start_line": 1}); got != "one\ntwo" {
		t.Errorf("read = %q, want %q", got, "one\ntwo")
	}
}

// TestBearerTokenMatches: the scheme is case-insensitive, the token is not.
func TestBearerTokenMatches(t *testing.T) {
	const token = "SeCrEt-Token"
	ok := []string{"Bearer " + token, "bearer " + token, "BEARER " + token, "  Bearer   " + token + "  "}
	for _, header := range ok {
		if !bearerTokenMatches(header, token) {
			t.Errorf("bearerTokenMatches(%q) = false, want true", header)
		}
	}
	bad := []string{
		"",
		"Bearer",
		"Bearer " + strings.ToLower(token), // the token's own case must match
		"Bearer " + strings.ToUpper(token),
		"Bearer wrong",
		"Basic " + token,
		token,
	}
	for _, header := range bad {
		if bearerTokenMatches(header, token) {
			t.Errorf("bearerTokenMatches(%q) = true, want false", header)
		}
	}
}

// TestResolveExisting is the containment check's new footing: a path whose
// leaf does not exist yet is still judged by where its directory really is.
func TestResolveExisting(t *testing.T) {
	dir := t.TempDir()
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveExisting(filepath.Join(dir, "not", "there", "yet.txt")); got != filepath.Join(real, "not", "there", "yet.txt") {
		t.Errorf("resolveExisting = %q, want the missing tail rejoined onto %q", got, real)
	}
	if got := resolveExisting(dir); got != real {
		t.Errorf("resolveExisting(%q) = %q, want %q", dir, got, real)
	}
}

// linkDir points link at target, however this platform will let it. Windows
// reserves symlinks for a privilege an ordinary account does not hold, but a
// directory junction needs none and is resolved the same way - and without one
// of the two this test would quietly skip on the machine most likely to have
// the bug.
func linkDir(t *testing.T, target, link string) error {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %v: %s", err, out)
	}
	return nil
}

// TestWriteThroughSymlinkedDirectoryIsContained covers the hole that check
// closes: creating a file under a link that points out of the workspace. The
// leaf does not exist yet, so nothing about it can be resolved - which is
// exactly the case the containment check used to decide lexically.
func TestWriteThroughSymlinkedDirectoryIsContained(t *testing.T) {
	s, root := newTestServer(t)
	outside := t.TempDir()
	if err := linkDir(t, outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("cannot link a directory here: %v", err)
	}

	for _, tool := range []struct {
		name string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "link/escaped.txt", "content": "oops"}},
		{"apply_diff", map[string]any{"diff": "--- /dev/null\n+++ b/link/escaped.txt\n@@ -0,0 +1 @@\n+oops\n"}},
	} {
		text := toolText(t, s, tool.name, tool.args)
		if _, err := os.Stat(filepath.Join(outside, "escaped.txt")); err == nil {
			t.Fatalf("%s wrote outside the workspace through a linked directory: %s", tool.name, text)
		}
		if !strings.HasPrefix(text, "ERROR:") {
			t.Errorf("%s through the link should be refused: %s", tool.name, text)
		}
	}

	// A link that stays inside the workspace is not the problem, and must
	// still work.
	inside := filepath.Join(root, "real")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := linkDir(t, inside, filepath.Join(root, "inside-link")); err != nil {
		t.Skipf("cannot link a directory here: %v", err)
	}
	if text := toolText(t, s, "write_file", map[string]any{
		"path": "inside-link/fine.txt", "content": "ok",
	}); strings.HasPrefix(text, "ERROR:") {
		t.Errorf("a link inside the workspace should still be writable: %s", text)
	}
}

// TestFindFilesSaysWhenItStopped: a truncated list that says nothing reads as
// a complete one, and a model concludes the file it wants does not exist.
func TestFindFilesSaysWhenItStopped(t *testing.T) {
	s, root := newTestServer(t)
	s.workspace().MaxResults = 5
	for i := range 12 {
		write(t, root, filepath.Join("many", string(rune('a'+i))+".txt"), "x\n")
	}

	text := toolText(t, s, "find_files", map[string]any{"pattern": "many/*.txt"})
	if !strings.Contains(text, "stopped at 5 files") {
		t.Errorf("find_files should say it stopped: %s", text)
	}
}

// TestGrepFilesOnlyRespectsTheLimit: files_only used to walk the whole tree
// however many files matched.
func TestGrepFilesOnlyRespectsTheLimit(t *testing.T) {
	s, root := newTestServer(t)
	for i := range 12 {
		write(t, root, filepath.Join("many", string(rune('a'+i))+".txt"), "needle\n")
	}

	text := toolText(t, s, "grep_files", map[string]any{
		"pattern": "needle", "files_only": true, "path": "many", "max_matches": 4,
	})
	if !strings.Contains(text, "stopped at 4 files") {
		t.Errorf("grep_files files_only should stop at the limit: %s", text)
	}
	if n := strings.Count(text, ".txt"); n != 4 {
		t.Errorf("listed %d files, want 4: %s", n, text)
	}
}

// TestToolPanicBecomesAnError: a bug in one tool should cost that one call.
// On stdio a panic would otherwise take the process down and the model would
// see the transport die rather than something it could act on.
func TestToolPanicBecomesAnError(t *testing.T) {
	s, _ := newTestServer(t)
	s.RegisterTool(Tool{
		Name:        "boom",
		Title:       "Panics",
		Description: "Panics on purpose.",
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var empty []int
		_ = empty[3]
		return nil, nil
	})

	got := call(t, s, "tools/call", map[string]any{"name": "boom"})
	if got["isError"] != true {
		t.Fatalf("a panicking tool should come back as an error result: %v", got)
	}
	if text := toolText(t, s, "boom", nil); !strings.Contains(text, "internal error") {
		t.Errorf("error should name it as a server bug: %s", text)
	}
	// The server is still serving.
	if text := toolText(t, s, "project_commands", nil); strings.HasPrefix(text, "ERROR:") {
		t.Errorf("the server should still answer after a panic: %s", text)
	}
}
