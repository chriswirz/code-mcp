package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// applyDiff drives the apply_diff tool and returns the decoded result.
func applyDiff(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	return call(t, s, "tools/call", map[string]any{"name": "apply_diff", "arguments": args})
}

func mustRead(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n")
}

func write(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyDiffModifiesAFile(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "greet.txt", "alpha\nbravo\ncharlie\ndelta\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/greet.txt
+++ b/greet.txt
@@ -1,4 +1,4 @@
 alpha
-bravo
+BRAVO
 charlie
 delta
`})
	if got["isError"] == true {
		t.Fatalf("apply_diff failed: %v", got)
	}
	if want := "alpha\nBRAVO\ncharlie\ndelta\n"; mustRead(t, root, "greet.txt") != want {
		t.Errorf("content = %q, want %q", mustRead(t, root, "greet.txt"), want)
	}
	structured, _ := got["structuredContent"].(map[string]any)
	if structured["additions"] != float64(1) || structured["deletions"] != float64(1) {
		t.Errorf("counts = +%v -%v, want +1 -1", structured["additions"], structured["deletions"])
	}
}

func TestApplyDiffMultipleHunksAndFiles(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "1\n2\n3\n4\n5\n6\n7\n8\n9\n10\n")
	write(t, root, "sub/b.txt", "keep\nchange me\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,4 @@
 1
+one-and-a-half
 2
 3
@@ -8,3 +9,3 @@
 8
-9
+nine
 10
--- a/sub/b.txt
+++ b/sub/b.txt
@@ -1,2 +1,2 @@
 keep
-change me
+changed
`})
	if got["isError"] == true {
		t.Fatalf("apply_diff failed: %v", got)
	}
	if want := "1\none-and-a-half\n2\n3\n4\n5\n6\n7\n8\nnine\n10\n"; mustRead(t, root, "a.txt") != want {
		t.Errorf("a.txt = %q, want %q", mustRead(t, root, "a.txt"), want)
	}
	if want := "keep\nchanged\n"; mustRead(t, root, "sub/b.txt") != want {
		t.Errorf("b.txt = %q", mustRead(t, root, "sub/b.txt"))
	}
	structured, _ := got["structuredContent"].(map[string]any)
	files, _ := structured["files"].([]any)
	if len(files) != 2 {
		t.Errorf("want 2 files in the result, got %d", len(files))
	}
}

func TestApplyDiffCreatesAndDeletes(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "doomed.txt", "gone\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- /dev/null
+++ b/fresh/new.txt
@@ -0,0 +1,2 @@
+hello
+world
--- a/doomed.txt
+++ /dev/null
@@ -1 +0,0 @@
-gone
`})
	if got["isError"] == true {
		t.Fatalf("apply_diff failed: %v", got)
	}
	if want := "hello\nworld\n"; mustRead(t, root, "fresh/new.txt") != want {
		t.Errorf("new.txt = %q, want %q", mustRead(t, root, "fresh/new.txt"), want)
	}
	if _, err := os.Stat(filepath.Join(root, "doomed.txt")); !os.IsNotExist(err) {
		t.Error("doomed.txt should have been deleted")
	}
}

func TestApplyDiffRename(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "old/name.txt", "one\ntwo\n")

	got := applyDiff(t, s, map[string]any{"diff": `diff --git a/old/name.txt b/new/name.txt
similarity index 80%
rename from old/name.txt
rename to new/name.txt
--- a/old/name.txt
+++ b/new/name.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
`})
	if got["isError"] == true {
		t.Fatalf("apply_diff failed: %v", got)
	}
	if want := "one\nTWO\n"; mustRead(t, root, "new/name.txt") != want {
		t.Errorf("new path = %q, want %q", mustRead(t, root, "new/name.txt"), want)
	}
	if _, err := os.Stat(filepath.Join(root, "old/name.txt")); !os.IsNotExist(err) {
		t.Error("the old path should be gone after a rename")
	}
}

func TestApplyDiffToleratesWrongLineNumbers(t *testing.T) {
	s, root := newTestServer(t)
	// The patch claims line 2, but the text really sits at line 5. A model
	// writing a diff against a stale copy produces exactly this.
	write(t, root, "drift.txt", "pad\npad\npad\npad\ntarget\ntail\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/drift.txt
+++ b/drift.txt
@@ -2,2 +2,2 @@
-target
+hit
 tail
`})
	if got["isError"] == true {
		t.Fatalf("apply_diff should search for context: %v", got)
	}
	if want := "pad\npad\npad\npad\nhit\ntail\n"; mustRead(t, root, "drift.txt") != want {
		t.Errorf("content = %q, want %q", mustRead(t, root, "drift.txt"), want)
	}
	// The offset is reported, since it usually means the diff is stale.
	structured, _ := got["structuredContent"].(map[string]any)
	files, _ := structured["files"].([]any)
	first, _ := files[0].(map[string]any)
	hunks, _ := first["hunks"].([]any)
	h, _ := hunks[0].(map[string]any)
	if h["offset"] == float64(0) {
		t.Error("a hunk that moved should report a non-zero offset")
	}
}

