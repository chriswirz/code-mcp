package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tunnelTestConfig(t *testing.T) Config {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Workspace.Root = t.TempDir()
	cfg.Tunnel = TunnelConfig{
		Enabled:   true,
		ServerURL: "https://tunnel.example.com",
		APIKey:    "key",
	}
	return cfg
}

func TestTunnelNormalize(t *testing.T) {
	t.Run("accepts a complete section", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		if err := cfg.Normalize(cfg.Workspace.Root); err != nil {
			t.Fatalf("Normalize: %v", err)
		}
	})

	t.Run("requires a server url", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		cfg.Tunnel.ServerURL = ""
		err := cfg.Normalize(cfg.Workspace.Root)
		if err == nil || !strings.Contains(err.Error(), "tunnel.server_url") {
			t.Fatalf("want a tunnel.server_url error, got %v", err)
		}
	})

	t.Run("requires an api key", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		cfg.Tunnel.APIKey = ""
		cfg.Tunnel.APIKeyEnv = "CODEMCP_TEST_TUNNEL_KEY_UNSET"
		err := cfg.Normalize(cfg.Workspace.Root)
		if err == nil || !strings.Contains(err.Error(), "CODEMCP_TEST_TUNNEL_KEY_UNSET") {
			t.Fatalf("want an api key error naming the env var, got %v", err)
		}
	})

	t.Run("reads the api key from the environment", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		cfg.Tunnel.APIKey = ""
		t.Setenv("TUNNEL_API_KEY", "from-env")
		if err := cfg.Normalize(cfg.Workspace.Root); err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if got := cfg.Tunnel.APIKeyValue(); got != "from-env" {
			t.Fatalf("APIKeyValue = %q, want %q", got, "from-env")
		}
	})

	t.Run("rejects the stdio transport", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		cfg.Server.Transport = "stdio"
		err := cfg.Normalize(cfg.Workspace.Root)
		if err == nil || !strings.Contains(err.Error(), "transport") {
			t.Fatalf("want a transport error, got %v", err)
		}
	})

	t.Run("resolves the session file against the workspace", func(t *testing.T) {
		cfg := tunnelTestConfig(t)
		cfg.Tunnel.SessionFile = ".codemcp-tunnel-session"
		if err := cfg.Normalize(cfg.Workspace.Root); err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		want := filepath.Join(cfg.Workspace.Root, ".codemcp-tunnel-session")
		if cfg.Tunnel.SessionFile != want {
			t.Fatalf("SessionFile = %q, want %q", cfg.Tunnel.SessionFile, want)
		}
	})

	t.Run("leaves the section alone when it is disabled", func(t *testing.T) {
		cfg := DefaultConfig()
		cfg.Workspace.Root = t.TempDir()
		cfg.Tunnel = TunnelConfig{ServerURL: "not a url", SessionFile: "relative"}
		if err := cfg.Normalize(cfg.Workspace.Root); err != nil {
			t.Fatalf("Normalize: %v", err)
		}
	})
}

func TestTunnelSessionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session")
	cfg := TunnelConfig{SessionFile: path}

	if got := tunnelSession(cfg); got != "" {
		t.Fatalf("tunnelSession before any run = %q, want empty", got)
	}
	if err := saveTunnelSession(path, "abc123"); err != nil {
		t.Fatalf("saveTunnelSession: %v", err)
	}
	if got := tunnelSession(cfg); got != "abc123" {
		t.Fatalf("tunnelSession = %q, want %q", got, "abc123")
	}
	// An explicitly configured id wins over the persisted one.
	cfg.SessionID = "explicit"
	if got := tunnelSession(cfg); got != "explicit" {
		t.Fatalf("tunnelSession = %q, want %q", got, "explicit")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("session file: %v", err)
	}
}

func TestTunnelFlags(t *testing.T) {
	opts, err := parseFlags([]string{
		"--tunnel", "https://tunnel.example.com",
		"--tunnel-key", "key",
		"--tunnel-subdomain", "my-mcp",
		"--tunnel-session-file", "s",
		"--tunnel-only",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if opts.tunnel != "https://tunnel.example.com" || opts.tunnelKey != "key" ||
		opts.tunnelSub != "my-mcp" || opts.tunnelSession != "s" || !opts.tunnelOnly {
		t.Fatalf("flags not parsed: %+v", opts)
	}
}
