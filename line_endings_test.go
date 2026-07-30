package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func lineEndingWorkspace(t *testing.T, mode string) *Workspace {
	t.Helper()
	return NewWorkspace(WorkspaceConfig{
		Root:         t.TempDir(),
		MaxFileBytes: 1 << 20,
		MaxResults:   100,
		AllowWrite:   true,
		LineEndings:  mode,
	})
}

func TestWorkspaceNormalizesOnRead(t *testing.T) {
	for _, mode := range []string{"lf", "crlf"} {
		ws := lineEndingWorkspace(t, mode)
		// Mixed on disk, including a lone CR, which is the case that would
		// otherwise arrive as one enormous line.
		raw := "one\r\ntwo\nthree\rfour\n"
		if err := os.WriteFile(filepath.Join(ws.Root, "f.txt"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := ws.ReadFile("f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if want := "one\ntwo\nthree\nfour\n"; got != want {
			t.Errorf("%s: read %q, want %q", mode, got, want)
		}
	}
}

func TestWorkspaceNormalizesOnWrite(t *testing.T) {
	cases := map[string]string{
		"lf":   "a\nb\n",
		"crlf": "a\r\nb\r\n",
	}
	for mode, want := range cases {
		ws := lineEndingWorkspace(t, mode)
		// Deliberately mixed input: the file must still come out consistent.
		if _, err := ws.WriteFile("f.txt", "a\r\nb\n"); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(ws.Root, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Errorf("%s: wrote %q, want %q", mode, data, want)
		}
	}
}

func TestWorkspacePreserveLeavesFileAlone(t *testing.T) {
	ws := lineEndingWorkspace(t, "preserve")
	raw := "a\r\nb\n"
	if _, err := ws.WriteFile("f.txt", raw); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ws.Root, "f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Errorf("wrote %q, want it untouched (%q)", data, raw)
	}
	got, err := ws.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != raw {
		t.Errorf("read %q, want it untouched (%q)", got, raw)
	}
}

// The point of the setting for find-and-replace: an anchor written with CRLF
// matches a file stored with LF, and the other way round.
func TestNormalizedEditMatchesEitherConvention(t *testing.T) {
	ws := lineEndingWorkspace(t, "crlf")
	if err := os.WriteFile(filepath.Join(ws.Root, "f.txt"), []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := ws.ReadFile("f.txt")
	if err != nil {
		t.Fatal(err)
	}
	op := editOp{Path: "f.txt", OldString: "alpha\r\nbeta\r\n", NewString: "alpha\r\nBETA\r\n"}
	res, err := applyEdit(content, op.normalizedFor(ws), false)
	if err != nil {
		t.Fatalf("edit with a CRLF anchor against LF content: %v", err)
	}
	if res.Count != 1 {
		t.Fatalf("count = %d, want 1", res.Count)
	}
	if _, err := ws.WriteFile("f.txt", res.Content); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(ws.Root, "f.txt"))
	if want := "alpha\r\nBETA\r\ngamma\r\n"; string(data) != want {
		t.Errorf("file = %q, want %q", data, want)
	}
}

func TestPreserveEditKeepsAnchorExact(t *testing.T) {
	ws := lineEndingWorkspace(t, "preserve")
	op := editOp{Path: "f.txt", OldString: "a\r\nb", NewString: "a\r\nB"}
	if got := op.normalizedFor(ws); got.OldString != "a\r\nb" {
		t.Errorf("preserve rewrote the anchor to %q", got.OldString)
	}
}

func TestConfigLineEndingsNormalize(t *testing.T) {
	native := "lf"
	if runtime.GOOS == "windows" {
		native = "crlf"
	}
	cases := map[string]string{
		"":        "preserve",
		"keep":    "preserve",
		"LF":      "lf",
		"unix":    "lf",
		"CRLF":    "crlf",
		"windows": "crlf",
		"native":  native,
	}
	for in, want := range cases {
		cfg := DefaultConfig()
		cfg.Workspace.LineEndings = in
		if err := cfg.Normalize(t.TempDir()); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if cfg.Workspace.LineEndings != want {
			t.Errorf("%q normalized to %q, want %q", in, cfg.Workspace.LineEndings, want)
		}
	}

	cfg := DefaultConfig()
	cfg.Workspace.LineEndings = "mixed"
	err := cfg.Normalize(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "workspace.line_endings") {
		t.Fatalf("bad value: err = %v, want a workspace.line_endings error", err)
	}
}

func TestLineEndingNoteOnlyWhenNormalizing(t *testing.T) {
	if note := lineEndingWorkspace(t, "preserve").LineEndingNote(); note != "" {
		t.Errorf("preserve should say nothing, got %q", note)
	}
	if note := lineEndingWorkspace(t, "crlf").LineEndingNote(); !strings.Contains(note, "CRLF") {
		t.Errorf("note = %q, want it to name CRLF", note)
	}
}
