package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests are a small polyglot repository: one file per language, each
// with the definition being looked for, a call to it, a mention of it in a
// comment and in a string, and something else with a similar name. What the
// tools have to do is tell those apart.

func definitionServer(t *testing.T) (*Server, string) {
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
	return s, root
}

// findDefs calls a tool and returns the decoded structured content.
func findDefs(t *testing.T, s *Server, tool string, args map[string]any) definitionResult {
	t.Helper()
	got := call(t, s, "tools/call", map[string]any{"name": tool, "arguments": args})
	if got["isError"] == true {
		t.Fatalf("%s failed: %v", tool, got["content"])
	}
	encoded, err := json.Marshal(got["structuredContent"])
	if err != nil {
		t.Fatal(err)
	}
	var result definitionResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func onlyDefinition(t *testing.T, result definitionResult) definition {
	t.Helper()
	if len(result.Definitions) != 1 {
		t.Fatalf("want exactly one definition, got %d: %+v", len(result.Definitions), result.Definitions)
	}
	return result.Definitions[0]
}

func TestFindMethodDefinitionInGo(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "svc/handler.go", `package svc

import "fmt"

// Serve handles one request.
// It is the entry point for the whole package.
func (h *Handler) Serve(req string) error {
	if req == "" {
		return fmt.Errorf("empty")
	}
	return h.serveInner(req)
}

func other() {
	h.Serve("x") // Serve is called here, not defined
}
`)

	result := findDefs(t, s, "find_method_definition", map[string]any{"name": "Serve"})
	def := onlyDefinition(t, result)

	if def.Language != "go" || def.Path != "svc/handler.go" {
		t.Errorf("wrong file or language: %+v", def)
	}
	if def.StartLine != 5 || def.DefinitionLine != 7 || def.EndLine != 12 {
		t.Errorf("lines = %d/%d-%d, want the comment at 5, the func at 7 and the brace at 12",
			def.StartLine, def.DefinitionLine, def.EndLine)
	}
	if def.TotalLines != 8 {
		t.Errorf("total_lines = %d, want 8", def.TotalLines)
	}
	if !strings.HasPrefix(def.Body, "// Serve handles one request.") {
		t.Errorf("the body should start at the doc comment: %q", def.Body)
	}
	if !strings.HasSuffix(strings.TrimSpace(def.Body), "}") {
		t.Errorf("the body should end at the closing brace: %q", def.Body)
	}
	if !def.HasBody {
		t.Error("a Go func has a body")
	}
}

func TestFindMethodDefinitionAcrossLanguages(t *testing.T) {
	s, root := definitionServer(t)

	write(t, root, "a.c", `/* compute adds up the input. */
int compute(int a, int b) {
    return a + b;
}

int caller(void) { return compute(1, 2); }
`)
	write(t, root, "b.cpp", `// compute in a class.
class Widget {
public:
    int compute(int a) {
        return a * 2;
    }
};
`)
	write(t, root, "C.cs", `namespace N {
  public class Service {
    /// <summary>compute does the work.</summary>
    [Obsolete]
    public int compute(int a) {
      return a;
    }
  }
}
`)
	write(t, root, "D.java", `class Service {
    /** compute does the work. */
    @Override
    public int compute(int a) {
        return a;
    }
}
`)
	write(t, root, "e.js", `// compute adds.
function compute(a, b) {
  return a + b;
}
const other = () => compute(1, 2);
`)
	write(t, root, "f.ts", `export function compute(a: number): number {
  return a;
}
`)
	write(t, root, "g.rs", `/// compute adds.
#[inline]
pub fn compute(a: i32) -> i32 {
    a + 1
}
`)
	write(t, root, "h.py", `# compute adds.
@cache
def compute(a):
    total = a + 1
    return total

print(compute(1))
`)
	write(t, root, "i.rb", `# compute adds.
def compute(a)
  a + 1
end

puts compute(1)
`)
	write(t, root, "j.php", `<?php
class Service {
    public function compute($a) {
        return $a;
    }
}
`)
	write(t, root, "k.swift", `// compute adds.
func compute(a: Int) -> Int {
    return a + 1
}
`)
	write(t, root, "l.kt", `// compute adds.
fun compute(a: Int): Int {
    return a + 1
}
`)

	result := findDefs(t, s, "find_method_definition", map[string]any{"name": "compute"})
	byLanguage := map[string]definition{}
	for _, def := range result.Definitions {
		byLanguage[def.Language] = def
	}
	for _, want := range []string{"c", "cpp", "csharp", "java", "javascript", "typescript",
		"rust", "python", "ruby", "php", "swift", "kotlin"} {
		def, ok := byLanguage[want]
		if !ok {
			t.Errorf("no definition found in %s", want)
			continue
		}
		if def.TotalLines < 3 {
			t.Errorf("%s: total_lines = %d, want the whole definition: %q", want, def.TotalLines, def.Body)
		}
		if !strings.Contains(def.Body, "compute") {
			t.Errorf("%s: the body does not contain the definition: %q", want, def.Body)
		}
		if !def.HasBody {
			t.Errorf("%s: this definition has a body", want)
		}
	}
}

