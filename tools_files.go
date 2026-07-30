package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// registerFileTools adds the file-system half of the coding agent: the tools
// it needs to look at and change the workspace.
func (s *Server) registerFileTools() {
	// Read under the lock: a configuration reload can replace the workspace
	// while a request is in flight.
	ws := s.workspace()

	s.RegisterTool(Tool{
		Name:        "list_directory",
		Title:       "List directory",
		Description: "List the files and subdirectories of a workspace directory. Paths are relative to the workspace root.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"path":      propDefault("string", "Directory relative to the workspace root.", "."),
			"recursive": propDefault("boolean", "Walk subdirectories as well.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path      string `json:"path"`
			Recursive bool   `json:"recursive"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		dir, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		var entries []string
		if args.Recursive {
			count := 0
			walkErr := ws.Walk(dir, func(path string, d fs.DirEntry) bool {
				entries = append(entries, ws.Rel(path))
				count++
				return count < ws.MaxResults
			})
			if walkErr != nil {
				return toolError("%v", walkErr), nil
			}
		} else {
			list, readErr := os.ReadDir(dir)
			if readErr != nil {
				return toolError("%v", readErr), nil
			}
			for _, d := range list {
				full := filepath.Join(dir, d.Name())
				if ws.IsExcluded(full) {
					continue
				}
				if ws.IsExcludedUnder(full, dir) {
					continue
				}
				name := ws.Rel(full)
				if d.IsDir() {
					name += "/"
				}
				entries = append(entries, name)
			}
		}
		sort.Strings(entries)
		if len(entries) == 0 {
			return toolResult("(empty)"), nil
		}
		return toolResult(strings.Join(entries, "\n")), nil
	})

	s.RegisterTool(Tool{
		Name:        "read_file",
		Title:       "Read file",
		Description: "Read a text file from the workspace. Use start_line and end_line to read part of a large file; lines are 1-based and inclusive.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path":       prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
			"start_line": prop("integer", "First line to return, 1-based."),
			"end_line":   prop("integer", "Last line to return, inclusive."),
			"line_numbers": propDefault("boolean", "Prefix every line with its 1-based number, as edit_lines expects. "+
				"The numbers are not part of the file.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path        string `json:"path"`
			StartLine   int    `json:"start_line"`
			EndLine     int    `json:"end_line"`
			LineNumbers bool   `json:"line_numbers"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		text, _, note, fail := readFileRange(ws, args.Path, args.StartLine, args.EndLine, args.LineNumbers)
		if fail != nil {
			return fail, nil
		}
		return withNote(toolResult(text), note), nil
	})

	s.RegisterTool(Tool{
		Name:        "write_file",
		Title:       "Write file",
		Description: "Create or overwrite a workspace file with the given contents. Parent directories are created as needed.",
		Annotations: &ToolAnnotations{DestructiveHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"path", "content"}, map[string]any{
			"path":    prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
			"content": prop("string", "The complete new contents of the file."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		abs, err := ws.WriteFile(args.Path, args.Content)
		if err != nil {
			return toolError("%v", err), nil
		}
		msg := fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), ws.Rel(abs))
		if note := ws.AdjustmentNote(args.Path, abs); note != "" {
			msg += "\n" + note
		}
		return toolResult(msg), nil
	})

	s.RegisterTool(Tool{
		Name:  "edit_file",
		Title: "Edit file",
		Description: "Replace an exact string in a workspace file. The old string must appear exactly once unless replace_all is set, " +
			"which keeps the edit unambiguous. Returns the changed lines as a diff hunk, so there is no need to re-read the file to " +
			"confirm the edit landed. Use multi_edit when a change touches several places.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"path", "old_string", "new_string"}, map[string]any{
			"path":       prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
			"old_string": prop("string", "Exact text to replace, including indentation."),
			"new_string": prop("string", "Replacement text. Required: a call that carries no new_string is "+
				"refused rather than applied, since that would delete old_string instead of changing it. "+
				"Pass an empty string to delete deliberately."),
			"oldText":     prop("string", "Alias for old_string. Ignored when old_string is given."),
			"newText":     prop("string", "Alias for new_string. Ignored when new_string is given."),
			"replace_all": propDefault("boolean", "Replace every occurrence instead of requiring exactly one.", false),
			"normalize_line_endings": propDefault("boolean",
				"When the old string does not match, retry it re-encoded to the file's own line endings. "+
					"An exact match always takes precedence, so an edit that deliberately changes a line ending is unaffected.",
				true),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		// The edit itself is decoded as an editOp, so this tool and multi_edit
		// read exactly the same fields - including which keys they did not
		// recognise, which is what tells a lost replacement from a deletion.
		var op editOp
		if bad := decodeArgs(raw, &op); bad != nil {
			return bad, nil
		}
		var args struct {
			NormalizeLineEndings *bool `json:"normalize_line_endings"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		op = op.normalizedFor(ws)
		oldStr, newStr, hasNew := op.anchor()
		if op.Path == "" || oldStr == "" {
			return toolError("path and old_string are required"), nil
		}
		if !hasNew {
			return toolError("%v (nothing was written)", missingReplacementError(op)), nil
		}
		actual, note, err := ws.ResolveForRead(op.Path)
		if err != nil {
			return pathToolError(ws, op.Path, err), nil
		}
		op.Path = actual
		content, err := ws.ReadFile(actual)
		if err != nil {
			return pathToolError(ws, actual, err), nil
		}
		normalize := args.NormalizeLineEndings == nil || *args.NormalizeLineEndings
		res, editErr := applyEdit(content, op, normalize)
		if editErr != nil {
			return toolError("%v", editErr), nil
		}
		if _, err := ws.WriteFile(actual, res.Content); err != nil {
			return toolError("%v", err), nil
		}
		adjustedNote := ""
		if res.Adjusted {
			adjustedNote = " (anchor re-encoded to the file's line endings)"
		}
		// Report the path the workspace resolved to rather than the one the
		// caller typed: the two differ whenever a rooted path was anchored.
		name := ws.Canonical(actual)
		result := withNote(toolResult(fmt.Sprintf("Replaced %d occurrence(s) in %s%s\n%s",
			res.Count, name, adjustedNote, changedHunk(name, content, res.Content))), note)
		if newStr == "" {
			result = withNamingWarning(result, removalWarning)
		}
		return result, nil
	})

	s.RegisterTool(Tool{
		Name:        "search_files",
		Title:       "Search file contents",
		Description: "Search the workspace for a regular expression and return matching lines with their file and line number.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"pattern"}, map[string]any{
			"pattern":     prop("string", "Go (RE2) regular expression to search for."),
			"path":        propDefault("string", "Directory to search, relative to the workspace root.", "."),
			"glob":        prop("string", "Only search files whose name matches this glob, e.g. *.go"),
			"ignore_case": propDefault("boolean", "Match case-insensitively.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Pattern    string `json:"pattern"`
			Path       string `json:"path"`
			Glob       string `json:"glob"`
			IgnoreCase bool   `json:"ignore_case"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Pattern == "" {
			return toolError("pattern is required"), nil
		}
		expr := args.Pattern
		if args.IgnoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return toolError("invalid pattern: %v", err), nil
		}
		dir, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		var matches []string
		walkErr := ws.Walk(dir, func(path string, d fs.DirEntry) bool {
			if args.Glob != "" {
				if ok, matchErr := filepath.Match(args.Glob, d.Name()); matchErr != nil || !ok {
					return true
				}
			}
			info, statErr := d.Info()
			if statErr != nil || info.Size() > ws.MaxFileBytes {
				return true
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil || isBinary(data) {
				return true
			}
			for i, line := range strings.Split(ws.ToLF(string(data)), "\n") {
				if re.MatchString(line) {
					matches = append(matches, fmt.Sprintf("%s:%d:%s", ws.Rel(path), i+1, strings.TrimRight(line, "\r")))
					if len(matches) >= ws.MaxResults {
						return false
					}
				}
			}
			return true
		})
		if walkErr != nil {
			return toolError("%v", walkErr), nil
		}
		if len(matches) == 0 {
			return toolResult("No matches."), nil
		}
		out := strings.Join(matches, "\n")
		if len(matches) >= ws.MaxResults {
			out += fmt.Sprintf("\n\n(stopped at %d matches)", ws.MaxResults)
		}
		return toolResult(out), nil
	})

	s.RegisterTool(Tool{
		Name:        "find_files",
		Title:       "Find files by name",
		Description: "Find workspace files whose path matches a glob, for example **/*.go or cmd/*/main.go",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"pattern"}, map[string]any{
			"pattern": prop("string", "Glob matched against the workspace-relative path. ** matches across directories."),
			"path":    propDefault("string", "Directory to search, relative to the workspace root.", "."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Pattern string `json:"pattern"`
			Path    string `json:"path"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Pattern == "" {
			return toolError("pattern is required"), nil
		}
		re, err := globToRegexp(args.Pattern)
		if err != nil {
			return toolError("invalid pattern: %v", err), nil
		}
		dir, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		var found []string
		walkErr := ws.Walk(dir, func(path string, d fs.DirEntry) bool {
			rel := ws.Rel(path)
			if re.MatchString(rel) || re.MatchString(d.Name()) {
				found = append(found, rel)
			}
			return len(found) < ws.MaxResults
		})
		if walkErr != nil {
			return toolError("%v", walkErr), nil
		}
		if len(found) == 0 {
			return toolResult("No files matched."), nil
		}
		sort.Strings(found)
		out := strings.Join(found, "\n")
		// Silence here would read as "these are all of them", and a model
		// would conclude the file it was looking for does not exist.
		if len(found) >= ws.MaxResults {
			out += fmt.Sprintf("\n\n(stopped at %d files; narrow the pattern or the path)", ws.MaxResults)
		}
		return toolResult(out), nil
	})

	s.RegisterTool(Tool{
		Name:  "find_file",
		Title: "Find a file by name",
		Description: "Find workspace files whose file name contains a search term (case-insensitive, no glob " +
			"syntax) - for what find_files needs a pattern for, this takes a plain word like \"config\" or " +
			"\"readme\". Reports the full workspace-relative path, size and line count of each match. Use " +
			"find_files instead when the shape of the path matters (an extension, a directory), not just the name.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"term"}, map[string]any{
			"term": prop("string", "Text to look for in the file name, case-insensitive. Matched as a substring, not a glob."),
			"path": propDefault("string", "Directory to search, relative to the workspace root.", "."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Term string `json:"term"`
			Path string `json:"path"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Term == "" {
			return toolError("term is required"), nil
		}
		dir, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		term := strings.ToLower(args.Term)
		var matches []fileNameMatch
		stopped := false
		walkErr := ws.Walk(dir, func(path string, d fs.DirEntry) bool {
			if !strings.Contains(strings.ToLower(d.Name()), term) {
				return true
			}
			matches = append(matches, statFileMatch(ws, path))
			if len(matches) >= ws.MaxResults {
				stopped = true
				return false
			}
			return true
		})
		if walkErr != nil {
			return toolError("%v", walkErr), nil
		}
		if len(matches) == 0 {
			return toolResult("No files matched."), nil
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].Path < matches[j].Path })
		out := formatFileMatches(matches)
		// Silence here would read as "these are all of them", and a model
		// would conclude the file it was looking for does not exist.
		if stopped {
			out += fmt.Sprintf("\n\n(stopped at %d files; narrow the term or the path)", ws.MaxResults)
		}
		return &CallToolResult{
			Content:           textContent(out),
			StructuredContent: matches,
		}, nil
	})
}

// fileNameMatch is one find_file result.
type fileNameMatch struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	// Lines is nil when it was not counted - Skipped then says why (binary,
	// or over the file-read limit), rather than a bare zero that would read
	// as an empty file.
	Lines   *int   `json:"lines,omitempty"`
	Skipped string `json:"lines_unavailable,omitempty"`
}

