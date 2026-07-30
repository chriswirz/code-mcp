package main

import (
	"strings"
	"testing"
)

func TestApplyEdit(t *testing.T) {
	const content = "alpha\nbeta\nbeta\ngamma\n"

	t.Run("single occurrence", func(t *testing.T) {
		res, err := applyEdit(content, editOp{Path: "f", OldString: "alpha", NewString: "ALPHA"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Count != 1 {
			t.Fatalf("count = %d, want 1", res.Count)
		}
		if res.Content != "ALPHA\nbeta\nbeta\ngamma\n" {
			t.Fatalf("unexpected result: %q", res.Content)
		}
	})

	t.Run("ambiguous anchor is refused", func(t *testing.T) {
		if _, err := applyEdit(content, editOp{Path: "f", OldString: "beta", NewString: "B"}, true); err == nil {
			t.Fatal("expected an error for an anchor that appears twice")
		}
	})

	t.Run("replace_all", func(t *testing.T) {
		res, err := applyEdit(content, editOp{Path: "f", OldString: "beta", NewString: "B", ReplaceAll: true}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Count != 2 {
			t.Fatalf("count = %d, want 2", res.Count)
		}
		if res.Content != "alpha\nB\nB\ngamma\n" {
			t.Fatalf("unexpected result: %q", res.Content)
		}
	})

	t.Run("missing anchor", func(t *testing.T) {
		if _, err := applyEdit(content, editOp{Path: "f", OldString: "delta", NewString: "D"}, true); err == nil {
			t.Fatal("expected an error for an anchor that does not appear")
		}
	})

	t.Run("empty anchor", func(t *testing.T) {
		if _, err := applyEdit(content, editOp{Path: "f", NewString: "x"}, true); err == nil {
			t.Fatal("expected an error for an empty old_string")
		}
	})
}

func TestApplyEditLineEndings(t *testing.T) {
	const crlf = "alpha\r\nbeta\r\ngamma\r\n"

	t.Run("LF anchor matches a CRLF file", func(t *testing.T) {
		res, err := applyEdit(crlf, editOp{Path: "f", OldString: "alpha\nbeta", NewString: "alpha\nBETA"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !res.Adjusted {
			t.Error("expected the anchor to be reported as re-encoded")
		}
		if res.Content != "alpha\r\nBETA\r\ngamma\r\n" {
			t.Fatalf("replacement did not keep CRLF: %q", res.Content)
		}
	})

	t.Run("CRLF anchor matches an LF file", func(t *testing.T) {
		res, err := applyEdit("alpha\nbeta\n", editOp{Path: "f", OldString: "alpha\r\nbeta", NewString: "alpha\r\nBETA"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Content != "alpha\nBETA\n" {
			t.Fatalf("replacement did not keep LF: %q", res.Content)
		}
	})

	t.Run("disabled by default off", func(t *testing.T) {
		_, err := applyEdit(crlf, editOp{Path: "f", OldString: "alpha\nbeta", NewString: "x"}, false)
		if err == nil {
			t.Fatal("expected the mismatched anchor to fail")
		}
		if !strings.Contains(err.Error(), "normalize_line_endings") {
			t.Fatalf("error should point at the option: %v", err)
		}
	})

	t.Run("exact match wins over normalisation", func(t *testing.T) {
		// Deliberately converting one CRLF line to LF must still work.
		res, err := applyEdit(crlf, editOp{Path: "f", OldString: "beta\r\n", NewString: "beta\n"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Adjusted {
			t.Error("an exact match should not report an adjustment")
		}
		if res.Content != "alpha\r\nbeta\ngamma\r\n" {
			t.Fatalf("deliberate line-ending change was undone: %q", res.Content)
		}
	})

	t.Run("dominant ending", func(t *testing.T) {
		if got := dominantLineEnding("a\r\nb\r\nc\n"); got != "\r\n" {
			t.Errorf("got %q, want CRLF", got)
		}
		if got := dominantLineEnding("a\nb\nc\r\n"); got != "\n" {
			t.Errorf("got %q, want LF", got)
		}
		if got := dominantLineEnding("no newlines here"); got != "\n" {
			t.Errorf("got %q, want LF for a file with no newlines", got)
		}
	})
}

func TestApplyEditCamelCaseAliases(t *testing.T) {
	const content = "alpha\nbeta\n"

	t.Run("oldText and newText stand in", func(t *testing.T) {
		res, err := applyEdit(content, editOp{Path: "f", OldText: "alpha", NewText: "ALPHA"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Content != "ALPHA\nbeta\n" {
			t.Fatalf("unexpected result: %q", res.Content)
		}
	})

	t.Run("snake_case wins when both are given", func(t *testing.T) {
		op := editOp{Path: "f", OldString: "alpha", NewString: "A", OldText: "beta", NewText: "B"}
		res, err := applyEdit(content, op, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Content != "A\nbeta\n" {
			t.Fatalf("alias should not override old_string: %q", res.Content)
		}
	})

	t.Run("aliases still normalise line endings", func(t *testing.T) {
		res, err := applyEdit("alpha\r\nbeta\r\n", editOp{Path: "f", OldText: "alpha\nbeta", NewText: "alpha\nBETA"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Content != "alpha\r\nBETA\r\n" {
			t.Fatalf("unexpected result: %q", res.Content)
		}
	})

	t.Run("anchor resolution", func(t *testing.T) {
		cases := []struct {
			name         string
			op           editOp
			wantOldValue string
			wantNewValue string
		}{
			{"snake_case only", editOp{OldString: "a", NewString: "b"}, "a", "b"},
			{"camelCase only", editOp{OldText: "a", NewText: "b"}, "a", "b"},
			{"snake_case wins", editOp{OldString: "a", NewString: "b", OldText: "x", NewText: "y"}, "a", "b"},
			{"mixed spellings", editOp{OldString: "a", NewText: "b"}, "a", "b"},
			{"neither", editOp{}, "", ""},
		}
		for _, tc := range cases {
			gotOld, gotNew := tc.op.anchor()
			if gotOld != tc.wantOldValue || gotNew != tc.wantNewValue {
				t.Errorf("%s: got (%q, %q), want (%q, %q)", tc.name, gotOld, gotNew, tc.wantOldValue, tc.wantNewValue)
			}
		}
	})

	t.Run("deleting text with an empty replacement", func(t *testing.T) {
		// new_string empty must mean "delete", not "fall back to newText".
		res, err := applyEdit("keep\ndrop\n", editOp{Path: "f", OldString: "drop\n"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Content != "keep\n" {
			t.Fatalf("unexpected result: %q", res.Content)
		}
	})
}

func TestChangedHunk(t *testing.T) {
	t.Run("no change", func(t *testing.T) {
		if got := changedHunk("f", "same", "same"); !strings.Contains(got, "no change") {
			t.Fatalf("expected a no-change report, got %q", got)
		}
	})

	t.Run("marks added and removed lines", func(t *testing.T) {
		before := "one\ntwo\nthree\nfour\nfive\n"
		after := "one\ntwo\nTHREE\nfour\nfive\n"
		got := changedHunk("f", before, after)
		if !strings.Contains(got, "-three") {
			t.Errorf("missing removed line in:\n%s", got)
		}
		if !strings.Contains(got, "+THREE") {
			t.Errorf("missing added line in:\n%s", got)
		}
		if !strings.Contains(got, " two") {
			t.Errorf("missing leading context in:\n%s", got)
		}
		if !strings.Contains(got, "@@") {
			t.Errorf("missing hunk header in:\n%s", got)
		}
	})

	t.Run("insertion at the end of file", func(t *testing.T) {
		got := changedHunk("f", "one\n", "one\ntwo\n")
		if !strings.Contains(got, "+two") {
			t.Fatalf("missing added line in:\n%s", got)
		}
	})

	t.Run("caps very large changes", func(t *testing.T) {
		before := "head\n"
		after := "head\n" + strings.Repeat("filler\n", maxHunkLines*2)
		got := changedHunk("f", before, after)
		if !strings.Contains(got, "more line(s) not shown") {
			t.Fatalf("expected the hunk to be truncated:\n%s", got)
		}
		if lines := strings.Count(got, "\n"); lines > maxHunkLines+5 {
			t.Fatalf("hunk was not capped: %d lines", lines)
		}
	})
}

func TestCommandLinePlaceholder(t *testing.T) {
	cmd := CommandConfig{Command: "go test {{args}}", AcceptsArgs: true, DefaultArgs: "./..."}

	if got := commandLine(cmd, "-run TestFoo ./pkg/..."); got != "go test -run TestFoo ./pkg/..." {
		t.Errorf("substitution: got %q", got)
	}
	if got := commandLine(cmd, ""); got != "go test ./..." {
		t.Errorf("default args: got %q", got)
	}
	if got := commandDisplay(cmd); got != "go test {{args}}" {
		t.Errorf("display should keep the placeholder: got %q", got)
	}

	noDefault := CommandConfig{Command: "gofmt -l -w {{args}}", AcceptsArgs: true}
	if got := commandLine(noDefault, ""); got != "gofmt -l -w" {
		t.Errorf("empty placeholder should leave no stray space: got %q", got)
	}

	appended := CommandConfig{Command: "go build", Args: []string{"./..."}, AcceptsArgs: true}
	if got := commandLine(appended, "-v"); got != "go build ./... -v" {
		t.Errorf("commands without a placeholder should still append: got %q", got)
	}
}
