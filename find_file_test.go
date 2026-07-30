package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindFileMatchesBySubstringCaseInsensitive(t *testing.T) {
	s, root := newTestServer(t)
	writeFile(t, root, "internal/UserConfig.go", "package internal\n\nvar X = 1\n")
	writeFile(t, root, "internal/other.go", "package internal\n")

	got := call(t, s, "tools/call", map[string]any{
		"name":      "find_file",
		"arguments": map[string]any{"term": "config"},
	})
	if got["isError"] == true {
		t.Fatalf("find_file failed: %v", got)
	}

	matches, _ := got["structuredContent"].([]any)
	if len(matches) != 1 {
		t.Fatalf("expected exactly one match, got %v", matches)
	}
	m, _ := matches[0].(map[string]any)
	if m["path"] != filepath.ToSlash("internal/UserConfig.go") && m["path"] != "internal/UserConfig.go" {
		t.Errorf("path = %v, want internal/UserConfig.go", m["path"])
	}
	if size, _ := m["size_bytes"].(float64); size <= 0 {
		t.Errorf("size_bytes = %v, want > 0", m["size_bytes"])
	}
	if lines, _ := m["lines"].(float64); lines != 3 {
		t.Errorf("lines = %v, want 3", m["lines"])
	}
}

func TestFindFileReportsBinarySkip(t *testing.T) {
	s, root := newTestServer(t)
	if err := os.WriteFile(filepath.Join(root, "asset-config.bin"), []byte{0, 1, 2, 3, 0, 5}, 0o644); err != nil {
		t.Fatal(err)
	}

	got := call(t, s, "tools/call", map[string]any{
		"name":      "find_file",
		"arguments": map[string]any{"term": "config"},
	})
	matches, _ := got["structuredContent"].([]any)
	if len(matches) != 1 {
		t.Fatalf("expected one match, got %v", matches)
	}
	m, _ := matches[0].(map[string]any)
	if _, hasLines := m["lines"]; hasLines {
		t.Errorf("a binary file should not report a line count: %v", m)
	}
	if m["lines_unavailable"] != "binary" {
		t.Errorf("lines_unavailable = %v, want %q", m["lines_unavailable"], "binary")
	}

	text := toolText(t, s, "find_file", map[string]any{"term": "config"})
	if !strings.Contains(text, "(binary)") {
		t.Errorf("text summary should note the file is binary: %q", text)
	}
}

func TestFindFileNoMatches(t *testing.T) {
	s, _ := newTestServer(t)
	text := toolText(t, s, "find_file", map[string]any{"term": "zzz_nonexistent_zzz"})
	if !strings.Contains(text, "No files matched") {
		t.Errorf("expected a no-matches message, got %q", text)
	}
}

func TestFindFileRequiresTerm(t *testing.T) {
	s, _ := newTestServer(t)
	text := toolText(t, s, "find_file", map[string]any{})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("expected an error for a missing term, got %q", text)
	}
}

func TestCountLines(t *testing.T) {
	cases := []struct {
		data []byte
		want int
	}{
		{nil, 0},
		{[]byte(""), 0},
		{[]byte("one line, no trailing newline"), 1},
		{[]byte("line one\nline two\n"), 2},
		{[]byte("line one\nline two"), 2},
		{[]byte("\n\n\n"), 3},
	}
	for _, tc := range cases {
		if got := countLines(tc.data); got != tc.want {
			t.Errorf("countLines(%q) = %d, want %d", tc.data, got, tc.want)
		}
	}
}

// writeFile creates a file (and its parent directories) under root, failing
// the test on error.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
