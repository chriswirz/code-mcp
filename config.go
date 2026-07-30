package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Config is the whole of config.json.
type Config struct {
	Server    ServerConfig    `json:"server"`
	Tunnel    TunnelConfig    `json:"tunnel"`
	Workspace WorkspaceConfig `json:"workspace"`
	Commands  []CommandConfig `json:"commands"`
	Git       GitConfig       `json:"git"`
	GitHub    GitHubConfig    `json:"github"`
	Database  DatabaseConfig  `json:"database"`
	Downloads DownloadsConfig `json:"downloads"`
	Sudo      SudoConfig      `json:"sudo"`

	// path is the file this was loaded from, so a session id the tunnel
	// server issues can be written back to it. It is not part of the file.
	path string
}

// Path returns the file the configuration was loaded from.
func (c *Config) Path() string { return c.path }

// ErrNoConfigFile reports that there is nowhere to persist a session id: the
// server is running on its defaults and flags, with no config.json to write
// back to. It is not a failure of the tunnel, only of remembering the URL.
var ErrNoConfigFile = errors.New("no config file to write to")

// configWriteMu serialises the read-modify-write of the config file. It is a
// package variable rather than a field so that Config stays copyable - the
// server keeps its own copy, and a mutex inside would make that a vet error.
var configWriteMu sync.Mutex

// SaveSessionID writes a session id the tunnel server issued back into the
// tunnel section of config.json, so restarting the server reclaims the same
// public URL instead of being handed a new one.
//
// Every other key is left exactly as it was found: the file is decoded into
// raw messages, one field is replaced, and the result is written through a
// temporary file so an interrupted write cannot truncate the configuration.
func (c *Config) SaveSessionID(id string) error {
	if id == "" || id == c.Tunnel.SessionID {
		return nil
	}
	c.Tunnel.SessionID = id
	if c.path == "" {
		return ErrNoConfigFile
	}
	if _, err := os.Stat(c.path); err != nil {
		if os.IsNotExist(err) {
			return ErrNoConfigFile
		}
		return err
	}
	return c.updateSection("tunnel", "session_id", func(section map[string]json.RawMessage) error {
		encoded, err := json.Marshal(id)
		if err != nil {
			return err
		}
		section["session_id"] = encoded
		return nil
	})
}