// TestDefinitionBodyEndsWithTheBlock: the end line is the close of the
// definition, not the end of the file or the next thing in it.
func TestDefinitionBodyEndsWithTheBlock(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "x.py", `def target(a):
    if a:
        return 1
    return 0

def after():
    return target(1)
`)

	def := onlyDefinition(t, findDefs(t, s, "find_method_definition", map[string]any{"name": "target"}))
	if def.DefinitionLine != 1 || def.EndLine != 4 {
		t.Errorf("lines = %d-%d, want 1-4: %q", def.DefinitionLine, def.EndLine, def.Body)
	}
	if strings.Contains(def.Body, "def after") {
		t.Errorf("the body ran into the next definition: %q", def.Body)
	}
}

func TestRubyDefinitionEndsAtItsEnd(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "x.rb", `class Thing
  def target(a)
    if a
      puts "end"
    end
    a
  end

  def after
  end
end
`)

	def := onlyDefinition(t, findDefs(t, s, "find_method_definition", map[string]any{"name": "target"}))
	if def.DefinitionLine != 2 || def.EndLine != 7 {
		t.Errorf("lines = %d-%d, want 2-7: %q", def.DefinitionLine, def.EndLine, def.Body)
	}
}

// TestDefinitionIgnoresCommentsAndStrings is the whole point of stripping the
// source before matching: a name in a comment or a string is not a definition.
func TestDefinitionIgnoresCommentsAndStrings(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "x.go", `package x

// func Ghost() is mentioned here but not defined.
var doc = "func Ghost() {"

func Real() {
	_ = "func Ghost() {"
}
`)

	if result := findDefs(t, s, "find_method_definition", map[string]any{"name": "Ghost"}); result.Count != 0 {
		t.Errorf("a name in a comment or a string is not a definition: %+v", result.Definitions)
	}
	if result := findDefs(t, s, "find_method_definition", map[string]any{"name": "Real"}); result.Count != 1 {
		t.Errorf("the real definition should still be found: %+v", result)
	}
}

func TestDefinitionIgnoresCallSites(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "x.go", `package x

func caller() {
	compute(1)
	x := compute(2)
	_ = x
}

func compute(a int) int { return a }
`)

	def := onlyDefinition(t, findDefs(t, s, "find_method_definition", map[string]any{"name": "compute"}))
	if def.DefinitionLine != 9 {
		t.Errorf("definition_line = %d, want the func at 9", def.DefinitionLine)
	}
}

// TestSeveralDefinitionsAreAllReturned: one name defined in several places is
// the normal case, and the answer is a list.
func TestSeveralDefinitionsAreAllReturned(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "one/a.go", "package one\n\nfunc Handle() {}\n")
	write(t, root, "two/b.go", "package two\n\nfunc Handle() error { return nil }\n")
	write(t, root, "three/c.go", "package three\n\ntype T struct{}\n\nfunc (t T) Handle() {}\n")

	result := findDefs(t, s, "find_method_definition", map[string]any{"name": "Handle"})
	if result.Count != 3 {
		t.Fatalf("count = %d, want 3: %+v", result.Count, result.Definitions)
	}
	paths := map[string]bool{}
	for _, def := range result.Definitions {
		paths[def.Path] = true
	}
	for _, want := range []string{"one/a.go", "two/b.go", "three/c.go"} {
		if !paths[want] {
			t.Errorf("no definition reported in %s", want)
		}
	}
}

