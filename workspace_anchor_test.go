package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func anchorWorkspace(t *testing.T) *Workspace {
	t.Helper()
	return NewWorkspace(WorkspaceConfig{Root: t.TempDir(), AllowWrite: true, MaxFileBytes: 1 << 20})
}

// The case that matters: a rooted path names a place in the workspace, not the
// root of the filesystem.
func TestRootedPathsLandInTheWorkspace(t *testing.T) {
	ws := anchorWorkspace(t)
	for _, path := range []string{"/README.md", "/docs/README.md", string(filepath.Separator) + "README.md"} {
		abs, _, err := ws.resolveNewAdjusted(path)
		if err != nil {
			t.Fatalf("resolve(%q): %v", path, err)
		}
		if !within(ws.Root, abs) {
			t.Errorf("resolve(%q) = %q, outside the workspace", path, abs)
		}
		if note := ws.AdjustmentNote(path, abs); note == "" {
			t.Errorf("resolve(%q) = %q produced no adjustment note", path, abs)
		}
	}
}

func TestWriteFileAnchorsAbsolutePath(t *testing.T) {
	ws := anchorWorkspace(t)
	abs, err := ws.WriteFile("/README.md", "hello")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	want := filepath.Join(ws.Root, "README.md")
	if abs != want {
		t.Fatalf("wrote to %q, want %q", abs, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("file not created: %v", err)
	}
	if note := ws.AdjustmentNote("/README.md", abs); !strings.Contains(note, "README.md") {
		t.Errorf("no adjustment note for a re-anchored write: %q", note)
	}
}

// An absolute path that is genuinely inside the workspace is left alone, and
// no note is produced for an ordinary relative path.
func TestContainedPathsAreNotAdjusted(t *testing.T) {
	ws := anchorWorkspace(t)
	inside := filepath.Join(ws.Root, "src", "main.go")
	abs, adjusted, err := ws.resolveNewAdjusted(inside)
	if err != nil || adjusted || abs != inside {
		t.Fatalf("resolve(%q) = %q adjusted=%v err=%v", inside, abs, adjusted, err)
	}
	if note := ws.AdjustmentNote("src/main.go", filepath.Join(ws.Root, "src", "main.go")); note != "" {
		t.Errorf("relative path produced a note: %q", note)
	}
}

// Re-anchoring must not become a way to escape: "../" traversal is still an
// error rather than being silently clamped.
func TestTraversalStillRefused(t *testing.T) {
	ws := anchorWorkspace(t)
	for _, path := range []string{"../outside.txt", "..", filepath.Join("sub", "..", "..", "escape")} {
		if _, err := ws.Resolve(path); err == nil {
			t.Errorf("Resolve(%q) should be refused", path)
		}
		if _, err := ws.WriteFile(path, "x"); err == nil {
			t.Errorf("WriteFile(%q) should be refused", path)
		}
	}
}

// A workspace root of "." is the documented way to ask for a server that is not
// fenced into a project directory.
func TestUnrestrictedWorkspaceReachesTheWholeSystem(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "elsewhere.txt")
	ws := NewWorkspace(WorkspaceConfig{Root: root, AllowWrite: true, Unrestricted: true, MaxFileBytes: 1 << 20})

	abs, err := ws.WriteFile(outside, "hello")
	if err != nil {
		t.Fatalf("WriteFile(%q): %v", outside, err)
	}
	if data, err := os.ReadFile(abs); err != nil || string(data) != "hello" {
		t.Fatalf("file not written outside the root: %v", err)
	}
	// Relative paths keep resolving against the root, so ordinary use is
	// unchanged.
	if got, err := ws.Resolve("src/main.go"); err != nil || got != filepath.Join(root, "src", "main.go") {
		t.Fatalf("Resolve(relative) = %q, %v", got, err)
	}
	// And traversal is no longer an error, because nothing is out of bounds.
	if _, err := ws.Resolve(".." + string(filepath.Separator) + "sibling"); err != nil {
		t.Errorf("unrestricted Resolve refused a traversal: %v", err)
	}
	if note := ws.ScopeNote(); !strings.Contains(note, "NOT") {
		t.Errorf("unrestricted workspace has no scope note: %q", note)
	}
	if note := (&Workspace{Root: root}).ScopeNote(); note != "" {
		t.Errorf("contained workspace produced a scope note: %q", note)
	}
	// Paths outside the root are reported by name, not as a pile of "..".
	if got := ws.Rel(outside); got != filepath.ToSlash(outside) {
		t.Errorf("Rel(%q) = %q", outside, got)
	}
}

// The "." root has to survive config normalisation, which rewrites it to an
// absolute path.
func TestConfigRootDotEnablesUnrestricted(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{}
	cfg.Workspace.Root = "."
	if err := cfg.Normalize(dir); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !cfg.Workspace.Unrestricted {
		t.Error(`a root of "." should have enabled unrestricted access`)
	}

	contained := Config{}
	contained.Workspace.Root = dir
	if err := contained.Normalize(dir); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if contained.Workspace.Unrestricted {
		t.Error("an explicit directory root should stay contained")
	}
}

// callText runs a tool and returns the text of its first content block.
func callText(t *testing.T, s *Server, name string, args map[string]any) string {
	t.Helper()
	got := call(t, s, "tools/call", map[string]any{"name": name, "arguments": args})
	if got["isError"] == true {
		t.Fatalf("%s: %v", name, got["content"])
	}
	content, _ := got["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// Every tool that changes a file must report the path the workspace resolved
// to, so a model that passed "/hello.txt" learns the change landed at
// "hello.txt" rather than having its own input echoed back.
func TestFileToolsReportTheCanonicalPath(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "doc.md"), []byte("One sentence. Another one.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"write_file", map[string]any{"path": "/new.txt", "content": "x"}},
		{"edit_file", map[string]any{"path": "/hello.txt", "old_string": "two", "new_string": "TWO"}},
		{"multi_edit", map[string]any{"edits": []any{map[string]any{
			"path": "/hello.txt", "old_string": "three", "new_string": "THREE"}}}},
		{"format_markdown", map[string]any{"path": "/doc.md"}},
	}
	for _, c := range cases {
		text := callText(t, s, c.tool, c.args)
		// The first line is the report; write_file adds a note that quotes the
		// caller's own path on purpose.
		text = strings.SplitN(text, "\n", 2)[0]
		if strings.Contains(text, "/hello.txt") || strings.Contains(text, "/new.txt") || strings.Contains(text, "/doc.md") {
			t.Errorf("%s echoed the rooted path back:\n%s", c.tool, text)
		}
		if !strings.Contains(text, "hello.txt") && !strings.Contains(text, "new.txt") && !strings.Contains(text, "doc.md") {
			t.Errorf("%s does not name the file it changed:\n%s", c.tool, text)
		}
	}
}
