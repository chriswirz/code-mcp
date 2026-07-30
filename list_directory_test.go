package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// summaryLineFor returns the one line of a list_directory result that starts
// with prefix, so a test can check the summary appended to it without caring
// about the order entries come back in.
func summaryLineFor(text, prefix string) string {
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestListDirectorySummaryOffByDefault(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	os.WriteFile(filepath.Join(root, "one.txt"), []byte("a\nb\nc\n"), 0o644)

	text := toolText(t, s, "list_directory", map[string]any{})
	if strings.Contains(text, "bytes") || strings.Contains(text, "lines") {
		t.Errorf("list_directory should not summarize by default:\n%s", text)
	}
}

func TestListDirectorySummaryShowsSizeMimeAndLines(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	content := "line one\nline two\nline three\n"
	os.WriteFile(filepath.Join(root, "one.txt"), []byte(content), 0o644)

	text := toolText(t, s, "list_directory", map[string]any{"summary": true})
	line := summaryLineFor(text, "one.txt")
	if line == "" {
		t.Fatalf("one.txt not found in:\n%s", text)
	}
	for _, want := range []string{strconv.Itoa(len(content)) + " bytes", "text/plain", "3 lines"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q missing %q", line, want)
		}
	}
}

func TestListDirectorySummarySkipsLineCountForBinary(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0x00, 0x01, 0x02, 0x03}, 0o644)

	text := toolText(t, s, "list_directory", map[string]any{"summary": true})
	line := summaryLineFor(text, "bin.dat")
	if line == "" {
		t.Fatalf("bin.dat not found in:\n%s", text)
	}
	if !strings.Contains(line, "4 bytes") {
		t.Errorf("line %q missing the byte count", line)
	}
	if strings.Contains(line, "lines") {
		t.Errorf("line %q should not carry a line count for a binary file", line)
	}
}

func TestListDirectorySummaryLeavesDirectoriesBare(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)

	text := toolText(t, s, "list_directory", map[string]any{"summary": true})
	if line := summaryLineFor(text, "sub/"); line != "sub/" {
		t.Errorf("directory entry = %q, want the bare name unchanged", line)
	}
}

func TestListDirectorySummaryRecursive(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	os.MkdirAll(filepath.Join(root, "sub"), 0o755)
	os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("x\n"), 0o644)

	text := toolText(t, s, "list_directory", map[string]any{"summary": true, "recursive": true})
	line := summaryLineFor(text, "sub/nested.txt")
	if line == "" {
		t.Fatalf("sub/nested.txt not found in:\n%s", text)
	}
	if !strings.Contains(line, "2 bytes") || !strings.Contains(line, "1 lines") {
		t.Errorf("recursive summary line = %q", line)
	}
}

func TestListDirectorySummarySkipsLineCountOverMaxFileBytes(t *testing.T) {
	s, root := newTestServer(t)
	defer s.processes.stopAll()
	s.ws.MaxFileBytes = 4
	os.WriteFile(filepath.Join(root, "big.txt"), []byte("this is well over the limit\n"), 0o644)

	text := toolText(t, s, "list_directory", map[string]any{"summary": true})
	line := summaryLineFor(text, "big.txt")
	if line == "" {
		t.Fatalf("big.txt not found in:\n%s", text)
	}
	if strings.Contains(line, "lines") {
		t.Errorf("line %q should not carry a line count over workspace.max_file_bytes", line)
	}
	if !strings.Contains(line, "bytes") {
		t.Errorf("line %q should still carry a byte count", line)
	}
}
