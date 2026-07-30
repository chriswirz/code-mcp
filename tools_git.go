package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const gitTimeout = 120 * time.Second

// gitDiffArgs builds the argument list for git_diff. It is separate from the
// handler so the flag combinations can be tested without a repository, and
// because the interactions are easy to get wrong: --unified is meaningless
// alongside a summary, and git rejects the pair outright.
func gitDiffArgs(staged, stat, nameOnly bool, contextLines *int, revision, path string) []string {
	cmd := []string{"diff"}
	if staged {
		cmd = append(cmd, "--staged")
	}
	switch {
	case nameOnly:
		cmd = append(cmd, "--name-only")
	case stat:
		cmd = append(cmd, "--stat")
	}
	if contextLines != nil && !nameOnly && !stat {
		cmd = append(cmd, "--unified="+strconv.Itoa(*contextLines))
	}
	if revision != "" {
		cmd = append(cmd, revision)
	}
	if path != "" {
		cmd = append(cmd, "--", path)
	}
	return cmd
}

// registerGitTools adds the version-control half of the agent. Everything runs
// through the git binary; nothing here re-implements git.
func (s *Server) registerGitTools(cfg GitConfig) {
	git := func(ctx context.Context, args ...string) (*CallToolResult, *RPCError) {
		res, err := runExec(ctx, cfg.GitPath, args, s.ws.Root, gitTimeout)
		if err != nil {
			return toolError("%v", err), nil
		}
		return res.asToolResult(), nil
	}

	s.RegisterTool(Tool{
		Name:        "git_status",
		Title:       "Git status",
		Description: "Show the working tree status: branch, staged, unstaged and untracked files.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		return git(ctx, "status", "--short", "--branch")
	})

	s.RegisterTool(Tool{
		Name:  "git_diff",
		Title: "Git diff",
		Description: "Show the diff of the working tree, of the index (staged), or against an arbitrary revision. " +
			"A full patch is often more than the question needs: use stat to see which files changed and by how much, " +
			"name_only for just the paths, and context to narrow the lines shown around each change. " +
			"Reach for one of those first when the working tree is large, then take the full patch of the file you actually care about.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"staged":    propDefault("boolean", "Diff the index against HEAD instead of the working tree.", false),
			"revision":  prop("string", "Diff against this revision or revision range instead."),
			"path":      prop("string", "Limit the diff to this path."),
			"stat":      propDefault("boolean", "Summarise as a per-file count of changed lines instead of a patch.", false),
			"name_only": propDefault("boolean", "List only the paths that changed. Takes precedence over stat.", false),
			"context":   prop("integer", "Lines of context around each hunk. Defaults to git's 3; 0 shows changed lines only."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Staged   bool   `json:"staged"`
			Revision string `json:"revision"`
			Path     string `json:"path"`
			Stat     bool   `json:"stat"`
			NameOnly bool   `json:"name_only"`
			Context  *int   `json:"context"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Context != nil && *args.Context < 0 {
			return toolError("context must not be negative"), nil
		}
		return git(ctx, gitDiffArgs(args.Staged, args.Stat, args.NameOnly, args.Context, args.Revision, args.Path)...)
	})

	s.RegisterTool(Tool{
		Name:        "git_log",
		Title:       "Git log",
		Description: "Show recent commits, newest first.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"limit": propDefault("integer", "How many commits to show.", 20),
			"path":  prop("string", "Only commits touching this path."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Limit int    `json:"limit"`
			Path  string `json:"path"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}
		cmd := []string{"log", "--oneline", "--decorate", "-n", strconv.Itoa(args.Limit)}
		if args.Path != "" {
			cmd = append(cmd, "--", args.Path)
		}
		return git(ctx, cmd...)
	})

	s.RegisterTool(Tool{
		Name:        "git_branch",
		Title:       "Git branches",
		Description: "List local branches, or create and switch to a new one.",
		Annotations: &ToolAnnotations{},
		InputSchema: schema(nil, map[string]any{
			"create":   prop("string", "Create this branch from the current HEAD and switch to it."),
			"checkout": prop("string", "Switch to this existing branch."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Create   string `json:"create"`
			Checkout string `json:"checkout"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		switch {
		case args.Create != "":
			return git(ctx, "checkout", "-b", args.Create)
		case args.Checkout != "":
			return git(ctx, "checkout", args.Checkout)
		default:
			return git(ctx, "branch", "--list", "-vv")
		}
	})

	s.RegisterTool(Tool{
		Name:        "git_add",
		Title:       "Stage changes",
		Description: "Stage the named paths for the next commit.",
		InputSchema: schema([]string{"paths"}, map[string]any{
			"paths": map[string]any{
				"type":        "array",
				"description": "Paths to stage, relative to the workspace root. Use [\".\"] for everything.",
				"items":       map[string]any{"type": "string"},
			},
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Paths []string `json:"paths"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if len(args.Paths) == 0 {
			return toolError("paths is required"), nil
		}
		return git(ctx, append([]string{"add", "--"}, args.Paths...)...)
	})

	if cfg.AllowCommit {
		s.RegisterTool(Tool{
			Name:        "git_commit",
			Title:       "Commit",
			Description: "Commit the staged changes with the given message.",
			InputSchema: schema([]string{"message"}, map[string]any{
				"message": prop("string", "Commit message. The first line is the subject."),
				"all":     propDefault("boolean", "Stage every tracked, modified file first (git commit -a).", false),
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				Message string `json:"message"`
				All     bool   `json:"all"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			if strings.TrimSpace(args.Message) == "" {
				return toolError("message is required"), nil
			}
			cmd := []string{"commit", "-m", args.Message}
			if args.All {
				cmd = append(cmd, "-a")
			}
			return git(ctx, cmd...)
		})
	}

	if cfg.AllowPush {
		s.RegisterTool(Tool{
			Name:  "git_push",
			Title: "Push",
			Description: "Push the current branch to the remote. This is what triggers the GitHub Actions " +
				"deployment workflows, so it is an outward-facing action.",
			Annotations: &ToolAnnotations{OpenWorldHint: true},
			InputSchema: schema(nil, map[string]any{
				"remote":       propDefault("string", "Remote to push to.", cfg.DefaultRemote),
				"branch":       prop("string", "Branch to push. Defaults to the current branch."),
				"set_upstream": propDefault("boolean", "Set the upstream tracking branch (git push -u).", false),
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				Remote      string `json:"remote"`
				Branch      string `json:"branch"`
				SetUpstream bool   `json:"set_upstream"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			remote := args.Remote
			if remote == "" {
				remote = cfg.DefaultRemote
			}
			cmd := []string{"push"}
			if args.SetUpstream {
				cmd = append(cmd, "-u")
			}
			cmd = append(cmd, remote)
			if args.Branch != "" {
				cmd = append(cmd, args.Branch)
			}
			return git(ctx, cmd...)
		})
	}

	s.RegisterTool(Tool{
		Name:        "git_show",
		Title:       "Show a commit",
		Description: "Show the message and diff of one commit.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"revision"}, map[string]any{
			"revision": prop("string", "Commit-ish to show, e.g. HEAD or a SHA."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Revision string `json:"revision"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Revision == "" {
			args.Revision = "HEAD"
		}
		return git(ctx, "show", "--stat", "--patch", args.Revision)
	})

	s.RegisterTool(Tool{
		Name:  "git_blame",
		Title: "Blame lines",
		Description: "Show which commit last changed each line of a file, with its author and date. " +
			"Use start_line and end_line to blame only the region you care about, which is usually what you want: " +
			"it answers why a line is the way it is before you change it.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path":       prop("string", "File relative to the workspace root."),
			"start_line": prop("integer", "First line to blame, 1-based."),
			"end_line":   prop("integer", "Last line to blame, inclusive. Defaults to the end of the file."),
			"revision":   prop("string", "Blame the file as of this revision instead of the working tree."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
			Revision  string `json:"revision"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Path == "" {
			return toolError("path is required"), nil
		}
		if args.EndLine > 0 && args.StartLine > args.EndLine {
			return toolError("start_line %d is after end_line %d", args.StartLine, args.EndLine), nil
		}
		// -w ignores whitespace-only changes, so a reformat does not hide the
		// commit that actually wrote the line.
		cmd := []string{"blame", "--date=short", "-w"}
		if args.StartLine > 0 || args.EndLine > 0 {
			start := args.StartLine
			if start <= 0 {
				start = 1
			}
			end := "$"
			if args.EndLine > 0 {
				end = strconv.Itoa(args.EndLine)
			}
			cmd = append(cmd, "-L", strconv.Itoa(start)+","+end)
		}
		if args.Revision != "" {
			cmd = append(cmd, args.Revision)
		}
		cmd = append(cmd, "--", args.Path)
		return git(ctx, cmd...)
	})

	s.RegisterTool(Tool{
		Name:  "git_stash",
		Title: "Stash changes",
		Description: "Save, restore, list, inspect or drop stashed working-tree changes. This is the undo for a change that went wrong: " +
			"push sets the working tree back to HEAD and keeps the changes, pop puts them back again.",
		InputSchema: schema(nil, map[string]any{
			"action": map[string]any{
				"type":        "string",
				"description": "What to do with the stash.",
				"enum":        []string{"push", "pop", "apply", "list", "show", "drop"},
				"default":     "list",
			},
			"message":           prop("string", "Label for the entry, when pushing."),
			"include_untracked": propDefault("boolean", "Also stash untracked files, when pushing.", false),
			"index":             propDefault("integer", "Which entry to act on, 0 being the most recent.", 0),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Action           string `json:"action"`
			Message          string `json:"message"`
			IncludeUntracked bool   `json:"include_untracked"`
			Index            int    `json:"index"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Index < 0 {
			return toolError("index must not be negative"), nil
		}
		entry := "stash@{" + strconv.Itoa(args.Index) + "}"
		switch strings.ToLower(strings.TrimSpace(args.Action)) {
		case "", "list":
			return git(ctx, "stash", "list")
		case "push":
			cmd := []string{"stash", "push"}
			if args.IncludeUntracked {
				cmd = append(cmd, "--include-untracked")
			}
			if args.Message != "" {
				cmd = append(cmd, "-m", args.Message)
			}
			return git(ctx, cmd...)
		case "pop":
			return git(ctx, "stash", "pop", entry)
		case "apply":
			return git(ctx, "stash", "apply", entry)
		case "show":
			return git(ctx, "stash", "show", "--patch", entry)
		case "drop":
			return git(ctx, "stash", "drop", entry)
		default:
			return toolError("unknown action %q: use push, pop, apply, list, show or drop", args.Action), nil
		}
	})

	if cfg.AllowRestore {
		s.RegisterTool(Tool{
			Name:  "git_restore",
			Title: "Discard changes",
			Description: "Discard uncommitted changes to the named paths, restoring them from the index or from a revision. " +
				"This throws work away and cannot be undone: use git_stash first if the changes might still be wanted.",
			Annotations: &ToolAnnotations{DestructiveHint: true},
			InputSchema: schema([]string{"paths"}, map[string]any{
				"paths": map[string]any{
					"type":        "array",
					"description": "Paths to restore, relative to the workspace root.",
					"items":       map[string]any{"type": "string"},
				},
				"staged": propDefault("boolean", "Unstage the paths instead of changing the working tree.", false),
				"source": prop("string", "Restore the content from this revision instead of the index."),
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				Paths  []string `json:"paths"`
				Staged bool     `json:"staged"`
				Source string   `json:"source"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			if len(args.Paths) == 0 {
				return toolError("paths is required; restoring everything by accident is not worth the convenience"), nil
			}
			cmd := []string{"restore"}
			if args.Staged {
				cmd = append(cmd, "--staged")
			}
			if args.Source != "" {
				cmd = append(cmd, "--source", args.Source)
			}
			cmd = append(cmd, "--")
			return git(ctx, append(cmd, args.Paths...)...)
		})
	}
}
