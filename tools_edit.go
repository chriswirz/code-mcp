package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
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
	Path      string  `json:"path"`
	OldString string  `json:"old_string"`
	NewString *string `json:"new_string"`
	OldText   string  `json:"oldText,omitempty"`
	NewText   *string `json:"newText,omitempty"`
	// OldTextSnake and NewTextSnake are the snake_case spellings of the two
	// aliases above, which a model that has seen oldText/newText will write as
	// often as not.
	OldTextSnake string  `json:"old_text,omitempty"`
	NewTextSnake *string `json:"new_text,omitempty"`
	ReplaceAll   bool    `json:"replace_all"`

	// Extra holds the keys this edit carried that none of the fields claim.
	// A replacement that arrived under a name nothing reads is the difference
	// between an edit and a deletion, so the error gets to name it.
	Extra []string `json:"-"`
}

// editOpFields are the keys an edit may carry, including the camelCase
// spellings decodeArgs aliases for. Anything else is collected into Extra.
var editOpFields = map[string]bool{
	"path": true, "old_string": true, "new_string": true,
	"oldtext": true, "newtext": true, "old_text": true, "new_text": true,
	"oldstring": true, "newstring": true,
	"replace_all": true, "replaceall": true,
}

// UnmarshalJSON decodes an edit and remembers the keys it did not recognise.
func (o *editOp) UnmarshalJSON(data []byte) error {
	type plain editOp
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*o = editOp(decoded)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for key := range raw {
		if !editOpFields[strings.ToLower(key)] {
			o.Extra = append(o.Extra, key)
		}
	}
	sort.Strings(o.Extra)
	return nil
}

// anchor resolves the several spellings down to the pair the edit actually
// uses, and reports whether a replacement was supplied at all.
//
// Absent and empty are different answers. An empty new_string means "delete
// this", which is a real edit; an absent one means the replacement did not
// arrive - misspelled, dropped, or never written - and applying it anyway
// deletes the code the caller meant to change. That used to be silent, and
// silent is the worst thing it could be.
func (o editOp) anchor() (oldStr, newStr string, hasNew bool) {
	oldStr = firstNonEmpty(o.OldString, o.OldText, o.OldTextSnake)
	for _, candidate := range []*string{o.NewString, o.NewText, o.NewTextSnake} {
		if candidate != nil {
			return oldStr, *candidate, true
		}
	}
	return oldStr, "", false
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
	o.OldText = w.ToLF(o.OldText)
	o.OldTextSnake = w.ToLF(o.OldTextSnake)
	o.NewString = normalizedPointer(w, o.NewString)
	o.NewText = normalizedPointer(w, o.NewText)
	o.NewTextSnake = normalizedPointer(w, o.NewTextSnake)
	return o
}

