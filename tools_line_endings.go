package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lineEndingScope is how far fix_line_endings reaches: one file, the files
// directly in a directory, a whole subtree, or the workspace.
type lineEndingScope string

const (
	scopeFile      lineEndingScope = "file"
	scopeFolder    lineEndingScope = "folder"
	scopeTree      lineEndingScope = "tree"
	scopeWorkspace lineEndingScope = "workspace"
)

// lineEndingPlan is what the tool worked out it would do, and - after a run
// that was not a dry run - what it did. The plan is always computed in full
// before anything is written, so the limit is enforced and reported against
// the real number of files rather than discovered part way through.
type lineEndingPlan struct {
	Scope       string   `json:"scope"`
	Path        string   `json:"path"`
	Ending      string   `json:"ending"`
	Mask        string   `json:"mask,omitempty"`
	Examined    int      `json:"files_examined"`
	Changed     int      `json:"files_changed"`
	Files       []string `json:"files,omitempty"`
	Truncated   int      `json:"files_not_listed,omitempty"`
	SkippedBin  []string `json:"skipped_binary,omitempty"`
	SkippedBig  []string `json:"skipped_too_large,omitempty"`
	DryRun      bool     `json:"dry_run"`
	Limit       int      `json:"limit"`
	AlreadyDone bool     `json:"already_consistent"`
}

// maxListedFiles caps the file list a plan echoes back, so converting a large
// tree does not flood the model's context with paths it did not ask for.
const maxListedFiles = 50

