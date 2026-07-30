package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// diffFileResult is what apply_diff reports per file.
type diffFileResult struct {
	Path      string       `json:"path"`
	Action    string       `json:"action"` // modified, created, deleted, renamed
	From      string       `json:"from,omitempty"`
	Hunks     []hunkResult `json:"hunks,omitempty"`
	Additions int          `json:"additions"`
	Deletions int          `json:"deletions"`
	// Notes carry anything the patch got away with but should not have, such
	// as a hunk header whose counts did not match its body.
	Notes []string `json:"notes,omitempty"`
}

// diffResult is the structured content of an apply_diff call.
type diffResult struct {
	Applied   bool             `json:"applied"`
	DryRun    bool             `json:"dry_run,omitempty"`
	Files     []diffFileResult `json:"files"`
	Additions int              `json:"additions"`
	Deletions int              `json:"deletions"`
}

// pendingWrite is one file change held back until every file has been checked.
type pendingWrite struct {
	path    string
	content string
	delete  bool
	result  diffFileResult
}

// registerDiffTools adds apply_diff, which applies a unified diff to the
// workspace. It is the right tool for a change that spans several files or
// several places in one file, where a sequence of edit_file calls would be
// both slower and easier to get half-done.
func (s *Server) registerDiffTools() {
	s.RegisterTool(Tool{
		Name:  "apply_diff",
		Title: "Apply a unified diff",
		Description: "Apply a patch in unified diff format to the workspace. Handles multiple files, " +
			"file creation and deletion, and renames. Either the whole patch applies or nothing is " +
			"written. Use dry_run first when you are unsure the patch is against the current state " +
			"of the files.\n\n" +
			"What has to be right is the body of each hunk: every context line must match the file, " +
			"including its leading space - a context line without one cannot be told from prose, and " +
			"is refused. What need not be right: the line numbers and the counts in the @@ header " +
			"(the body is trusted over them), the order of the hunks, and whether the ---/+++ pair " +
			"is there at all when a diff --git line names the paths. A file may be given in more " +
			"than one section; the sections apply in order.\n\n" +
			"When a hunk does not apply, the error names the line that disagreed, or where the " +
			"context actually is - patch that rather than resending the same diff.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"diff"}, map[string]any{
			"diff": prop("string", "The patch, in unified diff format: --- and +++ header lines followed by @@ hunks."),
			"strip": propDefault("integer",
				"Leading path components to drop, as patch -pN. 1 removes the a/ and b/ prefixes git produces.", 1),
			"dry_run": propDefault("boolean",
				"Check that the patch applies and report what it would do, without writing anything.", false),
			"ignore_whitespace": propDefault("boolean",
				"Match context lines ignoring leading and trailing whitespace. The file's own indentation is kept.", false),
			"max_offset": propDefault("integer",
				"How far from the line a hunk header names to search for its context. 0 searches the whole file.", 200),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Diff             string `json:"diff"`
			Strip            *int   `json:"strip"`
			DryRun           bool   `json:"dry_run"`
			IgnoreWhitespace bool   `json:"ignore_whitespace"`
			MaxOffset        *int   `json:"max_offset"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.Diff) == "" {
			return toolError("diff is required"), nil
		}
		// One reference to the workspace for the whole call, read under the
		// lock: a configuration reload can replace it mid-request.
		ws := s.workspace()
		// The files this patch is matched against are read as LF when the
		// workspace normalises, so the patch has to be read the same way or
		// its context lines would never match.
		args.Diff = ws.ToLF(args.Diff)
		if !ws.AllowWrite && !args.DryRun {
			return toolError("writes are disabled (workspace.allow_write is false); pass dry_run to check the patch"), nil
		}
		strip := 1
		if args.Strip != nil {
			strip = *args.Strip
		}
		maxOffset := 200
		if args.MaxOffset != nil {
			maxOffset = *args.MaxOffset
		}
		opts := applyOptions{MaxOffset: maxOffset, IgnoreWhitespace: args.IgnoreWhitespace}

		files, err := parseUnifiedDiff(args.Diff, strip)
		if err != nil {
			return toolError("could not parse the diff: %v", err), nil
		}

		// Everything is computed and checked before anything is written, so a
		// patch that fails on its third file does not leave the first two
		// applied.
		writes, corrections, failure := s.planDiff(files, opts)
		if failure != nil {
			return failure, nil
		}

		result := diffResult{Applied: !args.DryRun, DryRun: args.DryRun}
		for _, w := range writes {
			result.Files = append(result.Files, w.result)
			result.Additions += w.result.Additions
			result.Deletions += w.result.Deletions
		}

		if !args.DryRun {
			for _, w := range writes {
				if w.delete {
					abs, resolveErr := ws.Resolve(w.path)
					if resolveErr != nil {
						return toolError("%v", resolveErr), nil
					}
					if removeErr := os.Remove(abs); removeErr != nil && !os.IsNotExist(removeErr) {
						return toolError("could not delete %s: %v", w.path, removeErr), nil
					}
					continue
				}
				if _, writeErr := ws.WriteFile(w.path, w.content); writeErr != nil {
					return toolError("could not write %s: %v", w.path, writeErr), nil
				}
			}
		}
		return withNote(&CallToolResult{
			Content:           textContent(result.summarize()),
			StructuredContent: result,
		}, strings.Join(corrections, "\n")), nil
	})
}

// planDiff turns parsed file changes into the writes that would apply them,
// or returns the tool error explaining why one of them cannot.
//
// Sections are applied in order against a running picture of the workspace,
// not each against the file on disk: a patch that touches one file in two
// sections, or deletes a file and recreates it, means what it says instead of
// silently keeping only the last section.
func (s *Server) planDiff(files []fileDiff, opts applyOptions) ([]pendingWrite, []string, *CallToolResult) {
	ws := s.workspace()
	plan := newDiffPlan()
	var corrections []string

	for _, f := range files {
		path := f.Path()
		// Resolve through the workspace so a patch cannot write outside it,
		// whatever paths its headers name.
		if _, err := ws.Resolve(path); err != nil {
			if _, newErr := ws.WriteFileCheck(path); newErr != nil {
				return nil, nil, toolError("%s: %v", path, newErr)
			}
		}

		var old string
		switch {
		case f.IsNew:
			exists, err := s.exists(plan, path)
			if err != nil {
				return nil, nil, toolError("%s: %v", path, err)
			}
			if exists {
				return nil, nil, toolError("%s already exists, but the patch creates it "+
					"(its old side is /dev/null)", path)
			}
		default:
			readPath := f.OldPath
			if readPath == "" {
				readPath = path
			}
			// A section whose file is not there, but whose name matches
			// exactly one file in the workspace, is taken to mean that file.
			// Not for a rename: there the new path is the caller's own choice
			// and moving it somewhere else would be a second guess.
			if !f.IsRename() && !plan.has(readPath) {
				if actual, note, resolveErr := ws.ResolveForRead(readPath); resolveErr == nil && note != "" {
					corrections = append(corrections, note)
					readPath, path = actual, actual
					f.OldPath, f.NewPath = actual, actual
				}
			}
			content, err := s.readForPatch(plan, readPath)
			if err != nil {
				if isNotFound(err) {
					// A patch against a file that is not there is nearly always
					// a path the patch got wrong, not a file to create: a
					// creating patch says so with /dev/null on its old side.
					return nil, nil, pathToolError(ws, readPath, err)
				}
				return nil, nil, toolError("%s: %v", readPath, err)
			}
			old = content
		}

		updated, hunks, err := applyHunks(old, f.Hunks, opts)
		if err != nil {
			return nil, nil, toolError("%s: %v", path, err)
		}

		// The patch's own headers name the file; report it as the workspace
		// resolved it, so a rooted or re-anchored path is not echoed back.
		result := diffFileResult{Path: ws.Canonical(path), Hunks: hunks}
		for _, h := range f.Hunks {
			if h.Miscounted != "" {
				result.Notes = append(result.Notes, h.Miscounted)
			}
			for _, hl := range h.Lines {
				switch hl.Op {
				case '+':
					result.Additions++
				case '-':
					result.Deletions++
				}
			}
		}
		switch {
		case f.IsDelete:
			result.Action = "deleted"
			plan.put(pendingWrite{path: f.OldPath, delete: true, result: result})
		case f.IsNew:
			result.Action = "created"
			plan.put(pendingWrite{path: path, content: updated, result: result})
		case f.IsRename():
			result.Action = "renamed"
			result.From = ws.Canonical(f.OldPath)
			plan.put(pendingWrite{path: path, content: updated, result: result})
			plan.put(pendingWrite{path: f.OldPath, delete: true,
				result: diffFileResult{Path: ws.Canonical(f.OldPath), Action: "renamed-from"}})
		default:
			result.Action = "modified"
			plan.put(pendingWrite{path: path, content: updated, result: result})
		}
	}
	return plan.writes(), corrections, nil
}

// diffPlan is the picture of the workspace as the patch has changed it so far:
// one entry per path, in the order the paths were first touched.
type diffPlan struct {
	order []string
	byKey map[string]pendingWrite
}

func newDiffPlan() *diffPlan {
	return &diffPlan{byKey: map[string]pendingWrite{}}
}

// put records a change, replacing any earlier one for the same path so a file
// is written once, with everything the patch did to it.
func (p *diffPlan) put(w pendingWrite) {
	key := planKey(w.path)
	if prior, ok := p.byKey[key]; ok {
		// A second section for a file already changed keeps the first
		// section's counts, so the totals reflect the whole patch.
		w.result.Additions += prior.result.Additions
		w.result.Deletions += prior.result.Deletions
		w.result.Hunks = append(append([]hunkResult{}, prior.result.Hunks...), w.result.Hunks...)
		w.result.Notes = append(append([]string{}, prior.result.Notes...), w.result.Notes...)
		if prior.result.Action == "created" && w.result.Action == "modified" {
			w.result.Action = "created"
		}
	} else {
		p.order = append(p.order, key)
	}
	p.byKey[key] = w
}

// has reports whether an earlier section of the same patch already touched
// this path, in which case it is a real path whatever is on disk.
func (p *diffPlan) has(path string) bool {
	_, ok := p.byKey[planKey(path)]
	return ok
}

func (p *diffPlan) writes() []pendingWrite {
	out := make([]pendingWrite, 0, len(p.order))
	for _, key := range p.order {
		out = append(out, p.byKey[key])
	}
	return out
}

// planKey normalises a path enough that two spellings of the same file - "sub/a.go"
// and "./sub/a.go" - land on one entry.
func planKey(path string) string {
	return strings.TrimPrefix(strings.ReplaceAll(path, "\\", "/"), "./")
}

// readForPatch returns the content a hunk should be matched against: what an
// earlier section of the same patch left behind, or the file on disk.
func (s *Server) readForPatch(plan *diffPlan, path string) (string, error) {
	ws := s.workspace()
	if w, ok := plan.byKey[planKey(path)]; ok {
		if w.delete {
			return "", fmt.Errorf("this patch deletes the file earlier on, so there is nothing left to patch")
		}
		return w.content, nil
	}
	return ws.ReadFile(path)
}

// exists reports whether a path is there once the patch so far is taken into
// account, so deleting a file and recreating it in one patch is allowed.
func (s *Server) exists(plan *diffPlan, path string) (bool, error) {
	ws := s.workspace()
	if w, ok := plan.byKey[planKey(path)]; ok {
		return !w.delete, nil
	}
	if _, err := ws.Stat(path); err == nil {
		return true, nil
	}
	return false, nil
}

// summarize renders the result as the text block of the tool result.
func (r diffResult) summarize() string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("Dry run: the patch applies cleanly. Nothing was written.\n\n")
	} else {
		b.WriteString("Patch applied.\n\n")
	}
	for _, f := range r.Files {
		if f.Action == "renamed-from" {
			continue
		}
		switch f.Action {
		case "renamed":
			fmt.Fprintf(&b, "  %s -> %s  (+%d -%d)\n", f.From, f.Path, f.Additions, f.Deletions)
		case "deleted":
			fmt.Fprintf(&b, "  %s  deleted\n", f.Path)
		case "created":
			fmt.Fprintf(&b, "  %s  created (+%d)\n", f.Path, f.Additions)
		default:
			fmt.Fprintf(&b, "  %s  (+%d -%d)\n", f.Path, f.Additions, f.Deletions)
		}
		// A hunk that landed away from its stated line is worth saying out
		// loud: it usually means the diff was written against an older file.
		for _, h := range f.Hunks {
			if h.Offset != 0 {
				fmt.Fprintf(&b, "      %s applied at line %d (offset %+d)\n", h.Header, h.Line, h.Offset)
			}
		}
		// A header whose counts did not match its body applied anyway, because
		// every context and removed line is checked against the file first.
		// Say so all the same: the next patch from the same source will have
		// the same fault.
		for _, note := range f.Notes {
			fmt.Fprintf(&b, "      note: %s\n", note)
		}
	}
	fmt.Fprintf(&b, "\n%d file(s), %d insertion(s), %d deletion(s)\n", r.fileCount(), r.Additions, r.Deletions)
	return b.String()
}

func (r diffResult) fileCount() int {
	n := 0
	for _, f := range r.Files {
		if f.Action != "renamed-from" {
			n++
		}
	}
	return n
}
