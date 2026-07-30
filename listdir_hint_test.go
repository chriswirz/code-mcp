package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestInvokesListDir(t *testing.T) {
	flagged := []string{
		"dir",
		"  ls   -la  ",
		"DIR /s",
		"ls.exe -la",
		"/bin/ls -la",
		`C:\Windows\System32\cmd.exe /c dir`,
		"sudo ls /root",
		"env FOO=bar ls",
		"cd sub && ls",
		"go build ./... && dir",
		"echo hi; ls",
		"go test ./... | ls",
	}
	for _, line := range flagged {
		if !invokesListDir(line) {
			t.Errorf("invokesListDir(%q) = false, want true", line)
		}
	}

	notFlagged := []string{
		"go build ./...",
		"lsof -i",
		"ls-lint",
		"lsblk",
		"grep dir README.md",
		"echo list the directory with ls",
		"cat directory.txt",
		"npm run build",
	}
	for _, line := range notFlagged {
		if invokesListDir(line) {
			t.Errorf("invokesListDir(%q) = true, want false", line)
		}
	}
}

// TestRunCommandWarnsOnDirAndLs is the point of the nudge: a model that
// shells out to list a directory still gets its output, but is told to use
// list_directory next time instead.
func TestRunCommandWarnsOnDirAndLs(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	command := "dir"
	if runtime.GOOS != "windows" {
		command = "ls"
	}
	got := call(t, s, "tools/call", map[string]any{
		"name":      "run_command",
		"arguments": map[string]any{"command": command},
	})
	if got["isError"] == true {
		t.Fatalf("run_command %q failed: %v", command, got)
	}
	body, _ := got["content"].([]any)
	block, _ := body[0].(map[string]any)
	text, _ := block["text"].(string)
	if !strings.Contains(text, "list_directory") {
		t.Errorf("run_command %q result missing the list_directory nudge:\n%s", command, text)
	}
}

// TestRunCommandNoWarningForUnrelatedCommands makes sure the nudge is
// specific to dir/ls and does not follow every run_command call around.
func TestRunCommandNoWarningForUnrelatedCommands(t *testing.T) {
	s, _ := newTestServer(t)
	defer s.processes.stopAll()

	command := "echo hi"
	got := call(t, s, "tools/call", map[string]any{
		"name":      "run_command",
		"arguments": map[string]any{"command": command},
	})
	if got["isError"] == true {
		t.Fatalf("run_command %q failed: %v", command, got)
	}
	body, _ := got["content"].([]any)
	block, _ := body[0].(map[string]any)
	text, _ := block["text"].(string)
	if strings.Contains(text, "list_directory") {
		t.Errorf("run_command %q result should not carry the list_directory nudge:\n%s", command, text)
	}
}

// TestRunCommandDescriptionMentionsListDirectory pins the description-level
// half of the nudge, alongside the runtime warning: a model choosing a tool
// before it ever calls run_command should already see the steer.
func TestRunCommandDescriptionMentionsListDirectory(t *testing.T) {
	s := NewServer("test", "test", "", false)
	s.ws = NewWorkspace(WorkspaceConfig{Root: t.TempDir()})
	s.registerShellTool()

	tool, ok := s.tools["run_command"]
	if !ok {
		t.Fatal("run_command was not registered")
	}
	if !strings.Contains(tool.def.Description, "list_directory") {
		t.Errorf("run_command description does not mention list_directory:\n%s", tool.def.Description)
	}
}
