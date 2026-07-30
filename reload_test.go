package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// reloadServer builds a server whose configuration lives in a real file, with
// the reloader wired up exactly as main does.
func reloadServer(t *testing.T, commands []CommandConfig) (*Server, string, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "config.json")

	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Commands = commands
	writeConfigFile(t, path, cfg)

	loaded, err := LoadConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := loaded.Normalize(root); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	logger := log.New(logs, "", 0)

	s := NewServer(loaded.Server.Name, "test", loaded.Server.Instructions, loaded.Server.LegacyCompatibility)
	s.logger = logger
	if err := s.applyConfig(loaded); err != nil {
		t.Fatal(err)
	}
	s.reload = newConfigReloader(path, true, root, loaded, logger, nil, s.applyConfig)
	return s, path, logs
}

func writeConfigFile(t *testing.T, path string, cfg Config) {
	t.Helper()
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasTool(s *Server, name string) bool {
	for _, n := range s.ToolNames() {
		if n == name {
			return true
		}
	}
	return false
}

// An edit to config.json takes effect on the next request, without a restart.
func TestConfigIsRereadBeforeEachCycle(t *testing.T) {
	s, path, _ := reloadServer(t, []CommandConfig{{Name: "build", Description: "Build it.", Command: "echo built"}})
	if hasTool(s, "deploy") {
		t.Fatal("the deploy command should not exist yet")
	}

	cfg := DefaultConfig()
	cfg.Workspace.Root = filepath.Dir(path)
	cfg.Commands = []CommandConfig{{Name: "deploy", Description: "Ship it.", Command: "echo shipped"}}
	writeConfigFile(t, path, cfg)

	call(t, s, "tools/list", map[string]any{})
	if !hasTool(s, "deploy") {
		t.Errorf("the reloaded command is missing: %v", s.ToolNames())
	}
	if hasTool(s, "build") {
		t.Errorf("a command removed from the file is still registered: %v", s.ToolNames())
	}
}

// A config file that cannot be parsed must leave the running values alone, and
// must say so once rather than on every request.
func TestUnreadableConfigKeepsPreviousValues(t *testing.T) {
	s, path, logs := reloadServer(t, []CommandConfig{{Name: "build", Description: "Build it.", Command: "echo built"}})

	if err := os.WriteFile(path, []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		call(t, s, "tools/list", map[string]any{})
	}
	if !hasTool(s, "build") {
		t.Errorf("the previous command was lost on a bad reload: %v", s.ToolNames())
	}
	if got := strings.Count(logs.String(), "could not reload"); got != 1 {
		t.Errorf("a broken config was reported %d times, want once:\n%s", got, logs.String())
	}

	// Deleting the file is the same situation: keep running on what we have.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	call(t, s, "tools/list", map[string]any{})
	if !hasTool(s, "build") {
		t.Errorf("the previous command was lost when the file was removed: %v", s.ToolNames())
	}

	// And when the file comes back, it is picked up again.
	cfg := DefaultConfig()
	cfg.Workspace.Root = filepath.Dir(path)
	cfg.Commands = []CommandConfig{{Name: "deploy", Description: "Ship it.", Command: "echo shipped"}}
	writeConfigFile(t, path, cfg)
	call(t, s, "tools/list", map[string]any{})
	if !hasTool(s, "deploy") {
		t.Errorf("a repaired config was not picked up: %v", s.ToolNames())
	}
	if !strings.Contains(logs.String(), "is readable again") {
		t.Errorf("the recovery was not reported:\n%s", logs.String())
	}
}

// Reloading must not undo a command-line flag: the operator asked for it on
// this run, and the file has no say in it.
func TestReloadReappliesCommandLineFlags(t *testing.T) {
	s, path, _ := reloadServer(t, nil)
	root := filepath.Dir(path)
	pinned := filepath.Join(root, "pinned")
	if err := os.Mkdir(pinned, 0o755); err != nil {
		t.Fatal(err)
	}
	s.reload.overlay = func(cfg *Config) { cfg.Workspace.Root = pinned }

	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Commands = []CommandConfig{{Name: "deploy", Description: "Ship it.", Command: "echo shipped"}}
	writeConfigFile(t, path, cfg)

	call(t, s, "tools/list", map[string]any{})
	if !hasTool(s, "deploy") {
		t.Error("the reload did not happen at all")
	}
	if s.ws.Root != pinned {
		t.Errorf("workspace root = %q, want the flag's %q", s.ws.Root, pinned)
	}
}

// An unchanged file must not churn the tool registry, since re-registering on
// every request would be pure waste.
func TestUnchangedConfigIsNotReapplied(t *testing.T) {
	s, _, logs := reloadServer(t, []CommandConfig{{Name: "build", Description: "Build it.", Command: "echo built"}})
	for i := 0; i < 3; i++ {
		call(t, s, "tools/list", map[string]any{})
	}
	if strings.Contains(logs.String(), "reloaded") {
		t.Errorf("an untouched config was reapplied:\n%s", logs.String())
	}
}

// The instructions are rebuilt on reload, not appended to.
func TestReloadReplacesInstructions(t *testing.T) {
	s, path, _ := reloadServer(t, nil)
	cfg := DefaultConfig()
	cfg.Workspace.Root = filepath.Dir(path)
	cfg.Server.Instructions = "Second instructions."
	writeConfigFile(t, path, cfg)

	call(t, s, "server/discover", map[string]any{})
	result := s.discover(context.Background())
	if strings.Count(result.Instructions, "Second instructions.") != 1 {
		t.Errorf("instructions were not replaced cleanly:\n%s", result.Instructions)
	}
	if strings.Contains(result.Instructions, DefaultConfig().Server.Instructions) {
		t.Errorf("the previous instructions survived the reload:\n%s", result.Instructions)
	}
}