func TestApplyDiffRejectsBadContext(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "strict.txt", "actual content\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/strict.txt
+++ b/strict.txt
@@ -1 +1 @@
-something else entirely
+replacement
`})
	if got["isError"] != true {
		t.Fatal("a hunk whose context does not match must fail")
	}
	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	// The message has to say what did not match, or the model cannot fix it.
	if !strings.Contains(text, "actual content") {
		t.Errorf("the error should quote the mismatch: %q", text)
	}
	if mustRead(t, root, "strict.txt") != "actual content\n" {
		t.Error("a failed patch must not modify the file")
	}
}

func TestApplyDiffErrorPointsAtTheClosestMatch(t *testing.T) {
	s, root := newTestServer(t)
	// The hunk almost matches far down the file. The error must point there,
	// not at line 1 where the search happened to start.
	write(t, root, "far.txt", "pad\npad\npad\npad\npad\npad\nkeep\nWRONG\ntail\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/far.txt
+++ b/far.txt
@@ -1,3 +1,3 @@
 keep
-EXPECTED
+new
 tail
`})
	if got["isError"] != true {
		t.Fatal("the hunk should not apply")
	}
	content, _ := got["content"].([]any)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if !strings.Contains(text, "WRONG") || !strings.Contains(text, "EXPECTED") {
		t.Errorf("the error should contrast the file's line with the hunk's: %q", text)
	}
	if !strings.Contains(text, "line 8") {
		t.Errorf("the error should name line 8, where the near-match is: %q", text)
	}
}

