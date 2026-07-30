package main

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const agentUsage = `codemcp agent - an interactive coding agent in the console

It runs a model against the same tools the server exposes (files, edits, search,
git, project commands, ...) in this workspace, plus the tools of any external MCP
servers and OpenAPI services named in the "agent" section of config.json.

Usage:
  codemcp agent [flags]

Flags:
  -c, --config <path>        configuration file (default: config.json in the workspace)
      --workspace <dir>      workspace directory (default: the current directory)
  -m, --model <name|id>      configured model to start with, or a model id to use
                             with --endpoint
      --endpoint <url>       use this LLM endpoint instead of the configured ones,
                             e.g. 10.0.0.200:11434/v1 or https://llmapi.example.com/v1
      --api-key <key>        credential for --endpoint (default: $CODEMCP_AGENT_API_KEY)
      --style <style>        API style for --endpoint: auto, openai, responses,
                             anthropic, completions or ollama (default: auto)
  -p, --prompt <text>        run this one prompt non-interactively and exit
      --max-turns <n>        model/tool round trips per prompt (default: 50)
      --no-local-tools       expose only the external connections' tools
      --show-thinking        show the model's reasoning when the endpoint sends
                             it, instead of collapsing it to one line
      --dangerously-skip-permissions
                             run every tool without asking, including file
                             changes and shell commands. Only use this in a
                             workspace you can afford to lose
  -h, --help                 show this help

In the console:
  /help                 list the commands
  /reload               re-read the configuration file: models, connections
                        and local tools are all rebuilt from it
  /connect [name]       (re)connect and test every external connection and model,
                        or only the one named
  /tools [filter]       list the tools the model is given
  /models               list the configured model endpoints
  /model <name|id>      switch endpoint, or set the model id on the current one
  /thinking [on|off]    show or collapse the model's reasoning; no argument toggles
  /clear                start a new conversation
  /save [path]          save the conversation to a JSONL file (default: a
                        timestamped file in the workspace root)
  /summary              ask the model to summarize the conversation so far
  /savesummary [path]   summarize the conversation and save it to a text file
  /loadsummary [path]   load a saved summary into the conversation (default:
                        the most recently saved one in the workspace root)
  /compress             summarize the conversation and replace the history
                        with it, freeing up context to keep going
  /stats                CPU, GPU and disk usage for this machine
  /permissions          show the permission mode and the tools allowed this session
  /exit                 quit (Ctrl-D works too)
  End a line with \ to continue on the next line. Ctrl-C cancels a running turn
                        and returns to this prompt; press it again there to quit.
`

type agentOptions struct {
	configPath   string
	workspace    string
	model        string
	endpoint     string
	apiKey       string
	style        string
	prompt       string
	maxTurns     int
	noLocal      bool
	showThinking bool
	skipPerms    bool
	showHelp     bool
}

// agentBoolFlags names the flags that stand alone. Every other flag consumes
// the token after it, unless it was written as --flag=value.
var agentBoolFlags = map[string]bool{
	"no-local-tools":               true,
	"show-thinking":                true,
	"dangerously-skip-permissions": true,
	"help":                         true,
	"h":                            true,
}

// splitAgentArgs separates the flags from the bare prompt words. Go's flag
// package stops at the first operand, so without this `codemcp agent "fix the
// build" --config other.json` would silently fold the flag into the prompt and
// load config.json anyway. Flags count wherever they are written; everything
// after a lone `--` is prompt.
func splitAgentArgs(args []string) (flags, operands []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return flags, append(operands, args[i+1:]...)
		}
		if len(arg) < 2 || arg[0] != '-' {
			operands = append(operands, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if hasValue || agentBoolFlags[name] {
			continue
		}
		if i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return flags, operands
}

func parseAgentFlags(args []string) (*agentOptions, error) {
	var o agentOptions
	fs := flag.NewFlagSet(appName+" agent", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, n := range []string{"config", "c"} {
		fs.StringVar(&o.configPath, n, "", "")
	}
	fs.StringVar(&o.workspace, "workspace", "", "")
	for _, n := range []string{"model", "m"} {
		fs.StringVar(&o.model, n, "", "")
	}
	fs.StringVar(&o.endpoint, "endpoint", "", "")
	fs.StringVar(&o.apiKey, "api-key", "", "")
	fs.StringVar(&o.style, "style", "", "")
	for _, n := range []string{"prompt", "p"} {
		fs.StringVar(&o.prompt, n, "", "")
	}
	fs.IntVar(&o.maxTurns, "max-turns", 0, "")
	fs.BoolVar(&o.noLocal, "no-local-tools", false, "")
	fs.BoolVar(&o.showThinking, "show-thinking", false, "")
	fs.BoolVar(&o.skipPerms, "dangerously-skip-permissions", false, "")
	for _, n := range []string{"help", "h"} {
		fs.BoolVar(&o.showHelp, n, false, "")
	}
	flagArgs, operands := splitAgentArgs(args)
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			o.showHelp = true
			return &o, nil
		}
		return nil, fmt.Errorf("%v (see codemcp agent --help)", err)
	}
	if len(operands) > 0 {
		// A bare argument is taken as the prompt, which is what people type.
		if o.prompt == "" {
			o.prompt = strings.Join(operands, " ")
		} else {
			return nil, fmt.Errorf("unexpected argument %q", operands[0])
		}
	}
	return &o, nil
}

// agentTool is one tool the model is offered, wherever it comes from.
type agentTool struct {
	spec     toolSpec
	source   string // "local", or the connection name
	readOnly bool
	run      func(ctx context.Context, args json.RawMessage) (*CallToolResult, error)
}

// agentConnection is an external MCP server or OpenAPI service.
type agentConnection struct {
	name     string
	kind     string // "mcp" or "openapi"
	disabled bool
	mcp      *mcpClient
	api      *openAPIClient
	auth     AgentAuth

	connected bool
	lastErr   error
	detail    string
}

