package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"
)

// startProcessResult starts a process and renders it as a tool result: the
// shared body of run_process and of any config.json command marked background.
// waitSeconds > 0 gives a short-lived process the chance to come back complete
// in the same call, rather than making the caller poll for a one-second job.
func (s *Server) startProcessResult(ctx context.Context, line, dir string, env map[string]string, timeout time.Duration, waitSeconds int) *CallToolResult {
	if blocked := s.checkGitAllowed(line); blocked != nil {
		return blocked
	}
	agent := s.sudoAgentRef()
	p, err := s.processes.start(line, dir, env, timeout, agent)
	if err != nil {
		return toolError("%v", err)
	}
	if waitSeconds > 0 {
		timer := time.NewTimer(time.Duration(waitSeconds) * time.Second)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	st := p.status(0, agent)
	text := st.summarize()
	if st.Running {
		text += fmt.Sprintf("\nStill running. Call check_process_id with process_id %q for its output.\n", st.ID)
	}
	return &CallToolResult{
		Content:           textContent(text),
		StructuredContent: st,
	}
}

// registerProcessTools adds the long-running counterpart to run_command:
// start something, get an id back at once, and poll it afterwards.
func (s *Server) registerProcessTools() {
	shellPath, flavor := shellCommandLine()
	platform := osDisplayName(runtime.GOOS)

	s.RegisterTool(Tool{
		Name:  "run_process",
		Title: "Start a long-running process",
		Description: fmt.Sprintf(
			"Start a command in the background and return a process id immediately, without waiting for it to finish. "+
				"Use this instead of run_command whenever the work may outlast a single request - a full build, a test suite, "+
				"a migration, a dev server, anything you would otherwise watch time out. Poll it with check_process_id, "+
				"which reports whether it is still running and returns the output collected so far, and end it early with "+
				"stop_process.\n\n"+
				"This server runs on %s/%s, and the command line is executed by %s, so write it in %s syntax: %s\n\n"+
				"Paths are separated by %s. The process is killed after timeout_seconds (default %d) however far it has got.",
			platform, runtime.GOARCH, shellPath, flavor, shellSyntaxNote(flavor),
			string(os.PathSeparator), int(defaultProcessTimeout/time.Second)) +
			privilegeDescriptionSuffix() + sudoDescriptionSuffix(s.sudo) + s.gitDescriptionSuffix(),
		Annotations: &ToolAnnotations{OpenWorldHint: true},
		InputSchema: schema([]string{"command"}, map[string]any{
			"command": prop("string", "Command line, run through the platform shell."),
			"dir":     prop("string", "Working directory relative to the workspace root."),
			"timeout_seconds": propDefault("integer",
				"Kill the process after this many seconds.", int(defaultProcessTimeout/time.Second)),
			"wait_seconds": prop("integer",
				"Wait up to this many seconds for the process to finish before returning. "+
					"A process that finishes within it comes back complete, in one call; anything still running "+
					"returns its id as usual. Keep it well under the client's own request timeout."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Command        string `json:"command"`
			Dir            string `json:"dir"`
			TimeoutSeconds int    `json:"timeout_seconds"`
			WaitSeconds    int    `json:"wait_seconds"`
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
		return s.startProcessResult(ctx, args.Command, dir, nil,
			time.Duration(args.TimeoutSeconds)*time.Second, args.WaitSeconds), nil
	})

	s.RegisterTool(Tool{
		Name:  "check_process_id",
		Title: "Check a running process",
		Description: "Report whether a process started by run_process is still running, and return the stdout and stderr " +
			"it has produced so far. Safe to call repeatedly: the output is collected continuously, so polling " +
			"a build every few seconds shows its progress. Once the process has exited, the exit code is reported " +
			"and the output stays readable until the session ends.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema([]string{"process_id"}, map[string]any{
			"process_id": prop("string", "The id returned by run_process, e.g. \"proc-1\"."),
			"tail_lines": prop("integer",
				"Return only the last this many lines of each stream. Omit for everything collected."),
			"wait_seconds": prop("integer",
				"Wait up to this many seconds for the process to finish before answering, instead of "+
					"returning its current state at once. Keep it well under the client's request timeout."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			ProcessID   string `json:"process_id"`
			TailLines   int    `json:"tail_lines"`
			WaitSeconds int    `json:"wait_seconds"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		p, ok := s.processes.get(args.ProcessID)
		if !ok {
			return toolError("no process %q; call list_processes to see which ids exist", args.ProcessID), nil
		}
		if args.WaitSeconds > 0 {
			timer := time.NewTimer(time.Duration(args.WaitSeconds) * time.Second)
			defer timer.Stop()
			select {
			case <-p.done:
			case <-timer.C:
			case <-ctx.Done():
			}
		}
		st := p.status(args.TailLines, s.sudoAgentRef())
		return &CallToolResult{
			Content:           textContent(st.summarize()),
			StructuredContent: st,
			// A process that is still running is not a failure, and neither is
			// one that exited cleanly. Anything else the model should react to.
			IsError: !st.Running && st.ExitCode != nil && *st.ExitCode != 0,
		}, nil
	})

	s.RegisterTool(Tool{
		Name:        "list_processes",
		Title:       "List processes",
		Description: "List the processes started by run_process in this session, running and finished, with their ids and status. Output is not included; use check_process_id for that.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		agent := s.sudoAgentRef()
		all := s.processes.list()
		if len(all) == 0 {
			return toolResult("No processes have been started in this session."), nil
		}
		out := make([]processStatus, 0, len(all))
		for _, p := range all {
			st := p.status(0, agent)
			st.Stdout, st.Stderr = "", ""
			out = append(out, st)
		}
		return toolResultJSON(out), nil
	})

	s.RegisterTool(Tool{
		Name:        "stop_process",
		Title:       "Stop a process",
		Description: "Kill a process started by run_process, along with anything it spawned. Its output stays readable through check_process_id afterwards. Stopping a process that has already exited does nothing.",
		Annotations: &ToolAnnotations{DestructiveHint: true},
		InputSchema: schema([]string{"process_id"}, map[string]any{
			"process_id": prop("string", "The id returned by run_process."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			ProcessID string `json:"process_id"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		p, ok := s.processes.get(args.ProcessID)
		if !ok {
			return toolError("no process %q; call list_processes to see which ids exist", args.ProcessID), nil
		}
		if err := p.stop(); err != nil {
			return toolError("could not stop %s: %v", args.ProcessID, err), nil
		}
		// The kill is asynchronous; give the reaper a moment so the status
		// reported here is the settled one rather than "running".
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
		return toolResult(p.status(0, s.sudoAgentRef()).summarize()), nil
	})
}
