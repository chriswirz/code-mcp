package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func codeServer(t *testing.T, commands ...CommandConfig) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Commands = commands
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.cfg = cfg
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerAll(cfg)
	return s, root
}

func mustSucceed(t *testing.T, text string) {
	t.Helper()
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatal(text)
	}
}

func TestReadFileLineNumbers(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")
	text := toolText(t, s, "read_file", map[string]any{"path": "a.txt", "start_line": 2, "line_numbers": true})
	if text != "2\ttwo\n3\tthree" {
		t.Fatalf("got %q", text)
	}
}

func TestReadFiles(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.txt", "alpha\n")
	write(t, root, "b.txt", "one\ntwo\nthree\n")
	text := toolText(t, s, "read_files", map[string]any{"files": []any{
		"a.txt",
		map[string]any{"path": "b.txt", "start_line": 2, "end_line": 2},
		"missing.txt",
	}})
	mustSucceed(t, text)
	for _, want := range []string{"==> a.txt <==\nalpha", "==> b.txt (lines 2-2) <==\ntwo", "==> missing.txt <==\nERROR:"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
}

func TestEditLines(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.txt", "a\r\nb\r\nc\r\n")
	mustSucceed(t, toolText(t, s, "edit_lines", map[string]any{
		"path": "a.txt", "start_line": 2, "new_text": "B1\nB2", "expected_text": "b",
	}))
	// mustRead folds CRLF, so read the raw bytes: the endings are the point.
	if raw, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(raw) != "a\r\nB1\r\nB2\r\nc\r\n" {
		got := string(raw)
		t.Fatalf("got %q", got)
	}
	text := toolText(t, s, "edit_lines", map[string]any{
		"path": "a.txt", "start_line": 1, "end_line": 2, "new_text": "x", "expected_text": "a\nb",
	})
	if !strings.HasPrefix(text, "ERROR:") || !strings.Contains(text, "do not match") {
		t.Fatalf("stale edit should be refused: %s", text)
	}
}

func TestInsertText(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.go", "func f() {\n}\n")
	mustSucceed(t, toolText(t, s, "insert_text", map[string]any{"path": "a.go", "anchor": "func f() {", "text": "\tx()"}))
	mustSucceed(t, toolText(t, s, "insert_text", map[string]any{"path": "a.go", "anchor": "f()", "position": "before", "text": "// f"}))
	if got := mustRead(t, root, "a.go"); got != "// f\nfunc f() {\n\tx()\n}\n" {
		t.Fatalf("got %q", got)
	}
	write(t, root, "b.txt", "a")
	mustSucceed(t, toolText(t, s, "insert_text", map[string]any{"path": "b.txt", "line": 2, "text": "b\n"}))
	if got := mustRead(t, root, "b.txt"); got != "a\nb" {
		t.Fatalf("got %q", got)
	}
}

func TestReplaceInFiles(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.go", "oldName(1)\nxoldName()\n")
	write(t, root, "sub/b.go", "oldName(2)\n")
	text := toolText(t, s, "replace_in_files", map[string]any{
		"pattern": "oldName", "replacement": "newName", "whole_word": true, "literal": true, "dry_run": true,
	})
	if !strings.Contains(text, "2 replacement(s) in 2 file(s)") || mustRead(t, root, "a.go") != "oldName(1)\nxoldName()\n" {
		t.Fatalf("dry run: %s", text)
	}
	mustSucceed(t, toolText(t, s, "replace_in_files", map[string]any{
		"pattern": `oldName\((\d)\)`, "replacement": "newName($1)", "glob": "*.go",
	}))
	if mustRead(t, root, "a.go") != "newName(1)\nxoldName()\n" || mustRead(t, root, "sub/b.go") != "newName(2)\n" {
		t.Fatalf("got %q / %q", mustRead(t, root, "a.go"), mustRead(t, root, "sub/b.go"))
	}
	mustSucceed(t, toolText(t, s, "rollback", map[string]any{"path": "sub/b.go"}))
	if mustRead(t, root, "sub/b.go") != "oldName(2)\n" {
		t.Fatal("rollback did not undo replace_in_files")
	}
}

func TestFileOutlineAndReferences(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.go", "package a\n\ntype Server struct {\n\tn int\n}\n\n// Start calls helper.\nfunc (s *Server) Start() {\n\thelper()\n}\n\nfunc helper() {}\n")
	text := toolText(t, s, "file_outline", map[string]any{"path": "a.go"})
	mustSucceed(t, text)
	for _, want := range []string{"3-5  class type Server struct {", "method func (s *Server) Start() {", "method func helper() {}"} {
		if !strings.Contains(text, want) {
			t.Errorf("outline missing %q:\n%s", want, text)
		}
	}
	text = toolText(t, s, "find_references", map[string]any{"name": "helper"})
	mustSucceed(t, text)
	if !strings.Contains(text, "2 reference(s)") || !strings.Contains(text, "a.go:12: func helper() {}  [definition]") ||
		!strings.Contains(text, "a.go:9: helper()") {
		t.Errorf("references:\n%s", text)
	}
}

func TestDiagnostics(t *testing.T) {
	s, _ := codeServer(t, CommandConfig{Name: "lint", Description: "Lint.", Command: "echo a.go:3:5: error: bad thing"})
	text := toolText(t, s, "diagnostics", map[string]any{})
	mustSucceed(t, text)
	if !strings.Contains(text, "a.go:3:5: error: bad thing") {
		t.Fatalf("got:\n%s", text)
	}
	if text := toolText(t, s, "diagnostics", map[string]any{"paths": []any{"other.go"}}); !strings.Contains(text, "0 diagnostic(s)") {
		t.Errorf("paths filter: %s", text)
	}
}

func TestEditHistoryAndPreviewRollback(t *testing.T) {
	s, root := codeServer(t)
	write(t, root, "a.txt", "v0\n")
	toolText(t, s, "write_file", map[string]any{"path": "a.txt", "content": "v1\n"})
	toolText(t, s, "write_file", map[string]any{"path": "a.txt", "content": "v2\n"})

	text := toolText(t, s, "edit_history", map[string]any{"path": "a.txt"})
	if !strings.Contains(text, "2 tracked state(s)") || !strings.Contains(text, "#2") {
		t.Fatalf("history: %s", text)
	}
	text = toolText(t, s, "preview_rollback", map[string]any{"files": []any{"a.txt", "hello.txt"}})
	if !strings.Contains(text, "-v2") || !strings.Contains(text, "+v1") || !strings.Contains(text, "earliest tracked state") {
		t.Fatalf("preview: %s", text)
	}
	if mustRead(t, root, "a.txt") != "v2\n" {
		t.Fatal("preview must not change the file")
	}
}

func TestRunCommandPointsAtDedicatedTools(t *testing.T) {
	s, _ := codeServer(t)
	tool, ok := s.lookupTool("run_command")
	if !ok || !strings.Contains(tool.def.Description, "no dedicated tool") {
		t.Fatal("run_command should steer toward the dedicated tools")
	}
}
