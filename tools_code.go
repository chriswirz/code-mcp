package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// registerCodeTools adds the tools that work on code by line and by symbol
// rather than by exact string: line-range editing, insertion, project-wide
// replacement, outlines, references, diagnostics and edit history.
func (s *Server) registerCodeTools() {
	// Read under the lock: a configuration reload can replace the workspace
	// while a request is in flight.
	ws := s.workspace()
	s.registerLineTools(ws)
	s.registerReplaceInFiles(ws)
	s.registerNavigationTools(ws)
	s.registerHistoryTools(ws)
	s.registerDiagnostics(ws)
}

// numberLines renders lines with their 1-based numbers, first being the number
// of the first one, in the "N<tab>text" layout edit_lines expects back.
func numberLines(lines []string, first int) string {
	width := len(strconv.Itoa(first + len(lines) - 1))
	var b strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&b, "%*d\t%s\n", width, first+i, line)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// readFileRange is read_file's reading, shared with read_files: a whole file,
// or a 1-based inclusive range of it, optionally numbered.
func readFileRange(ws *Workspace, path string, start, end int, numbered bool) (text, actual, note string, fail *CallToolResult) {
	actual, note, err := ws.ResolveForRead(path)
	if err != nil {
		return "", "", "", pathToolError(ws, path, err)
	}
	content, err := ws.ReadFile(actual)
	if err != nil {
		return "", "", "", pathToolError(ws, actual, err)
	}
	ranged := start > 0 || end > 0
	if !ranged && !numbered {
		return content, actual, note, nil
	}
	// A file ending in a newline splits with a trailing empty element, which
	// is not a line: counting it makes the file look one line longer than it
	// is and hands back a spurious blank at the end of a range.
	lines, finalNewline := splitLines(content)
	if !ranged && len(lines) == 0 {
		return "", actual, note, nil
	}
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", "", "", toolError("%s has %d lines, start_line %d is past the end", actual, len(lines), start)
	}
	if start > end {
		return "", "", "", toolError("start_line %d is after end_line %d", start, end)
	}
	if numbered {
		return numberLines(lines[start-1:end], start), actual, note, nil
	}
	// The slice keeps a final newline only if the file had one and the range
	// runs to its end, so the text reads like the file does.
	return joinLines(lines[start-1:end], finalNewline && end == len(lines)), actual, note, nil
}

// lineOffsets returns the byte offset each line of content starts at, followed
// by len(content), so line i (0-based) is content[offsets[i]:offsets[i+1]]
// including its terminator. Working in offsets rather than split lines leaves
// every untouched line's ending exactly as it was, even in a mixed file.
func lineOffsets(content string) []int {
	if content == "" {
		return []int{0}
	}
	offsets := []int{0}
	for i := 0; i < len(content); i++ {
		if content[i] == '\n' && i+1 < len(content) {
			offsets = append(offsets, i+1)
		}
	}
	return append(offsets, len(content))
}

// textBlock converts text a caller supplied to the file's line ending, as whole
// lines, with a terminator on the last one when terminate is set.
func textBlock(text, ending string, terminate bool) string {
	text = strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	text = toLineEnding(text, ending)
	if terminate {
		text += ending
	}
	return text
}

