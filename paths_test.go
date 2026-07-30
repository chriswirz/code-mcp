package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A wrong path is the commonest way a call fails, and the answer decides
// whether the caller's next move is the right file or another guess. These
// tests are about that answer.

// treeServer builds a workspace with a shape a path is easy to get wrong in,
// with the whole tool set registered: the suggestion has to reach every tool
// that reads or changes a file, not only the ones a smaller fixture wires up.
func treeServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.cfg = cfg
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerAll(cfg)

	for _, name := range []string{
		"main.go",
		"README.md",
		"internal/server/handler.go",
		"internal/server/handler_test.go",
		"internal/store/store.go",
		"cmd/codemcp/main.go",
		"docs/guide.md",
	} {
		write(t, root, name, "package main\n")
	}
	return s, root
}

func TestReadFileSuggestsTheRightPath(t *testing.T) {
	s, _ := treeServer(t)

	// main.go exists twice in this tree, so there is nothing to correct to and
	// the answer is the suggestions.
	text := toolText(t, s, "read_file", map[string]any{"path": "internal/main.go"})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("a missing file should be an error: %s", text)
	}
	if !strings.Contains(text, "path parameter was specified incorrectly") {
		t.Errorf("the error should name the parameter: %s", text)
	}
	if !strings.Contains(text, "cmd/codemcp/main.go") || !strings.Contains(text, "main.go") {
		t.Errorf("both candidates should be suggested: %s", text)
	}
}

func TestSuggestionsFindATypo(t *testing.T) {
	s, _ := treeServer(t)

	text := toolText(t, s, "read_file", map[string]any{"path": "mian.go"})
	if !strings.Contains(text, "main.go") {
		t.Errorf("a transposition should still find the file: %s", text)
	}
}

// TestSuggestionsFindAWrongCase goes through SuggestPaths rather than through
// read_file: on a case-insensitive filesystem the read would simply succeed,
// and the ranking is the part that has to hold everywhere.
func TestSuggestionsFindAWrongCase(t *testing.T) {
	s, _ := treeServer(t)

	got := s.workspace().SuggestPaths("readme.md")
	if len(got) == 0 || got[0] != "README.md" {
		t.Errorf("SuggestPaths(\"readme.md\") = %v, want README.md first", got)
	}
}

func TestSuggestionsAreCapped(t *testing.T) {
	s, root := newTestServer(t)
	for _, name := range []string{"a/x.go", "b/x.go", "c/x.go", "d/x.go", "e/x.go", "f/x.go", "g/x.go"} {
		write(t, root, name, "x\n")
	}

	text := toolText(t, s, "read_file", map[string]any{"path": "z/x.go"})
	if n := strings.Count(text, "x.go") - 1; n != defaultPathSuggestions {
		t.Errorf("offered %d suggestions, want the default %d: %s", n, defaultPathSuggestions, text)
	}

	s.workspace().PathSuggestions = 2
	text = toolText(t, s, "read_file", map[string]any{"path": "z/x.go"})
	if n := strings.Count(text, "x.go") - 1; n != 2 {
		t.Errorf("offered %d suggestions, want the configured 2: %s", n, text)
	}
}

func TestSuggestionsCanBeSwitchedOff(t *testing.T) {
	s, _ := treeServer(t)
	s.workspace().PathSuggestions = 0

	text := toolText(t, s, "read_file", map[string]any{"path": "mian.go"})
	if !strings.Contains(text, "path parameter was specified incorrectly") {
		t.Errorf("the error itself stays: %s", text)
	}
	if strings.Contains(text, "Did you mean") {
		t.Errorf("no suggestions were asked for: %s", text)
	}
}

func TestNoSuggestionWhenNothingIsClose(t *testing.T) {
	s, _ := treeServer(t)

	text := toolText(t, s, "read_file", map[string]any{"path": "qqqqzzzz.bin"})
	if strings.Contains(text, "Did you mean") {
		t.Errorf("nothing resembles this, so nothing should be offered: %s", text)
	}
	if !strings.Contains(text, "find_files") {
		t.Errorf("the answer should still say how to look: %s", text)
	}
}

// The suggestion goes to every tool that reads or changes a file, not only to
// read_file, since a model that got the path wrong got it wrong everywhere.
func TestEveryFileToolSuggests(t *testing.T) {
	s, _ := treeServer(t)

	cases := []struct {
		tool string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": "internal/main.go"}},
		{"edit_file", map[string]any{"path": "internal/main.go", "old_string": "package", "new_string": "pkg"}},
		{"format_markdown", map[string]any{"path": "docs/guid.md"}},
		{"fix_line_endings", map[string]any{"path": "internal/main.go", "scope": "file"}},
		{"multi_edit", map[string]any{"edits": []any{
			map[string]any{"path": "internal/main.go", "old_string": "package", "new_string": "pkg"},
		}}},
		{"apply_diff", map[string]any{
			"diff": "--- a/internal/main.go\n+++ b/internal/main.go\n@@ -1 +1 @@\n-package main\n+package other\n",
		}},
	}
	for _, c := range cases {
		text := toolText(t, s, c.tool, c.args)
		if !strings.HasPrefix(text, "ERROR:") {
			t.Errorf("%s: a missing file should be an error: %s", c.tool, text)
			continue
		}
		if !strings.Contains(text, "path parameter was specified incorrectly") {
			t.Errorf("%s should name the parameter: %s", c.tool, text)
		}
		if !strings.Contains(text, "main.go") && !strings.Contains(text, "guide.md") {
			t.Errorf("%s should suggest the real file: %s", c.tool, text)
		}
	}
}

