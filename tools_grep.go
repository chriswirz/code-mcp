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

// grepMatch is one matching line and the lines around it.
type grepMatch struct {
	Path   string   `json:"path"`
	Line   int      `json:"line"`
	Text   string   `json:"text"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
	// FirstLine is the line number of Before[0], or of Text when there is no
	// leading context, so a caller can number the block without recounting.
	FirstLine int `json:"first_line"`
}

// grepResult is the structured content of a grep_files call.
type grepResult struct {
	Pattern   string      `json:"pattern"`
	Matches   []grepMatch `json:"matches"`
	FileCount int         `json:"file_count"`
	Truncated bool        `json:"truncated,omitempty"`
}

// registerGrepTools adds grep_files: a content search that can return the
// lines around each hit, which is usually what makes a match interpretable
// without a second call to read the file.
func (s *Server) registerGrepTools() {
	ws := s.ws

	s.RegisterTool(Tool{
		Name:  "grep_files",
		Title: "Search file contents with context",
		Description: "Search every file under a path for lines matching a pattern, and return each match " +
			"with the surrounding lines. context defaults to 0, which returns only the matching line " +
			"itself; set it to 3 to get three lines either side. Use before and after for an uneven " +
			"window. Binary files and the configured excludes are skipped.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"pattern"}, map[string]any{
			"pattern":     prop("string", "Go (RE2) regular expression, or a literal string when literal is set."),
			"path":        propDefault("string", "File or directory to search, relative to the workspace root.", "."),
			"glob":        prop("string", "Only search files whose name matches this glob, e.g. *.go"),
			"context":     propDefault("integer", "Lines of context to return either side of each match.", 0),
			"before":      prop("integer", "Lines of context before each match. Overrides context."),
			"after":       prop("integer", "Lines of context after each match. Overrides context."),
			"ignore_case": propDefault("boolean", "Match case-insensitively.", false),
			"literal": propDefault("boolean",
				"Treat the pattern as literal text rather than a regular expression.", false),
			"files_only": propDefault("boolean",
				"Return just the list of files that contain a match, with no lines.", false),
			"max_matches": prop("integer", "Stop after this many matches. Defaults to the configured max_results."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Pattern    string `json:"pattern"`
			Path       string `json:"path"`
			Glob       string `json:"glob"`
			Context    int    `json:"context"`
			Before     *int   `json:"before"`
			After      *int   `json:"after"`
			IgnoreCase bool   `json:"ignore_case"`
			Literal    bool   `json:"literal"`
			FilesOnly  bool   `json:"files_only"`
			MaxMatches int    `json:"max_matches"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Pattern == "" {
			return toolError("pattern is required"), nil
		}

		expr := args.Pattern
		if args.Literal {
			expr = regexp.QuoteMeta(expr)
		}
		if args.IgnoreCase {
			expr = "(?i)" + expr
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			return toolError("invalid pattern: %v", err), nil
		}

		before, after := args.Context, args.Context
		if args.Before != nil {
			before = *args.Before
		}
		if args.After != nil {
			after = *args.After
		}
		if before < 0 {
			before = 0
		}
		if after < 0 {
			after = 0
		}
		limit := args.MaxMatches
		if limit <= 0 {
			limit = ws.MaxResults
		}

		target, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}

		result := grepResult{Pattern: args.Pattern}
		files := map[string]bool{}

		search := func(path string) bool {
			data, readErr := os.ReadFile(path)
			if readErr != nil || isBinary(data) {
				return true
			}
			lines, _ := splitLines(ws.ToLF(string(data)))
			for i, line := range lines {
				if !re.MatchString(line) {
					continue
				}
				files[ws.Rel(path)] = true
				if args.FilesOnly {
					// One hit settles the question for this file.
					return true
				}
				match := grepMatch{
					Path:      ws.Rel(path),
					Line:      i + 1,
					Text:      line,
					FirstLine: i + 1,
				}
				if before > 0 {
					start := max(0, i-before)
					match.Before = append([]string(nil), lines[start:i]...)
					match.FirstLine = start + 1
				}
				if after > 0 {
					end := min(len(lines), i+after+1)
					match.After = append([]string(nil), lines[i+1:end]...)
				}
				result.Matches = append(result.Matches, match)
				if len(result.Matches) >= limit {
					result.Truncated = true
					return false
				}
			}
			return true
		}

		// A path may name one file as easily as a directory; searching a single
		// file is a common enough request to handle rather than reject.
		info, statErr := os.Stat(target)
		if statErr != nil {
			return toolError("%v", statErr), nil
		}
		if info.IsDir() {
			walkErr := ws.Walk(target, func(path string, d fs.DirEntry) bool {
				if args.Glob != "" {
					if ok, matchErr := filepath.Match(args.Glob, d.Name()); matchErr != nil || !ok {
						return true
					}
				}
				if fi, infoErr := d.Info(); infoErr != nil || fi.Size() > ws.MaxFileBytes {
					return true
				}
				return search(path)
			})
			if walkErr != nil {
				return toolError("%v", walkErr), nil
			}
		} else {
			search(target)
		}

		result.FileCount = len(files)
		if args.FilesOnly {
			names := make([]string, 0, len(files))
			for name := range files {
				names = append(names, name)
			}
			sort.Strings(names)
			if len(names) == 0 {
				return toolResult("No files matched."), nil
			}
			return &CallToolResult{
				Content:           textContent(strings.Join(names, "\n")),
				StructuredContent: map[string]any{"pattern": args.Pattern, "files": names},
			}, nil
		}
		return &CallToolResult{
			Content:           textContent(result.summarize(before > 0 || after > 0)),
			StructuredContent: result,
		}, nil
	})
}

// summarize renders matches in the familiar grep layout: "path:line:text" for
// a match, "path-line-text" for a context line, and a -- separator between
// blocks that are not adjacent.
func (r grepResult) summarize(withContext bool) string {
	if len(r.Matches) == 0 {
		return "No matches."
	}
	var b strings.Builder
	prevPath, prevLast := "", 0

	for _, m := range r.Matches {
		if withContext && prevPath != "" {
			if m.Path != prevPath || m.FirstLine > prevLast+1 {
				b.WriteString("--\n")
			}
		}
		line := m.FirstLine
		for _, text := range m.Before {
			fmt.Fprintf(&b, "%s-%d-%s\n", m.Path, line, text)
			line++
		}
		fmt.Fprintf(&b, "%s:%d:%s\n", m.Path, m.Line, m.Text)
		line++
		for _, text := range m.After {
			fmt.Fprintf(&b, "%s-%d-%s\n", m.Path, line, text)
			line++
		}
		prevPath, prevLast = m.Path, line-1
	}

	fmt.Fprintf(&b, "\n%d match(es) in %d file(s)", len(r.Matches), r.FileCount)
	if r.Truncated {
		b.WriteString(" (stopped at the match limit)")
	}
	return b.String()
}