// TestDeclarationsWithoutBodiesComeLast: an interface method and a C prototype
// are declarations, and a caller looking for the definition wants the one with
// the code in it first.
func TestDeclarationsWithoutBodiesComeLast(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "api.h", "int compute(int a);\n")
	write(t, root, "api.c", "int compute(int a) {\n    return a;\n}\n")

	result := findDefs(t, s, "find_method_definition", map[string]any{"name": "compute"})
	if result.Count != 2 {
		t.Fatalf("count = %d, want the definition and the prototype: %+v", result.Count, result.Definitions)
	}
	if !result.Definitions[0].HasBody {
		t.Errorf("the definition should come first: %+v", result.Definitions)
	}
	if result.Definitions[1].HasBody {
		t.Errorf("the prototype should be marked as having no body: %+v", result.Definitions[1])
	}
	if result.Definitions[1].EndLine != result.Definitions[1].DefinitionLine {
		t.Errorf("a prototype ends on its own line: %+v", result.Definitions[1])
	}
}

func TestFindClassDefinition(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "a.go", `package a

// Widget does things.
type Widget struct {
	Name string
}
`)
	write(t, root, "b.java", `/** Widget does things. */
public class Widget extends Base implements Thing {
    private int x;
}
`)
	write(t, root, "c.py", `class Widget(Base):
    """Widget does things."""

    def method(self):
        return 1
`)
	write(t, root, "d.ts", `export interface Widget {
  name: string;
}
`)
	write(t, root, "e.rs", `pub struct Widget {
    name: String,
}

impl Widget {
    fn new() -> Self { Widget { name: String::new() } }
}
`)

	result := findDefs(t, s, "find_class_definition", map[string]any{"name": "Widget"})
	byLanguage := map[string][]definition{}
	for _, def := range result.Definitions {
		byLanguage[def.Language] = append(byLanguage[def.Language], def)
	}
	for _, want := range []string{"go", "java", "python", "typescript", "rust"} {
		if len(byLanguage[want]) == 0 {
			t.Errorf("no class definition found in %s: %+v", want, result.Definitions)
		}
	}
	if got := len(byLanguage["rust"]); got != 2 {
		t.Errorf("rust should report the struct and its impl block, got %d", got)
	}
	for _, def := range byLanguage["go"] {
		if !strings.Contains(def.Body, "Name string") {
			t.Errorf("the Go type body should be included: %q", def.Body)
		}
		if def.Kind != "class" {
			t.Errorf("kind = %q, want class", def.Kind)
		}
	}
}

// TestClassAndMethodAreDifferentQuestions: a name that is both should answer
// differently to each tool.
func TestClassAndMethodAreDifferentQuestions(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "a.go", `package a

type Handler struct{}

func Handler2() {}
`)

	classes := findDefs(t, s, "find_class_definition", map[string]any{"name": "Handler"})
	if classes.Count != 1 || classes.Definitions[0].DefinitionLine != 3 {
		t.Errorf("the type should be the class answer: %+v", classes.Definitions)
	}
	methods := findDefs(t, s, "find_method_definition", map[string]any{"name": "Handler"})
	if methods.Count != 0 {
		t.Errorf("there is no method called Handler: %+v", methods.Definitions)
	}
}

func TestDefinitionSearchCanBeNarrowed(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "go/a.go", "package a\n\nfunc target() {}\n")
	write(t, root, "py/b.py", "def target():\n    pass\n")

	if result := findDefs(t, s, "find_method_definition", map[string]any{
		"name": "target", "path": "py",
	}); result.Count != 1 || result.Definitions[0].Language != "python" {
		t.Errorf("path should narrow the search: %+v", result.Definitions)
	}
	if result := findDefs(t, s, "find_method_definition", map[string]any{
		"name": "target", "language": "go",
	}); result.Count != 1 || result.Definitions[0].Language != "go" {
		t.Errorf("language should narrow the search: %+v", result.Definitions)
	}
	// The spellings a caller is likely to use for a language.
	for _, spelling := range []string{"py", "Python", "PY"} {
		if result := findDefs(t, s, "find_method_definition", map[string]any{
			"name": "target", "language": spelling,
		}); result.Count != 1 {
			t.Errorf("language %q should be understood: %+v", spelling, result)
		}
	}
}

func TestDefinitionBodyCanBeLeftOutAndCapped(t *testing.T) {
	s, root := definitionServer(t)
	var b strings.Builder
	b.WriteString("package a\n\nfunc big() {\n")
	for i := range 50 {
		b.WriteString("\tx := ")
		b.WriteString(strings.Repeat("1", i%5+1))
		b.WriteString("\n\t_ = x\n")
	}
	b.WriteString("}\n")
	write(t, root, "a.go", b.String())

	withoutBody := onlyDefinition(t, findDefs(t, s, "find_method_definition", map[string]any{
		"name": "big", "include_body": false,
	}))
	if withoutBody.Body != "" {
		t.Errorf("include_body false should return no body: %q", withoutBody.Body)
	}
	if withoutBody.TotalLines < 100 {
		t.Errorf("the line count should be reported even without the body: %+v", withoutBody)
	}

	capped := onlyDefinition(t, findDefs(t, s, "find_method_definition", map[string]any{
		"name": "big", "max_body_lines": 10,
	}))
	if !capped.Truncated {
		t.Error("a capped body should be marked truncated")
	}
	if lines := strings.Count(capped.Body, "\n") + 1; lines != 10 {
		t.Errorf("body has %d lines, want the cap of 10", lines)
	}
	if capped.TotalLines == 10 {
		t.Error("total_lines should describe the definition, not the truncated body")
	}
}

