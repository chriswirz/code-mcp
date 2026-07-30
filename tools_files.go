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
	ws := s.ws

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
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		content, err := ws.ReadFile(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		if args.StartLine <= 0 && args.EndLine <= 0 {
			return toolResult(content), nil
		}
		lines := strings.Split(content, "\n")
		start := args.StartLine
		if start <= 0 {
			start = 1
		}
		end := args.EndLine
		if end <= 0 || end > len(lines) {
			end = len(lines)
		}
		if start > len(lines) {
			return toolError("%s has %d lines, start_line %d is past the end", args.Path, len(lines), start), nil
		}
		return toolResult(strings.Join(lines[start-1:end], "\n")), nil
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
		InputSchema: schema([]string{"path"}, map[string]any{
			"path":        prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
			"old_string":  prop("string", "Exact text to replace, including indentation."),
			"new_string":  prop("string", "Replacement text."),
			"oldText":     prop("string", "Alias for old_string. Ignored when old_string is given."),
			"newText":     prop("string", "Alias for new_string. Ignored when new_string is given."),
			"replace_all": propDefault("boolean", "Replace every occurrence instead of requiring exactly one.", false),
			"normalize_line_endings": propDefault("boolean",
				"When the old string does not match, retry it re-encoded to the file's own line endings. "+
					"An exact match always takes precedence, so an edit that deliberately changes a line ending is unaffected.",
				true),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path                 string `json:"path"`
			OldString            string `json:"old_string"`
			NewString            string `json:"new_string"`
			OldText              string `json:"oldText"`
			NewText              string `json:"newText"`
			ReplaceAll           bool   `json:"replace_all"`
			NormalizeLineEndings *bool  `json:"normalize_line_endings"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		op := editOp{
			Path:       args.Path,
			OldString:  args.OldString,
			NewString:  args.NewString,
			OldText:    args.OldText,
			NewText:    args.NewText,
			ReplaceAll: args.ReplaceAll,
		}
		oldStr, _ := op.anchor()
		if args.Path == "" || oldStr == "" {
			return toolError("path and old_string are required"), nil
		}
		content, err := ws.ReadFile(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		normalize := args.NormalizeLineEndings == nil || *args.NormalizeLineEndings
		res, editErr := applyEdit(content, op, normalize)
		if editErr != nil {
			return toolError("%v", editErr), nil
		}
		if _, err := ws.WriteFile(args.Path, res.Content); err != nil {
			return toolError("%v", err), nil
		}
		note := ""
		if res.Adjusted {
			note = " (anchor re-encoded to the file's line endings)"
		}
		// Report the path the workspace resolved to rather than the one the
		// caller typed: the two differ whenever a rooted path was anchored.
		name := ws.Canonical(args.Path)
		return toolResult(fmt.Sprintf("Replaced %d occurrence(s) in %s%s\n%s",
			res.Count, name, note, changedHunk(name, content, res.Content))), nil
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
			for i, line := range strings.Split(string(data), "\n") {
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
		return toolResult(strings.Join(found, "\n")), nil
	})
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