func TestApplyDiffIsAllOrNothing(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "first.txt", "good\n")
	write(t, root, "second.txt", "unexpected\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- a/first.txt
+++ b/first.txt
@@ -1 +1 @@
-good
+changed
--- a/second.txt
+++ b/second.txt
@@ -1 +1 @@
-does not match
+never written
`})
	if got["isError"] != true {
		t.Fatal("the patch should fail on its second file")
	}
	// The first file must be untouched: a half-applied patch is worse than a
	// rejected one, because nothing says which half landed.
	if mustRead(t, root, "first.txt") != "good\n" {
		t.Error("the first file was written even though the patch failed")
	}
	if mustRead(t, root, "second.txt") != "unexpected\n" {
		t.Error("the second file was modified")
	}
}

func TestApplyDiffDryRun(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "peek.txt", "before\n")

	got := applyDiff(t, s, map[string]any{"dry_run": true, "diff": `--- a/peek.txt
+++ b/peek.txt
@@ -1 +1 @@
-before
+after
`})
	if got["isError"] == true {
		t.Fatalf("the dry run should succeed: %v", got)
	}
	if mustRead(t, root, "peek.txt") != "before\n" {
		t.Error("a dry run must not write")
	}
	structured, _ := got["structuredContent"].(map[string]any)
	if structured["applied"] == true {
		t.Error("a dry run should not report itself as applied")
	}
	if structured["dry_run"] != true {
		t.Error("the result should say it was a dry run")
	}
}

func TestApplyDiffIgnoreWhitespace(t *testing.T) {
	s, root := newTestServer(t)
	// The file is tab-indented; the patch context arrives space-indented.
	write(t, root, "ws.go", "func main() {\n\tprintln(\"a\")\n}\n")

	diff := "--- a/ws.go\n+++ b/ws.go\n@@ -1,3 +1,3 @@\n func main() {\n-    println(\"a\")\n+    println(\"b\")\n }\n"
	if got := applyDiff(t, s, map[string]any{"diff": diff}); got["isError"] != true {
		t.Error("without ignore_whitespace the indentation mismatch should fail")
	}

	got := applyDiff(t, s, map[string]any{"diff": diff, "ignore_whitespace": true})
	if got["isError"] == true {
		t.Fatalf("ignore_whitespace should let it apply: %v", got)
	}
	// The context line keeps the file's own tab; only the changed line comes
	// from the patch.
	content := mustRead(t, root, "ws.go")
	if !strings.HasPrefix(content, "func main() {\n") {
		t.Errorf("content = %q", content)
	}
	if !strings.Contains(content, `println("b")`) {
		t.Errorf("the replacement was not applied: %q", content)
	}
}

func TestApplyDiffStripLevels(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "plain.txt", "x\n")

	// strip 0 means the header paths are used as they stand.
	got := applyDiff(t, s, map[string]any{"strip": 0, "diff": `--- plain.txt
+++ plain.txt
@@ -1 +1 @@
-x
+y
`})
	if got["isError"] == true {
		t.Fatalf("strip 0 failed: %v", got)
	}
	if mustRead(t, root, "plain.txt") != "y\n" {
		t.Errorf("content = %q", mustRead(t, root, "plain.txt"))
	}
}

func TestApplyDiffRefusesToEscapeTheWorkspace(t *testing.T) {
	s, _ := newTestServer(t)
	got := applyDiff(t, s, map[string]any{"strip": 0, "diff": `--- a/../../escape.txt
+++ b/../../escape.txt
@@ -0,0 +1 @@
+owned
`})
	if got["isError"] != true {
		t.Fatal("a patch must not be able to write outside the workspace")
	}
}

func TestApplyDiffRejectsGarbage(t *testing.T) {
	s, _ := newTestServer(t)
	for _, bad := range []string{
		"this is not a diff at all",
		"@@ -1 +1 @@\n-a\n+b\n", // hunk with no file header
		"--- a/x\n+++ b/x\n@@ nonsense @@\n",
	} {
		got := applyDiff(t, s, map[string]any{"diff": bad})
		if got["isError"] != true {
			t.Errorf("should have been rejected: %q", bad)
		}
	}
}

func TestApplyDiffCreateOverExistingFileFails(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "there.txt", "already here\n")

	got := applyDiff(t, s, map[string]any{"diff": `--- /dev/null
+++ b/there.txt
@@ -0,0 +1 @@
+new content
`})
	if got["isError"] != true {
		t.Fatal("creating a file that exists should fail rather than clobber it")
	}
	if mustRead(t, root, "there.txt") != "already here\n" {
		t.Error("the existing file was overwritten")
	}
}

// --- the parser and applier directly --------------------------------------

func TestParseHunkHeader(t *testing.T) {
	cases := []struct {
		line                                   string
		oldStart, oldCount, newStart, newCount int
	}{
		{"@@ -1,4 +1,6 @@", 1, 4, 1, 6},
		{"@@ -1 +1 @@", 1, 1, 1, 1},                       // absent counts mean one line
		{"@@ -0,0 +1,3 @@", 0, 0, 1, 3},                   // a creation
		{"@@ -10,2 +10,2 @@ func main() {", 10, 2, 10, 2}, // trailing heading
	}
	for _, tc := range cases {
		h, err := parseHunkHeader(tc.line)
		if err != nil {
			t.Errorf("%q: %v", tc.line, err)
			continue
		}
		if h.OldStart != tc.oldStart || h.OldCount != tc.oldCount ||
			h.NewStart != tc.newStart || h.NewCount != tc.newCount {
			t.Errorf("%q parsed as -%d,%d +%d,%d", tc.line, h.OldStart, h.OldCount, h.NewStart, h.NewCount)
		}
	}
	for _, bad := range []string{"@@ nonsense @@", "not a header", "@@ -1,2 +3,4"} {
		if _, err := parseHunkHeader(bad); err == nil {
			t.Errorf("%q should not parse", bad)
		}
	}
}

func TestStripPath(t *testing.T) {
	cases := []struct {
		path string
		n    int
		want string
	}{
		{"a/main.go", 1, "main.go"},
		{"b/pkg/thing.go", 1, "pkg/thing.go"},
		{"main.go", 0, "main.go"},
		{"a/b/c/d.go", 3, "d.go"},
		{"a/main.go", 5, "main.go"}, // stripping past the end keeps the base
		{"/dev/null", 1, "/dev/null"},
		{"./x.go", 0, "x.go"},
	}
	for _, tc := range cases {
		if got := stripPath(tc.path, tc.n); got != tc.want {
			t.Errorf("stripPath(%q, %d) = %q, want %q", tc.path, tc.n, got, tc.want)
		}
	}
}

func TestSplitJoinLinesRoundTrip(t *testing.T) {
	for _, content := range []string{"a\nb\n", "a\nb", "", "\n", "single"} {
		lines, final := splitLines(content)
		if got := joinLines(lines, final); got != content {
			t.Errorf("round trip of %q gave %q", content, got)
		}
	}
}

func TestApplyHunksPreservesMissingFinalNewline(t *testing.T) {
	// A file with no trailing newline must not silently gain one.
	files, err := parseUnifiedDiff("--- a/x\n+++ b/x\n@@ -1 +1 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := applyHunks("old", files[0].Hunks, applyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got != "new" {
		t.Errorf("content = %q, want %q with no trailing newline", got, "new")
	}
}

func TestApplyHunksAppendsToEndOfFile(t *testing.T) {
	files, err := parseUnifiedDiff("--- a/x\n+++ b/x\n@@ -2,1 +2,2 @@\n b\n+c\n", 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := applyHunks("a\nb\n", files[0].Hunks, applyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if want := "a\nb\nc\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}