func TestDefinitionResultLimit(t *testing.T) {
	s, root := definitionServer(t)
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		write(t, root, name+"/x.go", "package "+name+"\n\nfunc target() {}\n")
	}

	result := findDefs(t, s, "find_method_definition", map[string]any{
		"name": "target", "max_results": 2,
	})
	if result.Count != 2 || !result.Truncated {
		t.Errorf("max_results should cap and say so: %+v", result)
	}
}

func TestDefinitionNotFoundSaysWhy(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "a.go", "package a\n\nfunc other() {}\n")

	text := toolText(t, s, "find_method_definition", map[string]any{"name": "missing"})
	if !strings.Contains(text, "No method named") {
		t.Errorf("the answer should say nothing was found: %s", text)
	}
	if !strings.Contains(text, "grep_files") {
		t.Errorf("the answer should offer the fallback: %s", text)
	}
	if strings.HasPrefix(text, "ERROR:") {
		t.Errorf("finding nothing is an answer, not an error: %s", text)
	}
}

func TestDefinitionRequiresAName(t *testing.T) {
	s, _ := definitionServer(t)

	if text := toolText(t, s, "find_method_definition", map[string]any{"name": "  "}); !strings.HasPrefix(text, "ERROR:") {
		t.Errorf("an empty name should be refused: %s", text)
	}
	if text := toolText(t, s, "find_method_definition", map[string]any{
		"name": "x", "language": "cobol",
	}); !strings.Contains(text, "no language called") {
		t.Errorf("an unknown language should say so: %s", text)
	}
}

// TestDefinitionNameIsNotAPattern: the name is quoted before it becomes a
// regexp, so a caller passing metacharacters gets a search for them.
func TestDefinitionNameIsNotAPattern(t *testing.T) {
	s, root := definitionServer(t)
	write(t, root, "a.go", "package a\n\nfunc target() {}\n")

	if result := findDefs(t, s, "find_method_definition", map[string]any{"name": "t.rget"}); result.Count != 0 {
		t.Errorf("a dot should be a dot, not any character: %+v", result.Definitions)
	}
	if result := findDefs(t, s, "find_method_definition", map[string]any{"name": ".*"}); result.Count != 0 {
		t.Errorf("a pattern should match nothing: %+v", result.Definitions)
	}
}

func TestLanguageStripsCodeBeforeMatching(t *testing.T) {
	lang := languageByName("go")
	lines := []string{
		`x := "a string with func Ghost() in it"`,
		`// func Ghost()`,
		"y := `raw func Ghost()`",
		`func Real() {`,
	}
	code := lang.stripCode(lines)
	for i, line := range code[:3] {
		if strings.Contains(line, "Ghost") {
			t.Errorf("line %d still carries the masked name: %q", i, line)
		}
		if len(line) != len([]rune(lines[i])) {
			t.Errorf("line %d changed length, so positions would move: %q", i, line)
		}
	}
	if !strings.Contains(code[3], "func Real() {") {
		t.Errorf("real code should survive: %q", code[3])
	}
}

func TestLanguageForExtension(t *testing.T) {
	cases := map[string]string{
		"a.go": "go", "a.c": "c", "a.hpp": "cpp", "a.cs": "csharp", "a.java": "java",
		"a.js": "javascript", "a.tsx": "typescript", "a.rs": "rust", "a.py": "python",
		"a.rb": "ruby", "a.php": "php", "a.swift": "swift", "a.kt": "kotlin", "a.scala": "scala",
	}
	for path, want := range cases {
		lang := languageFor(path)
		if lang == nil || lang.Name != want {
			t.Errorf("languageFor(%q) = %v, want %s", path, lang, want)
		}
	}
	if languageFor("a.txt") != nil {
		t.Error("a text file has no language here")
	}
	if languageFor("Makefile") != nil {
		t.Error("a file with no extension has no language here")
	}
}
