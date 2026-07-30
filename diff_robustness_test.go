package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tests here are about the diffs models actually write, as opposed to the
// ones git emits: headers whose counts do not add up, hunks out of order, a
// file touched twice, "diff --git" with no ---/+++ pair under it. Each of
// these used to fail, silently drop a change, or fail with an error that did
// not say what to fix.

// nl, cr and bs spell out the characters some of these tests are about, so a
// stray editor setting cannot quietly change what is being asserted.
const (
	nl = "\n"
	cr = "\r"
	bs = "\\"
)

// diffText runs apply_diff and returns its text block, marked when the call
// failed, which is what most of these tests assert against.
func diffText(t *testing.T, s *Server, args map[string]any) string {
	t.Helper()
	got := applyDiff(t, s, args)
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

func mustApply(t *testing.T, s *Server, diff string) string {
	t.Helper()
	text := diffText(t, s, map[string]any{"diff": diff})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("patch did not apply: %s", text)
	}
	return text
}

func mustFail(t *testing.T, s *Server, diff string) string {
	t.Helper()
	text := diffText(t, s, map[string]any{"diff": diff})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("patch applied but should not have: %s", text)
	}
	return text
}

// readRaw reads a file without normalising its line endings, for the tests
// that are about exactly those.
func readRaw(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func wantFile(t *testing.T, root, name, want string) {
	t.Helper()
	if got := mustRead(t, root, name); got != want {
		t.Errorf("%s = %q, want %q", name, got, want)
	}
}

// TestApplyDiffWrongHunkCounts is the most common fault in a model-written
// diff: the body is right, the numbers after the @@ are not. The body wins,
// and the result says the header was wrong.
func TestApplyDiffWrongHunkCounts(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	text := mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,9 +1,12 @@
 one
-two
+TWO
 three
`)
	wantFile(t, root, "a.txt", "one\nTWO\nthree\n")
	if !strings.Contains(text, "note:") {
		t.Errorf("a miscounted header applied without saying so: %s", text)
	}
}

// TestApplyDiffCountTooSmall is the same fault the other way round: the header
// promises fewer lines than the body holds. The extra lines used to be dropped
// on the floor, applying half the hunk.
func TestApplyDiffCountTooSmall(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\nfour\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
 three
-four
+FOUR
`)
	wantFile(t, root, "a.txt", "one\nTWO\nthree\nFOUR\n")
}

// TestApplyDiffOverlongCountDoesNotEatTheNextFile guards the worst version of
// a wrong count: a hunk that runs past its body reads the next file's header
// as though it were content.
func TestApplyDiffOverlongCountDoesNotEatTheNextFile(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\n")
	write(t, root, "b.txt", "x\ny\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,9 +1,9 @@
 one
-two
+TWO
--- a/b.txt
+++ b/b.txt
@@ -1,2 +1,2 @@
 x
-y
+Y
`)
	wantFile(t, root, "a.txt", "one\nTWO\n")
	wantFile(t, root, "b.txt", "x\nY\n")
}

// TestApplyDiffSameFileTwice covers a patch that changes one file in two
// sections. The second section used to be applied to the file on disk and
// written over the first, losing it.
func TestApplyDiffSameFileTwice(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\nfour\n")

	text := mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
--- a/a.txt
+++ b/a.txt
@@ -3,2 +3,2 @@
 three
-four
+FOUR
`)
	wantFile(t, root, "a.txt", "one\nTWO\nthree\nFOUR\n")
	if !strings.Contains(text, "1 file(s), 2 insertion(s), 2 deletion(s)") {
		t.Errorf("both sections should be counted once, against one file: %s", text)
	}
}

// TestApplyDiffDeleteThenRecreate is the other patch that needs the running
// picture of the workspace rather than what is on disk.
func TestApplyDiffDeleteThenRecreate(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "old\n")

	mustApply(t, s, `--- a/a.txt
+++ /dev/null
@@ -1 +0,0 @@
-old
--- /dev/null
+++ b/a.txt
@@ -0,0 +1 @@
+fresh
`)
	wantFile(t, root, "a.txt", "fresh\n")
}