// TestMultiEditSuggestionNamesTheEdit: in a batch, which entry was wrong
// matters as much as what it should have said.
func TestMultiEditSuggestionNamesTheEdit(t *testing.T) {
	s, root := treeServer(t)
	write(t, root, "a.txt", "one\n")

	text := toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "a.txt", "old_string": "one", "new_string": "ONE"},
		map[string]any{"path": "internal/main.go", "old_string": "package", "new_string": "pkg"},
	}})
	if !strings.Contains(text, "edits[1]") {
		t.Errorf("the answer should name the entry that was wrong: %s", text)
	}
	if !strings.Contains(text, "cmd/codemcp/main.go") {
		t.Errorf("the answer should still suggest the real files: %s", text)
	}
	if got := mustRead(t, root, "a.txt"); got != "one\n" {
		t.Errorf("nothing should have been written: a.txt = %q", got)
	}
}

func TestSuggestPathsRanking(t *testing.T) {
	s, _ := treeServer(t)
	ws := s.workspace()

	cases := map[string]string{
		"handler.go":                 "internal/server/handler.go",
		"internal/handler.go":        "internal/server/handler.go",
		"store.go":                   "internal/store/store.go",
		"cmd/main.go":                "cmd/codemcp/main.go",
		"guide.md":                   "docs/guide.md",
		"internal/server/handler.go": "internal/server/handler.go",
	}
	for asked, want := range cases {
		got := ws.SuggestPaths(asked)
		if len(got) == 0 {
			t.Errorf("SuggestPaths(%q) found nothing, want %q first", asked, want)
			continue
		}
		if got[0] != want {
			t.Errorf("SuggestPaths(%q) = %v, want %q first", asked, got, want)
		}
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"main.go", "main.go", 0},
		{"main.go", "mian.go", 2},
		{"handler", "handlers", 1},
		{"kitten", "sitting", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// An unrestricted workspace reaches the whole machine, where a near-miss list
// would be noise rather than help.
func TestNoSuggestionsWhenUnrestricted(t *testing.T) {
	s, _ := treeServer(t)
	ws := s.workspace()
	ws.Unrestricted = true

	if got := ws.SuggestPaths("mian.go"); got != nil {
		t.Errorf("an unrestricted workspace should offer nothing, got %v", got)
	}
}

// One file with exactly that name is not a guess: the caller had the name
// right and the directory wrong, and failing would only produce the same call
// again with the path fixed. With workspace.exact_path_not_required set, the
// operation goes ahead and the answer says which file it went to.

// forgivingServer is treeServer with the setting on.
func forgivingServer(t *testing.T) (*Server, string) {
	t.Helper()
	s, root := treeServer(t)
	s.workspace().ExactPathNotRequired = true
	return s, root
}

func TestReadFileCorrectsAUniqueName(t *testing.T) {
	s, _ := forgivingServer(t)

	text := toolText(t, s, "read_file", map[string]any{"path": "internal/handler.go"})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("a unique name should have been acted on: %s", text)
	}
	if !strings.Contains(text, "internal/server/handler.go") {
		t.Errorf("the answer should name the file it read: %s", text)
	}
	if !strings.Contains(text, "package main") {
		t.Errorf("the file's contents should still come back: %s", text)
	}
}

func TestEditFileCorrectsAUniqueNameAndWritesThere(t *testing.T) {
	s, root := forgivingServer(t)

	text := toolText(t, s, "edit_file", map[string]any{
		"path": "handler.go", "old_string": "package main", "new_string": "package server",
	})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("edit_file should have corrected the path: %s", text)
	}
	if !strings.Contains(text, "internal/server/handler.go") {
		t.Errorf("the answer should name the file it changed: %s", text)
	}
	if got := mustRead(t, root, "internal/server/handler.go"); got != "package server\n" {
		t.Errorf("the real file should have been edited, got %q", got)
	}
	if _, err := readIfExists(root, "handler.go"); err == nil {
		t.Error("nothing should have been created at the path that was asked for")
	}
}

func TestApplyDiffCorrectsAUniqueName(t *testing.T) {
	s, root := forgivingServer(t)

	text := toolText(t, s, "apply_diff", map[string]any{
		"diff": "--- a/store.go\n+++ b/store.go\n@@ -1 +1 @@\n-package main\n+package store\n",
	})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("apply_diff should have corrected the path: %s", text)
	}
	if !strings.Contains(text, "internal/store/store.go") {
		t.Errorf("the answer should name the file it patched: %s", text)
	}
	if got := mustRead(t, root, "internal/store/store.go"); got != "package store\n" {
		t.Errorf("the real file should have been patched, got %q", got)
	}
}

