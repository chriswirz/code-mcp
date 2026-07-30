package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func absPathTestSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			"path": prop("string", "File relative to the workspace root."),
			"dir":  propDefault("string", "Directory to search, relative to the workspace root.", "."),
			"spec": prop("string", "Limit the diff to this path."),
		},
	}
}

func TestAbsolutePathNoteOffByDefault(t *testing.T) {
	ws := &Workspace{Root: t.TempDir()}
	note := absolutePathNote(ws, absPathTestSchema(), json.RawMessage(`{"path":"sub/file.txt"}`))
	if note != "" {
		t.Errorf("note = %q, want empty when workspace.note_absolute_paths is off", note)
	}
}

func TestAbsolutePathNoteRelativePath(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, NoteAbsolutePaths: true}
	note := absolutePathNote(ws, absPathTestSchema(), json.RawMessage(`{"path":"sub/file.txt"}`))
	want := filepath.ToSlash(filepath.Join(root, "sub/file.txt"))
	if !strings.Contains(note, `"sub/file.txt"`) || !strings.Contains(note, want) {
		t.Errorf("note = %q, want it naming sub/file.txt and %s", note, want)
	}
}

// singlePathSchema is a schema with just one workspace-relative "path"
// property and no default, so a test can check for an empty note without an
// unrelated defaulted property (like absPathTestSchema's "dir") supplying
// one of its own.
func singlePathSchema() map[string]any {
	return map[string]any{
		"properties": map[string]any{
			"path": prop("string", "File relative to the workspace root."),
		},
	}
}

// TestAbsolutePathNoteSkipsAbsoluteInput: an absolute or rooted path is
// already reported by Workspace.AdjustmentNote where a tool calls it; this
// note would only repeat that.
func TestAbsolutePathNoteSkipsAbsoluteInput(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, NoteAbsolutePaths: true}
	note := absolutePathNote(ws, singlePathSchema(), json.RawMessage(`{"path":"/README.md"}`))
	if note != "" {
		t.Errorf("note = %q, want empty for a rooted path", note)
	}
}

// TestAbsolutePathNoteSkipsNonWorkspaceMarker: "spec" has no "relative to the
// workspace root" wording in its description - a git pathspec, say - so it
// must not be treated as a resolvable workspace path.
func TestAbsolutePathNoteSkipsNonWorkspaceMarker(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, NoteAbsolutePaths: true}
	schema := map[string]any{"properties": map[string]any{
		"spec": prop("string", "Limit the diff to this path."),
	}}
	note := absolutePathNote(ws, schema, json.RawMessage(`{"spec":"sub/file.txt"}`))
	if note != "" {
		t.Errorf("note = %q, want empty for a property without the workspace-relative marker", note)
	}
}

// TestAbsolutePathNoteUsesSchemaDefault: a call that omits "dir" altogether
// still means "." per the schema's own default, the same way the tool
// handler itself would read it.
func TestAbsolutePathNoteUsesSchemaDefault(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, NoteAbsolutePaths: true}
	note := absolutePathNote(ws, absPathTestSchema(), json.RawMessage(`{}`))
	want := filepath.ToSlash(root)
	if !strings.Contains(note, `"."`) || !strings.Contains(note, want) {
		t.Errorf("note = %q, want it naming . and %s", note, want)
	}
}

func TestAbsolutePathNoteUnresolvablePathSkipped(t *testing.T) {
	root := t.TempDir()
	ws := &Workspace{Root: root, NoteAbsolutePaths: true}
	note := absolutePathNote(ws, singlePathSchema(), json.RawMessage(`{"path":"../../../../outside.txt"}`))
	if note != "" {
		t.Errorf("note = %q, want empty when the path cannot be resolved", note)
	}
}

// TestReadFileNotesAbsolutePathWhenEnabled exercises the real wiring through
// runTool, not just the helper in isolation.
func TestReadFileNotesAbsolutePathWhenEnabled(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	s.ws.NoteAbsolutePaths = true

	text := toolText(t, s, "read_file", map[string]any{"path": "hello.txt"})
	want := filepath.ToSlash(filepath.Join(root, "hello.txt"))
	if !strings.Contains(text, want) {
		t.Errorf("read_file result missing the absolute path note:\n%s", text)
	}
}

func TestReadFileNoAbsolutePathNoteByDefault(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()

	text := toolText(t, s, "read_file", map[string]any{"path": "hello.txt"})
	if strings.Contains(text, filepath.ToSlash(root)) {
		t.Errorf("read_file result should not carry an absolute path note by default:\n%s", text)
	}
}

// TestListDirectoryNotesAbsolutePathForDefault covers a call that leaves
// path out entirely, relying on the schema default ".".
func TestListDirectoryNotesAbsolutePathForDefault(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	s.ws.NoteAbsolutePaths = true

	text := toolText(t, s, "list_directory", map[string]any{})
	if !strings.Contains(text, filepath.ToSlash(root)) {
		t.Errorf("list_directory result missing the absolute path note for the default \".\":\n%s", text)
	}
}
