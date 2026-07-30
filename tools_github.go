package main

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

const ghTimeout = 180 * time.Second

// registerGitHubTools drives the GitHub Actions workflows that build and
// deploy the repository, in the shape encrypt-go uses: a workflow that tests,
// builds a matrix of binaries, packages them and publishes a release. The
// tools shell out to the gh CLI so they inherit its authentication.
func (s *Server) registerGitHubTools(cfg GitHubConfig) {
	// repoArgs appends "--repo owner/name" when the config names one; without
	// it gh infers the repository from the checkout in the workspace.
	repoArgs := func(args []string) []string {
		if cfg.Repo != "" {
			return append(args, "--repo", cfg.Repo)
		}
		return args
	}
	gh := func(ctx context.Context, args ...string) (*CallToolResult, *RPCError) {
		res, err := runExec(ctx, cfg.GhPath, repoArgs(args), s.ws.Root, ghTimeout)
		if err != nil {
			return toolError("%v (is the gh CLI installed and authenticated?)", err), nil
		}
		return res.asToolResult(), nil
	}

	s.RegisterTool(Tool{
		Name:        "github_workflows",
		Title:       "List workflows",
		Description: "List the GitHub Actions workflows defined in this repository, with their state and id.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		return gh(ctx, "workflow", "list", "--all")
	})

	s.RegisterTool(Tool{
		Name:        "github_runs",
		Title:       "List workflow runs",
		Description: "List recent GitHub Actions runs, newest first, optionally filtered to one workflow, branch or status.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema(nil, map[string]any{
			"workflow": propDefault("string", "Workflow file name or id, e.g. release.yml", cfg.DefaultWorkflow),
			"branch":   prop("string", "Only runs on this branch."),
			"status":   prop("string", "Only runs in this state: queued, in_progress, completed, success, failure, cancelled."),
			"limit":    propDefault("integer", "How many runs to list.", 10),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Workflow string `json:"workflow"`
			Branch   string `json:"branch"`
			Status   string `json:"status"`
			Limit    int    `json:"limit"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Workflow == "" {
			args.Workflow = cfg.DefaultWorkflow
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		cmd := []string{"run", "list", "--limit", strconv.Itoa(args.Limit)}
		if args.Workflow != "" {
			cmd = append(cmd, "--workflow", args.Workflow)
		}
		if args.Branch != "" {
			cmd = append(cmd, "--branch", args.Branch)
		}
		if args.Status != "" {
			cmd = append(cmd, "--status", args.Status)
		}
		return gh(ctx, cmd...)
	})

	s.RegisterTool(Tool{
		Name:        "github_run_view",
		Title:       "View a workflow run",
		Description: "Show the jobs and steps of one workflow run, including which step failed.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"run_id"}, map[string]any{
			"run_id": prop("string", "The run id from github_runs."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			RunID string `json:"run_id"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.RunID == "" {
			return toolError("run_id is required"), nil
		}
		return gh(ctx, "run", "view", args.RunID, "--verbose")
	})

	s.RegisterTool(Tool{
		Name:  "github_run_logs",
		Title: "Read workflow run logs",
		Description: "Fetch the logs of a workflow run. Set failed_only to get just the failed steps, " +
			"which is usually what you want when diagnosing a broken build.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"run_id"}, map[string]any{
			"run_id":      prop("string", "The run id from github_runs."),
			"failed_only": propDefault("boolean", "Only return the logs of failed steps.", true),
			"job":         prop("string", "Restrict to a single job id."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			RunID      string `json:"run_id"`
			FailedOnly *bool  `json:"failed_only"`
			Job        string `json:"job"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.RunID == "" {
			return toolError("run_id is required"), nil
		}
		cmd := []string{"run", "view", args.RunID}
		if args.FailedOnly == nil || *args.FailedOnly {
			cmd = append(cmd, "--log-failed")
		} else {
			cmd = append(cmd, "--log")
		}
		if args.Job != "" {
			cmd = append(cmd, "--job", args.Job)
		}
		return gh(ctx, cmd...)
	})

	s.RegisterTool(Tool{
		Name:        "github_run_watch",
		Title:       "Wait for a workflow run",
		Description: "Block until a workflow run finishes and report its conclusion. Use after dispatching a deploy.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"run_id"}, map[string]any{
			"run_id": prop("string", "The run id from github_runs."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			RunID string `json:"run_id"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.RunID == "" {
			return toolError("run_id is required"), nil
		}
		res, err := runExec(ctx, cfg.GhPath, repoArgs([]string{"run", "watch", args.RunID, "--exit-status"}), s.ws.Root, 45*time.Minute)
		if err != nil {
			return toolError("%v", err), nil
		}
		return res.asToolResult(), nil
	})

	if cfg.AllowDispatch {
		s.RegisterTool(Tool{
			Name:  "github_workflow_run",
			Title: "Dispatch a workflow",
			Description: "Trigger a workflow_dispatch run of a GitHub Actions workflow. This starts a real " +
				"build and, for deployment workflows, a real deploy.",
			Annotations: &ToolAnnotations{OpenWorldHint: true},
			InputSchema: schema(nil, map[string]any{
				"workflow": propDefault("string", "Workflow file name, e.g. release.yml", cfg.DefaultWorkflow),
				"ref":      propDefault("string", "Branch or tag to run the workflow from.", cfg.DefaultRef),
				"inputs": map[string]any{
					"type":                 "object",
					"description":          "workflow_dispatch inputs, as name/value pairs.",
					"additionalProperties": map[string]any{"type": "string"},
				},
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				Workflow string            `json:"workflow"`
				Ref      string            `json:"ref"`
				Inputs   map[string]string `json:"inputs"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			workflow := args.Workflow
			if workflow == "" {
				workflow = cfg.DefaultWorkflow
			}
			if workflow == "" {
				return toolError("workflow is required (or set github.default_workflow in config.json)"), nil
			}
			cmd := []string{"workflow", "run", workflow}
			ref := args.Ref
			if ref == "" {
				ref = cfg.DefaultRef
			}
			if ref != "" {
				cmd = append(cmd, "--ref", ref)
			}
			for k, v := range args.Inputs {
				cmd = append(cmd, "--field", k+"="+v)
			}
			return gh(ctx, cmd...)
		})

		s.RegisterTool(Tool{
			Name:        "github_run_rerun",
			Title:       "Re-run a workflow run",
			Description: "Re-run a workflow run, optionally only its failed jobs.",
			Annotations: &ToolAnnotations{OpenWorldHint: true},
			InputSchema: schema([]string{"run_id"}, map[string]any{
				"run_id":      prop("string", "The run id from github_runs."),
				"failed_only": propDefault("boolean", "Only re-run the jobs that failed.", true),
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				RunID      string `json:"run_id"`
				FailedOnly *bool  `json:"failed_only"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			if args.RunID == "" {
				return toolError("run_id is required"), nil
			}
			cmd := []string{"run", "rerun", args.RunID}
			if args.FailedOnly == nil || *args.FailedOnly {
				cmd = append(cmd, "--failed")
			}
			return gh(ctx, cmd...)
		})

		s.RegisterTool(Tool{
			Name:        "github_run_cancel",
			Title:       "Cancel a workflow run",
			Description: "Cancel an in-progress workflow run.",
			Annotations: &ToolAnnotations{DestructiveHint: true, OpenWorldHint: true},
			InputSchema: schema([]string{"run_id"}, map[string]any{
				"run_id": prop("string", "The run id from github_runs."),
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				RunID string `json:"run_id"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			if args.RunID == "" {
				return toolError("run_id is required"), nil
			}
			return gh(ctx, "run", "cancel", args.RunID)
		})
	}

	s.RegisterTool(Tool{
		Name:        "github_releases",
		Title:       "List releases",
		Description: "List the repository releases published by the deployment workflow, newest first.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema(nil, map[string]any{
			"limit": propDefault("integer", "How many releases to list.", 10),
			"tag":   prop("string", "Show one release in detail, including its assets."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Limit int    `json:"limit"`
			Tag   string `json:"tag"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Tag != "" {
			return gh(ctx, "release", "view", args.Tag)
		}
		if args.Limit <= 0 {
			args.Limit = 10
		}
		return gh(ctx, "release", "list", "--limit", strconv.Itoa(args.Limit))
	})

	s.RegisterTool(Tool{
		Name:        "github_pr",
		Title:       "Pull requests",
		Description: "List open pull requests, view one, or check the status of the checks on one.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: true},
		InputSchema: schema(nil, map[string]any{
			"number": prop("integer", "View this pull request instead of listing."),
			"checks": propDefault("boolean", "With number, show the CI check status instead of the description.", false),
			"limit":  propDefault("integer", "How many pull requests to list.", 20),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Number int  `json:"number"`
			Checks bool `json:"checks"`
			Limit  int  `json:"limit"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Number > 0 {
			if args.Checks {
				return gh(ctx, "pr", "checks", strconv.Itoa(args.Number))
			}
			return gh(ctx, "pr", "view", strconv.Itoa(args.Number))
		}
		if args.Limit <= 0 {
			args.Limit = 20
		}
		return gh(ctx, "pr", "list", "--limit", strconv.Itoa(args.Limit))
	})

	s.RegisterTool(Tool{
		Name:  "github_workflow_file",
		Title: "Read a workflow file",
		Description: "Read one of the repository workflow definitions from .github/workflows, so you can see " +
			"exactly what the deployment does before triggering it.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, map[string]any{
			"name": prop("string", "Workflow file name, e.g. release.yml. Omit to list what is there."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Name string `json:"name"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Name == "" {
			dir, err := s.ws.Resolve(".github/workflows")
			if err != nil {
				return toolError("%v", err), nil
			}
			entries, err := readDirNames(dir)
			if err != nil {
				return toolError("no .github/workflows directory in this workspace"), nil
			}
			return toolResult(strings.Join(entries, "\n")), nil
		}
		content, err := s.ws.ReadFile(".github/workflows/" + strings.TrimPrefix(args.Name, ".github/workflows/"))
		if err != nil {
			return toolError("%v", err), nil
		}
		return toolResult(content), nil
	})
}