// TestApplyDiffPatchingADeletedFileIsRefused is the same machinery saying no.
func TestApplyDiffPatchingADeletedFileIsRefused(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "old\n")

	text := mustFail(t, s, `--- a/a.txt
+++ /dev/null
@@ -1 +0,0 @@
-old
--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-old
+new
`)
	if !strings.Contains(text, "deletes the file") {
		t.Errorf("error should say the patch deleted the file first: %s", text)
	}
	wantFile(t, root, "a.txt", "old\n")
}

// TestApplyDiffHunksOutOfOrder covers hunks listed bottom-up, which a model
// writing a patch from the end of a file backwards produces naturally.
func TestApplyDiffHunksOutOfOrder(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\nfour\nfive\nsix\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -5,2 +5,2 @@
 five
-six
+SIX
@@ -1,2 +1,2 @@
 one
-two
+TWO
`)
	wantFile(t, root, "a.txt", "one\nTWO\nthree\nfour\nfive\nSIX\n")
}

// TestApplyDiffDuplicateHunkSaysSo: the same change twice cannot apply twice,
// and the error should say that rather than blaming the context.
func TestApplyDiffDuplicateHunkSaysSo(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	text := mustFail(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
@@ -1,2 +1,2 @@
 one
-two
+AGAIN
`)
	if !strings.Contains(text, "overlap") {
		t.Errorf("error should name the overlap: %s", text)
	}
	wantFile(t, root, "a.txt", "one\ntwo\nthree\n")
}

// TestApplyDiffFarContextNamesTheLine: when the context is in the file but
// beyond max_offset, saying where it is beats "not found anywhere".
func TestApplyDiffFarContextNamesTheLine(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", strings.Repeat("filler\n", 400)+"target\n")

	patch := `--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-target
+TARGET
`
	text := mustFail(t, s, patch)
	if !strings.Contains(text, "line 401") || !strings.Contains(text, "max_offset") {
		t.Errorf("error should name the line and the setting: %s", text)
	}

	// With the search unbounded it applies.
	if out := diffText(t, s, map[string]any{"max_offset": 0, "diff": patch}); strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("max_offset 0 should search the whole file: %s", out)
	}
	wantFile(t, root, "a.txt", strings.Repeat("filler\n", 400)+"TARGET\n")
}

// TestApplyDiffContextLineMissingItsSpace is the fault worth failing on: a
// context line written without its leading space is indistinguishable from
// prose, so the error has to say exactly that.
func TestApplyDiffContextLineMissingItsSpace(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	text := mustFail(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
one
-two
+TWO
three
`)
	if !strings.Contains(text, "leading space") {
		t.Errorf("error should explain the missing space: %s", text)
	}
	wantFile(t, root, "a.txt", "one\ntwo\nthree\n")
}

// TestApplyDiffTrailingProseIsIgnored: a complete hunk followed by the model's
// own explanation still applies.
func TestApplyDiffTrailingProseIsIgnored(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO

That renames the second line.
`)
	wantFile(t, root, "a.txt", "one\nTWO\n")
}