type agent struct {
	cfg          Config
	opts         *agentOptions
	srv          *Server
	out          io.Writer
	color        bool
	skipPerms    bool
	maxTurns     int
	showThinking bool

	// configPath, explicit and workspace are what /reload re-reads the
	// configuration with; they mirror the arguments runAgent resolved at
	// startup.
	configPath string
	explicit   bool
	workspace  string

	models []*llmClient
	model  *llmClient

	conns []*agentConnection

	tools     []*agentTool
	toolIndex map[string]*agentTool
	allowed   map[string]bool

	history []chatMessage
	lines   chan string
	signals chan os.Signal

	// interruptMu guards lastInterrupt, which a running turn's interruptible
	// goroutine and repl's own idle-prompt handling both write and read - a
	// Ctrl-C that stops a turn has to count toward the "press it again to
	// quit" that follows at the prompt, not just a Ctrl-C caught while
	// already idle there.
	interruptMu   sync.Mutex
	lastInterrupt time.Time
}

// quitWindow is how soon a second Ctrl-C has to follow the first - whether
// that first one stopped a running turn or was caught sitting idle at the
// prompt - for it to exit the application instead of just being noted.
const quitWindow = 2 * time.Second

// noteInterrupt records a Ctrl-C and reports whether another one landed
// within quitWindow of it, regardless of what each one interrupted.
func (a *agent) noteInterrupt() bool {
	a.interruptMu.Lock()
	defer a.interruptMu.Unlock()
	now := time.Now()
	again := !a.lastInterrupt.IsZero() && now.Sub(a.lastInterrupt) < quitWindow
	a.lastInterrupt = now
	return again
}

// prepareAgentConfig adjusts a loaded config for `codemcp agent` use and then
// normalizes it. The agent reaches its tools in process, so nothing is
// listened on: anything that serves the HTTP handler is forced off, even if
// the config file - commonly shared with the server - turns it on.
func prepareAgentConfig(cfg *Config, workspace string) error {
	cfg.Server.Transport = "stdio"
	cfg.Downloads.Enabled = false
	cfg.Tunnel.Enabled = false
	return cfg.Normalize(workspace)
}