// updateSection rewrites one top-level section of the config file, leaving
// every other key as it was found.
func (c *Config) updateSection(name, field string, mutate func(map[string]json.RawMessage) error) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	raw, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF}), &doc); err != nil {
		return fmt.Errorf("%s: %w", c.path, err)
	}
	section := map[string]json.RawMessage{}
	if b, ok := doc[name]; ok {
		if err := json.Unmarshal(b, &section); err != nil {
			return fmt.Errorf("%s: %s: %w", c.path, name, err)
		}
	}
	if err := mutate(section); err != nil {
		return fmt.Errorf("updating %s.%s: %w", name, field, err)
	}
	if doc[name], err = json.Marshal(section); err != nil {
		return err
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	// Written to a temporary file and renamed, so a crash mid-write leaves the
	// old configuration intact rather than half a file. 0600 because the
	// config can carry an auth token and a tunnel key.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// ServerConfig covers identity and transport.
type ServerConfig struct {
	Name         string `json:"name"`
	Version      string `json:"version"`
	Instructions string `json:"instructions"`
	Transport    string `json:"transport"` // "http" (default) or "stdio"
	URL          string `json:"url"`       // full MCP endpoint URL clients connect to

	// AdditionalURLs serves several endpoints at once, typically one http and
	// one https on different ports. When set it replaces URL entirely.
	AdditionalURLs []string `json:"urls,omitempty"`

	AllowedOrigins []string `json:"allowed_origins"`

	// AllowedHeaders is what a CORS preflight is told a browser may send.
	// ["*"] allows any header. Empty means echo whatever the preflight asks
	// for, which is also permissive but keeps the answer specific; naming
	// headers explicitly is the way to restrict them.
	AllowedHeaders []string `json:"allowed_headers"`

	// AllowPrivateNetwork answers Chrome's Private Network Access preflight,
	// which a page on a public address must pass before it may reach a server
	// on a private one such as 127.0.0.1. Only an origin that is already
	// allowed gets the grant. On by default.
	AllowPrivateNetwork bool `json:"allow_private_network"`

	AuthToken string `json:"auth_token"` // optional bearer token

	// TLS. A browser page served over HTTPS cannot fetch an http:// URL, so an
	// https endpoint URL is the way to be reachable from one. Either point at a
	// certificate and key, or set TLSSelfSigned to generate one on startup.
	TLSCertFile   string `json:"tls_cert_file"`
	TLSKeyFile    string `json:"tls_key_file"`
	TLSSelfSigned bool   `json:"tls_self_signed"`

	// LegacyCompatibility serves the initialize-based revisions 2025-03-26
	// through 2025-11-25 alongside the current one, for clients that have not
	// caught up. On by default.
	LegacyCompatibility bool `json:"legacy_compatibility"`

	// SessionTimeoutSeconds is how long an idle legacy session is kept.
	SessionTimeoutSeconds int `json:"session_timeout_seconds"`
}

// TunnelConfig exposes this server on a public HTTPS URL through an
// https-tunnel server, without opening a port on this machine. The MCP handler
// is served in process by the tunnel client, so it works even where binding a
// local port is awkward, and it is what makes the server reachable from a
// hosted client that cannot see this network.
type TunnelConfig struct {
	Enabled bool `json:"enabled"`

	// ServerURL is the tunnel control plane, e.g. https://tunnel.example.com.
	ServerURL string `json:"server_url"`

	// APIKey authenticates against that server. Leave it empty to read
	// APIKeyEnv instead, which keeps the key out of the config file.
	APIKey    string `json:"api_key"`
	APIKeyEnv string `json:"api_key_env"`

	// Subdomain asks for a particular label. The server grants it when free
	// and issues a random one otherwise, so read the URL the client reports
	// rather than assuming this one.
	Subdomain string `json:"subdomain"`

	// SessionID resumes a previous session, keeping its public URL. It is
	// normally left empty and managed through SessionFile.
	SessionID string `json:"session_id"`

	// SessionFile persists the session id the server issues, so a restart
	// reclaims the same URL. Relative paths are resolved against the
	// workspace; empty means do not persist.
	SessionFile string `json:"session_file"`

	// Only serves the tunnel alone, with no local listener at all. By default
	// the same handler is served both ways, which is convenient while
	// developing.
	Only bool `json:"only"`

	ClientInfo string `json:"client_info"`
}

// APIKeyValue is the key to authenticate with, from the config or the
// environment variable it names.
func (t TunnelConfig) APIKeyValue() string {
	if t.APIKey != "" {
		return t.APIKey
	}
	if name := t.APIKeyEnvName(); name != "" {
		return os.Getenv(name)
	}
	return ""
}

// APIKeyEnvName is the environment variable the key is read from.
func (t TunnelConfig) APIKeyEnvName() string {
	if t.APIKeyEnv != "" {
		return t.APIKeyEnv
	}
	return "TUNNEL_API_KEY"
}

// WorkspaceConfig bounds what the file tools may touch.
type WorkspaceConfig struct {
	Root string `json:"root"` // default: the directory the server was started in; "." means the whole system
	// Unrestricted lifts containment: the file tools may read and write
	// anywhere on the machine. It is set by writing "." as the root, which is
	// the documented way to ask for a server that is not bounded by a project
	// directory, and can also be set on its own.
	Unrestricted bool     `json:"unrestricted,omitempty"`
	MaxFileBytes int64    `json:"max_file_bytes"`
	MaxResults   int      `json:"max_results"`
	Exclude      []string `json:"exclude"`
	AllowWrite   bool     `json:"allow_write"`
}

// CommandConfig is one user-defined command, exposed as its own MCP tool. This
// is the point of the file: the model should not have to guess how to build,
// test or lint this repository.
type CommandConfig struct {
	Name           string            `json:"name"`
	Description    string            `json:"description"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Dir            string            `json:"dir,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	AcceptsArgs    bool              `json:"accepts_args,omitempty"`
	DefaultArgs    string            `json:"default_args,omitempty"`
	ReadOnly       bool              `json:"read_only,omitempty"`
}

// GitConfig gates the git tools.
type GitConfig struct {
	Enabled       bool   `json:"enabled"`
	GitPath       string `json:"git_path"`
	AllowCommit   bool   `json:"allow_commit"`
	AllowPush     bool   `json:"allow_push"`
	AllowRestore  bool   `json:"allow_restore"`
	DefaultRemote string `json:"default_remote"`
}

// GitHubConfig gates the GitHub Actions tools, which shell out to the gh CLI.
type GitHubConfig struct {
	Enabled         bool   `json:"enabled"`
	GhPath          string `json:"gh_path"`
	Repo            string `json:"repo"` // owner/name; empty means infer from the checkout
	DefaultWorkflow string `json:"default_workflow"`
	DefaultRef      string `json:"default_ref"`
	AllowDispatch   bool   `json:"allow_dispatch"`
}

// DatabaseConfig is optional: the database tools only appear when it is
// enabled and carries enough credentials to build a DSN.
type DatabaseConfig struct {
	Enabled                 bool   `json:"enabled"`
	URL                     string `json:"url"` // takes precedence over the discrete fields
	Host                    string `json:"host"`
	Port                    int    `json:"port"`
	User                    string `json:"user"`
	Password                string `json:"password"`
	Database                string `json:"database"`
	SSLMode                 string `json:"sslmode"`
	MaxRows                 int    `json:"max_rows"`
	StatementTimeoutSeconds int    `json:"statement_timeout_seconds"`
	AllowWrite              bool   `json:"allow_write"`
}

// DownloadsConfig governs get_download_link: the temporary HTTP hosting of a
// workspace file at an unguessable URL. The links are served by the listener
// the MCP endpoint already has, under the endpoint path - /mcp/files/... for an
// endpoint on /mcp - so nothing extra is exposed. On stdio, where there is no
// such listener, the first link opens one on Addr.
type DownloadsConfig struct {
	Enabled bool `json:"enabled"`

	// DefaultTTLMinutes is how long a link lives when the caller does not say,
	// and MaxTTLMinutes is the longest it may ask for.
	DefaultTTLMinutes int `json:"default_ttl_minutes"`
	MaxTTLMinutes     int `json:"max_ttl_minutes"`

	// MaxFileBytes refuses to publish a file larger than this. Zero means no
	// limit; it is separate from workspace.max_file_bytes, which bounds what
	// the model may read into its context rather than what a browser may fetch.
	MaxFileBytes int64 `json:"max_file_bytes"`

	// MaxLinks bounds how many links are live at once. Past it, the link
	// closest to expiring is dropped to make room.
	MaxLinks int `json:"max_links"`

	// BaseURL overrides the URL links are built from, for when the endpoint a
	// client reaches is not the one this process serves - behind a reverse
	// proxy, say. It replaces everything up to and including the files
	// segment: "https://example.com/mcp/files".
	BaseURL string `json:"base_url"`

	// Addr is the standalone listener used only on stdio, where there is no
	// MCP listener to share. Port 0 picks a free one.
	Addr string `json:"addr"`
}

// DefaultConfig is the configuration used when no config.json is present.
func DefaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Name:           "code-mcp",
			Version:        version,
			Transport:      "http",
			URL:            "http://127.0.0.1:8765/mcp",
			AllowedOrigins: []string{"http://127.0.0.1", "http://localhost"},
			// Older clients are still common, so both eras are served unless
			// the operator turns the older one off.
			LegacyCompatibility:   true,
			SessionTimeoutSeconds: 7200,
			AllowPrivateNetwork:   true,
			Instructions: "Coding agent for the workspace this server was started in. " +
				"Call system_info before writing your first shell command or path: it reports the operating system, " +
				"the shell that run_command actually executes through, and the path separator, none of which are safe to infer. " +
				"Use the project commands (build, test, lint, ...) instead of guessing build incantations, " +
				"and the github_* tools to drive the GitHub Actions workflows that deploy this repository. " +
				"Ask git_diff for stat or name_only before a full patch, and prefer edit_file or multi_edit over write_file " +
				"on a file that already exists, so a change stays limited to what it names.",
		},
		Workspace: WorkspaceConfig{
			MaxFileBytes: 1 << 20,
			MaxResults:   200,
			Exclude:      []string{".git", "node_modules", "vendor", "dist", "out", "target", ".venv"},
			AllowWrite:   true,
		},
		Git: GitConfig{
			Enabled:       true,
			GitPath:       "git",
			AllowCommit:   true,
			AllowPush:     false,
			AllowRestore:  false,
			DefaultRemote: "origin",
		},
		GitHub: GitHubConfig{
			Enabled:       true,
			GhPath:        "gh",
			AllowDispatch: true,
		},
		Downloads: DownloadsConfig{
			Enabled:           true,
			DefaultTTLMinutes: 5,
			MaxTTLMinutes:     60,
			MaxFileBytes:      512 << 20,
			MaxLinks:          64,
			Addr:              "127.0.0.1:0",
		},
		Database: DatabaseConfig{
			Port:                    5432,
			SSLMode:                 "prefer",
			MaxRows:                 200,
			StatementTimeoutSeconds: 30,
		},
	}
}

