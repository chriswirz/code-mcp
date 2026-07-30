package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr(n int) *int { return &n }

func TestGitDiffArgs(t *testing.T) {
	cases := []struct {
		name     string
		staged   bool
		stat     bool
		nameOnly bool
		context  *int
		revision string
		path     string
		want     string
	}{
		{name: "default is a full patch", want: "diff"},
		{name: "stat", stat: true, want: "diff --stat"},
		{name: "name_only", nameOnly: true, want: "diff --name-only"},
		{name: "name_only beats stat", stat: true, nameOnly: true, want: "diff --name-only"},
		{name: "context zero", context: ptr(0), want: "diff --unified=0"},
		{name: "context with a path", context: ptr(1), path: "a.go", want: "diff --unified=1 -- a.go"},
		{name: "staged stat", staged: true, stat: true, want: "diff --staged --stat"},
		{name: "revision", revision: "HEAD~2", want: "diff HEAD~2"},
		{name: "revision and path", revision: "main", path: "cmd", want: "diff main -- cmd"},
		// --unified is meaningless alongside a summary, and git rejects the pair.
		{name: "context dropped for stat", stat: true, context: ptr(0), want: "diff --stat"},
		{name: "context dropped for name_only", nameOnly: true, context: ptr(5), want: "diff --name-only"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strings.Join(
				gitDiffArgs(tc.staged, tc.stat, tc.nameOnly, tc.context, tc.revision, tc.path), " ")
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestShippedConfigsParse guards the files that are edited by hand and are
// otherwise only validated when someone restarts the server. It goes through
// LoadConfig and Normalize rather than json.Unmarshal, because those are what
// startup actually does: they strip a BOM, reject unknown fields, and validate
// the result. A raw Unmarshal would both miss typos and fail on a byte order
// mark that the server handles perfectly well.
func TestShippedConfigsParse(t *testing.T) {
	for _, path := range []string{"config.json", "config.example.json"} {
		t.Run(path, func(t *testing.T) {
			if _, err := os.Stat(path); err != nil {
				t.Skipf("not present: %v", err)
			}
			cfg, err := LoadConfig(path, true)
			if err != nil {
				t.Fatalf("does not load: %v", err)
			}
			if err := cfg.Normalize(t.TempDir()); err != nil {
				t.Fatalf("does not validate: %v", err)
			}
			if strings.TrimSpace(cfg.Server.Instructions) == "" {
				t.Error("no server.instructions")
			}
		})
	}

	t.Run("embedded example", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(ExampleConfig), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path, true)
		if err != nil {
			t.Fatalf("ExampleConfig does not load: %v", err)
		}
		if err := cfg.Normalize(t.TempDir()); err != nil {
			t.Fatalf("ExampleConfig does not validate: %v", err)
		}
	})

	t.Run("a byte order mark is tolerated", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		body := append([]byte{0xEF, 0xBB, 0xBF}, []byte(ExampleConfig)...)
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadConfig(path, true); err != nil {
			t.Fatalf("BOM should be stripped: %v", err)
		}
	})
}
