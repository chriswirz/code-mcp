package main

import (
	"strings"
	"testing"
)

func TestInvokesGit(t *testing.T) {
	blocked := []string{
		"git status",
		"  git   push origin main  ",
		"git.exe commit -m hi",
		"/usr/bin/git log",
		`C:\Program Files\Git\cmd\git.exe fetch`,
		"sudo git clean -fd",
		"env GIT_DIR=.git git rev-parse HEAD",
		"go build ./... && git commit -am done",
		"echo hi; git push",
		"go test ./... | git hash-object --stdin",
		`bash -c "git reset --hard"`,
		"sh -c 'cd sub && git pull'",
		"GIT_AUTHOR_NAME=me git commit",
	}
	for _, line := range blocked {
		if !invokesGit(line) {
			t.Errorf("invokesGit(%q) = false, want true", line)
		}
	}

	allowed := []string{
		"go build ./...",
		"grep git README.md",
		"ls -la .git",
		"go get github.com/chriswirz/code-mcp",
		"git-lfs status",
		"echo 'commit with git later'",
		"npm run build",
		"gh pr list",
		"cat gitguard.go",
	}
	for _, line := range allowed {
		if invokesGit(line) {
			t.Errorf("invokesGit(%q) = true, want false", line)
		}
	}
}

// TestGitDisabledBlocksShellAndProcesses is the point of the guard: with
// git.enabled off, the git tools are gone and the shell is not a way back in.
func TestGitDisabledBlocksShellAndProcesses(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	s.mu.Lock()
	s.cfg.Git.Enabled = false
	s.mu.Unlock()

	for _, name := range []string{"run_command", "run_process"} {
		got := call(t, s, "tools/call", map[string]any{
			"name":      name,
			"arguments": map[string]any{"command": "git push"},
		})
		if got["isError"] != true {
			t.Errorf("%s ran git while git is disabled: %v", name, got)
		}
		body, _ := got["content"].([]any)
		block, _ := body[0].(map[string]any)
		if text, _ := block["text"].(string); !strings.Contains(text, "git.enabled") {
			t.Errorf("%s error does not name the setting: %q", name, text)
		}
	}
	if n := len(s.processes.list()); n != 0 {
		t.Errorf("a blocked command started %d processes, want 0", n)
	}

	// Something that merely mentions git still runs.
	if got := call(t, s, "tools/call", map[string]any{
		"name":      "run_command",
		"arguments": map[string]any{"command": "echo git"},
	}); got["isError"] == true {
		t.Errorf("a command that only mentions git was blocked: %v", got)
	}
}

// TestGitDisabledSkipsGitCommandTools covers the configured commands: one that
// runs git is not registered at all while git is off.
func TestGitDisabledSkipsGitCommandTools(t *testing.T) {
	s, _ := newTestServer(t)
	s.mu.Lock()
	s.cfg.Git.Enabled = false
	s.mu.Unlock()

	s.registerCommandTools([]CommandConfig{
		{Name: "build", Description: "Build it.", Command: "echo built"},
		{Name: "sync", Description: "Sync it.", Command: "git pull --rebase"},
	})

	listed := call(t, s, "tools/list", map[string]any{})
	tools, _ := listed["tools"].([]any)
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		if tool["name"] == "sync" {
			t.Fatalf("a git command was registered while git is disabled")
		}
	}
	got := call(t, s, "tools/call", map[string]any{"name": "project_commands"})
	body, _ := got["content"].([]any)
	block, _ := body[0].(map[string]any)
	if text, _ := block["text"].(string); strings.Contains(text, "sync") {
		t.Errorf("project_commands still lists the git command: %s", text)
	}
}
