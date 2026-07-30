package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// Finding where something is defined is the question a model asks most often
// about a codebase it has not read, and grep answers it badly: the pattern
// matches the call sites too, and a hit is a line rather than the thing that
// was asked for. This file knows enough about each language's syntax to find a
// declaration and to say where its body ends, without a parser per language
// and without a dependency.
//
// It is a lexical matcher, not a compiler. It reads what a careful person
// reads - the declaration line and the braces or indentation under it - and it
// is wrong in the ways that approach is wrong: a macro that builds a function
// name, a body whose braces are inside a string it does not understand. Every
// result carries its file and lines, so a caller can check it cheaply.

// bodyStyle is how a language marks the extent of a definition.
type bodyStyle int

const (
	// braceBody runs from the opening brace to its match.
	braceBody bodyStyle = iota
	// indentBody runs until a line indented no further than the declaration,
	// which is how Python delimits a block.
	indentBody
	// endKeywordBody runs to the "end" at the declaration's own indentation,
	// which is how Ruby reads to anyone who has not written a Ruby parser.
	endKeywordBody
)

// language describes one language well enough to find definitions in it.
type language struct {
	Name       string
	Extensions []string
	// LineComment and BlockComment are what the scanner blanks before the
	// patterns are applied, so a call inside a comment is not a definition.
	LineComment  []string
	BlockComment [2]string
	// RawStringDelimiters are quotes with no escape processing, such as Go's
	// backtick.
	RawStringDelimiters []string
	Body                bodyStyle
	// Attached are the lines that belong above a declaration and should be
	// returned with it: annotations, attributes, decorators.
	Attached []*regexp.Regexp
	// method and class patterns take the escaped identifier and produce the
	// expressions that find it. They are built per search rather than held as
	// compiled regexps because the name is part of the pattern.
	method func(name string) []string
	class  func(name string) []string
}

// languages is the table, in the order a file extension is matched against it.
var languages = buildLanguages()