// LoadConfig reads path on top of the defaults. A missing file at the default
// location is not an error; a missing explicitly requested file is.
func LoadConfig(path string, explicit bool) (Config, error) {
	cfg := DefaultConfig()
	cfg.path = path
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, err
	}
	// PowerShell's redirection writes a UTF-8 BOM, and `codemcp --example-config
	// > config.json` is the documented way to get started, so tolerate one.
	data = bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Normalize fills in derived values and validates the result. workspace is the
// directory the server was started in, used when the config names no root.
func (c *Config) Normalize(workspace string) error {
	if c.Server.Name == "" {
		c.Server.Name = "code-mcp"
	}
	if c.Server.Version == "" {
		c.Server.Version = version
	}
	switch c.Server.Transport {
	case "":
		c.Server.Transport = "http"
	case "http", "stdio":
	default:
		return fmt.Errorf("server.transport: want %q or %q, got %q", "http", "stdio", c.Server.Transport)
	}
	if c.Server.URL == "" {
		c.Server.URL = "http://127.0.0.1:8765/mcp"
	}
	if c.Server.SessionTimeoutSeconds <= 0 {
		c.Server.SessionTimeoutSeconds = 7200
	}
	if _, _, err := c.Endpoint(); err != nil {
		return err
	}
	if err := c.Sudo.validate(); err != nil {
		return err
	}

	root := c.Workspace.Root
	if root == "" {
		root = workspace
	}
	// A root of exactly "." asks for a server bounded by nothing: paths still
	// resolve against the working directory, but nothing is refused for being
	// outside it.
	if strings.TrimSpace(c.Workspace.Root) == "." {
		c.Workspace.Unrestricted = true
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("workspace.root: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("workspace.root: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace.root: %s is not a directory", abs)
	}
	c.Workspace.Root = abs
	if c.Workspace.MaxFileBytes <= 0 {
		c.Workspace.MaxFileBytes = 1 << 20
	}
	if c.Workspace.MaxResults <= 0 {
		c.Workspace.MaxResults = 200
	}

	if c.Downloads.DefaultTTLMinutes <= 0 {
		c.Downloads.DefaultTTLMinutes = 5
	}
	if c.Downloads.MaxTTLMinutes <= 0 {
		c.Downloads.MaxTTLMinutes = 60
	}
	if c.Downloads.DefaultTTLMinutes > c.Downloads.MaxTTLMinutes {
		return fmt.Errorf("downloads.default_ttl_minutes (%d) is longer than downloads.max_ttl_minutes (%d)",
			c.Downloads.DefaultTTLMinutes, c.Downloads.MaxTTLMinutes)
	}
	if c.Downloads.Addr == "" {
		c.Downloads.Addr = "127.0.0.1:0"
	}
	if c.Downloads.BaseURL != "" {
		u, err := url.Parse(c.Downloads.BaseURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("downloads.base_url: want an absolute http(s) URL, got %q", c.Downloads.BaseURL)
		}
	}

	seen := map[string]bool{}
	for i := range c.Commands {
		cmd := &c.Commands[i]
		if cmd.Name == "" {
			return fmt.Errorf("commands[%d]: name is required", i)
		}
		if !validToolName(cmd.Name) {
			return fmt.Errorf("commands[%d]: name %q may only contain letters, digits, _, - and .", i, cmd.Name)
		}
		if seen[cmd.Name] {
			return fmt.Errorf("commands[%d]: duplicate name %q", i, cmd.Name)
		}
		seen[cmd.Name] = true
		if cmd.Command == "" {
			return fmt.Errorf("commands[%d] (%s): command is required", i, cmd.Name)
		}
		if cmd.TimeoutSeconds <= 0 {
			cmd.TimeoutSeconds = 600
		}
		if cmd.Description == "" {
			cmd.Description = "Project command: " + cmd.Command
		}
	}

	if c.Git.GitPath == "" {
		c.Git.GitPath = "git"
	}
	if c.Git.DefaultRemote == "" {
		c.Git.DefaultRemote = "origin"
	}
	if c.GitHub.GhPath == "" {
		c.GitHub.GhPath = "gh"
	}
	if c.Database.Port == 0 {
		c.Database.Port = 5432
	}
	if c.Database.SSLMode == "" {
		c.Database.SSLMode = "prefer"
	}
	if c.Database.MaxRows <= 0 {
		c.Database.MaxRows = 200
	}
	if c.Database.StatementTimeoutSeconds <= 0 {
		c.Database.StatementTimeoutSeconds = 30
	}

	if c.Tunnel.Enabled {
		if c.Server.Transport != "http" {
			return fmt.Errorf("tunnel.enabled: the tunnel serves the HTTP handler, so it needs server.transport %q, not %q", "http", c.Server.Transport)
		}
		if c.Tunnel.ServerURL == "" {
			return fmt.Errorf("tunnel.server_url: required when the tunnel is enabled, e.g. https://tunnel.example.com")
		}
		u, err := url.Parse(c.Tunnel.ServerURL)
		if err != nil {
			return fmt.Errorf("tunnel.server_url %q: %w", c.Tunnel.ServerURL, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("tunnel.server_url: want an http(s) URL, got %q", c.Tunnel.ServerURL)
		}
		if c.Tunnel.APIKeyValue() == "" {
			return fmt.Errorf("tunnel.api_key: required when the tunnel is enabled, or set %s in the environment", c.Tunnel.APIKeyEnvName())
		}
		if c.Tunnel.SessionFile != "" && !filepath.IsAbs(c.Tunnel.SessionFile) {
			c.Tunnel.SessionFile = filepath.Join(c.Workspace.Root, c.Tunnel.SessionFile)
		}
	}
	return nil
}

// Endpoint splits the configured URL into a listen address and a path.
func (c *Config) Endpoint() (addr, path string, err error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return "", "", err
	}
	return endpoints[0].Addr, endpoints[0].Path, nil
}

// Endpoint is one URL this server answers on: where to listen, at what path,
// and whether the connection is encrypted.
type Endpoint struct {
	URL  string
	Addr string
	Path string
	TLS  bool
}

// URLs is every URL the server is configured to serve, in order. server.urls
// takes precedence over the single server.url.
func (s ServerConfig) URLs() []string {
	if len(s.AdditionalURLs) > 0 {
		return s.AdditionalURLs
	}
	if s.URL == "" {
		return nil
	}
	return []string{s.URL}
}

// Endpoints parses every configured URL. Serving both an http and an https
// endpoint at once is the point: a browser page on an https origin needs the
// encrypted one, while a local client is happier without the certificate.
func (c *Config) Endpoints() ([]Endpoint, error) {
	urls := c.Server.URLs()
	if len(urls) == 0 {
		return nil, fmt.Errorf("server.url: no endpoint URL configured")
	}
	var endpoints []Endpoint
	seen := map[string]string{}

	for _, raw := range urls {
		u, err := url.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("server.url %q: %w", raw, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("server.url: want an http(s) URL, got %q", raw)
		}
		host := u.Hostname()
		if host == "" {
			host = "127.0.0.1"
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		if _, convErr := strconv.Atoi(port); convErr != nil {
			return nil, fmt.Errorf("server.url %q: bad port %q", raw, port)
		}
		path := u.Path
		if path == "" {
			path = "/"
		}
		addr := net.JoinHostPort(host, port)

		// One port carries one protocol. Two URLs sharing an address must
		// agree on the scheme, or neither would work.
		if previous, ok := seen[addr]; ok && previous != u.Scheme {
			return nil, fmt.Errorf("server.urls: %s is configured for both %s and %s; "+
				"give the two schemes different ports", addr, previous, u.Scheme)
		}
		seen[addr] = u.Scheme

		for _, existing := range endpoints {
			if existing.Addr == addr && existing.Path == path {
				return nil, fmt.Errorf("server.urls: %q is listed twice", raw)
			}
		}
		endpoints = append(endpoints, Endpoint{URL: raw, Addr: addr, Path: path, TLS: u.Scheme == "https"})
	}
	return endpoints, nil
}

// Listeners groups the endpoints by listen address, since one address is one
// socket however many paths it serves.
func (c *Config) Listeners() ([]Listener, error) {
	endpoints, err := c.Endpoints()
	if err != nil {
		return nil, err
	}
	var listeners []Listener
	index := map[string]int{}
	for _, e := range endpoints {
		if at, ok := index[e.Addr]; ok {
			listeners[at].Paths = append(listeners[at].Paths, e.Path)
			listeners[at].URLs = append(listeners[at].URLs, e.URL)
			continue
		}
		index[e.Addr] = len(listeners)
		listeners = append(listeners, Listener{
			Addr:  e.Addr,
			Paths: []string{e.Path},
			URLs:  []string{e.URL},
			TLS:   e.TLS,
		})
	}
	return listeners, nil
}

// Listener is one socket and everything it serves.
type Listener struct {
	Addr  string
	Paths []string
	URLs  []string
	TLS   bool
}

// urlHost returns the hostname of a URL, or "" if it cannot be parsed.
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// DSN builds the PostgreSQL connection string, or "" when the database section
// does not carry credentials.
func (d DatabaseConfig) DSN() string {
	if !d.Enabled {
		return ""
	}
	if d.URL != "" {
		return d.URL
	}
	if d.Host == "" || d.Database == "" {
		return ""
	}
	u := url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(d.Host, strconv.Itoa(d.Port)),
		Path:   "/" + d.Database,
	}
	if d.User != "" {
		if d.Password != "" {
			u.User = url.UserPassword(d.User, d.Password)
		} else {
			u.User = url.User(d.User)
		}
	}
	q := url.Values{}
	q.Set("sslmode", d.SSLMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// Redacted returns the DSN with the password replaced, for logging.
func RedactDSN(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return dsn
	}
	if _, ok := u.User.Password(); ok {
		u.User = url.UserPassword(u.User.Username(), "****")
	}
	return u.String()
}

func validToolName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// ExampleConfig is written by --example-config. It is a working, fully
// populated configuration rather than a minimal one, so it doubles as the
// reference for every available setting.
const ExampleConfig = `{
  "server": {
    "name": "code-mcp",
    "version": "",
    "instructions": "Coding agent for this repository. Call system_info before writing your first shell command or path. Prefer the project commands over guessing build incantations, and ask git_diff for stat or name_only before a full patch.",
    "transport": "http",
    "url": "http://127.0.0.1:8765/mcp",
    "urls": [],
    "allowed_origins": ["http://127.0.0.1", "http://localhost"],
    "allowed_headers": [],
    "allow_private_network": true,
    "auth_token": "",
    "tls_cert_file": "",
    "tls_key_file": "",
    "tls_self_signed": false,
    "legacy_compatibility": true,
    "session_timeout_seconds": 7200
  },
  "tunnel": {
    "enabled": false,
    "server_url": "https://tunnel.example.com",
    "api_key": "",
    "api_key_env": "TUNNEL_API_KEY",
    "subdomain": "",
    "session_id": "",
    "session_file": ".codemcp-tunnel-session",
    "only": false,
    "client_info": ""
  },
  "workspace": {
    "root": "",
    "max_file_bytes": 1048576,
    "max_results": 200,
    "exclude": [".git", "node_modules", "vendor", "dist", "out", "target", ".venv"],
    "allow_write": true
  },
  "commands": [
    {
      "name": "build",
      "description": "Build every package in the module.",
      "command": "go build ./...",
      "timeout_seconds": 300
    },
    {
      "name": "test",
      "description": "Run the full test suite. Pass args like \"-run TestFoo ./pkg/...\".",
      "command": "go test ./...",
      "accepts_args": true,
      "timeout_seconds": 900
    },
    {
      "name": "lint",
      "description": "Vet and lint the code base.",
      "command": "go vet ./...",
      "read_only": true,
      "timeout_seconds": 300
    },
    {
      "name": "fmt",
      "description": "Format all Go sources in place.",
      "command": "gofmt -l -w ."
    }
  ],
  "git": {
    "enabled": true,
    "git_path": "git",
    "allow_commit": true,
    "allow_push": false,
    "allow_restore": false,
    "default_remote": "origin"
  },
  "github": {
    "enabled": true,
    "gh_path": "gh",
    "repo": "",
    "default_workflow": "release.yml",
    "default_ref": "main",
    "allow_dispatch": true
  },
  "database": {
    "enabled": false,
    "url": "",
    "host": "127.0.0.1",
    "port": 5432,
    "user": "postgres",
    "password": "",
    "database": "postgres",
    "sslmode": "prefer",
    "max_rows": 200,
    "statement_timeout_seconds": 30,
    "allow_write": false
  }
}
`