func runAgent(args []string) error {
	opts, err := parseAgentFlags(args)
	if err != nil {
		return err
	}
	if opts.showHelp {
		fmt.Print(agentUsage)
		return nil
	}
	workspace := opts.workspace
	if workspace == "" {
		if workspace, err = os.Getwd(); err != nil {
			return err
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
	if opts.workspace != "" {
		cfg.Workspace.Root = opts.workspace
	}
	if err := prepareAgentConfig(&cfg, workspace); err != nil {
		return err
	}

	a := &agent{
		cfg:          cfg,
		opts:         opts,
		out:          os.Stdout,
		color:        isTerminal(os.Stdout) && os.Getenv("NO_COLOR") == "",
		skipPerms:    opts.skipPerms,
		maxTurns:     firstPositive(opts.maxTurns, cfg.Agent.MaxTurns, 50),
		showThinking: opts.showThinking,
		allowed:      map[string]bool{},
		lines:        make(chan string),
		signals:      make(chan os.Signal, 1),
		configPath:   configPath,
		explicit:     explicit,
		workspace:    workspace,
	}
	for _, name := range cfg.Agent.AlwaysAllow {
		a.allowed[name] = true
	}
	signal.Notify(a.signals, os.Interrupt)
	defer signal.Stop(a.signals)

	ctx := context.Background()
	if !opts.noLocal && !cfg.Agent.DisableLocalTools {
		srv := NewServer(cfg.Server.Name, cfg.Server.Version, cfg.Server.Instructions, false)
		srv.logger = log.New(io.Discard, "", 0)
		db, err := OpenDB(ctx, cfg.Database)
		if err != nil {
			return err
		}
		defer db.Close()
		srv.db = db
		if err := srv.applyConfig(cfg); err != nil {
			return err
		}
		defer func() { srv.sudo.Close() }()
		defer srv.processes.stopAll()
		a.srv = srv
	}

	if err := a.setupModels(); err != nil {
		return err
	}
	a.setupConnections()
	defer a.closeConnections()

	go a.readInput(os.Stdin)

	a.printBanner()
	a.connectAll(ctx, "", false)
	a.rebuildTools()

	if opts.prompt != "" {
		return a.runTurn(opts.prompt)
	}
	return a.repl()
}

func firstPositive(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (a *agent) paint(code, text string) string {
	if !a.color {
		return text
	}
	return "\x1b[" + code + "m" + text + "\x1b[0m"
}

// printReasoning shows the model's reasoning when a.showThinking is set, or
// collapses it to a one-line indicator otherwise. Not every endpoint sends
// any, in which case reasoning is empty and nothing is printed.
func (a *agent) printReasoning(reasoning string) {
	reasoning = strings.TrimSpace(reasoning)
	if reasoning == "" {
		return
	}
	if a.showThinking {
		fmt.Fprintln(a.out, a.paint("90", "thinking:"))
		fmt.Fprintln(a.out, a.paint("90", reasoning))
		return
	}
	fmt.Fprintln(a.out, a.paint("90", fmt.Sprintf("[thinking, %d chars — /thinking to show]", len(reasoning))))
}

// printUsage reports token counts and generation speed for one model call.
// tok/s is output tokens over wall time for the call, the usual generation-
// speed reading (prompt processing time is included, same as any other tool
// reporting it, since the two aren't broken out separately here).
func (a *agent) printUsage(reply *llmReply, elapsed time.Duration) {
	if reply.InTokens+reply.OutTokens == 0 {
		return
	}
	rate := ""
	if reply.OutTokens > 0 && elapsed > 0 {
		rate = fmt.Sprintf(" · %.1f tok/s", float64(reply.OutTokens)/elapsed.Seconds())
	}
	fmt.Fprintln(a.out, a.paint("90", fmt.Sprintf("[%s · %d in / %d out tokens%s]", a.model.model, reply.InTokens, reply.OutTokens, rate)))
}

// modelConfigs lists the endpoints a config should offer: --endpoint first,
// when given, followed by agent.models from the file.
func (a *agent) modelConfigs(cfg Config) []AgentModelConfig {
	var configs []AgentModelConfig
	if a.opts.endpoint != "" {
		key := firstNonEmpty(a.opts.apiKey, os.Getenv("CODEMCP_AGENT_API_KEY"))
		configs = append(configs, AgentModelConfig{
			Name: "cli", BaseURL: a.opts.endpoint, Model: a.opts.model, Style: a.opts.style,
			Auth: AgentAuth{Token: key},
		})
	}
	return append(configs, cfg.Agent.Models...)
}

// buildLLMClients constructs a client per configured endpoint. It touches no
// agent state, so a caller - startup or /reload - can use it to validate a
// config before committing to it.
func (a *agent) buildLLMClients(cfg Config) ([]*llmClient, error) {
	var models []*llmClient
	for _, mc := range a.modelConfigs(cfg) {
		client, err := newLLMClient(mc)
		if err != nil {
			return nil, err
		}
		models = append(models, client)
	}
	return models, nil
}

func (a *agent) setupModels() error {
	models, err := a.buildLLMClients(a.cfg)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no model endpoint: add one to agent.models in config.json, or pass --endpoint " +
			"(e.g. --endpoint 10.0.0.200:11434/v1 --model qwen3-coder)")
	}
	a.models = models
	a.model = a.initialModel()
	return nil
}

// initialModel is the model startup picks, and what /reload falls back to
// when the model that was in use is no longer configured: agent.default_model
// (or --model, which overrides it), taken as a configured name first and
// otherwise as a raw model id on the first endpoint; failing both, the first
// endpoint as it stands. a.models must already be set.
func (a *agent) initialModel() *llmClient {
	model := a.models[0]
	want := a.cfg.Agent.DefaultModel
	if a.opts.endpoint == "" && a.opts.model != "" {
		want = a.opts.model
	}
	if want != "" && a.opts.endpoint == "" {
		if m := a.findModel(want); m != nil {
			return m
		}
		if a.opts.model != "" {
			// Not a configured name: take it as a model id on the default endpoint.
			model.model = a.opts.model
		}
	}
	return model
}

func (a *agent) findModel(name string) *llmClient {
	for _, m := range a.models {
		if strings.EqualFold(m.cfg.Name, name) {
			return m
		}
	}
	return nil
}

func (a *agent) setupConnections() {
	for _, s := range a.cfg.Agent.MCPServers {
		conn := &agentConnection{name: s.Name, kind: "mcp", disabled: s.Disabled, auth: s.Auth}
		client, err := newMCPClient(s)
		if err != nil {
			conn.lastErr = err
		}
		conn.mcp = client
		a.conns = append(a.conns, conn)
	}
	for _, s := range a.cfg.Agent.OpenAPIServers {
		a.conns = append(a.conns, &agentConnection{
			name: s.Name, kind: "openapi", disabled: s.Disabled, auth: s.Auth, api: newOpenAPIClient(s),
		})
	}
}

func (a *agent) closeConnections() {
	for _, c := range a.conns {
		if c.mcp != nil {
			c.mcp.close()
		}
	}
}

// reloadConfig re-reads the configuration file and applies it to the running
// agent: local tools, models and external connections are all rebuilt from
// it, the same way editing config.json does for the server (see
// Server.applyConfig). Everything is loaded and validated before anything is
// touched, so a bad file - unreadable, invalid, or one that now names no
// model at all - changes nothing; the agent keeps running on what it had.
//
// Two things do not move even though the file can name them: whether local
// tools run at all (--no-local-tools / agent.disable_local_tools) and the
// database connection, both fixed for the process the same as on the server.
func (a *agent) reloadConfig() error {
	cfg, err := LoadConfig(a.configPath, a.explicit)
	if err != nil {
		return err
	}
	if a.opts.workspace != "" {
		cfg.Workspace.Root = a.opts.workspace
	}
	if err := prepareAgentConfig(&cfg, a.workspace); err != nil {
		return err
	}
	models, err := a.buildLLMClients(cfg)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no model endpoint: add one to agent.models in config.json, or pass --endpoint " +
			"(e.g. --endpoint 10.0.0.200:11434/v1 --model qwen3-coder)")
	}
	if a.srv != nil {
		if err := a.srv.applyConfig(cfg); err != nil {
			return err
		}
	}

	// Everything above can fail without having changed anything; everything
	// below commits to the new configuration.
	current := ""
	if a.model != nil {
		current = a.model.cfg.Name
	}
	a.closeConnections()

	a.cfg = cfg
	a.maxTurns = firstPositive(a.opts.maxTurns, cfg.Agent.MaxTurns, 50)
	// always_allow only grows on reload: a tool the file stopped naming, or
	// one a person typed "a" for this session, stays allowed rather than
	// surprising them with a prompt mid-conversation.
	for _, name := range cfg.Agent.AlwaysAllow {
		a.allowed[name] = true
	}
	a.models = models
	if m := a.findModel(current); m != nil {
		a.model = m
	} else {
		a.model = a.initialModel()
	}

	a.conns = nil
	a.setupConnections()

	ctx, cancel := a.interruptible()
	a.connectAll(ctx, "", true)
	cancel()
	a.rebuildTools()
	return nil
}

func (c *agentConnection) target() string {
	if c.mcp != nil {
		return c.mcp.target()
	}
	if c.api != nil {
		return c.api.target()
	}
	return ""
}

