package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// Prompts are the recurring workflows of this server: verify a change, ship
// it through GitHub Actions, and work out why a deploy failed.

type promptDef struct {
	def    Prompt
	render func(args map[string]string) string
}

func (s *Server) promptDefs() map[string]promptDef {
	commandHint := func() string {
		names := s.commandNames()
		if len(names) == 0 {
			return "This project defines no commands in config.json; fall back to run_command."
		}
		return "Project commands available as tools: " + strings.Join(names, ", ") + "."
	}

	return map[string]promptDef{
		"verify_change": {
			def: Prompt{
				Name:        "verify_change",
				Title:       "Verify a change",
				Description: "Build, test and lint the workspace and report what broke.",
			},
			render: func(map[string]string) string {
				return fmt.Sprintf(`Verify the current state of this workspace.

%s

1. Call project_commands to see what is defined.
2. Run the build, then the tests, then the lint command, in that order.
3. Stop at the first failure and report the failing output verbatim, with the
   file and line it points at.
4. If everything passes, say so plainly and show git_status so the operator can
   see what is uncommitted.`, commandHint())
			},
		},
		"ship_change": {
			def: Prompt{
				Name:        "ship_change",
				Title:       "Ship a change",
				Description: "Verify, commit, push, and follow the GitHub Actions deployment through to its release.",
				Arguments: []PromptArgument{
					{Name: "message", Description: "Commit message for the change.", Required: true},
					{Name: "branch", Description: "Branch to push. Defaults to the current one."},
				},
			},
			render: func(args map[string]string) string {
				branch := args["branch"]
				if branch == "" {
					branch = "the current branch"
				}
				return fmt.Sprintf(`Ship the current change.

%s

1. Run the build, test and lint commands. Do not continue if any of them fail.
2. Show git_status and git_diff so the operator can see exactly what is going out.
3. Stage the change and commit it with this message:

%s

4. Push %s. This triggers the deployment workflow, so confirm with the operator
   before pushing unless they have already told you to go ahead.
5. Call github_runs to find the run that just started, then github_run_watch on
   its id.
6. If it fails, use github_run_logs with failed_only to get the failing step and
   report the cause. If it succeeds, call github_releases to confirm the release
   and its assets.`, commandHint(), args["message"], branch)
			},
		},
		"diagnose_deploy": {
			def: Prompt{
				Name:        "diagnose_deploy",
				Title:       "Diagnose a failed deploy",
				Description: "Work out why the most recent GitHub Actions run failed.",
				Arguments: []PromptArgument{
					{Name: "run_id", Description: "A specific run to look at. Defaults to the newest failure."},
				},
			},
			render: func(args map[string]string) string {
				target := "the newest failing run from github_runs with status=failure"
				if id := args["run_id"]; id != "" {
					target = "run " + id
				}
				return fmt.Sprintf(`Diagnose %s.

1. Read the workflow definition with github_workflow_file so you know what the
   job was actually doing.
2. Call github_run_view on the run to see which job and step failed.
3. Call github_run_logs with failed_only to get the failing output.
4. Reproduce the failure locally with the matching project command where you
   can, so the fix is verified before it is pushed.
5. Report the root cause and the smallest change that fixes it. Do not push
   anything without being asked.`, target)
			},
		},
		"explore_workspace": {
			def: Prompt{
				Name:        "explore_workspace",
				Title:       "Explore the workspace",
				Description: "Get oriented in an unfamiliar repository.",
			},
			render: func(map[string]string) string {
				return fmt.Sprintf(`Get oriented in this workspace.

%s

1. list_directory at the root, then read the README if there is one.
2. Read the config.json commands via project_commands: they say how this
   project is built, tested and linted.
3. Read .github/workflows via github_workflow_file to see how it is deployed.
4. Summarise: what this project is, how to build and test it, and what happens
   when it is pushed.`, commandHint())
			},
		},
	}
}

func (s *Server) commandNames() []string {
	var names []string
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, name := range s.commandToolNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) listPrompts(ctx context.Context) any {
	defs := s.promptDefs()
	names := make([]string, 0, len(defs))
	for name := range defs {
		names = append(names, name)
	}
	sort.Strings(names)
	prompts := make([]Prompt, 0, len(names))
	for _, name := range names {
		prompts = append(prompts, defs[name].def)
	}
	return &ListPromptsResult{
		Result:  s.completeResult(ctx).cacheable(3600000, CacheScopePrivate),
		Prompts: prompts,
	}
}

func (s *Server) getPrompt(ctx context.Context, req *Request) (any, *RPCError) {
	var params struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := req.Bind(&params); err != nil {
		return nil, err
	}
	def, ok := s.promptDefs()[params.Name]
	if !ok {
		return nil, Errorf(CodeInvalidParams, "unknown prompt: %s", params.Name)
	}
	for _, arg := range def.def.Arguments {
		if arg.Required && params.Arguments[arg.Name] == "" {
			return nil, Errorf(CodeInvalidParams, "prompt %s requires the %s argument", params.Name, arg.Name)
		}
	}
	return &GetPromptResult{
		Result:      s.completeResult(ctx),
		Description: def.def.Description,
		Messages: []PromptMessage{{
			Role:    "user",
			Content: Content{Type: "text", Text: def.render(params.Arguments)},
		}},
	}, nil
}
