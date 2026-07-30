package main

import (
	"strings"
	"testing"
)

func grepFiles(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	return call(t, s, "tools/call", map[string]any{"name": "grep_files", "arguments": args})
}

func grepText(t *testing.T, got map[string]any) string {
	t.Helper()
	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

// numbered builds a file whose contents are easy to assert line numbers on.
const numberedFile = "one\ntwo\nthree\nNEEDLE\nfive\nsix\nseven\n"

func TestGrepDefaultsToTheMatchingLineOnly(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "hay.txt", numberedFile)

	got := grepFiles(t, s, map[string]any{"pattern": "NEEDLE"})
	if got["isError"] == true {
		t.Fatalf("grep_files failed: %v", got)
	}
	text := grepText(t, got)
	if !strings.Contains(text, "hay.txt:4:NEEDLE") {
		t.Errorf("want the match at line 4: %q", text)
	}
	// Default context is 0, so no neighbouring line should appear.
	for _, neighbour := range []string{"three", "five"} {
		if strings.Contains(text, neighbour) {
			t.Errorf("context should be off by default, but %q appeared: %q", neighbour, text)
		}
	}
	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("want 1 match, got %d", len(matches))
	}
	m, _ := matches[0].(map[string]any)
	if m["line"] != float64(4) || m["text"] != "NEEDLE" {
		t.Errorf("match = %v", m)
	}
	if m["before"] != nil || m["after"] != nil {
		t.Errorf("no context expected: %v", m)
	}
}

func TestGrepWithSymmetricContext(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "hay.txt", numberedFile)

	got := grepFiles(t, s, map[string]any{"pattern": "NEEDLE", "context": 2})
	text := grepText(t, got)
	// Context lines use the dash separator, the match uses the colon, which is
	// how grep -C distinguishes them.
	for _, want := range []string{
		"hay.txt-2-two", "hay.txt-3-three", "hay.txt:4:NEEDLE", "hay.txt-5-five", "hay.txt-6-six",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, text)
		}
	}
	if strings.Contains(text, "one") || strings.Contains(text, "seven") {
		t.Errorf("context of 2 reached too far: %q", text)
	}

	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	m, _ := matches[0].(map[string]any)
	before, _ := m["before"].([]any)
	after, _ := m["after"].([]any)
	if len(before) != 2 || len(after) != 2 {
		t.Errorf("before/after = %v / %v, want two each", before, after)
	}
	if m["first_line"] != float64(2) {
		t.Errorf("first_line = %v, want 2", m["first_line"])
	}
}

func TestGrepAsymmetricContext(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "hay.txt", numberedFile)

	got := grepFiles(t, s, map[string]any{"pattern": "NEEDLE", "before": 1, "after": 3})
	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	m, _ := matches[0].(map[string]any)
	before, _ := m["before"].([]any)
	after, _ := m["after"].([]any)
	if len(before) != 1 || len(after) != 3 {
		t.Errorf("before/after = %v / %v, want 1 and 3", before, after)
	}
	if before[0] != "three" {
		t.Errorf("before = %v, want [three]", before)
	}
}

func TestGrepContextClampsAtFileEdges(t *testing.T) {
	s, root := newTestServer(t)
	// The match is the first line, so there is nothing before it.
	write(t, root, "edge.txt", "HIT\nsecond\n")

	got := grepFiles(t, s, map[string]any{"pattern": "HIT", "context": 5})
	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	m, _ := matches[0].(map[string]any)
	if before, _ := m["before"].([]any); len(before) != 0 {
		t.Errorf("before = %v, want none at the start of a file", before)
	}
	if after, _ := m["after"].([]any); len(after) != 1 {
		t.Errorf("after = %v, want just the one remaining line", after)
	}
}

func TestGrepSeparatesDistantBlocks(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "two.txt", "HIT\na\nb\nc\nd\ne\nf\nHIT\n")

	got := grepFiles(t, s, map[string]any{"pattern": "HIT", "context": 1})
	text := grepText(t, got)
	if !strings.Contains(text, "--\n") {
		t.Errorf("non-adjacent blocks should be separated by --:\n%s", text)
	}
}