// connectAll connects (or reconnects) the external connections concurrently.
// With verbose, every connection and model gets a line of test results, which
// is what /connect prints; at startup only the failures are worth a line.
func (a *agent) connectAll(parent context.Context, only string, verbose bool) {
	var wg sync.WaitGroup
	matched := false
	for _, conn := range a.conns {
		if only != "" && !strings.EqualFold(conn.name, only) {
			continue
		}
		matched = true
		if conn.disabled && only == "" {
			if verbose {
				fmt.Fprintf(a.out, "  %s %-16s %-8s %s  (disabled)\n", a.paint("90", "-"), conn.name, conn.kind, conn.target())
			}
			continue
		}
		wg.Add(1)
		go func(conn *agentConnection) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(parent, 45*time.Second)
			defer cancel()
			start := time.Now()
			var err error
			switch {
			case conn.mcp == nil && conn.api == nil:
				err = conn.lastErr
			case conn.mcp != nil:
				if err = conn.mcp.connect(ctx); err == nil {
					conn.detail = fmt.Sprintf("%d tools, server %s, protocol %s", len(conn.mcp.tools),
						firstNonEmpty(conn.mcp.serverName, "?"), firstNonEmpty(conn.mcp.protocol, "?"))
				}
			default:
				if err = conn.api.connect(ctx); err == nil {
					conn.detail = fmt.Sprintf("%d operations, %s, base %s", len(conn.api.ops),
						firstNonEmpty(conn.api.title, "untitled"), conn.api.baseURL)
				}
			}
			conn.connected, conn.lastErr = err == nil, err
			conn.detail = strings.TrimSpace(conn.detail) + fmt.Sprintf(" (%dms, auth %s)", time.Since(start).Milliseconds(), conn.auth.describe())
		}(conn)
	}
	testModels := verbose && (only == "" || a.findModel(only) != nil)
	results := make([]string, len(a.models))
	if testModels {
		for i, m := range a.models {
			if only != "" && !strings.EqualFold(m.cfg.Name, only) {
				continue
			}
			matched = true
			wg.Add(1)
			go func(i int, m *llmClient) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(parent, 90*time.Second)
				defer cancel()
				start := time.Now()
				ids, listErr := m.listModels(ctx)
				err := m.detect(ctx)
				ms := time.Since(start).Milliseconds()
				if err != nil {
					results[i] = fmt.Sprintf("  %s %-16s %-8s %s\n      %v", a.paint("31", "✗"), m.cfg.Name, "model", m.base, err)
					return
				}
				listed := "model list unavailable"
				if listErr == nil {
					listed = fmt.Sprintf("%d models listed", len(ids))
				}
				results[i] = fmt.Sprintf("  %s %-16s %-8s %s\n      style %s at %s, model %s, %s (%dms, auth %s)",
					a.paint("32", "✓"), m.cfg.Name, "model", m.base, m.style, m.endpoint, m.model, listed, ms, m.cfg.Auth.describe())
			}(i, m)
		}
	}
	wg.Wait()
	if only != "" && !matched {
		fmt.Fprintf(a.out, "no connection or model named %q; /connect with no name tests them all\n", only)
		return
	}
	if verbose && len(a.conns) == 0 && only == "" {
		fmt.Fprintln(a.out, "  no external connections are configured (agent.mcp_servers, agent.openapi_servers)")
	}
	for _, conn := range a.conns {
		if only != "" && !strings.EqualFold(conn.name, only) || conn.disabled && only == "" {
			continue
		}
		switch {
		case conn.connected && verbose:
			fmt.Fprintf(a.out, "  %s %-16s %-8s %s\n      %s\n", a.paint("32", "✓"), conn.name, conn.kind, conn.target(), conn.detail)
		case !conn.connected:
			fmt.Fprintf(a.out, "  %s %-16s %-8s %s\n      %v\n", a.paint("31", "✗"), conn.name, conn.kind, conn.target(), conn.lastErr)
		}
	}
	for _, line := range results {
		if line != "" {
			fmt.Fprintln(a.out, line)
		}
	}
}

// localReadOnly names local tools that only look, for the ones whose
// annotations do not already say so. Anything not known to be read-only asks.
var localReadOnlyPrefixes = []string{"read_", "list_", "find_", "grep", "get_", "check_", "search_", "describe_", "show_"}
var localReadOnlyNames = map[string]bool{
	"system_info": true, "project_commands": true, "file_outline": true, "file_info": true,
	"git_status": true, "git_diff": true, "git_log": true, "git_show": true, "git_blame": true,
	"db_query": true, "db_tables": true, "db_describe_table": true, "diff_files": true,
	"markdown_outline": true, "definitions": true, "find_definition": true, "find_references": true,
}

func localToolReadOnly(def Tool) bool {
	if def.Annotations != nil && def.Annotations.ReadOnlyHint {
		return true
	}
	if def.Annotations != nil && (def.Annotations.DestructiveHint || def.Annotations.OpenWorldHint) {
		return false
	}
	if localReadOnlyNames[def.Name] {
		return true
	}
	for _, p := range localReadOnlyPrefixes {
		if strings.HasPrefix(def.Name, p) {
			return true
		}
	}
	return false
}

// toolName makes a name every API accepts: letters, digits, _ and -, at most
// 64 characters, unique across the whole tool set.
func toolName(prefix, name string) string {
	full := name
	if prefix != "" {
		full = prefix + "__" + name
	}
	full = nonToolChars.ReplaceAllString(full, "_")
	if len(full) > 64 {
		sum := sha1.Sum([]byte(full))
		full = full[:55] + "_" + hex.EncodeToString(sum[:4])
	}
	return full
}

func normalizeSchema(s map[string]any) map[string]any {
	if len(s) == 0 {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	// Round-trip through JSON so []string and other typed values become the
	// plain shapes every encoder agrees on.
	data, err := json.Marshal(s)
	if err != nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	var out map[string]any
	json.Unmarshal(data, &out)
	if _, ok := out["type"]; !ok {
		out["type"] = "object"
	}
	if _, ok := out["properties"]; !ok {
		out["properties"] = map[string]any{}
	}
	return out
}

func (a *agent) rebuildTools() {
	a.tools = nil
	a.toolIndex = map[string]*agentTool{}
	add := func(t *agentTool) {
		if _, dup := a.toolIndex[t.spec.Name]; dup {
			return
		}
		a.tools = append(a.tools, t)
		a.toolIndex[t.spec.Name] = t
	}
	if a.srv != nil {
		for _, name := range a.srv.ToolNames() {
			reg, ok := a.srv.lookupTool(name)
			if !ok {
				continue
			}
			srv, regName, reg := a.srv, name, reg
			add(&agentTool{
				spec:     toolSpec{Name: toolName("", name), Description: reg.def.Description, Schema: normalizeSchema(reg.def.InputSchema)},
				source:   "local",
				readOnly: localToolReadOnly(reg.def),
				run: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
					res, rpcErr := srv.runTool(withVersion(ctx, ProtocolVersion), regName, reg, args)
					if rpcErr != nil {
						return nil, fmt.Errorf("%s", rpcErr.Message)
					}
					return res, nil
				},
			})
		}
	}
	for _, conn := range a.conns {
		if !conn.connected {
			continue
		}
		switch {
		case conn.mcp != nil:
			client := conn.mcp
			for _, def := range client.tools {
				remote := def.Name
				readOnly := slicesContains(client.cfg.ReadOnlyTools, remote) || def.Annotations != nil && def.Annotations.ReadOnlyHint
				add(&agentTool{
					spec: toolSpec{
						Name:        toolName(conn.name, remote),
						Description: fmt.Sprintf("[MCP server %s] %s", conn.name, def.Description),
						Schema:      normalizeSchema(def.InputSchema),
					},
					source:   conn.name,
					readOnly: readOnly,
					run: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
						return client.callTool(ctx, remote, args)
					},
				})
			}
		case conn.api != nil:
			client := conn.api
			for _, op := range client.ops {
				op := op
				add(&agentTool{
					spec: toolSpec{
						Name:        toolName(conn.name, op.name),
						Description: fmt.Sprintf("[REST API %s] %s", conn.name, op.description),
						Schema:      normalizeSchema(op.schema),
					},
					source:   conn.name,
					readOnly: op.method == "GET" || op.method == "HEAD" || op.method == "OPTIONS",
					run: func(ctx context.Context, args json.RawMessage) (*CallToolResult, error) {
						return client.invoke(ctx, op, args)
					},
				})
			}
		}
	}
}

