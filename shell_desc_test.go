package main

import (
	"runtime"
	"strings"
	"testing"
)

func TestShellSyntaxNote(t *testing.T) {
	if note := shellSyntaxNote("cmd"); !strings.Contains(note, "findstr") {
		t.Errorf("the cmd note should name the replacements: %q", note)
	}
	if note := shellSyntaxNote("sh"); !strings.Contains(note, "POSIX") {
		t.Errorf("the sh note should name the syntax: %q", note)
	}
	if note := shellSyntaxNote("fish"); !strings.Contains(note, "system_info") {
		t.Errorf("an unknown shell should defer to system_info: %q", note)
	}
}

// TestRunCommandDescriptionNamesPlatform pins the property that matters: the
// description states the platform this server is actually running on, rather
// than describing every platform and leaving the model to pick.
func TestRunCommandDescriptionNamesPlatform(t *testing.T) {
	s := NewServer("test", "test", "", false)
	s.ws = NewWorkspace(WorkspaceConfig{Root: t.TempDir()})
	s.registerShellTool()

	tool, ok := s.tools["run_command"]
	if !ok {
		t.Fatal("run_command was not registered")
	}
	desc := tool.def.Description

	platform := osDisplayName(runtime.GOOS)
	if !strings.Contains(desc, platform) {
		t.Errorf("description does not name %s:\n%s", platform, desc)
	}
	if !strings.Contains(desc, runtime.GOARCH) {
		t.Errorf("description does not name the architecture:\n%s", desc)
	}

	shellPath, flavor := shellCommandLine()
	if !strings.Contains(desc, shellPath) {
		t.Errorf("description does not name the shell %q:\n%s", shellPath, desc)
	}
	if !strings.Contains(desc, shellSyntaxNote(flavor)) {
		t.Errorf("description does not carry the syntax note for %q:\n%s", flavor, desc)
	}

	// The old wording talked about Windows unconditionally. On a non-Windows
	// host that is now actively misleading, so it must not appear.
	if runtime.GOOS != "windows" && strings.Contains(desc, "Windows") {
		t.Errorf("description mentions Windows on a %s host:\n%s", runtime.GOOS, desc)
	}
}

func TestCommandToolDescriptionNamesShell(t *testing.T) {
	tool := commandTool(CommandConfig{
		Name:        "test",
		Description: "Run the tests.",
		Command:     "go test {{args}}",
		AcceptsArgs: true,
		DefaultArgs: "./...",
	})

	_, flavor := shellCommandLine()
	for _, want := range []string{"go test {{args}}", flavor, osDisplayName(runtime.GOOS)} {
		if !strings.Contains(tool.Description, want) {
			t.Errorf("description missing %q:\n%s", want, tool.Description)
		}
	}
	if !strings.Contains(tool.InputSchema["properties"].(map[string]any)["args"].(map[string]any)["description"].(string), "./...") {
		t.Error("the args description should name the default")
	}
}