func TestGrepLiteralAndRegex(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "meta.txt", "price is a.b\nprice is axb\n")

	// As a regex, a.b matches both lines.
	got := grepFiles(t, s, map[string]any{"pattern": "a.b"})
	structured, _ := got["structuredContent"].(map[string]any)
	if matches, _ := structured["matches"].([]any); len(matches) != 2 {
		t.Errorf("regex a.b should match both lines, got %d", len(matches))
	}

	// As a literal, only the line containing a.b itself.
	got = grepFiles(t, s, map[string]any{"pattern": "a.b", "literal": true})
	structured, _ = got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("literal a.b should match one line, got %d", len(matches))
	}
	m, _ := matches[0].(map[string]any)
	if m["text"] != "price is a.b" {
		t.Errorf("matched the wrong line: %v", m["text"])
	}
}

func TestGrepIgnoreCaseAndGlob(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.go", "package Main\n")
	write(t, root, "b.txt", "package main\n")

	got := grepFiles(t, s, map[string]any{"pattern": "package main", "ignore_case": true})
	structured, _ := got["structuredContent"].(map[string]any)
	if matches, _ := structured["matches"].([]any); len(matches) != 2 {
		t.Errorf("case-insensitive search should find both, got %d", len(matches))
	}

	got = grepFiles(t, s, map[string]any{"pattern": "package", "glob": "*.go"})
	structured, _ = got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("the glob should restrict to a.go, got %d matches", len(matches))
	}
	m, _ := matches[0].(map[string]any)
	if m["path"] != "a.go" {
		t.Errorf("path = %v, want a.go", m["path"])
	}
}

func TestGrepFilesOnly(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "x.txt", "found\nfound\n")
	write(t, root, "y.txt", "found\n")
	write(t, root, "z.txt", "nothing here\n")

	got := grepFiles(t, s, map[string]any{"pattern": "found", "files_only": true})
	text := grepText(t, got)
	if !strings.Contains(text, "x.txt") || !strings.Contains(text, "y.txt") {
		t.Errorf("both matching files should be listed: %q", text)
	}
	if strings.Contains(text, "z.txt") {
		t.Errorf("a non-matching file was listed: %q", text)
	}
	// Only names, no line content.
	if strings.Contains(text, "found") {
		t.Errorf("files_only should not return lines: %q", text)
	}
}

func TestGrepSearchesASingleFile(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "only.txt", "target\n")
	write(t, root, "other.txt", "target\n")

	got := grepFiles(t, s, map[string]any{"pattern": "target", "path": "only.txt"})
	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	if len(matches) != 1 {
		t.Fatalf("searching one file should give one match, got %d", len(matches))
	}
	m, _ := matches[0].(map[string]any)
	if m["path"] != "only.txt" {
		t.Errorf("path = %v", m["path"])
	}
}

func TestGrepMaxMatches(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "many.txt", strings.Repeat("hit\n", 50))

	got := grepFiles(t, s, map[string]any{"pattern": "hit", "max_matches": 5})
	structured, _ := got["structuredContent"].(map[string]any)
	matches, _ := structured["matches"].([]any)
	if len(matches) != 5 {
		t.Errorf("want 5 matches, got %d", len(matches))
	}
	if structured["truncated"] != true {
		t.Error("the result should say it was truncated")
	}
}

func TestGrepNoMatches(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "quiet.txt", "nothing of interest\n")

	got := grepFiles(t, s, map[string]any{"pattern": "absent"})
	if got["isError"] == true {
		t.Fatalf("no matches is not an error: %v", got)
	}
	if !strings.Contains(grepText(t, got), "No matches") {
		t.Errorf("text = %q", grepText(t, got))
	}
}

func TestGrepRejectsBadPatternAndEscape(t *testing.T) {
	s, _ := newTestServer(t)
	if got := grepFiles(t, s, map[string]any{"pattern": "([unclosed"}); got["isError"] != true {
		t.Error("an invalid regex should be a tool error")
	}
	if got := grepFiles(t, s, map[string]any{"pattern": "x", "path": "../.."}); got["isError"] != true {
		t.Error("searching outside the workspace must be refused")
	}
}