func TestMultiEditCorrectsAUniqueName(t *testing.T) {
	s, root := forgivingServer(t)

	text := toolText(t, s, "multi_edit", map[string]any{"edits": []any{
		map[string]any{"path": "guide.md", "old_string": "package main", "new_string": "# Guide"},
	}})
	if strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("multi_edit should have corrected the path: %s", text)
	}
	if !strings.Contains(text, "docs/guide.md") {
		t.Errorf("the answer should name the file it changed: %s", text)
	}
	if got := mustRead(t, root, "docs/guide.md"); got != "# Guide\n" {
		t.Errorf("the real file should have been edited, got %q", got)
	}
}

// TestAmbiguousNameIsNotCorrected is the other half of the rule: two files of
// that name make it a guess, so nothing is touched.
func TestAmbiguousNameIsNotCorrected(t *testing.T) {
	s, root := forgivingServer(t)

	text := toolText(t, s, "edit_file", map[string]any{
		"path": "internal/main.go", "old_string": "package main", "new_string": "package other",
	})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("an ambiguous name should not be acted on: %s", text)
	}
	if got := mustRead(t, root, "main.go"); got != "package main\n" {
		t.Errorf("main.go was changed: %q", got)
	}
	if got := mustRead(t, root, "cmd/codemcp/main.go"); got != "package main\n" {
		t.Errorf("cmd/codemcp/main.go was changed: %q", got)
	}
}

// TestExistingPathIsNeverCorrected: a path that resolves is used as given,
// whatever else shares its name.
func TestExistingPathIsNeverCorrected(t *testing.T) {
	s, _ := forgivingServer(t)

	text := toolText(t, s, "read_file", map[string]any{"path": "main.go"})
	if strings.Contains(text, "does not exist") {
		t.Errorf("a real path should be read without comment: %s", text)
	}
}

// TestCorrectionIgnoresATypo: the rule is an exact name in the wrong place,
// not a name that is nearly right - correcting a typo would be a guess about
// what was meant, and lands as a write.
func TestCorrectionIgnoresATypo(t *testing.T) {
	s, _ := forgivingServer(t)

	text := toolText(t, s, "read_file", map[string]any{"path": "internal/hadnler.go"})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("a typo should be an error with suggestions, not a correction: %s", text)
	}
	if !strings.Contains(text, "handler.go") {
		t.Errorf("the near miss should still be suggested: %s", text)
	}
}

func TestUniqueByName(t *testing.T) {
	s, _ := treeServer(t)
	ws := s.workspace()

	if got, ok := ws.UniqueByName("anywhere/handler.go"); !ok || got != "internal/server/handler.go" {
		t.Errorf("UniqueByName(handler.go) = %q, %v", got, ok)
	}
	if _, ok := ws.UniqueByName("main.go"); ok {
		t.Error("main.go appears twice and must not be treated as unique")
	}
	if _, ok := ws.UniqueByName("nothing-like-this.go"); ok {
		t.Error("a name that is not there is not unique")
	}
	ws.Unrestricted = true
	if _, ok := ws.UniqueByName("handler.go"); ok {
		t.Error("an unrestricted workspace searches the machine; it must not correct")
	}
}

// readIfExists reports whether a file is there, for the assertions about files
// that should not have been created.
func readIfExists(root, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, name))
	return string(data), err
}

// TestStrictByDefaultNamesTheFile: with the setting off - the default - a
// unique name is not acted on, and the error says which file was meant and
// which setting would have used it.
func TestStrictByDefaultNamesTheFile(t *testing.T) {
	s, root := treeServer(t)
	if s.workspace().ExactPathNotRequired {
		t.Fatal("exact_path_not_required should be off unless configured")
	}

	text := toolText(t, s, "edit_file", map[string]any{
		"path": "handler.go", "old_string": "package main", "new_string": "package server",
	})
	if !strings.HasPrefix(text, "ERROR:") {
		t.Fatalf("strict mode should refuse the call: %s", text)
	}
	if !strings.Contains(text, "internal/server/handler.go") {
		t.Errorf("the error should still name the file that was meant: %s", text)
	}
	if !strings.Contains(text, "exact_path_not_required") {
		t.Errorf("the error should name the setting: %s", text)
	}
	if got := mustRead(t, root, "internal/server/handler.go"); got != "package main\n" {
		t.Errorf("nothing should have been written, got %q", got)
	}
}

// TestExactPathNotRequiredComesFromTheConfig checks the wiring rather than the
// behaviour: a setting that never reaches the workspace is a setting that does
// nothing.
func TestExactPathNotRequiredComesFromTheConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Workspace.ExactPathNotRequired {
		t.Error("the default should be strict")
	}
	cfg.Workspace.ExactPathNotRequired = true
	if !NewWorkspace(cfg.Workspace).ExactPathNotRequired {
		t.Error("the setting did not reach the workspace")
	}
}
