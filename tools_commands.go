package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// registerCommandTools exposes each user-defined command from config.json as
// its own MCP tool. This is what saves the model from guessing how to build,
// test or lint a particular repository: the answer is in the tool list.
func (s *Server) registerCommandTools(commands []CommandConfig) {
	for _, cmd := range commands {
		s.RegisterTool(commandTool(cmd), s.commandHandler(cmd))
		s.mu.Lock()
		s.commandToolNames = append(s.commandToolNames, cmd.Name)
		s.mu.Unlock()
	}
	s.registerCommandIndex(commands)
}

func commandTool(cmd CommandConfig) Tool {
	props := map[string]any{}
	var required []string
	if cmd.AcceptsArgs {
		argDesc := "Extra arguments appended to the command line."
		if strings.Contains(commandDisplay(cmd), argsPlaceholder) {
			argDesc = fmt.Sprintf("Arguments substituted at %s in the command line.", argsPlaceholder)
			if cmd.DefaultArgs != "" {
				argDesc += fmt.Sprintf(" Defaults to %q.", cmd.DefaultArgs)
			}
		}
		props["args"] = prop("string", argDesc)
	}
	desc := cmd.Description
	line := commandDisplay(cmd)
	if !strings.Contains(desc, line) {
		_, flavor := shellCommandLine()
		desc = fmt.Sprintf("%s\n\nRuns: %s\nThrough %s on %s, which is the syntax any extra arguments must be written in.",
			desc, line, flavor, osDisplayName(runtime.GOOS))
	}
	return Tool{
		Name:        cmd.Name,
		Title:       cmd.Name,
		Description: desc,
		Annotations: &ToolAnnotations{ReadOnlyHint: cmd.ReadOnly},
		InputSchema: schema(required, props),
	}
}

// argsPlaceholder lets a command say where the caller's arguments go, rather
// than always taking them at the end. "go test ./..." plus "-run TestFoo
// ./pkg/..." is a broken command line; "go test {{args}}" is not.
const argsPlaceholder = "{{args}}"

// commandDisplay is the configured command line with the placeholder left in,
// which is what the tool description should show.
func commandDisplay(cmd CommandConfig) string {
	line := cmd.Command
	if len(cmd.Args) > 0 {
		line += " " + strings.Join(cmd.Args, " ")
	}
	return line
}

func commandLine(cmd CommandConfig, extra string) string {
	line := commandDisplay(cmd)
	if !strings.Contains(line, argsPlaceholder) {
		if extra != "" {
			line += " " + extra
		}
		return line
	}
	if extra == "" {
		extra = cmd.DefaultArgs
	}
	if extra == "" {
		// Drop the placeholder along with one adjacent space, so an omitted
		// argument does not leave a doubled space in the command line.
		line = strings.ReplaceAll(line, " "+argsPlaceholder, "")
		line = strings.ReplaceAll(line, argsPlaceholder+" ", "")
		line = strings.ReplaceAll(line, argsPlaceholder, "")
		return strings.TrimSpace(line)
	}
	return strings.ReplaceAll(line, argsPlaceholder, extra)
}

