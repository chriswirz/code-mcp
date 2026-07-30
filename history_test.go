package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollbackStepsBackOneStateAtATime(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "v0\n")
	toolText(t, s, "edit_file", map[string]any{"path": "a.go", "old_string": "v0", "new_string": "v1"})
	toolText(t, s, "edit_file", map[string]any{"path": "a.go", "old_string": "v1", "new_string": "v2"})

	text := toolText(t, s, "rollback", map[string]any{"path": "a.go"})
	if strings.HasPrefix(text, "ERROR:") || mustRead(t, root, "a.go") != "v1\n" {
		t.Fatalf("first rollback: %s / %q", text, mustRead(t, root, "a.go"))
	}
	if strings.Contains(text, "Contents of") {
		t.Errorf("contents should only be returned on request: %s", text)
	}
	text = toolText(t, s, "rollback", map[string]any{"path": "a.go", "return_content": true})
	if mustRead(t, root, "a.go") != "v0\n" || !strings.Contains(text, "Contents of a.go:\nv0") {
		t.Fatalf("second rollback: %s", text)
	}
	text = toolText(t, s, "rollback", map[string]any{"path": "a.go"})
	if !strings.HasPrefix(text, "ERROR:") || !strings.Contains(text, "earliest tracked state") {
		t.Fatalf("expected the history to be spent: %s", text)
	}
}

func TestRollbackDepthIsBounded(t *testing.T) {
	s, root := newTestServer(t)
	s.ws.History.SetDepth(2)
	for _, v := range []string{"a", "b", "c", "d"} {
		toolText(t, s, "write_file", map[string]any{"path": "f.txt", "content": v})
	}
	toolText(t, s, "rollback", map[string]any{"path": "f.txt"})
	toolText(t, s, "rollback", map[string]any{"path": "f.txt"})
	if got := mustRead(t, root, "f.txt"); got != "b" {
		t.Fatalf("got %q, want b", got)
	}
	if text := toolText(t, s, "rollback", map[string]any{"path": "f.txt"}); !strings.Contains(text, "earliest tracked state") {
		t.Fatalf("expected spent history: %s", text)
	}
}

func TestRollbackPastCreationRemovesFile(t *testing.T) {
	s, root := newTestServer(t)
	toolText(t, s, "write_file", map[string]any{"path": "new.txt", "content": "x"})
	toolText(t, s, "rollback", map[string]any{"path": "new.txt"})
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
		t.Fatalf("file should have been removed: %v", err)
	}
}

func TestRollbackOfUntrackedFile(t *testing.T) {
	s, _ := newTestServer(t)
	if text := toolText(t, s, "rollback", map[string]any{"path": "hello.txt"}); !strings.Contains(text, "earliest tracked state") {
		t.Fatalf("got %s", text)
	}
}

func TestEmptyReplacementWarnsToUseRemoveSections(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "A\nB\n")
	text := toolText(t, s, "edit_file", map[string]any{"path": "a.go", "old_string": "A\n", "new_string": ""})
	if !strings.Contains(text, "use remove_sections") {
		t.Errorf("edit_file: %s", text)
	}
	text = toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "a.go", "old_string": "B\n", "new_string": ""},
	}})
	if !strings.Contains(text, "use remove_sections") {
		t.Errorf("multi_edit: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "" {
		t.Errorf("deletions should still happen: %q", got)
	}
}
