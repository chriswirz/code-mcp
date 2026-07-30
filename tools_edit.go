package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// hunkContext is how many unchanged lines are shown either side of a change
// when an edit reports back what it did.
const hunkContext = 3

// maxHunkLines caps the diff an edit echoes back, so rewriting a whole file
// cannot flood the model's context.
const maxHunkLines = 120

// editOp is one exact-string replacement in one file. multi_edit takes a list
// of these; edit_file is the single-op case.
//
// oldText/newText are accepted as aliases for old_string/new_string, because
// some clients emit the camelCase spelling. The snake_case fields win when both
// are present.
type editOp struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	OldText    string `json:"oldText,omitempty"`
	NewText    string `json:"newText,omitempty"`
	ReplaceAll bool   `json:"replace_all"`
}

// anchor resolves the two spellings down to the pair the edit actually uses.
func (o editOp) anchor() (oldStr, newStr string) {
	oldStr, newStr = o.OldString, o.NewString
	if oldStr == "" {
		oldStr = o.OldText
	}
	if newStr == "" {
		newStr = o.NewText
	}
	return oldStr, newStr
}

// normalizedFor folds the anchor and its replacement onto LF when the
// workspace normalises line endings. The file content is already LF by then -
// ReadFile saw to that - so this is what makes a comparison between the two
// meaningful whatever the client sent.
func (o editOp) normalizedFor(w *Workspace) editOp {
	if !w.NormalizesLineEndings() {
		return o
	}
	o.OldString = w.ToLF(o.OldString)
	o.NewString = w.ToLF(o.NewString)
	o.OldText = w.ToLF(o.OldText)
	o.NewText = w.ToLF(o.NewText)
	return o
}

// editResult is what one replacement did, so the caller can report it.
type editResult struct {
	Content  string
	Count    int
	Adjusted bool // the anchor was re-encoded to the file's line endings to match
}

// dominantLineEnding reports the convention a file mostly uses. Mixed files
// exist, so this is a majority vote rather than a claim about every line.
func dominantLineEnding(content string) string {
	crlf := strings.Count(content, "\r\n")
	lf := strings.Count(content, "\n") - crlf
	if crlf > lf {
		return "\r\n"
	}
	return "\n"
}

// toLineEnding re-encodes text to the given convention, normalising to LF
// first so text that is already CRLF does not gain a second carriage return.
func toLineEnding(s, ending string) string {
	lf := strings.ReplaceAll(s, "\r\n", "\n")
	if ending == "\n" {
		return lf
	}
	return strings.ReplaceAll(lf, "\n", ending)
}

// stripCR drops a trailing carriage return, so a hunk from a CRLF file reads
// cleanly instead of showing a stray ^M at the end of every line.
func stripCR(line string) string {
	return strings.TrimSuffix(line, "\r")
}

// applyEdit replaces old_string with new_string in content. The uniqueness rule
// is what keeps an edit unambiguous: without replace_all the anchor must match
// exactly once, so the model cannot silently change the wrong occurrence.
//
// When normalize is set and the anchor does not match at all, it is retried
// re-encoded to the file's own line endings. That is a fallback rather than an
// unconditional rewrite: an exact match always wins, so an edit that means to
// change a line ending still does exactly what it says.
func applyEdit(content string, op editOp, normalize bool) (editResult, error) {
	oldStr, newStr := op.anchor()
	if oldStr == "" {
		return editResult{}, fmt.Errorf("old_string is required for %s", op.Path)
	}
	count := strings.Count(content, oldStr)
	adjusted := false

	if count == 0 && normalize {
		ending := dominantLineEnding(content)
		if candidate := toLineEnding(oldStr, ending); candidate != oldStr {
			if n := strings.Count(content, candidate); n > 0 {
				oldStr, newStr, count, adjusted = candidate, toLineEnding(newStr, ending), n, true
			}
		}
	}

	switch {
	case count == 0:
		hint := ""
		if !normalize {
			candidate := toLineEnding(oldStr, dominantLineEnding(content))
			if candidate != oldStr && strings.Contains(content, candidate) {
				hint = "; it does match once line endings are reconciled, so set normalize_line_endings"
			}
		}
		return editResult{}, fmt.Errorf("old_string does not appear in %s%s", op.Path, hint)
	case count > 1 && !op.ReplaceAll:
		return editResult{}, fmt.Errorf("old_string appears %d times in %s; add more surrounding context or set replace_all", count, op.Path)
	}

	updated := strings.Replace(content, oldStr, newStr, 1)
	if op.ReplaceAll {
		updated = strings.ReplaceAll(content, oldStr, newStr)
	}
	return editResult{Content: updated, Count: count, Adjusted: adjusted}, nil
}

