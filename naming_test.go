package main

import (
	"strings"
	"testing"
)

func TestCamelCaseCallRunsWithWarning(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "x\nx\n")

	text := toolText(t, s, "edit_file", map[string]any{
		"path": "a.go", "oldText": "x", "new_string": "y", "replaceAll": true,
	})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("the call should still run: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "y\ny\n" {
		t.Fatalf("file = %q", got)
	}
	for _, want := range []string{"Warning:", `"replaceAll" was read as "replace_all"`, `"oldText" was read as "old_string"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in: %s", want, text)
		}
	}
}

func TestSnakeCaseWinsAndCamelIsReportedIgnored(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "x\n")

	text := toolText(t, s, "multi_edit", map[string]any{
		"dry_run": true, "dryRun": false,
		"edits": []any{map[string]any{"path": "a.go", "old_string": "x", "new_string": "y"}},
	})
	if !strings.Contains(text, "Dry run") {
		t.Errorf("dry_run should have won: %s", text)
	}
	if !strings.Contains(text, `"dryRun" was ignored because "dry_run" was also given`) {
		t.Errorf("missing ignored warning: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "x\n" {
		t.Errorf("file changed: %q", got)
	}
}

func TestSnakeCaseCallHasNoWarning(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "x\n")
	text := toolText(t, s, "edit_file", map[string]any{"path": "a.go", "old_string": "x", "new_string": "y"})
	if strings.Contains(text, "Warning:") {
		t.Errorf("unexpected warning: %s", text)
	}
}

func TestNamingWarningIgnoresUndeclaredKeys(t *testing.T) {
	sch := schema(nil, map[string]any{"env": prop("object", "free-form")})
	if w := namingWarning(sch, []byte(`{"env":{"goPath":"x"}}`)); w != "" {
		t.Errorf("free-form keys should not warn: %s", w)
	}
}

func TestRemoveSections(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "keep\nDROP1\nkeep\nDROP2\n")
	write(t, root, "b.go", "one\nGONE\ntwo\n")

	text := toolText(t, s, "remove_sections", map[string]any{
		"path": "a.go",
		"sections": []any{
			map[string]any{"old_string": "DROP1\n"},
			map[string]any{"old_string": "DROP2\n"},
			map[string]any{"path": "b.go", "old_string": "GONE\n"},
		},
	})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("unexpected error: %s", text)
	}
	if !strings.Contains(text, "removal(s)") {
		t.Errorf("report should count removals: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "keep\nkeep\n" {
		t.Errorf("a.go = %q", got)
	}
	if got := mustRead(t, root, "b.go"); got != "one\ntwo\n" {
		t.Errorf("b.go = %q", got)
	}
}

func TestRemoveSectionsIsAtomic(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "A\nB\n")
	text := toolText(t, s, "remove_sections", map[string]any{
		"path":     "a.go",
		"sections": []any{map[string]any{"old_string": "A\n"}, map[string]any{"old_string": "MISSING"}},
	})
	if !strings.HasPrefix(text, "ERROR:") || !strings.Contains(text, "sections[1]") {
		t.Fatalf("expected sections[1] error: %s", text)
	}
	if got := mustRead(t, root, "a.go"); got != "A\nB\n" {
		t.Errorf("file changed: %q", got)
	}
}

func TestRemoveSectionsRequiresContent(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "A\n")
	text := toolText(t, s, "remove_sections", map[string]any{
		"sections": []any{map[string]any{"path": "a.go", "oldText": ""}},
	})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("an empty section should be refused: %s", text)
	}
	text = toolText(t, s, "remove_sections", map[string]any{"sections": []any{map[string]any{"old_string": "A\n"}}})
	if !strings.Contains(text, "path is required") {
		t.Errorf("a section with no path should be refused: %s", text)
	}
}