// comparableText folds text for a guard comparison: line endings and a final
// newline are not what a caller means when it quotes lines back.
func comparableText(text string) string {
	return strings.TrimSuffix(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
}

func (s *Server) registerLineTools(ws *Workspace) {
	s.RegisterTool(Tool{
		Name:  "read_files",
		Title: "Read several files",
		Description: "Read several workspace files, or ranges of them, in one call. Each entry is a path, or an object with " +
			"path, start_line and end_line. A file that cannot be read is reported in place without failing the others.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"files"}, map[string]any{
			"files": map[string]any{
				"type":        "array",
				"description": "Files to read, in order: a path string, or {path, start_line, end_line}.",
				"minItems":    1,
				"items": map[string]any{
					"type": []string{"string", "object"},
					"properties": map[string]any{
						"path":       prop("string", "File relative to the workspace root."),
						"start_line": prop("integer", "First line to return, 1-based."),
						"end_line":   prop("integer", "Last line to return, inclusive."),
					},
				},
			},
			"line_numbers": propDefault("boolean", "Prefix every line with its 1-based number.", false),
			"preserve":     propDefault("boolean", preserveReadDescription, false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Files       []json.RawMessage `json:"files"`
			LineNumbers bool              `json:"line_numbers"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if len(args.Files) == 0 {
			return toolError("files is required and must not be empty"), nil
		}
		const maxFiles = 50
		if len(args.Files) > maxFiles {
			return toolError("at most %d files may be read in one call; %d were asked for", maxFiles, len(args.Files)), nil
		}
		var report strings.Builder
		for i, entry := range args.Files {
			var item struct {
				Path      string `json:"path"`
				StartLine int    `json:"start_line"`
				EndLine   int    `json:"end_line"`
			}
			if len(entry) > 0 && entry[0] == '"' {
				if err := json.Unmarshal(entry, &item.Path); err != nil {
					return toolError("files[%d]: %v", i, err), nil
				}
			} else if bad := decodeArgs(entry, &item); bad != nil {
				return prefixToolError(bad, fmt.Sprintf("files[%d]", i)), nil
			}
			if i > 0 {
				report.WriteString("\n\n")
			}
			if item.Path == "" {
				fmt.Fprintf(&report, "==> files[%d] <==\nERROR: path is required", i)
				continue
			}
			text, actual, note, fail := readFileRange(ws, item.Path, item.StartLine, item.EndLine, args.LineNumbers)
			if fail != nil {
				fmt.Fprintf(&report, "==> %s <==\nERROR: %s", item.Path, resultText(fail))
				continue
			}
			header := ws.Canonical(actual)
			if item.StartLine > 0 || item.EndLine > 0 {
				header += fmt.Sprintf(" (lines %d-%d)", max(item.StartLine, 1), item.EndLine)
			}
			fmt.Fprintf(&report, "==> %s <==\n", header)
			if note != "" {
				report.WriteString(note + "\n")
			}
			report.WriteString(text)
		}
		return toolResult(report.String()), nil
	})

	s.RegisterTool(Tool{
		Name:  "edit_lines",
		Title: "Replace a range of lines",
		Description: "Replace lines start_line through end_line (1-based, inclusive) of a workspace file with new_text. " +
			"Read the file with read_file and line_numbers first. Pass expected_text - the current text of those lines - " +
			"so an edit made against a stale read is refused instead of landing on the wrong lines. " +
			"Every other line, including its line ending, is left exactly as it was. " +
			"To add lines without replacing any, use insert_text; to delete, use remove_sections.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"path", "start_line", "new_text"}, map[string]any{
			"path":       prop("string", "File relative to the workspace root."),
			"start_line": prop("integer", "First line to replace, 1-based."),
			"end_line":   prop("integer", "Last line to replace, inclusive. Defaults to start_line."),
			"new_text":   prop("string", "The lines to put in their place. A trailing newline is optional."),
			"expected_text": prop("string", "The current text of the lines being replaced, without line numbers. "+
				"When given, the edit is refused unless it matches."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path         string  `json:"path"`
			StartLine    int     `json:"start_line"`
			EndLine      int     `json:"end_line"`
			NewText      *string `json:"new_text"`
			ExpectedText *string `json:"expected_text"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		if args.NewText == nil {
			return toolError("new_text is required; to delete lines use remove_sections (nothing was written)"), nil
		}
		actual, note, err := ws.ResolveForRead(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}
		content, err := ws.ReadFile(actual)
		if err != nil {
			return pathToolError(ws, actual, err), nil
		}
		name := ws.Canonical(actual)
		offsets := lineOffsets(content)
		total := len(offsets) - 1
		if args.EndLine == 0 {
			args.EndLine = args.StartLine
		}
		if args.StartLine < 1 || args.StartLine > total {
			return toolError("%s has %d lines; start_line %d is out of range (nothing was written)", name, total, args.StartLine), nil
		}
		if args.EndLine < args.StartLine || args.EndLine > total {
			return toolError("%s has %d lines; end_line %d is out of range for start_line %d (nothing was written)",
				name, total, args.EndLine, args.StartLine), nil
		}
		current := content[offsets[args.StartLine-1]:offsets[args.EndLine]]
		if args.ExpectedText != nil && comparableText(current) != comparableText(*args.ExpectedText) {
			lines, _ := splitLines(current)
			return toolError("lines %d-%d of %s do not match expected_text; the file has changed since it was read. "+
				"They are now:\n%s\n(nothing was written)", args.StartLine, args.EndLine, name, numberLines(lines, args.StartLine)), nil
		}
		replacementText := ""
		if *args.NewText != "" {
			replacementText = textBlock(*args.NewText, dominantLineEnding(content), strings.HasSuffix(current, "\n"))
		}
		updated := content[:offsets[args.StartLine-1]] + replacementText + content[offsets[args.EndLine]:]
		if _, err := ws.WriteFile(actual, updated); err != nil {
			return toolError("%v", err), nil
		}
		result := withNote(toolResult(fmt.Sprintf("Replaced lines %d-%d of %s\n%s",
			args.StartLine, args.EndLine, name, changedHunk(name, content, updated))), note)
		if *args.NewText == "" {
			result = withNamingWarning(result, removalWarning)
		}
		return result, nil
	})

	s.RegisterTool(Tool{
		Name:  "insert_text",
		Title: "Insert lines",
		Description: "Insert whole lines into a workspace file without replacing anything. Give either anchor - text that " +
			"appears exactly once - with position before (the line where the anchor starts) or after (the line where it ends), " +
			"or line, a 1-based line number to insert before (one past the last line appends). " +
			"Prefer this to an edit that repeats its anchor in new_string, which is where anchors get mangled.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"path", "text"}, map[string]any{
			"path":     prop("string", "File relative to the workspace root."),
			"text":     prop("string", "The lines to insert. A trailing newline is optional."),
			"anchor":   prop("string", "Exact text that appears once in the file. Use this or line."),
			"position": propDefault("string", "Insert before or after the anchor's line(s): \"before\" or \"after\".", "after"),
			"line":     prop("integer", "Insert before this 1-based line. Use this or anchor."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path     string `json:"path"`
			Text     string `json:"text"`
			Anchor   string `json:"anchor"`
			Position string `json:"position"`
			Line     int    `json:"line"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" || args.Text == "" {
			return toolError("path and text are required"), nil
		}
		if (args.Anchor == "") == (args.Line == 0) {
			return toolError("give exactly one of anchor or line (nothing was written)"), nil
		}
		actual, note, err := ws.ResolveForRead(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}
		content, err := ws.ReadFile(actual)
		if err != nil {
			return pathToolError(ws, actual, err), nil
		}
		name := ws.Canonical(actual)
		ending := dominantLineEnding(content)

		var point int
		if args.Line != 0 {
			offsets := lineOffsets(content)
			total := len(offsets) - 1
			if args.Line < 1 || args.Line > total+1 {
				return toolError("%s has %d lines; line must be between 1 and %d (nothing was written)", name, total, total+1), nil
			}
			point = offsets[args.Line-1]
			if args.Line == total+1 {
				point = len(content)
			}
		} else {
			anchor := args.Anchor
			count := strings.Count(content, anchor)
			if count == 0 {
				if candidate := toLineEnding(anchor, ending); candidate != anchor {
					anchor, count = candidate, strings.Count(content, candidate)
				}
			}
			switch {
			case count == 0:
				return toolError("anchor does not appear in %s (nothing was written)", name), nil
			case count > 1:
				return toolError("anchor appears %d times in %s; add surrounding text so it is unique, or use line (nothing was written)", count, name), nil
			}
			idx := strings.Index(content, anchor)
			switch strings.ToLower(args.Position) {
			case "before":
				point = strings.LastIndex(content[:idx], "\n") + 1
			case "", "after":
				end := idx + len(anchor)
				if strings.HasSuffix(anchor, "\n") {
					point = end
				} else if nl := strings.IndexByte(content[end:], '\n'); nl >= 0 {
					point = end + nl + 1
				} else {
					point = len(content)
				}
			default:
				return toolError("position must be \"before\" or \"after\", not %q", args.Position), nil
			}
		}

		block := textBlock(args.Text, ending, true)
		if point == len(content) && content != "" && !strings.HasSuffix(content, "\n") {
			// Appending to a file with no final newline: break the last line,
			// and leave the file as unterminated as it was.
			block = ending + strings.TrimSuffix(block, ending)
		}
		updated := content[:point] + block + content[point:]
		if _, err := ws.WriteFile(actual, updated); err != nil {
			return toolError("%v", err), nil
		}
		at := strings.Count(content[:point], "\n") + 1
		return withNote(toolResult(fmt.Sprintf("Inserted %d line(s) at line %d of %s\n%s",
			strings.Count(strings.TrimSuffix(block, ending), "\n")+1, at, name, changedHunk(name, content, updated))), note), nil
	})
}

func (s *Server) registerReplaceInFiles(ws *Workspace) {
	s.RegisterTool(Tool{
		Name:  "replace_in_files",
		Title: "Replace across files",
		Description: "Replace every match of a pattern in every file under a path, such as renaming an identifier across a project. " +
			"All files are staged before any is written, and dry_run previews the diff. Each written file can be undone with rollback. " +
			"For a regular expression, $1 or ${name} in replacement refers to a capture group.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"pattern", "replacement"}, map[string]any{
			"pattern":     prop("string", "Go (RE2) regular expression, or literal text when literal is set."),
			"replacement": prop("string", "Replacement text. Empty deletes the matches."),
			"path":        propDefault("string", "File or directory to search, relative to the workspace root.", "."),
			"glob":        prop("string", "Only change files whose name matches this glob, e.g. *.go"),
			"literal":     propDefault("boolean", "Treat pattern and replacement as literal text.", false),
			"whole_word":  propDefault("boolean", "Only match where the pattern is not part of a longer identifier.", false),
			"ignore_case": propDefault("boolean", "Match case-insensitively.", false),
			"dry_run":     propDefault("boolean", "Report what would change without writing anything.", false),
			"max_files":   propDefault("integer", "Refuse the whole operation if more files than this would change.", 100),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Pattern     string  `json:"pattern"`
			Replacement *string `json:"replacement"`
			Path        string  `json:"path"`
			Glob        string  `json:"glob"`
			Literal     bool    `json:"literal"`
			WholeWord   bool    `json:"whole_word"`
			IgnoreCase  bool    `json:"ignore_case"`
			DryRun      bool    `json:"dry_run"`
			MaxFiles    int     `json:"max_files"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Pattern == "" {
			return toolError("pattern is required"), nil
		}
		if args.Replacement == nil {
			return toolError("replacement is required; pass an empty string to delete matches"), nil
		}
		if !args.DryRun && !ws.AllowWrite {
			return toolError("writes are disabled (workspace.allow_write is false)"), nil
		}
		expr := args.Pattern
		if args.Literal {
			expr = regexp.QuoteMeta(expr)
		}
		if args.WholeWord {
			expr = `\b(?:` + expr + `)\b`
		}
		if args.IgnoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return toolError("invalid pattern: %v", err), nil
		}
		if args.MaxFiles <= 0 {
			args.MaxFiles = 100
		}
		target, err := ws.Resolve(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}

		type change struct {
			rel, before, after string
			count              int
		}
		var changes []change
		tooMany := false
		consider := func(path string) bool {
			data, readErr := os.ReadFile(path)
			if readErr != nil || isBinary(data) || int64(len(data)) > ws.MaxFileBytes {
				return true
			}
			rel := ws.Rel(path)
			content, readErr := ws.ReadFile(rel)
			if readErr != nil {
				return true
			}
			count := len(re.FindAllStringIndex(content, -1))
			if count == 0 {
				return true
			}
			var updated string
			if args.Literal {
				updated = re.ReplaceAllLiteralString(content, *args.Replacement)
			} else {
				updated = re.ReplaceAllString(content, *args.Replacement)
			}
			if updated == content {
				return true
			}
			if len(changes) >= args.MaxFiles {
				tooMany = true
				return false
			}
			changes = append(changes, change{rel: rel, before: content, after: updated, count: count})
			return true
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return pathToolError(ws, args.Path, statErr), nil
		}
		if info.IsDir() {
			walkErr := ws.Walk(target, func(path string, d fs.DirEntry) bool {
				if args.Glob != "" {
					if ok, matchErr := filepath.Match(args.Glob, d.Name()); matchErr != nil || !ok {
						return true
					}
				}
				return consider(path)
			})
			if walkErr != nil {
				return toolError("%v", walkErr), nil
			}
		} else {
			consider(target)
		}
		if tooMany {
			return toolError("matches in more than %d files; narrow path or glob, or raise max_files (nothing was written)", args.MaxFiles), nil
		}
		if len(changes) == 0 {
			return toolResult("No matches; nothing was written."), nil
		}
		if !args.DryRun {
			for _, c := range changes {
				if _, err := ws.WriteFileCheck(c.rel); err != nil {
					return toolError("%s: %v (nothing was written)", c.rel, err), nil
				}
			}
		}

		const maxHunks = 20
		var report strings.Builder
		total := 0
		for _, c := range changes {
			total += c.count
		}
		if args.DryRun {
			report.WriteString("Dry run: nothing was written.\n\n")
		}
		fmt.Fprintf(&report, "%d replacement(s) in %d file(s)\n\n", total, len(changes))
		for i, c := range changes {
			if !args.DryRun {
				if _, err := ws.WriteFile(c.rel, c.after); err != nil {
					return toolError("%s: %v (earlier files in this batch were already written)", c.rel, err), nil
				}
			}
			name := ws.Canonical(c.rel)
			if i < maxHunks {
				fmt.Fprintf(&report, "%s: %d replacement(s)\n%s\n\n", name, c.count, changedHunk(name, c.before, c.after))
			} else {
				fmt.Fprintf(&report, "%s: %d replacement(s)\n", name, c.count)
			}
		}
		return toolResult(strings.TrimSpace(report.String())), nil
	})
}

// identGroup stands in for the name in a language's declaration patterns, so
// the same patterns that find one definition can list them all.
const identGroup = `(?P<ident>[A-Za-z_$][\w$]*)`

// notDeclaredNames are words a permissive pattern can capture that are never
// the name being declared.
var notDeclaredNames = map[string]bool{
	"func": true, "function": true, "def": true, "class": true, "struct": true, "interface": true,
	"type": true, "fn": true, "impl": true, "enum": true, "trait": true, "public": true, "private": true,
	"protected": true, "static": true, "const": true, "var": true, "let": true, "sizeof": true,
}

// outlineEntry is one declaration in a file outline.
type outlineEntry struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Line        int    `json:"line"`
	EndLine     int    `json:"end_line"`
	Depth       int    `json:"depth"`
	Declaration string `json:"declaration"`
}

func outlinePatterns(build func(string) []string) []*regexp.Regexp {
	if build == nil {
		return nil
	}
	var out []*regexp.Regexp
	for _, expr := range build(identGroup) {
		if re, err := regexp.Compile(expr); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// matchIdent returns the name a declaration pattern captured on the line.
func matchIdent(patterns []*regexp.Regexp, line string) string {
	for _, re := range patterns {
		m := re.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for i, group := range re.SubexpNames() {
			if group == "ident" && m[i] != "" && !notDeclaredNames[m[i]] && !statementKeywords[m[i]] {
				return m[i]
			}
		}
	}
	return ""
}

// outlineOf lists the declarations in one file, nested by extent.
func outlineOf(lang *language, content string) []outlineEntry {
	lines, _ := splitLines(content)
	if len(lines) == 0 {
		return nil
	}
	code := lang.stripCode(lines)
	classes, methods := outlinePatterns(lang.class), outlinePatterns(lang.method)
	var entries []outlineEntry
	var open []int // end lines of the declarations enclosing the current one
	for i := range lines {
		kind, name := "class", matchIdent(classes, code[i])
		if name == "" {
			kind, name = "method", matchIdent(methods, code[i])
		}
		if name == "" || (lang.Body == braceBody && !plausibleDeclaration(code[i], name)) {
			continue
		}
		def := lang.extent(lines, code, i)
		if def == nil {
			continue
		}
		for len(open) > 0 && open[len(open)-1] < i+1 {
			open = open[:len(open)-1]
		}
		entries = append(entries, outlineEntry{
			Kind: kind, Name: name, Line: i + 1, EndLine: def.EndLine, Depth: len(open),
			Declaration: strings.TrimSpace(lines[i]),
		})
		open = append(open, def.EndLine)
	}
	return entries
}

// referencesIn classifies each whole-word occurrence of name in one file,
// skipping comments, as a definition or a reference.
func referencesIn(lang *language, name, content string) (lines []string, hits []int, definitions map[int]bool) {
	lines, _ = splitLines(content)
	code := lines
	var defPatterns []*regexp.Regexp
	if lang != nil {
		code = lang.stripCode(lines)
		methods, _ := compilePatterns(lang.method, name)
		classes, _ := compilePatterns(lang.class, name)
		defPatterns = append(methods, classes...)
	}
	word := regexp.MustCompile(`(?:^|[^\w$])` + regexp.QuoteMeta(name) + `(?:$|[^\w$])`)
	definitions = map[int]bool{}
	for i := range lines {
		if !word.MatchString(code[i]) {
			continue
		}
		hits = append(hits, i)
		if matchesAny(defPatterns, code[i]) && (lang.Body != braceBody || plausibleDeclaration(code[i], name)) {
			definitions[i] = true
		}
	}
	return lines, hits, definitions
}

func (s *Server) registerNavigationTools(ws *Workspace) {
	s.RegisterTool(Tool{
		Name:  "file_outline",
		Title: "Outline a file",
		Description: "List the classes, types, functions and methods declared in one file, with the lines each spans, nested " +
			"by containment. A cheap way to find your place in a large file before reading the part you need. Reads " +
			strings.Join(languageNames(), ", ") + ".",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path": prop("string", "File relative to the workspace root."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path string `json:"path"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		actual, note, err := ws.ResolveForRead(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}
		lang := languageFor(actual)
		if lang == nil {
			return toolError("%s is not in a language this server outlines (%s)", args.Path, strings.Join(languageNames(), ", ")), nil
		}
		content, err := ws.ReadFile(actual)
		if err != nil {
			return pathToolError(ws, actual, err), nil
		}
		name := ws.Canonical(actual)
		entries := outlineOf(lang, content)
		if len(entries) == 0 {
			return withNote(toolResult(fmt.Sprintf("No declarations found in %s.", name)), note), nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s (%s): %d declaration(s)\n", name, lang.Name, len(entries))
		for _, e := range entries {
			fmt.Fprintf(&b, "%s%d-%d  %s %s\n", strings.Repeat("  ", e.Depth), e.Line, e.EndLine, e.Kind, e.Declaration)
		}
		return withNote(&CallToolResult{
			Content:           textContent(strings.TrimSuffix(b.String(), "\n")),
			StructuredContent: map[string]any{"path": name, "language": lang.Name, "declarations": entries},
		}, note), nil
	})

	s.RegisterTool(Tool{
		Name:  "find_references",
		Title: "Find references to a name",
		Description: "Find every whole-word use of an identifier under a path - call sites, type uses, imports - with " +
			"comments skipped, and each definition marked as such. The counterpart to find_method_definition: " +
			"check it before changing a signature or renaming.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"name"}, map[string]any{
			"name":                prop("string", "The identifier, exactly as spelled in the source."),
			"path":                propDefault("string", "File or directory to search, relative to the workspace root.", "."),
			"glob":                prop("string", "Only search files whose name matches this glob, e.g. *.go"),
			"include_definitions": propDefault("boolean", "Include the lines that define the name, marked [definition].", true),
			"max_results":         prop("integer", "Stop after this many references. Defaults to the configured max_results."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Name               string `json:"name"`
			Path               string `json:"path"`
			Glob               string `json:"glob"`
			IncludeDefinitions *bool  `json:"include_definitions"`
			MaxResults         int    `json:"max_results"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		args.Name = strings.TrimSpace(args.Name)
		if args.Name == "" {
			return toolError("name is required"), nil
		}
		includeDefs := args.IncludeDefinitions == nil || *args.IncludeDefinitions
		limit := args.MaxResults
		if limit <= 0 {
			limit = ws.MaxResults
		}
		target, err := ws.Resolve(args.Path)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}

		type reference struct {
			Path       string `json:"path"`
			Line       int    `json:"line"`
			Text       string `json:"text"`
			Definition bool   `json:"definition,omitempty"`
		}
		var refs []reference
		truncated := false
		search := func(path string) bool {
			data, readErr := os.ReadFile(path)
			if readErr != nil || isBinary(data) || int64(len(data)) > ws.MaxFileBytes {
				return true
			}
			if !strings.Contains(string(data), args.Name) {
				return true
			}
			lines, hits, defs := referencesIn(languageFor(path), args.Name, ws.ToLF(string(data)))
			for _, i := range hits {
				if defs[i] && !includeDefs {
					continue
				}
				refs = append(refs, reference{Path: ws.Rel(path), Line: i + 1, Text: strings.TrimSpace(lines[i]), Definition: defs[i]})
				if len(refs) >= limit {
					truncated = true
					return false
				}
			}
			return true
		}
		info, statErr := os.Stat(target)
		if statErr != nil {
			return pathToolError(ws, args.Path, statErr), nil
		}
		if info.IsDir() {
			walkErr := ws.Walk(target, func(path string, d fs.DirEntry) bool {
				if args.Glob != "" {
					if ok, matchErr := filepath.Match(args.Glob, d.Name()); matchErr != nil || !ok {
						return true
					}
				}
				return search(path)
			})
			if walkErr != nil {
				return toolError("%v", walkErr), nil
			}
		} else {
			search(target)
		}
		if len(refs) == 0 {
			return toolResult(fmt.Sprintf("No references to %s found.", args.Name)), nil
		}
		var b strings.Builder
		files := map[string]bool{}
		for _, r := range refs {
			files[r.Path] = true
		}
		fmt.Fprintf(&b, "%d reference(s) to %s in %d file(s)\n", len(refs), args.Name, len(files))
		for _, r := range refs {
			tag := ""
			if r.Definition {
				tag = "  [definition]"
			}
			fmt.Fprintf(&b, "%s:%d: %s%s\n", r.Path, r.Line, r.Text, tag)
		}
		if truncated {
			fmt.Fprintf(&b, "(stopped at %d references)\n", limit)
		}
		return &CallToolResult{
			Content:           textContent(strings.TrimSuffix(b.String(), "\n")),
			StructuredContent: map[string]any{"name": args.Name, "references": refs, "truncated": truncated},
		}, nil
	})
}

// lineDiffStat counts the lines added and removed going from before to after,
// over the one contiguous region that differs, as changedHunk reports it.
func lineDiffStat(before, after string) (added, removed int) {
	if before == after {
		return 0, 0
	}
	b, a := strings.Split(before, "\n"), strings.Split(after, "\n")
	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix && b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}
	return len(a) - prefix - suffix, len(b) - prefix - suffix
}

// resolveTracked resolves a path for the history tools, which must accept a
// file that a rollback would recreate as well as one that exists.
func resolveTracked(ws *Workspace, path string) (string, *CallToolResult) {
	abs, err := ws.Resolve(path)
	if err != nil {
		if abs, err = ws.WriteFileCheck(path); err != nil {
			return "", pathToolError(ws, path, err)
		}
	}
	return abs, nil
}

func (s *Server) registerHistoryTools(ws *Workspace) {
	s.RegisterTool(Tool{
		Name:  "edit_history",
		Title: "List a file's tracked states",
		Description: "List the earlier states of a file that rollback can restore, newest first: when each was replaced, its size, " +
			"and how many lines the change after it added and removed. #1 is what the next rollback restores.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path":          prop("string", "File relative to the workspace root."),
			"include_diffs": propDefault("boolean", "Show the diff each rollback step would apply.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path         string `json:"path"`
			IncludeDiffs bool   `json:"include_diffs"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		abs, fail := resolveTracked(ws, args.Path)
		if fail != nil {
			return fail, nil
		}
		name := ws.Canonical(abs)
		states := ws.History.States(abs)
		if len(states) == 0 {
			return toolResult(fmt.Sprintf("%s has no tracked states: this server has not written it since starting, "+
				"or it has already been rolled back to the earliest tracked state.", name)), nil
		}
		currentData, _ := os.ReadFile(abs)
		newer := string(currentData)
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %d tracked state(s), newest first. rollback restores #1.\n", name, len(states))
		for step := 1; step <= len(states); step++ {
			st := states[len(states)-step]
			older := string(st.data)
			describe := fmt.Sprintf("%d line(s), %d bytes", strings.Count(older, "\n"), len(st.data))
			if !st.existed {
				describe = "file did not exist"
			}
			added, removed := lineDiffStat(older, newer)
			fmt.Fprintf(&b, "#%d  replaced %s  %s  (next change: +%d -%d lines)\n",
				step, st.at.Format("2006-01-02 15:04:05"), describe, added, removed)
			if args.IncludeDiffs {
				b.WriteString(changedHunk(name, newer, older) + "\n")
			}
			newer = older
		}
		return toolResult(strings.TrimSuffix(b.String(), "\n")), nil
	})

	s.RegisterTool(Tool{
		Name:  "preview_rollback",
		Title: "Preview a rollback",
		Description: "Show, for each file, the diff that rollback would apply - the change from the file as it is now to its " +
			"previous tracked state - without changing anything.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"files"}, map[string]any{
			"files": map[string]any{
				"type":        "array",
				"description": "Files to preview, relative to the workspace root.",
				"minItems":    1,
				"items":       map[string]any{"type": "string"},
			},
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Files []string `json:"files"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if len(args.Files) == 0 {
			return toolError("files is required and must not be empty"), nil
		}
		sections := make([]string, 0, len(args.Files))
		for _, path := range args.Files {
			abs, fail := resolveTracked(ws, path)
			if fail != nil {
				sections = append(sections, fmt.Sprintf("%s: %s", path, resultText(fail)))
				continue
			}
			name := ws.Canonical(abs)
			state, remaining, err := ws.History.Peek(abs)
			if err != nil {
				sections = append(sections, fmt.Sprintf("%s: %v", name, err))
				continue
			}
			current, _ := os.ReadFile(abs)
			if !state.existed {
				sections = append(sections, fmt.Sprintf("%s: rolling back would remove the file, which did not exist before. "+
					"%d earlier state(s) would remain.\n%s", name, remaining, changedHunk(name, string(current), "")))
				continue
			}
			sections = append(sections, fmt.Sprintf("%s: rolling back one state would apply this change. %d earlier state(s) would remain.\n%s",
				name, remaining, changedHunk(name, string(current), string(state.data))))
		}
		return toolResult(strings.Join(sections, "\n\n")), nil
	})
}

// diagnosticCommands are the project command names diagnostics tries, in
// order, when the caller does not name one.
var diagnosticCommands = []string{"lint", "typecheck", "check", "vet", "build"}

// diagnosticPatterns recognise the two layouts nearly every compiler and linter
// uses: "path:line[:col]: message" and "path(line[,col]): message".
var diagnosticPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^\s*((?:[A-Za-z]:)?[^\s:()"'][^:()"']*?\.[A-Za-z0-9]+):(\d+)(?::(\d+))?:\s*(.*)$`),
	regexp.MustCompile(`^\s*((?:[A-Za-z]:)?[^\s:()"'][^:()"']*?\.[A-Za-z0-9]+)\((\d+)(?:,(\d+))?\)\s*:\s*(.*)$`),
}

var severityPrefix = regexp.MustCompile(`(?i)^(fatal error|error|warning|warn|note|info|hint)\b\s*(?:[A-Za-z]+\d+)?\s*:?\s*`)

// diagnostic is one problem a command reported against a file.
type diagnostic struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Column   int    `json:"column,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
}

// parseDiagnostics pulls file:line diagnostics out of command output. Relative
// paths are taken relative to dir, the directory the command ran in.
func parseDiagnostics(ws *Workspace, dir, output string) []diagnostic {
	var out []diagnostic
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		for _, pattern := range diagnosticPatterns {
			m := pattern.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			path := filepath.FromSlash(m[1])
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			d := diagnostic{Path: filepath.ToSlash(ws.Rel(path)), Message: strings.TrimSpace(m[4])}
			d.Line, _ = strconv.Atoi(m[2])
			d.Column, _ = strconv.Atoi(m[3])
			if sev := severityPrefix.FindStringSubmatch(d.Message); sev != nil {
				d.Severity = strings.ToLower(sev[1])
				if d.Severity == "warn" {
					d.Severity = "warning"
				}
				d.Message = d.Message[len(sev[0]):]
			}
			key := fmt.Sprintf("%s:%d:%d:%s", d.Path, d.Line, d.Column, d.Message)
			if !seen[key] {
				seen[key] = true
				out = append(out, d)
			}
			break
		}
	}
	return out
}

func (s *Server) registerDiagnostics(ws *Workspace) {
	s.RegisterTool(Tool{
		Name:  "diagnostics",
		Title: "Collect compiler and linter diagnostics",
		Description: "Run the project's checking command and return its problems as structured file, line, column, severity " +
			"and message, optionally limited to the files you changed. Without command, the first project command named " +
			strings.Join(diagnosticCommands, ", ") + " is used. Run it after an edit; rollback undoes an edit that broke the build.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema(nil, map[string]any{
			"command": prop("string", "Name of the project command to run (see project_commands)."),
			"paths": map[string]any{
				"type":        "array",
				"description": "Only report diagnostics for these files, relative to the workspace root.",
				"items":       map[string]any{"type": "string"},
			},
			"max_results": propDefault("integer", "Stop after this many diagnostics.", 200),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Command    string   `json:"command"`
			Paths      []string `json:"paths"`
			MaxResults int      `json:"max_results"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		s.mu.RLock()
		commands := s.cfg.Commands
		s.mu.RUnlock()

		var cmd *CommandConfig
		wanted := diagnosticCommands
		if args.Command != "" {
			wanted = []string{args.Command}
		}
		for _, name := range wanted {
			for i := range commands {
				if commands[i].Name == name {
					cmd = &commands[i]
					break
				}
			}
			if cmd != nil {
				break
			}
		}
		if cmd == nil {
			if args.Command != "" {
				return toolError("there is no project command called %q; see project_commands", args.Command), nil
			}
			return toolError("no checking command is configured: define a project command named %s, or pass command",
				strings.Join(diagnosticCommands, ", ")), nil
		}
		if cmd.Background {
			return toolError("%s runs in the background, so it cannot be collected as diagnostics", cmd.Name), nil
		}
		dir := ws.Root
		if cmd.Dir != "" {
			resolved, err := ws.Resolve(cmd.Dir)
			if err != nil {
				return toolError("commands.%s.dir: %v", cmd.Name, err), nil
			}
			dir = resolved
		}
		line := commandLine(*cmd, "")
		if blocked := s.checkGitAllowed(line); blocked != nil {
			return blocked, nil
		}
		res, err := runShellSudo(ctx, s.sudoAgentRef(), line, dir, cmd.Env, time.Duration(cmd.TimeoutSeconds)*time.Second)
		if err != nil {
			return toolError("%v", err), nil
		}

		wantedPaths := map[string]bool{}
		for _, p := range args.Paths {
			wantedPaths[strings.TrimPrefix(filepath.ToSlash(filepath.Clean(strings.TrimLeft(p, "/"))), "./")] = true
		}
		limit := args.MaxResults
		if limit <= 0 {
			limit = 200
		}
		var diags []diagnostic
		truncated := false
		for _, d := range parseDiagnostics(ws, dir, res.Stdout+"\n"+res.Stderr) {
			if len(wantedPaths) > 0 && !wantedPaths[d.Path] {
				continue
			}
			if len(diags) >= limit {
				truncated = true
				break
			}
			diags = append(diags, d)
		}

		var b strings.Builder
		fmt.Fprintf(&b, "$ %s\nexit code %d: %d diagnostic(s)\n", res.Command, res.ExitCode, len(diags))
		for _, d := range diags {
			loc := fmt.Sprintf("%s:%d", d.Path, d.Line)
			if d.Column > 0 {
				loc += fmt.Sprintf(":%d", d.Column)
			}
			sev := ""
			if d.Severity != "" {
				sev = d.Severity + ": "
			}
			fmt.Fprintf(&b, "%s: %s%s\n", loc, sev, d.Message)
		}
		if truncated {
			fmt.Fprintf(&b, "(stopped at %d diagnostics)\n", limit)
		}
		if len(diags) == 0 && (res.ExitCode != 0 || res.TimedOut) && len(wantedPaths) == 0 {
			b.WriteString("\nThe command failed but no file:line diagnostics were recognised. Its output follows.\n\n")
			b.WriteString(res.summarize())
		}
		return &CallToolResult{
			Content: textContent(strings.TrimSuffix(b.String(), "\n")),
			StructuredContent: map[string]any{
				"command": res.Command, "exit_code": res.ExitCode, "timed_out": res.TimedOut,
				"diagnostics": diags, "truncated": truncated,
			},
		}, nil
	})
}