// registerLineEndingTools adds fix_line_endings: the bulk counterpart to the
// per-file normalisation workspace.line_endings does. The setting governs what
// this server writes from now on; this tool is how the files already on disk
// are brought into line.
func (s *Server) registerLineEndingTools(cfg WorkspaceConfig) {
	limit := cfg.MaxLineEndingFiles

	s.RegisterTool(Tool{
		Name:  "fix_line_endings",
		Title: "Normalize line endings",
		Description: fmt.Sprintf(
			"Rewrite files so every line ends the same way. scope selects how far it reaches: "+
				"%q (one file), %q (the files directly in a directory), %q (a whole subtree) or "+
				"%q (everything under the workspace root). "+
				"The effect is always worked out in full before anything is written: nothing is "+
				"touched if the plan exceeds the configured limit of %d files, and dry_run reports "+
				"the plan without writing. Binary files, excluded paths and files over the workspace "+
				"size limit are never rewritten, and are named in the result. mask narrows it further, "+
				"for example \"*.js\" or \"*.js,*.ts\".",
			scopeFile, scopeFolder, scopeTree, scopeWorkspace, limit),
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema(nil, map[string]any{
			"path": propDefault("string",
				"File or directory to act on, relative to the workspace root. Ignored for the workspace scope.", "."),
			"scope": propDefault("string",
				fmt.Sprintf("How far to reach: %q, %q, %q or %q.", scopeFile, scopeFolder, scopeTree, scopeWorkspace),
				string(scopeFile)),
			"ending": propDefault("string",
				"Line ending to write: \"lf\", \"crlf\" or \"native\". Defaults to workspace.line_endings, "+
					"or the platform's convention when the workspace preserves line endings.", ""),
			"mask": prop("string",
				"File mask: only touch files whose name matches, for example \"*.js\". "+
					"Several patterns can be given, separated by commas (\"*.js,*.ts\"), and a file "+
					"matching any of them is included. Ignored by the file scope, which names its file."),
			"dry_run": propDefault("boolean",
				"Report which files would change without writing any of them.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path   string `json:"path"`
			Scope  string `json:"scope"`
			Ending string `json:"ending"`
			Mask   string `json:"mask"`
			Glob   string `json:"glob"` // alias: "glob" is what the other tools call it
			DryRun bool   `json:"dry_run"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		ws := s.workspace()
		if !ws.AllowWrite && !args.DryRun {
			return toolError("writes are disabled (workspace.allow_write is false); pass dry_run to see the plan"), nil
		}

		scope, err := parseLineEndingScope(args.Scope)
		if err != nil {
			return toolError("%v", err), nil
		}
		ending, err := parseLineEndingName(args.Ending, ws)
		if err != nil {
			return toolError("%v", err), nil
		}
		mask := args.Mask
		if mask == "" {
			mask = args.Glob
		}
		patterns, err := parseFileMask(mask)
		if err != nil {
			return toolError("%v", err), nil
		}

		// The workspace scope is the one that can run away with itself, so it
		// is refused on a server whose file tools are not fenced at all.
		if scope == scopeWorkspace && ws.Unrestricted {
			return toolError("the workspace scope is refused when the workspace is unrestricted " +
				"(workspace.root is \".\"), because it would mean rewriting files across the whole " +
				"machine; name a directory and use the tree scope instead"), nil
		}

		target := args.Path
		if scope == scopeWorkspace || target == "" {
			target = "."
		}
		abs, err := ws.Resolve(target)
		if err != nil {
			return toolError("%v", err), nil
		}

		plan, err := planLineEndings(ws, abs, scope, ending, patterns)
		if err != nil {
			return toolError("%v", err), nil
		}
		plan.Scope = string(scope)
		plan.Path = ws.Rel(abs)
		plan.Ending = lineEndingLabel(ending)
		plan.Mask = strings.Join(patterns, ",")
		plan.DryRun = args.DryRun
		plan.Limit = limit

		if plan.Changed > limit {
			return toolError("%d files would be rewritten, over the limit of %d "+
				"(workspace.max_line_ending_files); nothing was written. Narrow the scope or the "+
				"mask, or raise the limit in config.json.", plan.Changed, limit), nil
		}
		if plan.Changed == 0 {
			plan.AlreadyDone = true
			plan.trim()
			return toolResultJSON(plan), nil
		}

		if !args.DryRun {
			// The plan named exactly these files, and each is rewritten whole:
			// a failure part way through leaves the files before it converted
			// and the rest untouched, which the error says.
			for i, rel := range plan.Files {
				if err := rewriteLineEndings(ws, rel, ending); err != nil {
					return toolError("%v (%d of %d files had already been rewritten)",
						err, i, len(plan.Files)), nil
				}
			}
		}
		plan.trim()
		return toolResultJSON(plan), nil
	})
}

// trim caps the file list after the count has been used, so a large conversion
// reports how many it touched without listing every path.
func (p *lineEndingPlan) trim() {
	if len(p.Files) > maxListedFiles {
		p.Truncated = len(p.Files) - maxListedFiles
		p.Files = p.Files[:maxListedFiles]
	}
	if len(p.SkippedBin) > maxListedFiles {
		p.SkippedBin = p.SkippedBin[:maxListedFiles]
	}
	if len(p.SkippedBig) > maxListedFiles {
		p.SkippedBig = p.SkippedBig[:maxListedFiles]
	}
}

// planLineEndings works out which files would actually change. Every candidate
// is read and compared, so the count is what the write would really do rather
// than a guess from the file list.
func planLineEndings(ws *Workspace, abs string, scope lineEndingScope, ending string, patterns []string) (lineEndingPlan, error) {
	var plan lineEndingPlan

	info, err := os.Stat(abs)
	if err != nil {
		return plan, err
	}
	if scope == scopeFile && info.IsDir() {
		return plan, fmt.Errorf("%s is a directory; use the folder, tree or workspace scope", ws.Rel(abs))
	}
	if scope != scopeFile && !info.IsDir() {
		return plan, fmt.Errorf("%s is a file; use the file scope", ws.Rel(abs))
	}

	consider := func(path string) {
		plan.Examined++
		rel := ws.Rel(path)
		stat, statErr := os.Stat(path)
		if statErr != nil {
			return
		}
		if stat.Size() > ws.MaxFileBytes {
			plan.SkippedBig = append(plan.SkippedBig, rel)
			return
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return
		}
		if isBinary(data) {
			// Rewriting a byte sequence that only looks like a line ending
			// would corrupt the file, so binaries are never touched.
			plan.SkippedBin = append(plan.SkippedBin, rel)
			return
		}
		if converted := toLineEnding(string(data), ending); converted != string(data) {
			plan.Files = append(plan.Files, rel)
		}
	}

	matches := func(name string) bool { return maskMatches(patterns, name) }

	switch scope {
	case scopeFile:
		consider(abs)
	case scopeFolder:
		entries, readErr := os.ReadDir(abs)
		if readErr != nil {
			return plan, readErr
		}
		for _, d := range entries {
			if d.IsDir() {
				continue
			}
			full := filepath.Join(abs, d.Name())
			if ws.IsExcluded(full) || ws.IsExcludedUnder(full, abs) || !matches(d.Name()) {
				continue
			}
			consider(full)
		}
	case scopeTree, scopeWorkspace:
		// The walk is not cut short at the limit: the whole point is to report
		// the true number of affected files before anything is written, and a
		// count that stopped early would understate it.
		walkErr := ws.Walk(abs, func(path string, d fs.DirEntry) bool {
			if matches(d.Name()) {
				consider(path)
			}
			return true
		})
		if walkErr != nil {
			return plan, walkErr
		}
	}

	sort.Strings(plan.Files)
	sort.Strings(plan.SkippedBin)
	sort.Strings(plan.SkippedBig)
	plan.Changed = len(plan.Files)
	return plan, nil
}

// rewriteLineEndings converts one file in place, keeping its mode. The path is
// resolved through the workspace again so the write is contained even if the
// tree changed between the plan and the write.
func rewriteLineEndings(ws *Workspace, rel, ending string) error {
	abs, err := ws.Resolve(rel)
	if err != nil {
		return err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	converted := toLineEnding(string(data), ending)
	if converted == string(data) {
		return nil
	}
	if err := os.WriteFile(abs, []byte(converted), info.Mode().Perm()); err != nil {
		return fmt.Errorf("could not rewrite %s: %w", rel, err)
	}
	return nil
}

// parseFileMask splits a comma-separated mask into patterns and checks each
// one, so a malformed pattern is reported rather than silently matching
// nothing. An empty mask means every file.
func parseFileMask(mask string) ([]string, error) {
	var patterns []string
	for _, part := range strings.Split(mask, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if _, err := filepath.Match(part, "probe"); err != nil {
			return nil, fmt.Errorf("mask %q is not a valid pattern: %v", part, err)
		}
		patterns = append(patterns, part)
	}
	return patterns, nil
}

// maskMatches reports whether a file name matches any pattern in the mask. No
// patterns means no mask, which matches everything.
func maskMatches(patterns []string, name string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if ok, err := filepath.Match(pattern, name); err == nil && ok {
			return true
		}
	}
	return false
}

// parseLineEndingScope maps the argument onto a scope, defaulting to the
// narrowest one so an omitted scope cannot rewrite more than the caller meant.
func parseLineEndingScope(s string) (lineEndingScope, error) {
	switch lineEndingScope(strings.ToLower(strings.TrimSpace(s))) {
	case "", scopeFile:
		return scopeFile, nil
	case scopeFolder:
		return scopeFolder, nil
	case scopeTree:
		return scopeTree, nil
	case scopeWorkspace:
		return scopeWorkspace, nil
	}
	return "", fmt.Errorf("scope: want %q, %q, %q or %q, got %q",
		scopeFile, scopeFolder, scopeTree, scopeWorkspace, s)
}

// parseLineEndingName resolves the ending to write. An omitted one follows the
// workspace setting, so the tool and the setting cannot disagree by default.
func parseLineEndingName(name string, ws *Workspace) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "lf", "unix":
		return "\n", nil
	case "crlf", "windows", "dos":
		return "\r\n", nil
	case "native", "platform", "os":
		return nativeLineEnding(), nil
	case "":
		if ending := ws.DiskLineEnding(); ending != "" {
			return ending, nil
		}
		return nativeLineEnding(), nil
	}
	return "", fmt.Errorf("ending: want %q, %q or %q, got %q", "lf", "crlf", "native", name)
}

// lineEndingLabel names an ending for the result, since "\r\n" in JSON is not
// something anyone wants to read.
func lineEndingLabel(ending string) string {
	if ending == "\r\n" {
		return "crlf"
	}
	return "lf"
}