// TestApplyDiffBlankContextLine: an editor that strips trailing whitespace
// turns a blank context line into an empty one, which is still a context line.
func TestApplyDiffBlankContextLine(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\n\ntwo\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1,3 +1,3 @@
 one

-two
+TWO
`)
	wantFile(t, root, "a.txt", "one\n\nTWO\n")
}

// TestApplyDiffGitHeaderWithoutMarkerLines: "diff --git" carries the paths, so
// a patch that goes straight to its hunks is still usable.
func TestApplyDiffGitHeaderWithoutMarkerLines(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\n")

	mustApply(t, s, `diff --git a/a.txt b/a.txt
index 1111111..2222222 100644
@@ -1,2 +1,2 @@
 one
-two
+TWO
`)
	wantFile(t, root, "a.txt", "one\nTWO\n")
}

// TestApplyDiffEmptyFileModes covers the two changes git describes with a mode
// line and no hunk at all.
func TestApplyDiffEmptyFileModes(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "gone.txt", "")

	mustApply(t, s, `diff --git a/empty.txt b/empty.txt
new file mode 100644
index 0000000..e69de29
diff --git a/gone.txt b/gone.txt
deleted file mode 100644
index e69de29..0000000
`)
	wantFile(t, root, "empty.txt", "")
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Errorf("gone.txt should have been deleted, stat error = %v", err)
	}
}

// TestApplyDiffMultipleGitSections is the shape of a real git patch: several
// files, each opened by diff --git and then a ---/+++ pair.
func TestApplyDiffMultipleGitSections(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\n")
	write(t, root, "b.txt", "two\n")

	text := mustApply(t, s, `diff --git a/a.txt b/a.txt
index 1..2 100644
--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-one
+ONE
diff --git a/b.txt b/b.txt
index 3..4 100644
--- a/b.txt
+++ b/b.txt
@@ -1 +1 @@
-two
+TWO
`)
	wantFile(t, root, "a.txt", "ONE\n")
	wantFile(t, root, "b.txt", "TWO\n")
	if !strings.Contains(text, "2 file(s)") {
		t.Errorf("both files should be reported: %s", text)
	}
}

// TestApplyDiffCRLFFileKeepsItsEndings: the workspace normalises what it reads
// and restores what it writes, so an LF patch applies to a CRLF file.
func TestApplyDiffCRLFFileKeepsItsEndings(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one"+cr+nl+"two"+cr+nl+"three"+cr+nl)

	mustApply(t, s, "--- a/a.txt"+nl+"+++ b/a.txt"+nl+"@@ -1,3 +1,3 @@"+nl+" one"+nl+"-two"+nl+"+TWO"+nl+" three"+nl)
	if got := readRaw(t, root, "a.txt"); !strings.Contains(got, "TWO") {
		t.Errorf("a.txt = %q, want the change applied", got)
	}
}

// TestApplyDiffCRLFPatchAgainstLFFile is the same problem from the other side:
// a patch pasted with Windows line endings.
func TestApplyDiffCRLFPatchAgainstLFFile(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one"+nl+"two"+nl)

	mustApply(t, s, "--- a/a.txt"+cr+nl+"+++ b/a.txt"+cr+nl+"@@ -1,2 +1,2 @@"+cr+nl+" one"+cr+nl+"-two"+cr+nl+"+TWO"+cr+nl)
	wantFile(t, root, "a.txt", "one"+nl+"TWO"+nl)
}

// TestApplyDiffRepeatedContextPicksTheNamedLine: with the same line three
// times over, the header's line number is what chooses between them.
func TestApplyDiffRepeatedContextPicksTheNamedLine(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "x\nmark\ny\nmark\nz\nmark\n")

	mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -4 +4 @@
-mark
+MARK
`)
	wantFile(t, root, "a.txt", "x\nmark\ny\nMARK\nz\nmark\n")
}

// TestApplyDiffPureInsertionReportsNoOffset: an insertion hunk that lands
// exactly where it said should not be reported as having drifted.
func TestApplyDiffPureInsertionReportsNoOffset(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\nthree\n")

	text := mustApply(t, s, `--- a/a.txt
+++ b/a.txt
@@ -2,0 +3 @@
+inserted
`)
	wantFile(t, root, "a.txt", "one\ntwo\ninserted\nthree\n")
	if strings.Contains(text, "offset") {
		t.Errorf("hunk landed where it said, but the result claims an offset: %s", text)
	}
}

// TestApplyDiffKeepsAndGainsFinalNewline covers both directions of the
// "no newline at end of file" marker.
func TestApplyDiffKeepsAndGainsFinalNewline(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "keep.txt", "one"+nl+"two")
	write(t, root, "gain.txt", "one"+nl+"two")

	mustApply(t, s, "--- a/keep.txt"+nl+"+++ b/keep.txt"+nl+"@@ -1,2 +1,2 @@"+nl+" one"+nl+"-two"+nl+
		bs+" No newline at end of file"+nl+"+TWO"+nl+bs+" No newline at end of file"+nl)
	if got := readRaw(t, root, "keep.txt"); strings.HasSuffix(got, nl) {
		t.Errorf("keep.txt = %q, want no final newline", got)
	}

	mustApply(t, s, "--- a/gain.txt"+nl+"+++ b/gain.txt"+nl+"@@ -1,2 +1,2 @@"+nl+" one"+nl+"-two"+nl+
		bs+" No newline at end of file"+nl+"+two"+nl)
	if got := readRaw(t, root, "gain.txt"); !strings.HasSuffix(got, nl) {
		t.Errorf("gain.txt = %q, want a final newline", got)
	}
}

