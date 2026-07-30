package main

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The session id the tunnel server issues is what reclaims the same public URL
// after a restart, so it is written back into config.json. These tests pin the
// two things that makes worth doing: the id survives a reload, and nothing else
// in the file is disturbed on the way.

func TestSaveSessionIDPersistsIntoConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(ExampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.SaveSessionID("sess-abc123"); err != nil {
		t.Fatalf("SaveSessionID: %v", err)
	}
	if cfg.Tunnel.SessionID != "sess-abc123" {
		t.Errorf("in-memory session id = %q", cfg.Tunnel.SessionID)
	}

	reloaded, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Tunnel.SessionID != "sess-abc123" {
		t.Errorf("persisted session id = %q, want sess-abc123", reloaded.Tunnel.SessionID)
	}

	// Every other section has to survive the rewrite untouched. The commands
	// are the ones that would hurt most to lose: they are hand-written, and
	// nothing else in the file records how this project is built.
	if len(reloaded.Commands) != len(cfg.Commands) || len(reloaded.Commands) == 0 {
		t.Fatalf("commands: %d survived, started with %d", len(reloaded.Commands), len(cfg.Commands))
	}
	if reloaded.Commands[0].Name != cfg.Commands[0].Name || reloaded.Commands[0].Command != cfg.Commands[0].Command {
		t.Errorf("the first command changed: %+v", reloaded.Commands[0])
	}
	if reloaded.Server.URL != cfg.Server.URL || reloaded.Git.GitPath != cfg.Git.GitPath {
		t.Errorf("the rewrite disturbed other sections: url=%q git=%q", reloaded.Server.URL, reloaded.Git.GitPath)
	}
	if reloaded.Tunnel.APIKeyEnv != cfg.Tunnel.APIKeyEnv {
		t.Errorf("the rewrite disturbed a sibling key in the same section: %q", reloaded.Tunnel.APIKeyEnv)
	}

	// Re-saving the same id must not touch the file: the client calls back on
	// every reconnect, and rewriting the config each time would be churn.
	before, _ := os.ReadFile(path)
	if err := cfg.SaveSessionID("sess-abc123"); err != nil {
		t.Fatalf("second SaveSessionID: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("re-saving the same session id rewrote the file")
	}
}

func TestSaveSessionIDWithoutConfigFile(t *testing.T) {
	// Started from flags alone, with no config.json: there is nowhere to keep
	// the id. That has to be reported rather than swallowed, and it is not a
	// failure of the tunnel itself.
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "config.json"), false)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if err := cfg.SaveSessionID("sess-xyz"); !errors.Is(err, ErrNoConfigFile) {
		t.Fatalf("SaveSessionID error = %v, want ErrNoConfigFile", err)
	}
	if cfg.Tunnel.SessionID != "sess-xyz" {
		t.Error("the id should still be held in memory for the life of the process")
	}
}

func TestSessionFileTakesOverFromTheConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(ExampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Tunnel.SessionFile = filepath.Join(dir, "session")
	quiet := log.New(io.Discard, "", 0)

	before, _ := os.ReadFile(path)
	if err := sessionSaver(&cfg, quiet)("sess-from-file"); err != nil {
		t.Fatalf("sessionSaver: %v", err)
	}

	// Naming a file says where the id goes; the config must be left alone, so
	// the two stores can never disagree about which id is current.
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Error("config.json was rewritten even though a session file was named")
	}
	saved, err := os.ReadFile(cfg.Tunnel.SessionFile)
	if err != nil {
		t.Fatalf("session file: %v", err)
	}
	if strings.TrimSpace(string(saved)) != "sess-from-file" {
		t.Errorf("session file holds %q", strings.TrimSpace(string(saved)))
	}

	// On restart the file is read, since nothing was written to the config.
	if got := tunnelSession(cfg.Tunnel); got != "sess-from-file" {
		t.Errorf("tunnelSession = %q, want the id from the file", got)
	}
	// A session id written into the config by hand still overrides it: that is
	// an operator naming the session to resume, not an automatic store.
	cfg.Tunnel.SessionID = "explicitly-configured"
	if got := tunnelSession(cfg.Tunnel); got != "explicitly-configured" {
		t.Errorf("tunnelSession = %q, want the explicitly configured id", got)
	}
}

func TestConfigIsUsedWhenNoSessionFileIsNamed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(ExampleConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	cfg.Tunnel.SessionFile = ""
	if err := sessionSaver(&cfg, log.New(io.Discard, "", 0))("sess-in-config"); err != nil {
		t.Fatalf("sessionSaver: %v", err)
	}
	reloaded, err := LoadConfig(path, true)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Tunnel.SessionID != "sess-in-config" {
		t.Errorf("persisted session id = %q, want sess-in-config", reloaded.Tunnel.SessionID)
	}
}
