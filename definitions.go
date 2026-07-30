package main

import (
	"io/fs"
	"os"
	"regexp"
	"strings"
)

// definitionKind is what is being looked for.
type definitionKind string

const (
	methodDefinition definitionKind = "method"
	classDefinition  definitionKind = "class"
)

// definition is one place something is defined.
type definition struct {
	Path     string `json:"path"`
	Language string `json:"language"`
	Name     string `json:"name"`
	// Kind is "method" or "class", and Declaration is the line that declares
	// it, so a caller can tell a Go method from the type it hangs off without
	// reading the body.
	Kind        string `json:"kind"`
	Declaration string `json:"declaration"`

	// StartLine is the first line returned, which is the first line of the
	// doc comment when there is one. DefinitionLine is the declaration
	// itself, for a caller that wants to point at it rather than at its
	// comment. Both are 1-based and inclusive, as EndLine is.
	StartLine      int `json:"start_line"`
	DefinitionLine int `json:"definition_line"`
	EndLine        int `json:"end_line"`
	TotalLines     int `json:"total_lines"`

	// HasBody distinguishes a definition from a declaration without one: a C
	// prototype, an interface method, an abstract member.
	HasBody bool `json:"has_body"`
	// Truncated marks a body cut short by max_body_lines.
	Truncated bool   `json:"truncated,omitempty"`
	Body      string `json:"body,omitempty"`
}

// definitionQuery is one search.
type definitionQuery struct {
	Name         string
	Kind         definitionKind
	Path         string
	Language     *language
	MaxResults   int
	IncludeBody  bool
	MaxBodyLines int
}