func buildLanguages() []*language {
	// A declaration is recognised by the keyword that introduces it wherever
	// the language has one. C and its descendants spell a method as a name, a parameter list, and
	// whatever the author put in front of it. There is no keyword to anchor
	// on, so these patterns are permissive and plausibleDeclaration does the
	// rest: telling a declaration from the call sites that look just like one.
	cLikeMethod := func(name string) []string {
		return []string{
			`^\s*[A-Za-z_~][\w\s\*&<>,:\[\]\.]*?\b` + name + `\s*(?:<[^>()]*>)?\s*\(`,
			// A constructor or a destructor, which carries no return type.
			`^\s*~?` + name + `\s*\(`,
			// An out-of-line member definition.
			`^[\w\s\*&<>,:\[\]]*\b\w+::~?` + name + `\s*\(`,
		}
	}
	cLikeClass := func(keywords string) func(string) []string {
		return func(name string) []string {
			return []string{
				`^\s*(?:template\s*<[^>]*>\s*)?(?:[\w\[\]\(\),\.]+\s+)*?(?:` + keywords + `)\s+(?:\w+\s+)*` + name + `\b`,
			}
		}
	}

	annotation := regexp.MustCompile(`^\s*@[\w.]+`)
	attribute := regexp.MustCompile(`^\s*\[[\w.]+`)
	rustAttribute := regexp.MustCompile(`^\s*#\[`)
	pythonDecorator := regexp.MustCompile(`^\s*@[\w.]+`)

	return []*language{
		{
			Name:                "go",
			Extensions:          []string{".go"},
			LineComment:         []string{"//"},
			BlockComment:        [2]string{"/*", "*/"},
			RawStringDelimiters: []string{"`"},
			Body:                braceBody,
			method: func(name string) []string {
				return []string{`^\s*func\s+(?:\([^)]*\)\s*)?` + name + `\b`}
			},
			class: func(name string) []string {
				return []string{
					`^\s*type\s+` + name + `\b`,
					`^\s*` + name + `\s+(?:struct|interface)\s*\{`, // inside a type ( ... ) block
				}
			},
		},
		{
			Name:         "c",
			Extensions:   []string{".c", ".h"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			method:       cLikeMethod,
			class:        cLikeClass(`struct|union|enum`),
		},
		{
			Name:         "cpp",
			Extensions:   []string{".cpp", ".cc", ".cxx", ".hpp", ".hh", ".hxx", ".ipp"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			method:       cLikeMethod,
			class:        cLikeClass(`class|struct|union|enum(?:\s+class)?`),
		},
		{
			Name:         "csharp",
			Extensions:   []string{".cs"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{attribute},
			method:       cLikeMethod,
			class:        cLikeClass(`class|struct|interface|record|enum`),
		},
		{
			Name:         "java",
			Extensions:   []string{".java"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{annotation},
			method:       cLikeMethod,
			class:        cLikeClass(`class|interface|enum|record|@interface`),
		},
		{
			Name:                "javascript",
			Extensions:          []string{".js", ".jsx", ".mjs", ".cjs"},
			LineComment:         []string{"//"},
			BlockComment:        [2]string{"/*", "*/"},
			RawStringDelimiters: []string{"`"},
			Body:                braceBody,
			Attached:            []*regexp.Regexp{annotation},
			method:              scriptMethod,
			class:               scriptClass,
		},
		{
			Name:                "typescript",
			Extensions:          []string{".ts", ".tsx", ".mts", ".cts"},
			LineComment:         []string{"//"},
			BlockComment:        [2]string{"/*", "*/"},
			RawStringDelimiters: []string{"`"},
			Body:                braceBody,
			Attached:            []*regexp.Regexp{annotation},
			method:              scriptMethod,
			class: func(name string) []string {
				return append(scriptClass(name),
					`^\s*(?:export\s+)?(?:declare\s+)?(?:type|interface|enum|namespace)\s+`+name+`\b`)
			},
		},
		{
			Name:         "rust",
			Extensions:   []string{".rs"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{rustAttribute},
			method: func(name string) []string {
				return []string{`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:default\s+)?(?:const\s+)?(?:async\s+)?(?:unsafe\s+)?(?:extern\s+"[^"]*"\s+)?fn\s+` + name + `\b`}
			},
			class: func(name string) []string {
				return []string{
					`^\s*(?:pub(?:\([^)]*\))?\s+)?(?:struct|enum|trait|union|type)\s+` + name + `\b`,
					`^\s*(?:unsafe\s+)?impl(?:\s*<[^>]*>)?\s+(?:[\w:<>, ]+\s+for\s+)?` + name + `\b`,
				}
			},
		},
		{
			Name:         "python",
			Extensions:   []string{".py", ".pyi"},
			LineComment:  []string{"#"},
			BlockComment: [2]string{"", ""},
			Body:         indentBody,
			Attached:     []*regexp.Regexp{pythonDecorator},
			method: func(name string) []string {
				return []string{`^\s*(?:async\s+)?def\s+` + name + `\b`}
			},
			class: func(name string) []string {
				return []string{`^\s*class\s+` + name + `\b`}
			},
		},
		{
			Name:         "ruby",
			Extensions:   []string{".rb"},
			LineComment:  []string{"#"},
			BlockComment: [2]string{"=begin", "=end"},
			Body:         endKeywordBody,
			method: func(name string) []string {
				return []string{`^\s*def\s+(?:self\.)?` + name + `\b`}
			},
			class: func(name string) []string {
				return []string{`^\s*(?:class|module)\s+` + name + `\b`}
			},
		},
		{
			Name:         "php",
			Extensions:   []string{".php"},
			LineComment:  []string{"//", "#"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{attribute},
			method: func(name string) []string {
				return []string{`^\s*(?:(?:public|protected|private|static|final|abstract)\s+)*function\s+&?` + name + `\s*\(`}
			},
			class: cLikeClass(`class|interface|trait|enum`),
		},
		{
			Name:         "swift",
			Extensions:   []string{".swift"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{annotation},
			method: func(name string) []string {
				return []string{`^\s*(?:(?:public|private|internal|fileprivate|open|static|class|final|override|mutating)\s+)*func\s+` + name + `\b`}
			},
			class: cLikeClass(`class|struct|enum|protocol|extension|actor`),
		},
		{
			Name:         "kotlin",
			Extensions:   []string{".kt", ".kts"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{annotation},
			method: func(name string) []string {
				return []string{`^\s*(?:(?:public|private|protected|internal|open|override|suspend|inline|operator|infix|tailrec|external|final|abstract)\s+)*fun\s+(?:<[^>]*>\s*)?(?:[\w.]+\.)?` + name + `\b`}
			},
			class: cLikeClass(`class|interface|object`),
		},
		{
			Name:         "scala",
			Extensions:   []string{".scala", ".sc"},
			LineComment:  []string{"//"},
			BlockComment: [2]string{"/*", "*/"},
			Body:         braceBody,
			Attached:     []*regexp.Regexp{annotation},
			method: func(name string) []string {
				return []string{`^\s*(?:(?:private|protected|final|override|implicit|lazy|inline)\s+)*def\s+` + name + `\b`}
			},
			class: cLikeClass(`class|trait|object|enum`),
		},
	}
}

// scriptMethod covers the several ways JavaScript and TypeScript spell a
// function: the keyword, a class or object member, and a name bound to an
// arrow function or a function expression.
func scriptMethod(name string) []string {
	prefix := `^\s*(?:export\s+(?:default\s+)?)?`
	return []string{
		prefix + `(?:async\s+)?function\s*\*?\s*` + name + `\b`,
		prefix + `(?:const|let|var)\s+` + name + `\s*[:=][^=]*?(?:\bfunction\b|=>|\()`,
		`^\s*(?:static\s+)?(?:async\s+)?(?:get\s+|set\s+)?\*?\s*` + name + `\s*(?:<[^>()]*>)?\s*\([^;]*$`,
		`^\s*` + name + `\s*:\s*(?:async\s+)?(?:function\b|\()`,
	}
}

// scriptClass covers class declarations and the assignment form.
func scriptClass(name string) []string {
	prefix := `^\s*(?:export\s+(?:default\s+)?)?`
	return []string{
		prefix + `(?:abstract\s+)?class\s+` + name + `\b`,
		prefix + `(?:const|let|var)\s+` + name + `\s*=\s*class\b`,
	}
}

// languageFor returns the language a file is written in, by extension.
func languageFor(path string) *language {
	ext := strings.ToLower(filepath.Ext(path))
	if ext == "" {
		return nil
	}
	for _, lang := range languages {
		for _, candidate := range lang.Extensions {
			if candidate == ext {
				return lang
			}
		}
	}
	return nil
}

// languageByName returns a language by its name, for the language argument.
func languageByName(name string) *language {
	name = strings.ToLower(strings.TrimSpace(name))
	switch name {
	case "":
		return nil
	case "c++", "cxx", "cc":
		name = "cpp"
	case "c#", "cs":
		name = "csharp"
	case "js", "node":
		name = "javascript"
	case "ts":
		name = "typescript"
	case "py":
		name = "python"
	case "rs":
		name = "rust"
	case "kt":
		name = "kotlin"
	case "rb":
		name = "ruby"
	}
	for _, lang := range languages {
		if lang.Name == name {
			return lang
		}
	}
	return nil
}

// languageNames lists what the tools understand, for their descriptions.
func languageNames() []string {
	names := make([]string, 0, len(languages))
	for _, lang := range languages {
		names = append(names, lang.Name)
	}
	return names
}

// stripCode blanks out comments and string contents, line by line, so a
// pattern matches a declaration and not a mention of one. The text keeps its
// length and its line structure, so a match's position still points at the
// real file.
func (l *language) stripCode(lines []string) []string {
	out := make([]string, len(lines))
	inBlockComment := false
	var inRaw string

	for i, line := range lines {
		var b strings.Builder
		runes := []rune(line)
		var quote rune
		escaped := false

		for j := 0; j < len(runes); j++ {
			rest := string(runes[j:])

			switch {
			case inRaw != "":
				if strings.HasPrefix(rest, inRaw) {
					for range len([]rune(inRaw)) {
						b.WriteByte(' ')
					}
					j += len([]rune(inRaw)) - 1
					inRaw = ""
					continue
				}
				b.WriteByte(' ')
				continue

			case inBlockComment:
				if l.BlockComment[1] != "" && strings.HasPrefix(rest, l.BlockComment[1]) {
					for range len([]rune(l.BlockComment[1])) {
						b.WriteByte(' ')
					}
					j += len([]rune(l.BlockComment[1])) - 1
					inBlockComment = false
					continue
				}
				b.WriteByte(' ')
				continue

			case quote != 0:
				// Inside a string: everything is blanked, and an escape takes
				// the next character with it.
				b.WriteByte(' ')
				if escaped {
					escaped = false
				} else if runes[j] == '\\' {
					escaped = true
				} else if runes[j] == quote {
					quote = 0
				}
				continue
			}

			// Outside everything: look for the start of something.
			if l.BlockComment[0] != "" && strings.HasPrefix(rest, l.BlockComment[0]) {
				inBlockComment = true
				for range len([]rune(l.BlockComment[0])) {
					b.WriteByte(' ')
				}
				j += len([]rune(l.BlockComment[0])) - 1
				continue
			}
			if l.startsLineComment(rest) {
				// The rest of the line is a comment.
				for range len(runes) - j {
					b.WriteByte(' ')
				}
				j = len(runes)
				break
			}
			if raw := l.rawDelimiterAt(rest); raw != "" {
				inRaw = raw
				for range len([]rune(raw)) {
					b.WriteByte(' ')
				}
				j += len([]rune(raw)) - 1
				continue
			}
			if runes[j] == '"' || runes[j] == '\'' {
				quote = runes[j]
				b.WriteByte(' ')
				continue
			}
			b.WriteRune(runes[j])
		}
		out[i] = b.String()
	}
	return out
}

func (l *language) startsLineComment(rest string) bool {
	for _, marker := range l.LineComment {
		if strings.HasPrefix(rest, marker) {
			return true
		}
	}
	return false
}

func (l *language) rawDelimiterAt(rest string) string {
	for _, delimiter := range l.RawStringDelimiters {
		if strings.HasPrefix(rest, delimiter) {
			return delimiter
		}
	}
	return ""
}

// isCommentLine reports whether a line is entirely comment or blank, which is
// what the walk above a declaration collects.
func (l *language) isCommentLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	for _, marker := range l.LineComment {
		if strings.HasPrefix(trimmed, marker) {
			return true
		}
	}
	if l.BlockComment[0] == "" {
		return false
	}
	// A line inside or around a block comment, judged loosely: the doc comment
	// above a definition is what this is for, not an exact parse.
	return strings.HasPrefix(trimmed, l.BlockComment[0]) ||
		strings.HasPrefix(trimmed, "*") ||
		strings.HasSuffix(trimmed, l.BlockComment[1])
}

// isAttachedLine reports whether a line above a declaration belongs to it: an
// annotation, an attribute, a decorator.
func (l *language) isAttachedLine(line string) bool {
	for _, pattern := range l.Attached {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

// compile builds the patterns for one search. An identifier is quoted, so a
// name with regexp characters in it cannot become a pattern of its own.
func compilePatterns(build func(string) []string, name string) ([]*regexp.Regexp, error) {
	if build == nil {
		return nil, nil
	}
	quoted := regexp.QuoteMeta(name)
	var out []*regexp.Regexp
	for _, expr := range build(quoted) {
		compiled, err := regexp.Compile(expr)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", expr, err)
		}
		out = append(out, compiled)
	}
	return out, nil
}

// statementKeywords begin a statement, not a declaration. "return compute(1);"
// is the shape a permissive C-like pattern matches by mistake, and the first
// word is what gives it away.
var statementKeywords = map[string]bool{
	"return": true, "if": true, "else": true, "for": true, "while": true, "do": true,
	"switch": true, "case": true, "throw": true, "new": true, "delete": true,
	"await": true, "yield": true, "goto": true, "assert": true, "print": true,
	"co_return": true, "co_await": true, "co_yield": true, "using": true,
	"typedef": true, "import": true, "package": true, "namespace": true,
	"catch": true, "try": true, "with": true, "lock": true, "foreach": true,
	"defer": true, "go": true, "select": true, "match": true, "when": true,
}

// plausibleDeclaration rejects the lines a permissive pattern matches that are
// not declarations at all. It is the price of having no parser, and it is
// cheap: a call statement and a declaration differ in ways that are visible
// without knowing the language's grammar.
func plausibleDeclaration(stripped, name string) bool {
	trimmed := strings.TrimSpace(stripped)
	if trimmed == "" {
		return false
	}
	first, _, _ := strings.Cut(trimmed, " ")
	if statementKeywords[strings.TrimSuffix(first, "(")] {
		return false
	}
	// A line that opens with the name and ends in a semicolon is a call. A
	// constructor opens with the name too, but ends in a brace, in a colon for
	// its initialiser list, or in the parameter list it continues on to.
	if strings.HasPrefix(trimmed, name) && strings.HasSuffix(trimmed, ";") {
		return false
	}
	// An assignment declares the thing on the left, not the call on the right.
	if before, _, found := strings.Cut(trimmed, name); found {
		if strings.ContainsAny(before, "=+-/%!?|^") {
			return false
		}
	}
	return true
}
