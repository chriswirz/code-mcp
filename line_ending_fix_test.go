package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixWorkspace lays out a small tree with mixed line endings:
//
//	a.txt        CRLF
//	b.js         CRLF
//	c.txt        already LF
//	bin.dat      binary, with CRLF bytes in it
//	sub/d.js     CRLF
func fixWorkspace(t *testing.T) *Workspace {
	t.Helper()
	root := t.TempDir()
	write := func(rel string, data []byte) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", []byte("one\r\ntwo\r\n"))
	write("b.js", []byte("let x = 1;\r\n"))
	write("c.txt", []byte("already\nlf\n"))
	write("bin.dat", []byte{0x00, 0x01, '\r', '\n', 0x02})
	write("sub/d.js", []byte("let y = 2;\r\n"))

	return NewWorkspace(WorkspaceConfig{
		Root:               root,
		MaxFileBytes:       1 << 20,
		MaxResults:         100,
		AllowWrite:         true,
		LineEndings:        "preserve",
		MaxLineEndingFiles: 500,
	})
}

func planOf(t *testing.T, ws *Workspace, scope lineEndingScope, rel, ending string, patterns []string) lineEndingPlan {
	t.Helper()
	abs, err := ws.Resolve(rel)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planLineEndings(ws, abs, scope, ending, patterns)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func readFileString(t *testing.T, ws *Workspace, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ws.Root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestFixScopeFile(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeFile, "a.txt", "\n", nil)
	if plan.Changed != 1 || len(plan.Files) != 1 {
		t.Fatalf("plan = %+v, want exactly a.txt", plan)
	}
	if err := rewriteLineEndings(ws, plan.Files[0], "\n"); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, ws, "a.txt"); got != "one\ntwo\n" {
		t.Errorf("a.txt = %q", got)
	}
	// Nothing else was touched.
	if got := readFileString(t, ws, "b.js"); got != "let x = 1;\r\n" {
		t.Errorf("b.js = %q, want it untouched", got)
	}
}

func TestFixScopeFolderIsShallow(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeFolder, ".", "\n", nil)
	for _, f := range plan.Files {
		if f == filepath.FromSlash("sub/d.js") || f == "sub/d.js" {
			t.Fatalf("folder scope reached into a subdirectory: %v", plan.Files)
		}
	}
	if plan.Changed != 2 {
		t.Fatalf("changed = %d (%v), want a.txt and b.js", plan.Changed, plan.Files)
	}
}

func TestFixScopeTreeIsDeep(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeTree, ".", "\n", nil)
	if plan.Changed != 3 {
		t.Fatalf("changed = %d (%v), want a.txt, b.js and sub/d.js", plan.Changed, plan.Files)
	}
}

func TestFixSkipsBinaryAndAlreadyCorrect(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeTree, ".", "\n", nil)
	if len(plan.SkippedBin) != 1 {
		t.Fatalf("skipped_binary = %v, want bin.dat", plan.SkippedBin)
	}
	for _, f := range plan.Files {
		if f == "bin.dat" {
			t.Fatal("a binary file was planned for rewriting")
		}
		if f == "c.txt" {
			t.Fatal("a file that already has the right endings was planned for rewriting")
		}
	}
	// The binary must be byte-for-byte intact after a real run.
	before := readFileString(t, ws, "bin.dat")
	for _, f := range plan.Files {
		if err := rewriteLineEndings(ws, f, "\n"); err != nil {
			t.Fatal(err)
		}
	}
	if after := readFileString(t, ws, "bin.dat"); after != before {
		t.Errorf("bin.dat changed: %q -> %q", before, after)
	}
}

func TestFixHonoursFileMask(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeTree, ".", "\n", []string{"*.js"})
	if plan.Changed != 2 {
		t.Fatalf("changed = %d (%v), want the two .js files", plan.Changed, plan.Files)
	}
	for _, f := range plan.Files {
		if filepath.Ext(f) != ".js" {
			t.Errorf("mask *.js matched %s", f)
		}
	}

	multi := planOf(t, ws, scopeTree, ".", "\n", []string{"*.js", "*.txt"})
	if multi.Changed != 3 {
		t.Fatalf("changed = %d (%v), want the .js and .txt files", multi.Changed, multi.Files)
	}
}

func TestFixToCRLF(t *testing.T) {
	ws := fixWorkspace(t)
	plan := planOf(t, ws, scopeFile, "c.txt", "\r\n", nil)
	if plan.Changed != 1 {
		t.Fatalf("changed = %d, want c.txt converted to CRLF", plan.Changed)
	}
	if err := rewriteLineEndings(ws, "c.txt", "\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := readFileString(t, ws, "c.txt"); got != "already\r\nlf\r\n" {
		t.Errorf("c.txt = %q", got)
	}
}

func TestParseFileMask(t *testing.T) {
	patterns, err := parseFileMask(" *.js , *.ts ,, ")
	if err != nil {
		t.Fatal(err)
	}
	if len(patterns) != 2 || patterns[0] != "*.js" || patterns[1] != "*.ts" {
		t.Fatalf("patterns = %v", patterns)
	}
	if _, err := parseFileMask("["); err == nil {
		t.Fatal("a malformed pattern should be rejected")
	}
	if got := maskMatches(nil, "anything"); !got {
		t.Error("an empty mask should match everything")
	}
	if maskMatches([]string{"*.js"}, "a.txt") {
		t.Error("*.js should not match a.txt")
	}
}