func (a *agent) specs() []toolSpec {
	out := make([]toolSpec, 0, len(a.tools))
	for _, t := range a.tools {
		out = append(out, t.spec)
	}
	return out
}

func (a *agent) systemPrompt() string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %s agent, an autonomous coding agent working in a terminal on the user's machine.\n", appName)
	fmt.Fprintf(&b, "Workspace: %s\nPlatform: %s/%s\nDate: %s\n\n", a.cfg.Workspace.Root, runtime.GOOS, runtime.GOARCH, time.Now().Format("2006-01-02"))
	b.WriteString("Work by calling tools: inspect before you change, keep edits minimal, verify changes with the " +
		"project's build and test commands, and report plainly what you did and what you could not do. " +
		"The user may decline a tool call; when that happens, do not retry the same call - adjust or ask. " +
		"Keep replies short; the terminal renders plain text.\n")
	if a.srv != nil {
		if text := a.srv.instructionText(); text != "" {
			b.WriteString("\nTool server notes:\n" + text + "\n")
		}
	}
	var external []string
	for _, c := range a.conns {
		if c.connected {
			external = append(external, fmt.Sprintf("- %s (%s): tools prefixed %s__", c.name, c.kind, c.name))
		}
	}
	if len(external) > 0 {
		b.WriteString("\nExternal connections:\n" + strings.Join(external, "\n") + "\n")
	}
	if a.cfg.Agent.SystemPrompt != "" {
		b.WriteString("\n" + a.cfg.Agent.SystemPrompt + "\n")
	}
	return b.String()
}

func (a *agent) printBanner() {
	fmt.Fprintf(a.out, "%s agent %s\n", appName, version)
	fmt.Fprintf(a.out, "  workspace    %s\n", a.cfg.Workspace.Root)
	fmt.Fprintf(a.out, "  model        %s\n", a.model.describe())
	if a.skipPerms {
		fmt.Fprintf(a.out, "  permissions  %s\n", a.paint("33", "SKIPPED (--dangerously-skip-permissions): every tool runs without asking"))
	} else {
		fmt.Fprintf(a.out, "  permissions  ask before tools that change files, run commands or reach external services\n")
	}
	if len(a.conns) > 0 {
		var names []string
		for _, c := range a.conns {
			names = append(names, c.name)
		}
		fmt.Fprintf(a.out, "  connections  %s\n", strings.Join(names, ", "))
	}
	if path, ok := a.findLatestSummary(); ok {
		fmt.Fprintf(a.out, "  summary      found %s - /loadsummary to resume\n", filepath.Base(path))
	}
	fmt.Fprintln(a.out, "  /help for commands, /exit to quit")
}

// Bracketed paste is what tells a paste of several lines from one keyboard or
// mouse action apart from the same lines typed and confirmed one at a time.
// A terminal that supports it wraps a paste's whole text in these two
// sequences, once asked to with enableBracketedPaste; without that request
// nothing sends them, and readInput's plain line-by-line behaviour is
// unchanged. Without this, the first line of a paste looks like a complete
// input and would be sent off before the rest of the paste even arrived.
const (
	bracketedPasteStart = "\x1b[200~"
	bracketedPasteEnd   = "\x1b[201~"
)

// enableBracketedPaste and disableBracketedPaste turn the mode above on and
// off. Off must happen before the process exits, or the terminal is left in
// that mode for whatever runs in it next.
func enableBracketedPaste(w io.Writer)  { fmt.Fprint(w, "\x1b[?2004h") }
func disableBracketedPaste(w io.Writer) { fmt.Fprint(w, "\x1b[?2004l") }

// readInput feeds stdin lines to the loop, so the loop can wait on a line and
// on Ctrl-C at the same time. A pasted block is collected whole, markers
// stripped, newlines inside it preserved, and sent as one line: from the
// loop's point of view a paste is indistinguishable from a very fast typist
// who never presses Enter until the end, except that its embedded newlines
// are real content rather than a request to keep collecting - only the
// backslash convention still means that, and a paste ending in a literal
// backslash right before Enter is the one case that convention still catches
// by mistake.
func (a *agent) readInput(r io.Reader) {
	reader := bufio.NewReaderSize(r, 1<<20)
	var pasting bool
	var paste strings.Builder
	for {
		raw, err := reader.ReadString('\n')
		line := strings.TrimRight(raw, "\r\n")

		if !pasting {
			if i := strings.Index(line, bracketedPasteStart); i >= 0 {
				pasting = true
				paste.WriteString(line[:i])
				line = line[i+len(bracketedPasteStart):]
			}
		}
		if pasting {
			if i := strings.Index(line, bracketedPasteEnd); i >= 0 {
				pasting = false
				paste.WriteString(line[:i])
				paste.WriteString(line[i+len(bracketedPasteEnd):])
				a.lines <- paste.String()
				paste.Reset()
			} else {
				paste.WriteString(line)
				paste.WriteString("\n")
			}
		} else if line != "" || err == nil {
			a.lines <- line
		}
		if err != nil {
			close(a.lines)
			return
		}
	}
}

