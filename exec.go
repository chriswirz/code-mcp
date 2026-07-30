package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// execResult is what every shelled-out command reports back.
type execResult struct {
	Command  string `json:"command"`
	Dir      string `json:"dir"`
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timed_out,omitempty"`
	Duration string `json:"duration"`
}

// maxOutputBytes caps each of stdout and stderr so a runaway build log cannot
// swamp the model's context.
const maxOutputBytes = 64 << 10

// runShell runs a command line through the platform shell with no sudo help.
func runShell(ctx context.Context, line, dir string, env map[string]string, timeout time.Duration) (*execResult, error) {
	return runShellSudo(ctx, nil, line, dir, env, timeout)
}

// runShellSudo runs a command line through the platform shell, which is what a
// user-defined command such as "go build ./... && go vet ./..." expects. When
// the line invokes sudo and an agent is configured, the agent puts its shim on
// PATH and feeds the password to the command's stdin, so the elevation happens
// without the password passing through the model.
func runShellSudo(ctx context.Context, agent *sudoAgent, line, dir string, env map[string]string, timeout time.Duration) (*execResult, error) {
	var name string
	var args []string
	if runtime.GOOS == "windows" {
		if shell := os.Getenv("COMSPEC"); shell != "" {
			name, args = shell, []string{"/c", line}
		} else {
			name, args = "cmd.exe", []string{"/c", line}
		}
	} else {
		name, args = "/bin/sh", []string{"-c", line}
	}
	if agent.wants(line) {
		merged := map[string]string{}
		for k, v := range agent.env() {
			merged[k] = v
		}
		// A command's own environment wins, except for PATH, where the shim
		// has to stay in front for sudo to be answered at all.
		for k, v := range env {
			if k == "PATH" {
				merged[k] = agent.shimDir + string(os.PathListSeparator) + v
				continue
			}
			merged[k] = v
		}
		res, err := runCommand(ctx, name, args, dir, merged, timeout, line, agent)
		return res, err
	}
	return runCommand(ctx, name, args, dir, env, timeout, line, nil)
}

// runExec runs a program directly, without a shell. Used by the git and gh
// tools, where arguments come from the model and must not be re-parsed.
func runExec(ctx context.Context, name string, args []string, dir string, timeout time.Duration) (*execResult, error) {
	display := name + " " + strings.Join(args, " ")
	return runCommand(ctx, name, args, dir, nil, timeout, display, nil)
}

func runCommand(ctx context.Context, name string, args []string, dir string, env map[string]string, timeout time.Duration, display string, agent *sudoAgent) (*execResult, error) {
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	if agent != nil {
		// sudo -S reads the password from stdin. It is written here, in this
		// process, and never stored anywhere the model can reach.
		cmd.Stdin = strings.NewReader(agent.password + "\n")
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	elapsed := time.Since(start)

	res := &execResult{
		Command:  display,
		Dir:      dir,
		Stdout:   agent.redact(truncateOutput(stdout.String())),
		Stderr:   agent.redact(truncateOutput(stderr.String())),
		Duration: elapsed.Round(time.Millisecond).String(),
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		// The program could not be started at all: a missing gh or git is a
		// setup problem the model should be told about plainly.
		return res, fmt.Errorf("could not run %s: %w", name, err)
	}
	return res, nil
}

func truncateOutput(s string) string {
	if len(s) <= maxOutputBytes {
		return s
	}
	// Keep the tail: compiler and test failures are at the end.
	return fmt.Sprintf("[... %d bytes truncated ...]\n%s", len(s)-maxOutputBytes, s[len(s)-maxOutputBytes:])
}

// summarize renders an execResult as the text block of a tool result.
func (r *execResult) summarize() string {
	var b strings.Builder
	fmt.Fprintf(&b, "$ %s\n", r.Command)
	switch {
	case r.TimedOut:
		fmt.Fprintf(&b, "timed out after %s\n", r.Duration)
	default:
		fmt.Fprintf(&b, "exit code %d (%s)\n", r.ExitCode, r.Duration)
	}
	if r.Stdout != "" {
		b.WriteString("\n--- stdout ---\n")
		b.WriteString(r.Stdout)
		if !strings.HasSuffix(r.Stdout, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Stderr != "" {
		b.WriteString("\n--- stderr ---\n")
		b.WriteString(r.Stderr)
		if !strings.HasSuffix(r.Stderr, "\n") {
			b.WriteString("\n")
		}
	}
	if r.Stdout == "" && r.Stderr == "" {
		b.WriteString("\n(no output)\n")
	}
	return b.String()
}

// asToolResult turns a command run into a tool result: a non-zero exit is a
// tool execution error, since the model can usually act on it.
func (r *execResult) asToolResult() *CallToolResult {
	return &CallToolResult{
		Content:           textContent(r.summarize()),
		StructuredContent: r,
		IsError:           r.ExitCode != 0 || r.TimedOut,
	}
}
