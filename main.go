package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
)

// version is overridden at build time with -ldflags "-X main.version=...".
// Releases stamp 0.1.NNNN, the same string the release is tagged with; this
// default is what a plain "go build" produces.
var version = "0.1.0000-dev"

const appName = "codemcp"

const usage = `codemcp - a Model Context Protocol server that gives a coding agent this workspace

Run it from the directory you want to work in: that directory becomes the
workspace, and every file tool is confined to it. Point your MCP client at the
URL it prints on startup.

Usage:
  codemcp [flags]

It speaks MCP revision ` + ProtocolVersion + `, which is stateless: there is no
initialize handshake and no session id. Clients send each request to the single
POST endpoint with its protocol version, identity and capabilities in _meta, and
may call server/discover first to learn what this server supports.

Older clients are served too. A client that opens with an initialize handshake
gets the revision it negotiates - 2025-11-25, 2025-06-18 or 2025-03-26 - with
the session, GET stream and result shape those revisions expect. Pass --no-legacy
to serve only the current revision.

What it exposes:
  - The commands you define in config.json (build, test, lint, ...), each as a
    tool of the same name, so the model does not have to guess how this project
    is built.
  - File tools scoped to the workspace: read, write, edit, search, find.
  - Git tools: status, diff, log, branch, add, commit and (when enabled) push.
  - GitHub Actions tools driving the deployment workflow through the gh CLI:
    list and dispatch workflows, watch runs, read failed logs, list releases.
  - PostgreSQL tools, when config.json carries database credentials: query,
    list tables, describe a table and (when enabled) execute statements.
  - Prompts for the recurring workflows: verify_change, ship_change,
    diagnose_deploy, explore_workspace.

Flags:
  -c, --config <path>      configuration file to read
                           (default: config.json in the workspace; a missing
                           default file is not an error)
      --workspace <dir>    directory to serve as the workspace
                           (default: the current directory)
  -u, --url <url>          full URL clients connect to; its host and port are
                           what the server binds to, and its path is the MCP
                           endpoint (default: http://127.0.0.1:8765/mcp, or
                           server.url from the config). Give a comma-separated
                           list to serve several endpoints at once, typically
                           one http and one https on different ports
      --transport <name>   "http" for the Streamable HTTP transport, or "stdio"
                           to speak JSON-RPC on stdin/stdout for a client that
                           launches this server as a subprocess
      --token <token>      require this bearer token on every HTTP request
      --allow-origin <o>   comma-separated origins a browser may call this
                           server from, replacing server.allowed_origins.
                           "*" allows any origin. Preflight OPTIONS requests
                           are answered for every allowed origin; an origin
                           that is not allowed gets 403, which is what stops a
                           web page reaching your local server by DNS rebinding
      --allow-header <h>   comma-separated request headers a browser may send,
                           replacing server.allowed_headers. "*" allows any
                           header. The default echoes whatever the preflight
                           asks for, so naming headers here restricts them
      --tunnel <url>       expose this server through an https-tunnel server at
                           this URL, e.g. https://tunnel.example.com. The MCP
                           handler is served in process by the tunnel client,
                           so the server becomes reachable on a public HTTPS
                           URL from a client that cannot see this network. Set
                           a --token as well: the tunnel is public
      --tunnel-key <key>   API key for that tunnel server (default: the
                           TUNNEL_API_KEY environment variable)
      --tunnel-subdomain <label>
                           subdomain to ask for. It is granted when free and a
                           random one is issued otherwise, so read the URL the
                           server prints when the tunnel comes up
      --tunnel-session-file <path>
                           keep the session id in this file instead of in
                           config.json. Without it the id is written into the
                           tunnel section of the config; naming a file makes
                           that file the only store, and the config is left
                           alone
      --tunnel-only        serve the tunnel alone, binding no local port. By
                           default the same handler is served both ways
      --db-url <url>       PostgreSQL connection URL; enables the database
                           tools without putting credentials in the config
      --tls-cert <path>    TLS certificate to serve with, and
      --tls-key <path>     its private key. Serving over HTTPS is what makes
                           this server reachable from a page that is itself
                           served over HTTPS: a browser blocks an https page
                           from fetching an http URL as mixed content, before
                           the request is ever sent
      --tls-self-signed    generate a self-signed certificate on startup
                           instead. Browsers do not trust one, so visit the URL
                           once and accept the warning, or use a locally
                           trusted certificate from a tool such as mkcert
      --no-legacy          serve only protocol version ` + ProtocolVersion + `,
                           refusing the older initialize-based revisions. By
                           default those are served too, so clients that have
                           not caught up still work
      --example-config     write a complete example config.json to stdout and
                           exit
      --check              load the config, report what would be served, and
                           exit without listening
  -v, --version            print the version and exit
  -h, --help               show this help and exit

Examples:
  codemcp --example-config > config.json
  codemcp
  codemcp --url http://127.0.0.1:9000/mcp --token "$MCP_TOKEN"
  codemcp --allow-origin "*" --allow-header "*"
  codemcp --tls-self-signed --allow-origin https://app.example
  codemcp --url http://127.0.0.1:8765/mcp,https://127.0.0.1:8766/mcp \
          --tls-self-signed
  codemcp --allow-origin https://inspector.example,http://localhost:5173
  codemcp --transport stdio
  codemcp --tunnel https://tunnel.example.com --tunnel-subdomain my-mcp           --token "$MCP_TOKEN"
  codemcp --tunnel-only --tunnel https://tunnel.example.com --token "$MCP_TOKEN"
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", appName, err)
		os.Exit(1)
	}
}

type options struct {
	configPath    string
	workspace     string
	url           string
	transport     string
	token         string
	allowOrigin   string
	allowHeader   string
	tunnel        string
	tunnelKey     string
	tunnelSub     string
	tunnelSession string
	tunnelOnly    bool
	tlsCert       string
	tlsKey        string
	tlsSelfSigned bool
	noLegacy      bool
	dbURL         string
	exampleConfig bool
	check         bool
	showVersion   bool
	showHelp      bool
}

func parseFlags(args []string) (*options, error) {
	var opts options
	fs := flag.NewFlagSet(appName, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	str := func(target *string, def string, names ...string) {
		for _, name := range names {
			fs.StringVar(target, name, def, "")
		}
	}
	boolean := func(target *bool, names ...string) {
		for _, name := range names {
			fs.BoolVar(target, name, false, "")
		}
	}
	str(&opts.configPath, "", "config", "c")
	str(&opts.workspace, "", "workspace")
	str(&opts.url, "", "url", "u")
	str(&opts.transport, "", "transport")
	str(&opts.token, "", "token")
	str(&opts.allowOrigin, "", "allow-origin")
	str(&opts.allowHeader, "", "allow-header")
	str(&opts.tunnel, "", "tunnel")
	str(&opts.tunnelKey, "", "tunnel-key")
	str(&opts.tunnelSub, "", "tunnel-subdomain")
	str(&opts.tunnelSession, "", "tunnel-session-file")
	boolean(&opts.tunnelOnly, "tunnel-only")
	str(&opts.tlsCert, "", "tls-cert")
	str(&opts.tlsKey, "", "tls-key")
	boolean(&opts.tlsSelfSigned, "tls-self-signed")
	str(&opts.dbURL, "", "db-url")
	boolean(&opts.noLegacy, "no-legacy")
	boolean(&opts.exampleConfig, "example-config")
	boolean(&opts.check, "check")
	boolean(&opts.showVersion, "version", "v")
	boolean(&opts.showHelp, "help", "h")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			opts.showHelp = true
			return &opts, nil
		}
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q; codemcp takes flags only", fs.Arg(0))
	}
	return &opts, nil
}

func run(args []string) error {
	opts, err := parseFlags(args)
	if err != nil {
		return err
	}
	switch {
	case opts.showHelp:
		fmt.Print(usage)
		return nil
	case opts.showVersion:
		fmt.Printf("%s %s (MCP %s)\n", appName, version, ProtocolVersion)
		return nil
	case opts.exampleConfig:
		fmt.Print(ExampleConfig)
		return nil
	}

	workspace := opts.workspace
	if workspace == "" {
		workspace, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("could not determine the working directory: %w", err)
		}
	}

	configPath := opts.configPath
	explicit := configPath != ""
	if !explicit {
		configPath = joinPath(workspace, "config.json")
	}
	cfg, err := LoadConfig(configPath, explicit)
	if err != nil {
		return err
	}
	applyFlags(&cfg, opts)
	if err := cfg.Normalize(workspace); err != nil {
		return err
	}

	// On stdio, stdout carries the protocol: everything human-readable has to
	// go to stderr or it corrupts the stream.
	banner := os.Stdout
	if cfg.Server.Transport == "stdio" {
		banner = os.Stderr
	}
	logger := log.New(os.Stderr, appName+": ", log.LstdFlags)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := NewServer(cfg.Server.Name, cfg.Server.Version, cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	srv.logger = logger

	db, err := OpenDB(ctx, cfg.Database)
	if err != nil {
		return err
	}
	defer db.Close()
	srv.db = db

	// Built before the tools are registered, since whether get_download_link
	// exists depends on it. On http it serves from the endpoint's own
	// listener; on stdio it opens one of its own with the first link.
	if cfg.Downloads.Enabled {
		srv.dl = newDownloadServer(cfg.Downloads, logger)
		if cfg.Server.Transport == "http" {
			base := cfg.Downloads.BaseURL
			if base != "" {
				srv.dl.Attach(strings.TrimSuffix(base, "/"+downloadSegment))
			} else {
				srv.dl.Attach(cfg.Server.URL)
			}
		} else if cfg.Downloads.BaseURL != "" {
			srv.dl.Attach(strings.TrimSuffix(cfg.Downloads.BaseURL, "/"+downloadSegment))
		}
		defer srv.dl.Close()
	}

	// The workspace, the sudo agent, the instructions and the tool set all come
	// from the configuration, and all of them are rebuilt the same way when it
	// is reloaded.
	if err := srv.applyConfig(cfg); err != nil {
		return err
	}
	defer func() { srv.sudo.Close() }()
	if cfg.Sudo.Password != "" {
		// An inline password sits in config.json, which the file tools can
		// read: the whole point of the feature is that the model never sees it.
		logger.Printf("warning: sudo.password is set inline in the config file, " +
			"which the file tools can read; prefer sudo.password_env or sudo.password_file")
	}

	// Each request re-reads config.json, so an edited command, workspace rule
	// or sudo password takes effect on the next call. A file that cannot be
	// read keeps the values already in effect.
	srv.reload = newConfigReloader(cfg.Path(), explicit, workspace, cfg, logger,
		func(next *Config) { applyFlags(next, opts) }, srv.applyConfig)

	// Built before the banner so that --check reports a bad certificate rather
	// than a server that only fails once someone tries to connect.
	var tlsConfig *tls.Config
	if cfg.Server.Transport == "http" {
		tlsConfig, err = cfg.TLSConfig()
		if err != nil {
			return err
		}
	}

	printBanner(banner, &cfg, srv, db)
	if opts.check {
		fmt.Fprintln(banner, "\nConfiguration is valid. Exiting because --check was given.")
		return nil
	}

	if cfg.Server.Transport == "stdio" {
		return NewStdioTransport(srv, os.Stdin, os.Stdout, cfg.Server.LegacyCompatibility, logger).Serve(ctx)
	}
	listeners, err := cfg.Listeners()
	if err != nil {
		return err
	}
	// One transport across every listener, so a legacy session established on
	// one endpoint is still valid on another.
	transport := NewHTTPTransport(srv, cfg.Server, listeners[0].Paths[0], logger)

	if !cfg.Tunnel.Enabled {
		return transport.ServeAll(ctx, listeners, tlsConfig)
	}
	// The tunnel serves the same handler in process, on every path the local
	// listeners answer on, so a client reaches the same endpoint either way.
	tunnel, err := NewTunnel(&cfg, transport.HandlerFor(endpointPaths(listeners)...), logger)
	if err != nil {
		return err
	}
	if cfg.Tunnel.Only {
		return tunnel.Run(ctx)
	}

	// Either half failing takes the other down: a server that is half up is
	// worse than one that refuses to start, because a client connected to the
	// surviving half has no way to know.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	go func() { errs <- transport.ServeAll(ctx, listeners, tlsConfig) }()
	go func() { errs <- tunnel.Run(ctx) }()
	var first error
	for range 2 {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// endpointPaths is every distinct path the configured listeners answer on.
func endpointPaths(listeners []Listener) []string {
	var paths []string
	seen := map[string]bool{}
	for _, l := range listeners {
		for _, path := range l.Paths {
			if !seen[path] {
				seen[path] = true
				paths = append(paths, path)
			}
		}
	}
	return paths
}

// printBanner is the startup summary: what is being served, from where, and -
// the thing an operator actually needs - the URL to connect to.
func printBanner(w *os.File, cfg *Config, srv *Server, db *DB) {
	fmt.Fprintf(w, "%s %s  (MCP protocol %s)\n", appName, version, ProtocolVersion)
	if cfg.Server.LegacyCompatibility {
		fmt.Fprintf(w, "  versions   %s\n", strings.Join(SupportedVersions, ", "))
	} else {
		fmt.Fprintf(w, "  versions   %s only (--no-legacy)\n", ProtocolVersion)
	}
	fmt.Fprintf(w, "  workspace  %s\n", cfg.Workspace.Root)
	fmt.Fprintf(w, "  transport  %s\n", cfg.Server.Transport)
	if cfg.Server.Transport == "stdio" {
		fmt.Fprintf(w, "  endpoint   stdin/stdout (JSON-RPC, newline delimited)\n")
	} else {
		endpoints, err := cfg.Endpoints()
		// With tunnel.only there is no socket, so naming a local address here
		// would send an operator at a URL nothing answers on.
		if err == nil && !cfg.Tunnel.Only {
			var addrs []string
			seen := map[string]bool{}
			for _, e := range endpoints {
				if !seen[e.Addr] {
					seen[e.Addr] = true
					addrs = append(addrs, e.Addr)
				}
			}
			fmt.Fprintf(w, "  listening  %s\n", strings.Join(addrs, ", "))

			if len(endpoints) == 1 {
				fmt.Fprintf(w, "\n  Connect your MCP client to:  %s\n\n", endpoints[0].URL)
			} else {
				fmt.Fprintf(w, "\n  Connect your MCP client to any of:\n")
				for _, e := range endpoints {
					scheme := "http "
					if e.TLS {
						scheme = "https"
					}
					fmt.Fprintf(w, "      %s  %s\n", scheme, e.URL)
				}
				fmt.Fprintln(w)
			}
		}
		if cfg.Server.AuthToken != "" {
			fmt.Fprintf(w, "  Authentication: send Authorization: Bearer <token>\n")
		}
		origins := strings.Join(cfg.Server.AllowedOrigins, ", ")
		if origins == "" {
			origins = "none (browser clients will be refused)"
		}
		if cfg.Server.IsTLS() {
			if cfg.Server.TLSSelfSigned && cfg.Server.TLSCertFile == "" {
				fmt.Fprintf(w, "  tls        self-signed, generated at startup for %s\n",
					strings.Join(cfg.Server.TLSHosts(), ", "))
				fmt.Fprintf(w, "             browsers will not trust it; open the URL once and accept the warning\n")
			} else {
				fmt.Fprintf(w, "  tls        %s\n", cfg.Server.TLSCertFile)
			}
		}
		fmt.Fprintf(w, "  origins    %s\n", origins)
		for _, origin := range cfg.Server.AllowedOrigins {
			if origin == "*" {
				fmt.Fprintf(w, "             any web page may call this server; set --token if it is not bound to localhost\n")
				break
			}
		}
		if cfg.Tunnel.Enabled {
			fmt.Fprintf(w, "  tunnel     %s", cfg.Tunnel.ServerURL)
			if cfg.Tunnel.Subdomain != "" {
				fmt.Fprintf(w, " (requesting subdomain %q)", cfg.Tunnel.Subdomain)
			}
			fmt.Fprintln(w)
			if cfg.Tunnel.Only {
				fmt.Fprintf(w, "             tunnel only; no local port is bound\n")
			}
			path := "/"
			if endpoints, err := cfg.Endpoints(); err == nil && len(endpoints) > 0 {
				path = endpoints[0].Path
			}
			fmt.Fprintf(w, "             the public URL is printed once the tunnel is up; the MCP endpoint is %s on it\n", path)
			if cfg.Tunnel.SessionFile != "" {
				fmt.Fprintf(w, "             session    %s\n", cfg.Tunnel.SessionFile)
			}
			if cfg.Server.AuthToken == "" {
				fmt.Fprintf(w, "             WARNING: the tunnel is public and no auth token is set; anyone with the URL\n")
				fmt.Fprintf(w, "                      can run every tool in this workspace. Set --token\n")
			}
		}
	}

	names := srv.ToolNames()
	sort.Strings(names)
	fmt.Fprintf(w, "  tools (%d)  %s\n", len(names), wrapList(names, 60, "              "))
	if len(cfg.Commands) > 0 {
		var commands []string
		for _, c := range cfg.Commands {
			commands = append(commands, c.Name)
		}
		fmt.Fprintf(w, "  commands   %s\n", strings.Join(commands, ", "))
	} else {
		fmt.Fprintf(w, "  commands   none defined; run %s --example-config for a starting point\n", appName)
	}
	if db != nil {
		fmt.Fprintf(w, "  database   %s\n", db.Describe())
	} else {
		fmt.Fprintf(w, "  database   not configured\n")
	}
}

// wrapList renders a long list of names over several indented lines.
func wrapList(items []string, width int, indent string) string {
	var lines []string
	var current string
	for _, item := range items {
		switch {
		case current == "":
			current = item
		case len(current)+len(item)+2 <= width:
			current += ", " + item
		default:
			lines = append(lines, current)
			current = item
		}
	}
	if current != "" {
		lines = append(lines, current)
	}
	return strings.Join(lines, "\n"+indent)
}

// splitList parses a comma-separated flag value, dropping empty entries.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func joinPath(dir, name string) string {
	if strings.HasSuffix(dir, string(os.PathSeparator)) {
		return dir + name
	}
	return dir + string(os.PathSeparator) + name
}

// applyFlags overlays the command-line options onto a configuration. It is a
// function rather than a straight line of startup because a config reload has
// to re-apply them: a flag the operator passed must keep winning over the file,
// however many times the file is re-read.
func applyFlags(cfg *Config, opts *options) {
	if opts.workspace != "" {
		cfg.Workspace.Root = opts.workspace
	}
	if opts.url != "" {
		// A comma-separated value asks for several endpoints at once, which is
		// how one http and one https endpoint are requested from the command
		// line.
		if list := splitList(opts.url); len(list) > 1 {
			cfg.Server.URL = list[0]
			cfg.Server.AdditionalURLs = list
		} else {
			cfg.Server.URL = opts.url
			cfg.Server.AdditionalURLs = nil
		}
	}
	if opts.transport != "" {
		cfg.Server.Transport = opts.transport
	}
	if opts.token != "" {
		cfg.Server.AuthToken = opts.token
	}
	if opts.allowOrigin != "" {
		cfg.Server.AllowedOrigins = splitList(opts.allowOrigin)
	}
	if opts.allowHeader != "" {
		cfg.Server.AllowedHeaders = splitList(opts.allowHeader)
	}
	if opts.tunnel != "" {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.ServerURL = opts.tunnel
	}
	if opts.tunnelKey != "" {
		cfg.Tunnel.APIKey = opts.tunnelKey
	}
	if opts.tunnelSub != "" {
		cfg.Tunnel.Subdomain = opts.tunnelSub
	}
	if opts.tunnelSession != "" {
		cfg.Tunnel.SessionFile = opts.tunnelSession
	}
	if opts.tunnelOnly {
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.Only = true
	}
	if opts.tlsCert != "" {
		cfg.Server.TLSCertFile = opts.tlsCert
	}
	if opts.tlsKey != "" {
		cfg.Server.TLSKeyFile = opts.tlsKey
	}
	if opts.tlsSelfSigned {
		cfg.Server.TLSSelfSigned = true
	}
	// Asking for TLS without saying so in the URL is a contradiction the
	// operator almost certainly did not mean; upgrade the scheme instead of
	// silently serving plaintext.
	if (cfg.Server.TLSSelfSigned || cfg.Server.TLSCertFile != "") && !cfg.Server.IsTLS() {
		cfg.Server.URL = strings.Replace(cfg.Server.URL, "http://", "https://", 1)
		for i, raw := range cfg.Server.AdditionalURLs {
			cfg.Server.AdditionalURLs[i] = strings.Replace(raw, "http://", "https://", 1)
		}
	}
	if opts.noLegacy {
		cfg.Server.LegacyCompatibility = false
	}
	if opts.dbURL != "" {
		cfg.Database.Enabled = true
		cfg.Database.URL = opts.dbURL
	}
}