func (a *agent) repl() error {
	// Only worth asking for when both ends are a real terminal: readInput
	// would never see the markers otherwise, and writing raw escape codes
	// into a redirected stdout would just be garbage in a file or pipe.
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		enableBracketedPaste(a.out)
		defer disableBracketedPaste(a.out)
	}
	for {
		fmt.Fprint(a.out, a.paint("1;36", "> "))
		var input string
	collect:
		for {
			select {
			case line, ok := <-a.lines:
				if !ok {
					fmt.Fprintln(a.out)
					return nil
				}
				if strings.HasSuffix(line, "\\") {
					input += strings.TrimSuffix(line, "\\") + "\n"
					fmt.Fprint(a.out, a.paint("36", ". "))
					continue
				}
				input += line
				break collect
			case <-a.signals:
				if a.noteInterrupt() {
					fmt.Fprintln(a.out)
					return nil
				}
				input = ""
				fmt.Fprint(a.out, "\n(press Ctrl-C again to quit)\n"+a.paint("1;36", "> "))
			}
		}
		input = strings.TrimSpace(input)
		if input == "" {
			continue
		}
		if strings.HasPrefix(input, "/") {
			if quit := a.command(input); quit {
				return nil
			}
			continue
		}
		if err := a.runTurn(input); err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		}
	}
}

func (a *agent) command(input string) (quit bool) {
	fields := strings.Fields(input)
	cmd, rest := strings.ToLower(fields[0]), fields[1:]
	switch cmd {
	case "/exit", "/quit", "/q":
		return true
	case "/help", "/?":
		fmt.Fprint(a.out, agentUsage[strings.Index(agentUsage, "In the console:"):])
	case "/clear", "/new", "/reset":
		a.history = nil
		fmt.Fprintln(a.out, "conversation cleared")
	case "/save":
		path, err := a.saveConversation(strings.Join(rest, " "))
		if err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		} else {
			fmt.Fprintf(a.out, "saved %d messages to %s\n", len(a.history), path)
		}
	case "/summary":
		ctx, cancel := a.interruptible()
		summary, err := a.summarizeConversation(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		} else {
			fmt.Fprintln(a.out, summary)
		}
	case "/savesummary":
		ctx, cancel := a.interruptible()
		summary, err := a.summarizeConversation(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
			return false
		}
		path, err := a.saveSummary(strings.Join(rest, " "), summary)
		if err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		} else {
			fmt.Fprintf(a.out, "saved summary to %s\n", path)
		}
	case "/loadsummary":
		path := strings.Join(rest, " ")
		if path == "" {
			found, ok := a.findLatestSummary()
			if !ok {
				fmt.Fprintln(a.out, a.paint("31", "error: no saved summary found in "+a.cfg.Workspace.Root))
				return false
			}
			path = found
		} else if !filepath.IsAbs(path) {
			path = filepath.Join(a.cfg.Workspace.Root, path)
		}
		if err := a.loadSummary(path); err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		} else {
			fmt.Fprintf(a.out, "loaded summary from %s into the conversation\n", path)
		}
	case "/compress":
		ctx, cancel := a.interruptible()
		summary, err := a.summarizeConversation(ctx)
		cancel()
		if err != nil {
			fmt.Fprintln(a.out, a.paint("31", "error: "+err.Error()))
		} else {
			before := len(a.history)
			a.history = []chatMessage{{
				Role:    "assistant",
				Content: "Here's a summary of our conversation so far, replacing the full history:\n\n" + summary,
			}}
			fmt.Fprintf(a.out, "compressed %d messages into a %d-character summary\n", before, len(summary))
		}
	case "/reload":
		if err := a.reloadConfig(); err != nil {
			fmt.Fprintln(a.out, a.paint("31", "could not reload "+a.configPath+": "+err.Error()))
			fmt.Fprintln(a.out, "keeping the previous configuration")
		} else {
			fmt.Fprintf(a.out, "reloaded %s: now using %s, %d tools available\n", a.configPath, a.model.describe(), len(a.tools))
		}
	case "/connect", "/reconnect", "/test":
		only := ""
		if len(rest) > 0 {
			only = rest[0]
		}
		ctx, cancel := a.interruptible()
		a.connectAll(ctx, only, true)
		cancel()
		a.rebuildTools()
		fmt.Fprintf(a.out, "%d tools available\n", len(a.tools))
	case "/tools":
		filter := strings.ToLower(strings.Join(rest, " "))
		count := 0
		for _, t := range a.tools {
			if filter != "" && !strings.Contains(strings.ToLower(t.spec.Name+" "+t.source), filter) {
				continue
			}
			count++
			mode := a.paint("33", "ask ")
			if t.readOnly || a.allowed[t.spec.Name] || a.skipPerms {
				mode = a.paint("32", "auto")
			}
			fmt.Fprintf(a.out, "  %s %-40s %-10s %s\n", mode, t.spec.Name, t.source, shorten(firstLine(t.spec.Description), 60))
		}
		fmt.Fprintf(a.out, "%d tools\n", count)
	case "/models":
		for _, m := range a.models {
			marker := "  "
			if m == a.model {
				marker = a.paint("32", "* ")
			}
			fmt.Fprintf(a.out, "%s%s\n", marker, m.describe())
		}
	case "/model":
		if len(rest) == 0 {
			fmt.Fprintf(a.out, "current: %s\n", a.model.describe())
			ctx, cancel := a.interruptible()
			ids, err := a.model.listModels(ctx)
			cancel()
			if err != nil {
				fmt.Fprintf(a.out, "the endpoint did not list its models: %v\n", err)
			} else {
				fmt.Fprintf(a.out, "served by this endpoint: %s\n", wrapList(ids, 100, "  "))
			}
			fmt.Fprintln(a.out, "/model <name> switches endpoint; /model <model id> changes the model on this one")
			return false
		}
		if m := a.findModel(rest[0]); m != nil {
			a.model = m
		} else {
			a.model.model = rest[0]
		}
		fmt.Fprintf(a.out, "now using %s\n", a.model.describe())
	case "/thinking", "/think":
		if len(rest) > 0 {
			switch strings.ToLower(rest[0]) {
			case "on", "show":
				a.showThinking = true
			case "off", "hide", "collapse":
				a.showThinking = false
			default:
				fmt.Fprintln(a.out, "usage: /thinking [on|off]")
				return false
			}
		} else {
			a.showThinking = !a.showThinking
		}
		if a.showThinking {
			fmt.Fprintln(a.out, "thinking: shown")
		} else {
			fmt.Fprintln(a.out, "thinking: collapsed")
		}
	case "/stats":
		ctx, cancel := a.interruptible()
		stats := gatherSystemStats(ctx, a.cfg.Workspace.Root)
		cancel()
		fmt.Fprint(a.out, stats.summarize())
	case "/permissions", "/perms":
		if a.skipPerms {
			fmt.Fprintln(a.out, "permissions are skipped (--dangerously-skip-permissions)")
		} else {
			fmt.Fprintln(a.out, "read-only tools run without asking; everything else asks")
		}
		var names []string
		for name := range a.allowed {
			names = append(names, name)
		}
		sort.Strings(names)
		if len(names) > 0 {
			fmt.Fprintf(a.out, "always allowed: %s\n", strings.Join(names, ", "))
		}
	default:
		fmt.Fprintf(a.out, "unknown command %s; /help lists them\n", cmd)
	}
	return false
}