// TestApplyDiffNothingIsWrittenWhenALaterFileFails restates the all-or-nothing
// promise against the sequential planner.
func TestApplyDiffNothingIsWrittenWhenALaterFileFails(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\n")
	write(t, root, "b.txt", "two\n")

	mustFail(t, s, `--- a/a.txt
+++ b/a.txt
@@ -1 +1 @@
-one
+ONE
--- a/b.txt
+++ b/b.txt
@@ -1 +1 @@
-nonsense
+NONSENSE
`)
	wantFile(t, root, "a.txt", "one\n")
	wantFile(t, root, "b.txt", "two\n")
}

// TestApplyDiffDryRunOnALenientPatch: a dry run of a miscounted patch reports
// the note and writes nothing.
func TestApplyDiffDryRunOnALenientPatch(t *testing.T) {
	s, root := newTestServer(t)
	write(t, root, "a.txt", "one\ntwo\n")

	text := diffText(t, s, map[string]any{"dry_run": true, "diff": `--- a/a.txt
+++ b/a.txt
@@ -1,7 +1,7 @@
 one
-two
+TWO
`})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("dry run failed: %s", text)
	}
	if !strings.Contains(text, "note:") {
		t.Errorf("dry run should report the miscounted header: %s", text)
	}
	wantFile(t, root, "a.txt", "one\ntwo\n")
}

func TestParseUnifiedDiffLenientCounts(t *testing.T) {
	files, err := parseUnifiedDiff("--- a/x"+nl+"+++ b/x"+nl+"@@ -1,5 +1,6 @@"+nl+" a"+nl+"-b"+nl+"+B"+nl, 1)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		t.Fatalf("parsed %#v", files)
	}
	h := files[0].Hunks[0]
	if h.OldCount != 2 || h.NewCount != 2 {
		t.Errorf("counts = %d/%d, want the body's 2/2", h.OldCount, h.NewCount)
	}
	if h.Miscounted == "" {
		t.Error("a corrected header should be recorded as a note")
	}
}

func TestIsSectionBoundary(t *testing.T) {
	lines := []string{
		"@@ -1,2 +1,2 @@", "diff --git a/x b/x", "--- a/x", "+++ b/x",
		"--- a plain removal", " context", "-gone", "+added",
	}
	for _, i := range []int{0, 1, 2} {
		if !isSectionBoundary(lines, i) {
			t.Errorf("lines[%d] = %q should be a boundary", i, lines[i])
		}
	}
	for _, i := range []int{4, 5, 6, 7} {
		if isSectionBoundary(lines, i) {
			t.Errorf("lines[%d] = %q should not be a boundary", i, lines[i])
		}
	}
}

func TestGitHeaderPaths(t *testing.T) {
	cases := []struct{ in, old, fresh string }{
		{"a/x.go b/x.go", "a/x.go", "b/x.go"},
		{"a/sub dir/x.go b/sub dir/x.go", "a/sub dir/x.go", "b/sub dir/x.go"},
		{"a/x.go b/y.go", "a/x.go", "b/y.go"},
	}
	for _, c := range cases {
		old, fresh, ok := gitHeaderPaths(c.in)
		if !ok || old != c.old || fresh != c.fresh {
			t.Errorf("gitHeaderPaths(%q) = %q, %q, %v", c.in, old, fresh, ok)
		}
	}
	if _, _, ok := gitHeaderPaths("nonsense"); ok {
		t.Error("a line with one path should not parse")
	}
}

func TestSortHunksIsStable(t *testing.T) {
	in := []hunk{{OldStart: 10, Header: "b"}, {OldStart: 1, Header: "a"}, {OldStart: 10, Header: "c"}}
	var order string
	for _, h := range sortHunks(in) {
		order += h.Header
	}
	if order != "abc" {
		t.Errorf("order = %q, want %q", order, "abc")
	}
	if in[0].Header != "b" {
		t.Error("sortHunks modified its input")
	}
}