// statFileMatch reports size unconditionally (a stat is cheap even for a huge
// or binary file) and counts lines only for a text file within the read
// limit, matching what grep_files is willing to open.
func statFileMatch(ws *Workspace, path string) fileNameMatch {
	m := fileNameMatch{Path: ws.Rel(path)}
	info, err := os.Stat(path)
	if err != nil {
		m.Skipped = "could not stat: " + err.Error()
		return m
	}
	m.SizeBytes = info.Size()
	if info.Size() > ws.MaxFileBytes {
		m.Skipped = "too large to count"
		return m
	}
	data, err := os.ReadFile(path)
	if err != nil {
		m.Skipped = "could not read: " + err.Error()
		return m
	}
	if isBinary(data) {
		m.Skipped = "binary"
		return m
	}
	n := countLines(data)
	m.Lines = &n
	return m
}

// countLines counts newline-terminated lines plus one more for a final line
// with no trailing newline, so a one-line file with no newline still reads
// as 1 line rather than 0.
func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := strings.Count(string(data), "\n")
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// formatFileMatches renders find_file's results as a column-aligned listing.
func formatFileMatches(matches []fileNameMatch) string {
	pathWidth := 0
	for _, m := range matches {
		pathWidth = max(pathWidth, len(m.Path))
	}
	var b strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&b, "%-*s  %8s  ", pathWidth, m.Path, formatBytes(uint64(m.SizeBytes)))
		switch {
		case m.Lines != nil:
			fmt.Fprintf(&b, "%d lines\n", *m.Lines)
		case m.Skipped != "":
			fmt.Fprintf(&b, "(%s)\n", m.Skipped)
		default:
			b.WriteString("\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// globToRegexp translates a glob with ** support into an anchored regexp.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// isBinary is the usual heuristic: a NUL byte near the start of the file.
func isBinary(data []byte) bool {
	limit := min(len(data), 8000)
	for i := 0; i < limit; i++ {
		if data[i] == 0 {
			return true
		}
	}
	return false
}