// normalizedPointer folds a replacement onto LF while keeping absent absent.
func normalizedPointer(w *Workspace, value *string) *string {
	if value == nil {
		return nil
	}
	folded := w.ToLF(*value)
	return &folded
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
	oldStr, newStr, hasNew := op.anchor()
	if oldStr == "" {
		return editResult{}, fmt.Errorf("old_string is required for %s", op.Path)
	}
	if !hasNew {
		return editResult{}, missingReplacementError(op)
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
	// Read under the lock: a configuration reload can replace the workspace
	// while a request is in flight.
	ws := s.workspace()

	s.RegisterTool(Tool{
		Name:  "multi_edit",
		Title: "Apply several edits at once",
		Description: "Apply a list of exact-string replacements across one or more workspace files in a single call. " +
			"Edits are applied in order and each one's old_string must appear exactly once in the file unless replace_all is set. " +
			"Every edit is checked before anything is written, so a bad anchor leaves the workspace untouched. " +
			"Prefer this over repeated edit_file calls when a change touches several places. To delete code, use remove_sections instead.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"edits"}, map[string]any{
			"edits": map[string]any{
				"type":        "array",
				"description": "Edits to apply, in order. Later edits see the result of earlier ones.",
				"minItems":    1,
				"items": map[string]any{
					"type":     "object",
					"required": []string{"path", "old_string", "new_string"},
					"properties": map[string]any{
						"path":       prop("string", "File relative to the workspace root. Paths are workspace-relative; an absolute path such as \"/README.md\" is interpreted relative to the workspace root, never the root of the filesystem."),
						"old_string": prop("string", "Exact text to replace, including indentation."),
						"new_string": prop("string", "Replacement text. Required: an edit that carries no "+
							"new_string is refused rather than applied, since that would delete old_string "+
							"instead of changing it. Pass an empty string to delete deliberately."),
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
		result := applyEditBatch(ws, args.Edits, "edits", "replacement(s)", args.DryRun, normalize)
		if !result.IsError {
			for _, op := range args.Edits {
				if _, newStr, hasNew := op.anchor(); hasNew && newStr == "" {
					result = withNamingWarning(result, removalWarning)
					break
				}
			}
		}
		return result, nil
	})

	s.RegisterTool(Tool{
		Name:  "remove_sections",
		Title: "Remove sections of code",
		Description: "Delete exact sections of text from one or more workspace files in a single call. " +
			"Use this instead of multi_edit or edit_file with an empty new_string whenever the intent is to remove code. " +
			"Each section must appear exactly once in its file unless replace_all is set, and every removal is checked " +
			"before anything is written, so a section that does not match leaves the workspace untouched.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"sections"}, map[string]any{
			"path": prop("string", "Default file for sections that do not name their own path, relative to the workspace root."),
			"sections": map[string]any{
				"type":        "array",
				"description": "Sections to remove, in order. Later removals see the result of earlier ones.",
				"minItems":    1,
				"items": map[string]any{
					"type":     "object",
					"required": []string{"old_string"},
					"properties": map[string]any{
						"path":        prop("string", "File relative to the workspace root. Falls back to the top-level path."),
						"old_string":  prop("string", "Exact text to remove, including indentation and line breaks."),
						"replace_all": propDefault("boolean", "Remove every occurrence instead of requiring exactly one.", false),
					},
				},
			},
			"dry_run": propDefault("boolean", "Report the diff the removals would produce without writing anything.", false),
			"normalize_line_endings": propDefault("boolean",
				"When a section does not match, retry it re-encoded to the file's own line endings.", true),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path     string `json:"path"`
			Sections []struct {
				Path       string `json:"path"`
				OldString  string `json:"old_string"`
				ReplaceAll bool   `json:"replace_all"`
			} `json:"sections"`
			DryRun               bool  `json:"dry_run"`
			NormalizeLineEndings *bool `json:"normalize_line_endings"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if len(args.Sections) == 0 {
			return toolError("sections is required and must not be empty"), nil
		}
		ops := make([]editOp, 0, len(args.Sections))
		for i, section := range args.Sections {
			if section.OldString == "" {
				return toolError("sections[%d]: old_string is required (nothing was written)", i), nil
			}
			path := firstNonEmpty(section.Path, args.Path)
			if path == "" {
				return toolError("sections[%d]: path is required, on the section or at the top level (nothing was written)", i), nil
			}
			ops = append(ops, editOp{
				Path:       path,
				OldString:  section.OldString,
				NewString:  replacement(""),
				ReplaceAll: section.ReplaceAll,
			})
		}
		normalize := args.NormalizeLineEndings == nil || *args.NormalizeLineEndings
		return applyEditBatch(ws, ops, "sections", "removal(s)", args.DryRun, normalize), nil
	})

	s.RegisterTool(Tool{
		Name:  "rollback",
		Title: "Roll a file back one state",
		Description: "Undo the most recent change this server made to a file, restoring the state before it. " +
			"Each call steps back one more state, up to the configured number kept per file (workspace.rollback_depth). " +
			"History is held in memory and covers writes made through this server only.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path":           prop("string", "File relative to the workspace root."),
			"return_content": propDefault("boolean", "Include the full contents of the file after it is rolled back.", false),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path          string `json:"path"`
			ReturnContent bool   `json:"return_content"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		if !ws.AllowWrite {
			return toolError("writes are disabled (workspace.allow_write is false)"), nil
		}
		abs, err := ws.Resolve(args.Path)
		if err != nil {
			if abs, err = ws.WriteFileCheck(args.Path); err != nil {
				return pathToolError(ws, args.Path, err), nil
			}
		}
		name := ws.Canonical(abs)
		before, _ := os.ReadFile(abs)
		state, remaining, err := ws.History.Pop(abs)
		if err != nil {
			return toolError("%s: %v", name, err), nil
		}
		if !state.existed {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				return toolError("%s: %v", name, err), nil
			}
			return toolResult(fmt.Sprintf("Rolled back %s: the file did not exist before, so it was removed. %d earlier state(s) remain.",
				name, remaining)), nil
		}
		if err := os.WriteFile(abs, state.data, 0o644); err != nil {
			return toolError("%s: %v", name, err), nil
		}
		var report strings.Builder
		fmt.Fprintf(&report, "Rolled back %s one state. %d earlier state(s) remain.\n%s",
			name, remaining, changedHunk(name, string(before), string(state.data)))
		if args.ReturnContent {
			content, err := ws.ReadFile(abs)
			if err != nil {
				return toolError("%s was rolled back but could not be read: %v", name, err), nil
			}
			fmt.Fprintf(&report, "\n\nContents of %s:\n%s", name, content)
		}
		return toolResult(report.String()), nil
	})
}

// removalWarning steers a caller that deleted through an edit toward the tool
// meant for it.
const removalWarning = "Warning: an empty new_string was used to delete text. The deletion was performed, " +
	"but use remove_sections to remove code in future calls."

// applyEditBatch stages a list of edits in memory and writes them only once
// every one has matched. label names the argument list in errors ("edits[2]")
// and unit names what each change is in the report.
func applyEditBatch(ws *Workspace, ops []editOp, label, unit string, dryRun, normalize bool) *CallToolResult {
	if !dryRun && !ws.AllowWrite {
		return toolError("writes are disabled (workspace.allow_write is false)")
	}

	// Stage every file in memory first. A failure part way down the list
	// then costs nothing, because no file has been touched yet.
	original := map[string]string{}
	staged := map[string]string{}
	counts := map[string]int{}
	adjusted := map[string]bool{}
	var order []string
	var notes []string

	for i, op := range ops {
		if op.Path == "" {
			return toolError("%s[%d]: path is required", label, i)
		}
		// Two edits can name one file two ways - "a.go" and "./a.go" - and
		// staging them separately would read the second from disk and then
		// write it over the first, losing an edit the call reported as
		// applied. The workspace's own name for the file is the key.
		// A path that is not there but names exactly one file in the
		// workspace is taken to mean that file, once, for every edit in
		// the batch that spells it the same way.
		actual, note, resolveErr := ws.ResolveForRead(op.Path)
		if resolveErr != nil {
			return prefixToolError(pathToolError(ws, op.Path, resolveErr), fmt.Sprintf("%s[%d]", label, i))
		}
		op.Path = actual
		key := ws.Canonical(actual)
		if _, seen := staged[key]; !seen {
			if note != "" {
				notes = append(notes, note)
			}
			content, err := ws.ReadFile(actual)
			if err != nil {
				return prefixToolError(pathToolError(ws, actual, err), fmt.Sprintf("%s[%d]", label, i))
			}
			original[key] = content
			staged[key] = content
			order = append(order, key)
		}
		res, err := applyEdit(staged[key], op.normalizedFor(ws), normalize)
		if err != nil {
			return toolError("%s[%d]: %v (nothing was written)", label, i, err)
		}
		staged[key] = res.Content
		counts[key] += res.Count
		if res.Adjusted {
			adjusted[key] = true
		}
	}

	// Check every destination is writable before writing any of them, so
	// the common failure modes are caught while the tree is still clean.
	if !dryRun {
		for _, path := range order {
			if _, err := ws.WriteFileCheck(path); err != nil {
				return toolError("%s: %v (nothing was written)", path, err)
			}
		}
	}

	var report strings.Builder
	for _, note := range notes {
		report.WriteString(note + "\n\n")
	}
	if dryRun {
		report.WriteString("Dry run: nothing was written.\n\n")
	}
	for _, path := range order {
		if !dryRun {
			if _, err := ws.WriteFile(path, staged[path]); err != nil {
				return toolError("%s: %v (earlier files in this batch were already written)", path, err)
			}
		}
		note := ""
		if adjusted[path] {
			note = " (anchor re-encoded to the file's line endings)"
		}
		name := ws.Canonical(path)
		fmt.Fprintf(&report, "%s: %d %s%s\n%s\n\n", name, counts[path], unit, note, changedHunk(name, original[path], staged[path]))
	}
	return toolResult(strings.TrimSpace(report.String()))
}

// missingReplacementError explains an edit that says what to remove but not
// what to put there. It names the keys the edit did carry, because the usual
// cause is a replacement written under a name nothing reads.
func missingReplacementError(op editOp) error {
	if len(op.Extra) > 0 {
		return fmt.Errorf("the edit for %s has no new_string, so it would delete %q rather than change it. "+
			"It carries %s, which this tool does not read: put the replacement in new_string",
			op.Path, truncateForError(firstNonEmpty(op.OldString, op.OldText, op.OldTextSnake)),
			quoteList(op.Extra))
	}
	return fmt.Errorf("the edit for %s has no new_string, so it would delete %q rather than change it. "+
		"Pass the replacement in new_string, or use remove_sections (or an empty string) if deleting is what you meant",
		op.Path, truncateForError(firstNonEmpty(op.OldString, op.OldText, op.OldTextSnake)))
}

// quoteList renders key names for an error message.
func quoteList(names []string) string {
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, strconv.Quote(name))
	}
	if len(quoted) == 1 {
		return "a key called " + quoted[0]
	}
	return "keys called " + strings.Join(quoted[:len(quoted)-1], ", ") + " and " + quoted[len(quoted)-1]
}

// truncateForError keeps an anchor quotable in a one-line message.
func truncateForError(text string) string {
	const limit = 60
	text = strings.ReplaceAll(text, "\n", "\n")
	if len(text) > limit {
		return text[:limit] + "..."
	}
	return text
}

// replacement builds the pointer an editOp's replacement fields take, for
// callers that construct an edit in code rather than decoding one from JSON.
// The pointer is what distinguishes "replace with nothing" from "no
// replacement was supplied".
func replacement(s string) *string { return &s }