func TestParseLineEndingScopeAndName(t *testing.T) {
	if scope, err := parseLineEndingScope(""); err != nil || scope != scopeFile {
		t.Errorf("empty scope = %q, %v; want the narrowest scope", scope, err)
	}
	if _, err := parseLineEndingScope("everything"); err == nil {
		t.Error("an unknown scope should be rejected")
	}

	crlfWS := NewWorkspace(WorkspaceConfig{Root: t.TempDir(), LineEndings: "crlf"})
	if got, err := parseLineEndingName("", crlfWS); err != nil || got != "\r\n" {
		t.Errorf("empty ending = %q, %v; want the workspace setting", got, err)
	}
	preserveWS := NewWorkspace(WorkspaceConfig{Root: t.TempDir(), LineEndings: "preserve"})
	if got, err := parseLineEndingName("", preserveWS); err != nil || got != nativeLineEnding() {
		t.Errorf("empty ending under preserve = %q, %v; want the platform's", got, err)
	}
	if got, err := parseLineEndingName("lf", crlfWS); err != nil || got != "\n" {
		t.Errorf("explicit lf = %q, %v", got, err)
	}
	if _, err := parseLineEndingName("cr", crlfWS); err == nil {
		t.Error("an unknown ending should be rejected")
	}
}

func TestFixPlanTrimsLongLists(t *testing.T) {
	plan := lineEndingPlan{}
	for i := range maxListedFiles + 7 {
		plan.Files = append(plan.Files, filepath.Join("dir", string(rune('a'+i%26))))
	}
	plan.Changed = len(plan.Files)
	plan.trim()
	if len(plan.Files) != maxListedFiles || plan.Truncated != 7 {
		t.Fatalf("files = %d, not listed = %d", len(plan.Files), plan.Truncated)
	}
	if plan.Changed != maxListedFiles+7 {
		t.Errorf("the count must survive trimming, got %d", plan.Changed)
	}
}

// fixToolServer registers just the line-ending tool over a workspace with the
// given limit, so the handler's own guards can be exercised.
func fixToolServer(t *testing.T, limit int) *Server {
	t.Helper()
	ws := fixWorkspace(t)
	cfg := WorkspaceConfig{
		Root:               ws.Root,
		MaxFileBytes:       ws.MaxFileBytes,
		MaxResults:         ws.MaxResults,
		AllowWrite:         ws.AllowWrite,
		LineEndings:        ws.LineEndings,
		MaxLineEndingFiles: limit,
	}
	s := NewServer("test", "test", "", false)
	s.ws = NewWorkspace(cfg)
	s.registerLineEndingTools(cfg)
	return s
}

func fixCall(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	return call(t, s, "tools/call", map[string]any{"name": "fix_line_endings", "arguments": args})
}

func TestFixToolRefusesOverTheLimit(t *testing.T) {
	s := fixToolServer(t, 2)
	got := fixCall(t, s, map[string]any{"path": ".", "scope": "tree", "ending": "lf"})
	if got["isError"] != true {
		t.Fatalf("three files against a limit of two should be refused, got %v", got)
	}
	// Nothing may have been written.
	data, err := os.ReadFile(filepath.Join(s.ws.Root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\r\ntwo\r\n" {
		t.Errorf("a.txt = %q, want it untouched after a refused run", data)
	}
}

func TestFixToolDryRunWritesNothing(t *testing.T) {
	s := fixToolServer(t, 500)
	got := fixCall(t, s, map[string]any{"path": ".", "scope": "tree", "ending": "lf", "dry_run": true})
	if got["isError"] == true {
		t.Fatalf("dry run failed: %v", got)
	}
	data, err := os.ReadFile(filepath.Join(s.ws.Root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "one\r\ntwo\r\n" {
		t.Errorf("a.txt = %q, want it untouched after a dry run", data)
	}
}

func TestFixToolWritesAndIsIdempotent(t *testing.T) {
	s := fixToolServer(t, 500)
	if got := fixCall(t, s, map[string]any{"path": ".", "scope": "tree", "ending": "lf"}); got["isError"] == true {
		t.Fatalf("run failed: %v", got)
	}
	for _, rel := range []string{"a.txt", "b.js", filepath.Join("sub", "d.js")} {
		data, err := os.ReadFile(filepath.Join(s.ws.Root, rel))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "\r") {
			t.Errorf("%s still has CR: %q", rel, data)
		}
	}
	// A second run has nothing left to do.
	got := fixCall(t, s, map[string]any{"path": ".", "scope": "tree", "ending": "lf"})
	structured, _ := got["structuredContent"].(map[string]any)
	if structured["already_consistent"] != true {
		t.Errorf("second run = %v, want already_consistent", structured)
	}
}

func TestFixToolRefusesWorkspaceScopeWhenUnrestricted(t *testing.T) {
	s := fixToolServer(t, 500)
	s.ws.Unrestricted = true
	got := fixCall(t, s, map[string]any{"scope": "workspace", "ending": "lf"})
	if got["isError"] != true {
		t.Fatalf("workspace scope on an unrestricted workspace should be refused, got %v", got)
	}
}