// interruptible is a context Ctrl-C cancels - stopping the current turn or
// command and returning to the prompt, not quitting the application. It
// still records the Ctrl-C via noteInterrupt so that one right after it, now
// caught idle at the prompt, is recognized as the second press and exits.
func (a *agent) interruptible() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		select {
		case <-a.signals:
			fmt.Fprintln(a.out, a.paint("33", "\ninterrupted"))
			a.noteInterrupt()
			cancel()
		case <-done:
		}
	}()
	return ctx, func() {
		close(done)
		cancel()
	}
}

// summaryPrompt is appended as one more turn when /summary, /savesummary or
// /compress asks the model to condense everything said so far. It is written
// as a standalone note rather than a reply, since /compress goes on to make
// it the entire visible history.
const summaryPrompt = "Summarize this conversation so far in enough detail that someone picking it up cold, " +
	"with no other context, could continue the work: what was asked, what was found or decided, which files, " +
	"commands or tool results mattered, and what is still unresolved or pending. Write it as a standalone note, " +
	"not as a reply to me."

// summarizeConversation asks the model to condense a.history without
// changing it. /summary shows the result, /savesummary writes it to a file,
// and /compress uses it to replace the history outright - all three share
// this one call rather than each building their own summarization prompt.
func (a *agent) summarizeConversation(ctx context.Context) (string, error) {
	if len(a.history) == 0 {
		return "", fmt.Errorf("there is nothing to summarize yet")
	}
	msgs := append(append([]chatMessage{}, a.history...), chatMessage{Role: "user", Content: summaryPrompt})
	// No tools are offered: this is a single request for prose, not another
	// turn of the regular tool loop.
	reply, err := a.model.complete(ctx, a.systemPrompt(), msgs, nil)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(reply.Text)
	if summary == "" {
		return "", fmt.Errorf("the model returned an empty summary")
	}
	return summary, nil
}

// resolveSavePath is what /save, /savesummary and friends turn a console
// argument into: an empty one gets a timestamped default name so a save never
// silently overwrites an earlier one, a relative one is resolved against the
// workspace root (where a person typing a bare file name expects it to land),
// and an absolute one is used as given.
func (a *agent) resolveSavePath(path, defaultPrefix, ext string) string {
	if path == "" {
		path = fmt.Sprintf("%s-%s.%s", defaultPrefix, time.Now().Format("20060102-150405"), ext)
	}
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(a.cfg.Workspace.Root, path)
}

// transcriptLine is one JSONL record written by /save. It mirrors chatMessage
// and toolCall with explicit, stable field names: those two have none of
// their own, since nothing before this used them as a wire or file format.
type transcriptLine struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCalls  []transcriptCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolName   string           `json:"tool_name,omitempty"`
}

type transcriptCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// saveConversation writes the conversation history to a JSONL file, one
// message per line in the order the conversation happened, and returns the
// path it was written to.
func (a *agent) saveConversation(path string) (string, error) {
	if len(a.history) == 0 {
		return "", fmt.Errorf("there is nothing to save yet")
	}
	path = a.resolveSavePath(path, "codemcp-conversation", "jsonl")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range a.history {
		line := transcriptLine{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, ToolName: m.ToolName}
		for _, tc := range m.ToolCalls {
			line.ToolCalls = append(line.ToolCalls, transcriptCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		if err := enc.Encode(line); err != nil {
			return "", err
		}
	}
	return path, nil
}

// saveSummary writes a summary from /summary or /compress to a plain text
// file - prose for a person to read, not a transcript of messages, so it
// gets its own default name and extension rather than /save's JSONL.
func (a *agent) saveSummary(path, summary string) (string, error) {
	path = a.resolveSavePath(path, "codemcp-summary", "md")
	if err := os.WriteFile(path, []byte(summary+"\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// findLatestSummary looks in the workspace root for files /savesummary wrote
// with its default name - "codemcp-summary-<timestamp>.md" - and returns the
// most recent one. The timestamp format sorts lexicographically the same as
// chronologically, so the greatest name is the latest file; a summary saved
// under a custom name isn't found, since nothing then distinguishes it from
// any other markdown file a person keeps in the workspace.
func (a *agent) findLatestSummary() (string, bool) {
	entries, err := os.ReadDir(a.cfg.Workspace.Root)
	if err != nil {
		return "", false
	}
	var latest string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, "codemcp-summary-") && strings.HasSuffix(name, ".md") && name > latest {
			latest = name
		}
	}
	if latest == "" {
		return "", false
	}
	return filepath.Join(a.cfg.Workspace.Root, latest), true
}

// loadSummary reads a summary file written by /savesummary and puts it at
// the front of the conversation as if the model had said it, so the next
// turn picks up with that context already in hand. It is prepended rather
// than replacing the history outright, unlike /compress, so /loadsummary
// stays safe to run on a conversation that is already under way.
func (a *agent) loadSummary(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	summary := strings.TrimSpace(string(data))
	if summary == "" {
		return fmt.Errorf("%s is empty", path)
	}
	msg := chatMessage{
		Role:    "assistant",
		Content: "Here's a summary from a previous session, resuming from where we left off:\n\n" + summary,
	}
	a.history = append([]chatMessage{msg}, a.history...)
	return nil
}

const maxToolResultBytes = 60_000

func (a *agent) runTurn(prompt string) error {
	ctx, cancel := a.interruptible()
	defer cancel()
	a.history = append(a.history, chatMessage{Role: "user", Content: prompt})
	system := a.systemPrompt()
	specs := a.specs()

	for turn := 0; turn < a.maxTurns; turn++ {
		stopSpinner := a.spinner()
		start := time.Now()
		reply, err := a.model.complete(ctx, system, a.history, specs)
		elapsed := time.Since(start)
		stopSpinner()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		a.history = append(a.history, chatMessage{Role: "assistant", Content: reply.Text, ToolCalls: reply.ToolCalls})
		a.printReasoning(reply.Reasoning)
		if text := strings.TrimSpace(reply.Text); text != "" {
			fmt.Fprintln(a.out, text)
		}
		a.printUsage(reply, elapsed)
		if len(reply.ToolCalls) == 0 {
			return nil
		}
		for _, call := range reply.ToolCalls {
			var result string
			if ctx.Err() != nil {
				result = "Cancelled by the user before this tool ran."
			} else {
				result = a.executeTool(ctx, call)
			}
			a.history = append(a.history, chatMessage{Role: "tool", ToolCallID: call.ID, ToolName: call.Name, Content: result})
		}
		if ctx.Err() != nil {
			return nil
		}
	}
	fmt.Fprintf(a.out, "%s\n", a.paint("33", fmt.Sprintf("stopped after %d model/tool round trips (--max-turns)", a.maxTurns)))
	return nil
}

func (a *agent) spinner() func() {
	if !a.color {
		return func() {}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
		start := time.Now()
		for i := 0; ; i++ {
			select {
			case <-done:
				fmt.Fprint(a.out, "\r\x1b[K")
				return
			case <-time.After(100 * time.Millisecond):
				fmt.Fprintf(a.out, "\r%s", a.paint("90", fmt.Sprintf("%s thinking (%ds)", frames[i%len(frames)], int(time.Since(start).Seconds()))))
			}
		}
	}()
	return func() {
		close(done)
		wg.Wait()
	}
}

func compactArgs(raw json.RawMessage, n int) string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return truncateText(string(raw), n)
	}
	data, _ := json.Marshal(v)
	s := string(data)
	if len(s) > n {
		s = s[:n] + "…"
	}
	return s
}

func (a *agent) executeTool(ctx context.Context, call toolCall) string {
	tool, ok := a.toolIndex[call.Name]
	fmt.Fprintf(a.out, "%s %s\n", a.paint("35", "⏺ "+call.Name), a.paint("90", compactArgs(call.Arguments, 160)))
	if !ok {
		var names []string
		for _, t := range a.tools {
			names = append(names, t.spec.Name)
		}
		msg := fmt.Sprintf("There is no tool named %q. Available tools: %s", call.Name, strings.Join(names, ", "))
		fmt.Fprintln(a.out, a.paint("31", "  unknown tool"))
		return msg
	}
	if !json.Valid(call.Arguments) {
		return "The arguments were not valid JSON; call the tool again with a JSON object."
	}
	if allowed, feedback := a.permit(ctx, tool, call); !allowed {
		if feedback != "" {
			return "The user declined this tool call and said: " + feedback
		}
		return "The user declined this tool call. Do not retry it; continue without it or ask the user."
	}
	start := time.Now()
	res, err := tool.run(ctx, call.Arguments)
	elapsed := time.Since(start)
	if err != nil {
		fmt.Fprintf(a.out, "  %s\n", a.paint("31", "error: "+truncateText(err.Error(), 300)))
		return "The tool failed: " + err.Error()
	}
	text := resultText(res)
	if text == "" && res != nil && res.StructuredContent != nil {
		data, _ := json.MarshalIndent(res.StructuredContent, "", "  ")
		text = string(data)
	}
	if res != nil {
		for _, block := range res.Content {
			if block.Type != "text" && block.Text == "" {
				text += fmt.Sprintf("\n[%s content omitted: %s]", block.Type, firstNonEmpty(block.MimeType, block.URI, block.Name))
			}
		}
	}
	if text == "" {
		text = "(no output)"
	}
	a.printPreview(text, res != nil && res.IsError, elapsed)
	if len(text) > maxToolResultBytes {
		text = text[:maxToolResultBytes] + fmt.Sprintf("\n[... %d bytes truncated]", len(text)-maxToolResultBytes)
	}
	if res != nil && res.IsError {
		return "Error: " + text
	}
	return text
}

func (a *agent) printPreview(text string, isError bool, elapsed time.Duration) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	const show = 6
	code := "90"
	if isError {
		code = "31"
	}
	for i, line := range lines {
		if i == show {
			fmt.Fprintf(a.out, "  %s\n", a.paint(code, fmt.Sprintf("… %d more lines", len(lines)-show)))
			break
		}
		fmt.Fprintf(a.out, "  %s\n", a.paint(code, truncateText(line, 200)))
	}
	if elapsed > 2*time.Second {
		fmt.Fprintf(a.out, "  %s\n", a.paint("90", fmt.Sprintf("(%.1fs)", elapsed.Seconds())))
	}
}

// permit asks the user before a tool that is not read-only runs. It returns
// whether to run it and, when declined, anything the user typed as a reason.
func (a *agent) permit(ctx context.Context, tool *agentTool, call toolCall) (bool, string) {
	if a.skipPerms || tool.readOnly || a.allowed[call.Name] {
		return true, ""
	}
	if a.opts.prompt != "" && !isTerminal(os.Stdin) {
		fmt.Fprintln(a.out, a.paint("33", "  declined: no terminal to ask for permission (use --dangerously-skip-permissions)"))
		return false, ""
	}
	var pretty any
	json.Unmarshal(call.Arguments, &pretty)
	detail, _ := json.MarshalIndent(pretty, "    ", "  ")
	fmt.Fprintf(a.out, "  %s wants to run %s (%s):\n    %s\n", a.paint("33", "permission:"), call.Name, tool.source, truncateText(string(detail), 2000))
	for {
		fmt.Fprint(a.out, a.paint("33", "  allow? [y]es / [a]lways for this tool / [n]o (or type a reason): "))
		select {
		case <-ctx.Done():
			return false, ""
		case line, ok := <-a.lines:
			if !ok {
				return false, ""
			}
			answer := strings.TrimSpace(line)
			switch strings.ToLower(answer) {
			case "y", "yes":
				return true, ""
			case "a", "always":
				a.allowed[call.Name] = true
				return true, ""
			case "n", "no", "":
				return false, ""
			default:
				return false, answer
			}
		}
	}
}

// shorten cuts a line for display, marking the cut with an ellipsis.
func shorten(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