// findDefinitions walks the workspace and returns every place the name is
// defined, best first: a definition with a body before a declaration without
// one, and then in path order so the answer is stable between calls.
func findDefinitions(ws *Workspace, q definitionQuery) ([]definition, bool, error) {
	target, err := ws.Resolve(q.Path)
	if err != nil {
		return nil, false, err
	}

	var found []definition
	truncated := false

	consider := func(path string) bool {
		lang := languageFor(path)
		if lang == nil || (q.Language != nil && lang != q.Language) {
			return true
		}
		info, statErr := os.Stat(path)
		if statErr != nil || info.Size() > ws.MaxFileBytes {
			return true
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || isBinary(data) {
			return true
		}
		matches, matchErr := definitionsIn(lang, ws.Rel(path), ws.ToLF(string(data)), q)
		if matchErr != nil {
			return true
		}
		found = append(found, matches...)
		if q.MaxResults > 0 && len(found) >= q.MaxResults {
			truncated = true
			return false
		}
		return true
	}

	info, statErr := os.Stat(target)
	if statErr != nil {
		return nil, false, statErr
	}
	if info.IsDir() {
		walkErr := ws.Walk(target, func(path string, d fs.DirEntry) bool {
			return consider(path)
		})
		if walkErr != nil {
			return nil, false, walkErr
		}
	} else {
		consider(target)
	}

	sortDefinitions(found)
	if q.MaxResults > 0 && len(found) > q.MaxResults {
		found = found[:q.MaxResults]
		truncated = true
	}
	return found, truncated, nil
}

// sortDefinitions puts the answer in the order a reader wants it: real
// definitions first, then by path and line.
func sortDefinitions(defs []definition) {
	sortSlice(defs, func(a, b definition) bool {
		if a.HasBody != b.HasBody {
			return a.HasBody
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.DefinitionLine < b.DefinitionLine
	})
}

// sortSlice is a small insertion sort, which is the right shape for the tens
// of results this produces and avoids a closure over an index.
func sortSlice[T any](items []T, less func(a, b T) bool) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && less(items[j], items[j-1]); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

// definitionsIn finds every definition of the name in one file.
func definitionsIn(lang *language, rel, content string, q definitionQuery) ([]definition, error) {
	build := lang.method
	if q.Kind == classDefinition {
		build = lang.class
	}
	patterns, err := compilePatterns(build, q.Name)
	if err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		return nil, nil
	}

	lines, _ := splitLines(content)
	if len(lines) == 0 {
		return nil, nil
	}
	code := lang.stripCode(lines)

	var out []definition
	for i := range lines {
		if !matchesAny(patterns, code[i]) {
			continue
		}
		// A brace-language pattern has no keyword to anchor on, so a call
		// site can match it. The shape of the line tells them apart.
		if lang.Body == braceBody && !plausibleDeclaration(code[i], q.Name) {
			continue
		}
		def := lang.extent(lines, code, i)
		if def == nil {
			continue
		}
		def.Path = rel
		def.Language = lang.Name
		def.Name = q.Name
		def.Kind = string(q.Kind)
		def.Declaration = strings.TrimSpace(lines[i])
		def.TotalLines = def.EndLine - def.StartLine + 1

		if q.IncludeBody {
			body := lines[def.StartLine-1 : def.EndLine]
			if q.MaxBodyLines > 0 && len(body) > q.MaxBodyLines {
				body = body[:q.MaxBodyLines]
				def.Truncated = true
			}
			def.Body = strings.Join(body, "\n")
		}
		out = append(out, *def)
	}
	return out, nil
}

func matchesAny(patterns []*regexp.Regexp, line string) bool {
	for _, pattern := range patterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	return false
}

// extent works out where a definition starts and ends. The start is the first
// line of the doc comment and any annotation above it, because that is what a
// reader of the definition needs; the end depends on how the language marks a
// block.
func (l *language) extent(lines, code []string, decl int) *definition {
	start := l.commentStart(lines, decl)
	end, hasBody := l.bodyEnd(lines, code, decl)
	if end < decl {
		return nil
	}
	return &definition{
		StartLine:      start + 1,
		DefinitionLine: decl + 1,
		EndLine:        end + 1,
		HasBody:        hasBody,
	}
}

// commentStart walks up from a declaration over its doc comment and anything
// attached to it, stopping at the first line that is neither.
func (l *language) commentStart(lines []string, decl int) int {
	start := decl
	for i := decl - 1; i >= 0; i-- {
		if l.isCommentLine(lines[i]) || l.isAttachedLine(lines[i]) {
			start = i
			continue
		}
		break
	}
	return start
}

// bodyEnd finds the last line of a definition, and whether it had a body at
// all. A declaration without one - a prototype, an interface method, an
// abstract member - ends at its semicolon.
func (l *language) bodyEnd(lines, code []string, decl int) (int, bool) {
	switch l.Body {
	case indentBody:
		return l.indentEnd(lines, decl), true
	case endKeywordBody:
		return l.keywordEnd(lines, code, decl), true
	default:
		return l.braceEnd(code, decl)
	}
}

// maxDeclarationLines is how far past the declaration line the opening brace
// may be. A signature broken over several lines is ordinary; a brace twenty
// lines later means the match was not a declaration.
const maxDeclarationLines = 12

// braceEnd matches the braces of a block, ignoring anything the scanner has
// already blanked. It returns the line of the closing brace, or of the
// semicolon when the declaration has no body.
func (l *language) braceEnd(code []string, decl int) (int, bool) {
	depth := 0
	opened := false

	for i := decl; i < len(code); i++ {
		if !opened && i > decl+maxDeclarationLines {
			return decl, false
		}
		for _, r := range code[i] {
			switch r {
			case '{':
				depth++
				opened = true
			case '}':
				if !opened {
					// A closing brace before an opening one: the match was
					// inside something else.
					return decl, false
				}
				depth--
				if depth == 0 {
					return i, true
				}
			case ';':
				if !opened {
					// The declaration ends without a body.
					return i, false
				}
			}
		}
	}
	if opened {
		// An unterminated block: report what there was rather than nothing.
		return len(code) - 1, true
	}
	return decl, false
}

// indentEnd is Python's rule: the block runs to the last line indented further
// than the declaration, with blank lines and comments between them carried
// along only when more code follows.
func (l *language) indentEnd(lines []string, decl int) int {
	base := indentWidth(lines[decl])
	end := decl
	for i := decl + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" {
			continue
		}
		if indentWidth(lines[i]) <= base {
			break
		}
		end = i
	}
	return end
}

// keywordEnd is Ruby's rule, read the way a person reads it: the "end" at the
// declaration's own indentation closes it.
func (l *language) keywordEnd(lines, code []string, decl int) int {
	base := indentWidth(lines[decl])
	for i := decl + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(code[i])
		if trimmed != "end" && !strings.HasPrefix(trimmed, "end ") {
			continue
		}
		if indentWidth(lines[i]) == base {
			return i
		}
	}
	return decl
}

// indentWidth counts leading whitespace with a tab as one level, which is all
// the comparison here needs.
func indentWidth(line string) int {
	width := 0
	for _, r := range line {
		switch r {
		case ' ':
			width++
		case '\t':
			width += 4
		default:
			return width
		}
	}
	return width
}

// definitionExtensions lists the file extensions a search will look at, for an
// error that has to say why nothing was searched.
func definitionExtensions(lang *language) string {
	if lang != nil {
		return strings.Join(lang.Extensions, ", ")
	}
	var all []string
	for _, l := range languages {
		all = append(all, l.Extensions...)
	}
	return strings.Join(all, ", ")
}
