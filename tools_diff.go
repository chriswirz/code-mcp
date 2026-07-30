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
			"file creation and deletion, and renames. Line numbers in the hunk headers need not be " +
			"exact - the surrounding context is searched for - but the context lines themselves must " +
			"match. Either the whole patch applies or nothing is written. Use dry_run first when you " +
			"are unsure the patch is against the current state of the files.",
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
		if !s.ws.AllowWrite && !args.DryRun {
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
		writes, failure := s.planDiff(files, opts)
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
					abs, resolveErr := s.ws.Resolve(w.path)
					if resolveErr != nil {
						return toolError("%v", resolveErr), nil
					}
					if removeErr := os.Remove(abs); removeErr != nil && !os.IsNotExist(removeErr) {
						return toolError("could not delete %s: %v", w.path, removeErr), nil
					}
					continue
				}
				if _, writeErr := s.ws.WriteFile(w.path, w.content); writeErr != nil {
					return toolError("could not write %s: %v", w.path, writeErr), nil
				}
			}
		}
		return &CallToolResult{
			Content:           textContent(result.summarize()),
			StructuredContent: result,
		}, nil
	})
}

// planDiff turns parsed file changes into the writes that would apply them,
// or returns the tool error explaining why one of them cannot.
func (s *Server) planDiff(files []fileDiff, opts applyOptions) ([]pendingWrite, *CallToolResult) {
	var writes []pendingWrite

	for _, f := range files {
		path := f.Path()
		// Resolve through the workspace so a patch cannot write outside it,
		// whatever paths its headers name.
		if _, err := s.ws.Resolve(path); err != nil {
			if _, newErr := s.ws.WriteFileCheck(path); newErr != nil {
				return nil, toolError("%s: %v", path, newErr)
			}
		}

		var old string
		switch {
		case f.IsNew:
			if _, err := s.ws.Stat(path); err == nil {
				return nil, toolError("%s already exists, but the patch creates it "+
					"(its old side is /dev/null)", path)
			}
		default:
			readPath := f.OldPath
			if readPath == "" {
				readPath = path
			}
			content, err := s.ws.ReadFile(readPath)
			if err != nil {
				return nil, toolError("%s: %v", readPath, err)
			}
			old = content
		}

		updated, hunks, err := applyHunks(old, f.Hunks, opts)
		if err != nil {
			return nil, toolError("%s: %v", path, err)
		}

		// The patch's own headers name the file; report it as the workspace
		// resolved it, so a rooted or re-anchored path is not echoed back.
		result := diffFileResult{Path: s.ws.Canonical(path), Hunks: hunks}
		for _, h := range f.Hunks {
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
			writes = append(writes, pendingWrite{path: f.OldPath, delete: true, result: result})
		case f.IsNew:
			result.Action = "created"
			writes = append(writes, pendingWrite{path: path, content: updated, result: result})
		case f.IsRename():
			result.Action = "renamed"
			result.From = s.ws.Canonical(f.OldPath)
			writes = append(writes,
				pendingWrite{path: path, content: updated, result: result},
				pendingWrite{path: f.OldPath, delete: true, result: diffFileResult{Path: s.ws.Canonical(f.OldPath), Action: "renamed-from"}})
		default:
			result.Action = "modified"
			writes = append(writes, pendingWrite{path: path, content: updated, result: result})
		}
	}
	return writes, nil
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
