package main

import (
	"strings"
	"testing"
)

// A call a server will not serve has to come back as something the model can
// read and act on. These tests are about that answer: never a protocol error,
// always the setting that governs it and somewhere to go next.

// serverWith builds a server from a configuration the test adjusts, so the
// switched-off cases can be exercised without a repository or a database.
func serverWith(t *testing.T, adjust func(*Config)) *Server {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Commands = []CommandConfig{
		{Name: "build", Description: "Build it.", Command: "echo built"},
		{Name: "sync", Description: "Sync it.", Command: "git pull --rebase"},
	}
	adjust(&cfg)
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.cfg = cfg
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerAll(cfg)
	return s
}

// errorText runs a tool and insists the answer was an error result rather than
// a protocol error, returning its text.
func errorText(t *testing.T, s *Server, name string) string {
	t.Helper()
	got := call(t, s, "tools/call", map[string]any{"name": name})
	if got["isError"] != true {
		t.Fatalf("%s should have come back as an error result: %v", name, got)
	}
	body, _ := got["content"].([]any)
	if len(body) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	block, _ := body[0].(map[string]any)
	text, _ := block["text"].(string)
	return text
}

func TestDisabledGitToolExplainsItself(t *testing.T) {
	s := serverWith(t, func(c *Config) { c.Git.Enabled = false })

	text := errorText(t, s, "git_status")
	for _, want := range []string{"git.enabled is false", "switched off", "Retrying will not help"} {
		if !strings.Contains(text, want) {
			t.Errorf("answer should contain %q: %s", want, text)
		}
	}
}

func TestDisabledPushNamesItsOwnSetting(t *testing.T) {
	s := serverWith(t, func(c *Config) {
		c.Git.Enabled = true
		c.Git.AllowPush = false
		c.Git.AllowRestore = false
	})

	if text := errorText(t, s, "git_push"); !strings.Contains(text, "git.allow_push is false") {
		t.Errorf("git_push should name allow_push: %s", text)
	}
	if text := errorText(t, s, "git_restore"); !strings.Contains(text, "git.allow_restore is false") {
		t.Errorf("git_restore should name allow_restore: %s", text)
	}
	// The tools that are on are unaffected.
	if _, ok := s.lookupTool("git_status"); !ok {
		t.Error("git_status should still be registered")
	}
}

func TestDisabledDatabaseAndDownloadsExplainThemselves(t *testing.T) {
	s := serverWith(t, func(c *Config) { c.Downloads.Enabled = false })

	if text := errorText(t, s, "db_query"); !strings.Contains(text, "no database is configured") {
		t.Errorf("db_query should say there is no database: %s", text)
	}
	if text := errorText(t, s, "get_download_link"); !strings.Contains(text, "downloads.enabled is false") {
		t.Errorf("get_download_link should name the setting: %s", text)
	}
	if text := errorText(t, s, "get_download_link"); !strings.Contains(text, "read_file") {
		t.Errorf("the answer should point somewhere useful: %s", text)
	}
}

func TestDisabledGitCommandExplainsItself(t *testing.T) {
	s := serverWith(t, func(c *Config) { c.Git.Enabled = false })

	text := errorText(t, s, "sync")
	if !strings.Contains(text, "git.enabled is false") || !strings.Contains(text, "git pull --rebase") {
		t.Errorf("the answer should name the command and the setting: %s", text)
	}
	if _, ok := s.lookupTool("build"); !ok {
		t.Error("the commands that do not run git should still be registered")
	}
}

func TestReadOnlyWorkspaceExplainsWrites(t *testing.T) {
	s := serverWith(t, func(c *Config) { c.Workspace.AllowWrite = false })

	// write_file is still registered and refuses on its own terms.
	got := call(t, s, "tools/call", map[string]any{
		"name": "write_file", "arguments": map[string]any{"path": "a.txt", "content": "x"},
	})
	if got["isError"] != true {
		t.Fatalf("a write on a read-only workspace should be an error result: %v", got)
	}
	body, _ := got["content"].([]any)
	block, _ := body[0].(map[string]any)
	if text, _ := block["text"].(string); !strings.Contains(text, "allow_write") {
		t.Errorf("the refusal should name the setting: %s", text)
	}
}

func TestUnknownToolSuggestsTheNearestNames(t *testing.T) {
	s := serverWith(t, func(c *Config) {})

	text := errorText(t, s, "readfile")
	if !strings.Contains(text, "read_file") {
		t.Errorf("a punctuation slip should be pointed at the real name: %s", text)
	}
	if text := errorText(t, s, "totally_made_up"); !strings.Contains(text, "tools/list") {
		t.Errorf("an unknown name should still point at the tool list: %s", text)
	}
}

func TestSuggestTools(t *testing.T) {
	s := serverWith(t, func(c *Config) {})
	cases := map[string]string{
		"readfile":   "read_file",
		"read-file":  "read_file",
		"grepFiles":  "grep_files",
		"applyDiff":  "apply_diff",
		"list_files": "list_directory",
	}
	for asked, want := range cases {
		got := s.suggestTools(asked)
		if len(got) == 0 {
			t.Errorf("suggestTools(%q) found nothing, want %q among them", asked, want)
			continue
		}
		found := false
		for _, name := range got {
			if name == want {
				found = true
			}
		}
		if !found {
			t.Errorf("suggestTools(%q) = %v, want %q among them", asked, got, want)
		}
	}
	if got := s.suggestTools("zzzz_nothing_like_this"); len(got) != 0 {
		t.Errorf("suggestTools should not reach for something unrelated: %v", got)
	}
}