func (s *Server) commandHandler(cmd CommandConfig) ToolHandler {
	return func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Args string `json:"args"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Args != "" && !cmd.AcceptsArgs {
			return toolError("the %s command does not accept extra arguments", cmd.Name), nil
		}
		ws := s.workspace()
		dir := ws.Root
		if cmd.Dir != "" {
			resolved, err := ws.Resolve(cmd.Dir)
			if err != nil {
				return toolError("commands.%s.dir: %v", cmd.Name, err), nil
			}
			dir = resolved
		}
		res, err := runShellSudo(ctx, s.sudoAgentRef(), commandLine(cmd, args.Args), dir, cmd.Env, time.Duration(cmd.TimeoutSeconds)*time.Second)
		if err != nil {
			return toolError("%v", err), nil
		}
		return res.asToolResult(), nil
	}
}

// registerCommandIndex adds a tool that lists the project commands, so a model
// can see the available workflows without re-reading the whole tool list.
func (s *Server) registerCommandIndex(commands []CommandConfig) {
	type commandInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Command     string `json:"command"`
		AcceptsArgs bool   `json:"accepts_args"`
		TimeoutSecs int    `json:"timeout_seconds"`
	}
	infos := make([]commandInfo, 0, len(commands))
	for _, cmd := range commands {
		infos = append(infos, commandInfo{
			Name:        cmd.Name,
			Description: cmd.Description,
			Command:     commandLine(cmd, ""),
			AcceptsArgs: cmd.AcceptsArgs,
			TimeoutSecs: cmd.TimeoutSeconds,
		})
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })

	s.RegisterTool(Tool{
		Name:        "project_commands",
		Title:       "List project commands",
		Description: "List the build, test, lint and other commands this project defines in its config.json. Each one is also callable as a tool of the same name.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		if len(infos) == 0 {
			return toolResult("This project defines no commands. Add a \"commands\" section to config.json."), nil
		}
		return toolResultJSON(infos), nil
	})
}

// shellSyntaxNote names the traps of the shell this server actually runs
// commands through. Describing every platform's quirks in one static string
// leaves a model to work out which half applies to it; naming the one it is
// talking to removes the guess.
func shellSyntaxNote(flavor string) string {
	switch flavor {
	case "cmd":
		return "grep, ls, rm, cat and touch do not exist, single quotes do not group, " +
			"$VAR is %VAR%, and a multi-line argument will not survive quoting. " +
			"Use findstr, dir, del and type, or call PowerShell explicitly."
	case "sh":
		return "Standard POSIX syntax applies. Do not assume bash builtins or GNU-only flags are available."
	default:
		return "Check system_info for the syntax this shell accepts."
	}
}

// registerShellTool adds the escape hatch: an arbitrary command line in the
// workspace, for the cases the configured commands do not cover.
func (s *Server) registerShellTool() {
	shellPath, flavor := shellCommandLine()
	platform := osDisplayName(runtime.GOOS)

	s.RegisterTool(Tool{
		Name:  "run_command",
		Title: "Run a shell command",
		Description: fmt.Sprintf(
			"Run an arbitrary shell command in the workspace. Prefer the named project commands "+
				"(see project_commands) when one covers what you need.\n\n"+
				"This server runs on %s/%s, and the command line is executed by %s, so write it in %s syntax: %s\n\n"+
				"Paths are separated by %s. Call system_info for the full picture, including which programs are installed.",
			platform, runtime.GOARCH, shellPath, flavor, shellSyntaxNote(flavor), string(os.PathSeparator)) +
			privilegeDescriptionSuffix() + sudoDescriptionSuffix(s.sudo),
		Annotations: &ToolAnnotations{OpenWorldHint: true},
		InputSchema: schema([]string{"command"}, map[string]any{
			"command":         prop("string", "Command line, run through the platform shell."),
			"dir":             prop("string", "Working directory relative to the workspace root."),
			"timeout_seconds": propDefault("integer", "Kill the command after this many seconds.", 300),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Command        string `json:"command"`
			Dir            string `json:"dir"`
			TimeoutSeconds int    `json:"timeout_seconds"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.Command) == "" {
			return toolError("command is required"), nil
		}
		dir, err := s.workspace().Resolve(args.Dir)
		if err != nil {
			return toolError("%v", err), nil
		}
		if args.TimeoutSeconds <= 0 {
			args.TimeoutSeconds = 300
		}
		res, runErr := runShellSudo(ctx, s.sudoAgentRef(), args.Command, dir, nil, time.Duration(args.TimeoutSeconds)*time.Second)
		if runErr != nil {
			return toolError("%v", runErr), nil
		}
		return res.asToolResult(), nil
	})
}