// changedHunk renders the region that differs between two versions of a file as
// a unified-diff hunk. It trims the common prefix and suffix rather than running
// a full LCS: the region an edit touches is contiguous, so that is enough, and
// it lets the caller see what landed without re-reading the file.
func changedHunk(path, before, after string) string {
	if before == after {
		return fmt.Sprintf("%s: no change", path)
	}
	b := strings.Split(before, "\n")
	a := strings.Split(after, "\n")

	prefix := 0
	for prefix < len(b) && prefix < len(a) && b[prefix] == a[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(b)-prefix && suffix < len(a)-prefix && b[len(b)-1-suffix] == a[len(a)-1-suffix] {
		suffix++
	}

	start := prefix - hunkContext
	if start < 0 {
		start = 0
	}
	oldEnd := len(b) - suffix + hunkContext
	if oldEnd > len(b) {
		oldEnd = len(b)
	}
	newEnd := len(a) - suffix + hunkContext
	if newEnd > len(a) {
		newEnd = len(a)
	}

	lines := make([]string, 0, (oldEnd-start)+(newEnd-start))
	for i := start; i < prefix; i++ {
		lines = append(lines, " "+stripCR(b[i]))
	}
	for i := prefix; i < len(b)-suffix; i++ {
		lines = append(lines, "-"+stripCR(b[i]))
	}
	for i := prefix; i < len(a)-suffix; i++ {
		lines = append(lines, "+"+stripCR(a[i]))
	}
	for i := len(b) - suffix; i < oldEnd; i++ {
		lines = append(lines, " "+stripCR(b[i]))
	}
	if len(lines) > maxHunkLines {
		omitted := len(lines) - maxHunkLines
		lines = lines[:maxHunkLines]
		lines = append(lines, fmt.Sprintf("... %d more line(s) not shown", omitted))
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", start+1, oldEnd-start, start+1, newEnd-start)
	sb.WriteString(strings.Join(lines, "\n"))
	return sb.String()
}

// registerEditTools adds the batch editor. A refactor usually touches several
// places at once, and doing that as one call keeps a file from being left half
// edited when the fourth anchor turns out not to match.
func (s *Server) registerEditTools() {
	ws := s.ws

	s.RegisterTool(Tool{
		Name:  "multi_edit",
		Title: "Apply several edits at once",
		Description: "Apply a list of exact-string replacements across one or more workspace files in a single call. " +
			"Edits are applied in order and each one's old_string must appear exactly once in the file unless replace_all is set. " +
			"Every edit is checked before anything is written, so a bad anchor leaves the workspace untouched. " +
			"Prefer this over repeated edit_file calls when a change touches several places.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"edits"}, map[string]any{
			"edits": map[string]any{
				"type":        "array",
				"description": "Edits to apply, in order. Later edits see the result of earlier ones.",
				"minItems":    1,
				"items": map[string]any{
					"type":     "object",
					"required": []string{"path"},
					"properties": map[string]any{
						"path":        prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
						"old_string":  prop("string", "Exact text to replace, including indentation."),
						"new_string":  prop("string", "Replacement text."),
						"oldText":     prop("string", "Alias for old_string. Ignored when old_string is given."),
						"newText":     prop("string", "Alias for new_string. Ignored when new_string is given."),
						"replace_all": propDefault("boolean", "Replace every occurrence instead of requiring exactly one.", false),
					},
				},
			},
			"dry_run": propDefault("boolean", "Report the diff the edits would produce without writing anything.", false),
			"normalize_line_endings": propDefault("boolean",
				"When an anchor does not match, retry it re-encoded to the file's own line endings. "+
					"An exact match always takes precedence, so an edit that deliberately changes a line ending is unaffected.",
				true),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Edits                []editOp `json:"edits"`
			DryRun               bool     `json:"dry_run"`
			NormalizeLineEndings *bool    `json:"normalize_line_endings"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if len(args.Edits) == 0 {
			return toolError("edits is required and must not be empty"), nil
		}
		normalize := args.NormalizeLineEndings == nil || *args.NormalizeLineEndings
		if !args.DryRun && !ws.AllowWrite {
			return toolError("writes are disabled (workspace.allow_write is false)"), nil
		}

		// Stage every file in memory first. A failure part way down the list
		// then costs nothing, because no file has been touched yet.
		original := map[string]string{}
		staged := map[string]string{}
		counts := map[string]int{}
		adjusted := map[string]bool{}
		var order []string

		for i, op := range args.Edits {
			if op.Path == "" {
				return toolError("edits[%d]: path is required", i), nil
			}
			if _, seen := staged[op.Path]; !seen {
				content, err := ws.ReadFile(op.Path)
				if err != nil {
					return toolError("edits[%d]: %v", i, err), nil
				}
				original[op.Path] = content
				staged[op.Path] = content
				order = append(order, op.Path)
			}
			res, err := applyEdit(staged[op.Path], op.normalizedFor(ws), normalize)
			if err != nil {
				return toolError("edits[%d]: %v (nothing was written)", i, err), nil
			}
			staged[op.Path] = res.Content
			counts[op.Path] += res.Count
			if res.Adjusted {
				adjusted[op.Path] = true
			}
		}

		// Check every destination is writable before writing any of them, so
		// the common failure modes are caught while the tree is still clean.
		if !args.DryRun {
			for _, path := range order {
				if _, err := ws.WriteFileCheck(path); err != nil {
					return toolError("%s: %v (nothing was written)", path, err), nil
				}
			}
		}

		var report strings.Builder
		if args.DryRun {
			report.WriteString("Dry run: nothing was written.\n\n")
		}
		for _, path := range order {
			if !args.DryRun {
				if _, err := ws.WriteFile(path, staged[path]); err != nil {
					return toolError("%s: %v (earlier files in this batch were already written)", path, err), nil
				}
			}
			note := ""
			if adjusted[path] {
				note = " (anchor re-encoded to the file's line endings)"
			}
			name := ws.Canonical(path)
			fmt.Fprintf(&report, "%s: %d replacement(s)%s\n%s\n\n", name, counts[path], note, changedHunk(name, original[path], staged[path]))
		}
		return toolResult(strings.TrimSpace(report.String())), nil
	})
}
