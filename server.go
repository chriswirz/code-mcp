package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// ToolHandler runs one tool call. Returning an error is reserved for protocol
// failures; problems the model could correct belong in the result with
// IsError set, which is what toolError produces.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*CallToolResult, *RPCError)

type registeredTool struct {
	def     Tool
	handler ToolHandler
}

// Server dispatches MCP requests. It holds no per-connection state: revision
// 2026-07-28 removed sessions, so every request is answered on its own.
type Server struct {
	name         string
	version      string
	instructions string

	// legacy is whether the initialize-based revisions are served alongside the
	// current one. It decides what server/discover advertises and which
	// versions a request may declare.
	legacy bool

	mu               sync.RWMutex
	tools            map[string]registeredTool
	order            []string
	commandToolNames []string
	// unavailable names the tools this configuration switched off, with the
	// guidance to answer a call to one with.
	unavailable map[string]unavailableTool

	ws   *Workspace
	db   *DB
	sudo *sudoAgent

	// dl hosts the temporary download links. It is built once, at startup,
	// because its listener - shared with the MCP endpoint or its own on stdio -
	// cannot be moved under a running process.
	dl *downloadServer

	// api is the OpenAPI face of the tool set, or nil when it is off. It is
	// built at startup alongside the transport, since it is mounted on the
	// same listener.
	api *openAPIServer

	// processes tracks the background processes run_process started. It is
	// built once and survives a configuration reload: rebuilding the tool set
	// must not lose track of something still running.
	processes *processRegistry

	// baseInstructions is the operator's own text, kept separate so the notes
	// this server appends can be rebuilt from scratch on a config reload
	// instead of accumulating.
	baseInstructions string
	// cfg is the configuration currently in effect, and reload re-reads it
	// before each request cycle.
	cfg    Config
	reload *configReloader
	logger *log.Logger
}

// NewServer builds a server with no tools registered. legacy enables the
// initialize-based revisions alongside the current one.
func NewServer(name, version, instructions string, legacy bool) *Server {
	base := instructions
	// A server running as root is something the client must be told about
	// before it calls anything, so the warning rides along with the
	// instructions rather than waiting for a system_info call.
	if note := currentPrivileges().note(); note != "" {
		if instructions == "" {
			instructions = note
		} else {
			instructions = instructions + "\n\n" + note
		}
	}
	return &Server{
		name:             name,
		version:          version,
		instructions:     instructions,
		baseInstructions: base,
		legacy:           legacy,
		tools:            make(map[string]registeredTool),
		processes:        newProcessRegistry(),
	}
}

// AddInstruction appends a paragraph to the server instructions, for facts
// that are only known once the workspace and tools are wired up.
func (s *Server) AddInstruction(note string) {
	if note == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.instructions == "" {
		s.instructions = note
		return
	}
	s.instructions += "\n\n" + note
}

// RegisterTool adds a tool. Later registrations of the same name win, which
// lets a user-defined command shadow a built-in.
func (s *Server) RegisterTool(def Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[def.Name]; !exists {
		s.order = append(s.order, def.Name)
	}
	s.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// ToolNames returns the registered tool names in registration order.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// versionKey carries the protocol version a transport negotiated for the
// request being handled. A legacy session fixes it once at initialize; a modern
// request declares it in _meta every time.
type versionKey struct{}

// withVersion returns a context carrying the protocol version to answer in.
func withVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, versionKey{}, version)
}

// versionFrom returns the protocol version for the request being handled.
func versionFrom(ctx context.Context) string {
	version, _ := ctx.Value(versionKey{}).(string)
	return version
}

// completeResult builds the result envelope with this server's identity
// attached, as servers SHOULD include on every result. Against a legacy client
// the envelope is empty: resultType and the cache hints were introduced in
// 2026-07-28 and mean nothing to an earlier revision.
func (s *Server) completeResult(ctx context.Context) Result {
	if !isModernVersion(versionFrom(ctx)) {
		return Result{}
	}
	name, version := s.identity()
	return Result{
		ResultType: ResultComplete,
		Meta: map[string]any{
			MetaServerInfo: Implementation{Name: name, Version: version},
		},
	}
}

func (s *Server) capabilities() ServerCapabilities {
	return ServerCapabilities{
		Tools:     &ToolsCapability{ListChanged: false},
		Resources: &ResourcesCapability{},
		Prompts:   &PromptsCapability{},
	}
}

// Handle dispatches a single request and returns its result, or an error. It
// returns (nil, nil) for a notification that needs no response.
//
// When the transport has already negotiated a protocol version - which is what
// a legacy initialize handshake does - it puts it on the context with
// withVersion. Otherwise the version is read from the request metadata, where a
// modern client puts it on every request.
func (s *Server) Handle(ctx context.Context, req *Request) (any, *RPCError) {
	// Each request is a cycle: re-read config.json first, so an edit takes
	// effect now. A file that cannot be read or does not parse leaves the
	// values already in effect alone.
	s.reload.Refresh()
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return nil, Errorf(CodeInvalidRequest, "unsupported jsonrpc version %q", req.JSONRPC)
	}
	if versionFrom(ctx) == "" {
		ctx = withVersion(ctx, req.MetaString(MetaProtocolVersion))
	}
	version := versionFrom(ctx)

	// server/discover is the one method a client may call to find out which
	// versions this server accepts, so it tolerates an absent declaration.
	if req.Method != "server/discover" && !s.supports(version) {
		return nil, UnsupportedVersionError(version)
	}

	switch req.Method {
	case "server/discover":
		return s.discover(ctx), nil
	case "tools/list":
		return s.listTools(ctx), nil
	case "tools/call":
		return s.callTool(ctx, req)
	case "resources/list":
		return s.listResources(ctx)
	case "resources/templates/list":
		return s.listResourceTemplates(ctx), nil
	case "resources/read":
		return s.readResource(ctx, req)
	case "prompts/list":
		return s.listPrompts(ctx), nil
	case "prompts/get":
		return s.getPrompt(ctx, req)

	// Methods that exist only in the legacy revisions. They are accepted so a
	// legacy client is not tripped up by a method-not-found it cannot recover
	// from, but this server has nothing to report through any of them: it
	// publishes no list-changed notifications and logs to stderr.
	case "ping":
		return emptyResult, nil
	case "logging/setLevel", "resources/subscribe", "resources/unsubscribe":
		if isModernVersion(version) {
			return nil, Errorf(CodeMethodNotFound,
				"%q was removed in protocol version %s", req.Method, ProtocolVersion)
		}
		return emptyResult, nil
	case "initialize":
		// Transports handle initialize themselves, because the handshake is
		// what creates the session or fixes the version for the process.
		return nil, Errorf(CodeInvalidRequest, "initialize is handled by the transport")
	}

	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, Errorf(CodeMethodNotFound, "unknown method %q", req.Method)
}

// supportedVersions is what this server actually serves, newest first.
func (s *Server) supportedVersions() []string {
	if !s.isLegacy() {
		return ModernVersions
	}
	return SupportedVersions
}

func (s *Server) supports(version string) bool {
	for _, v := range s.supportedVersions() {
		if v == version {
			return true
		}
	}
	return false
}

func (s *Server) discover(ctx context.Context) *DiscoverResult {
	return &DiscoverResult{
		Result:            s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		SupportedVersions: s.supportedVersions(),
		Capabilities:      s.capabilities(),
		Instructions:      s.instructionText(),
	}
}

func (s *Server) listTools(ctx context.Context) *ListToolsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The order must be deterministic so clients can cache the list; sorting
	// by name gives that regardless of registration order.
	names := append([]string(nil), s.order...)
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, s.tools[name].def)
	}
	return &ListToolsResult{
		Result: s.completeResult(ctx).cacheable(300000, CacheScopePrivate),
		Tools:  tools,
	}
}

func (s *Server) callTool(ctx context.Context, req *Request) (any, *RPCError) {
	var params CallToolParams
	if err := req.Bind(&params); err != nil {
		return nil, err
	}
	s.mu.RLock()
	tool, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		// A call that cannot be served is answered as a tool result, not as a
		// protocol error: a model reads a JSON-RPC error as the transport
		// failing and tries the same call again, where an error result is
		// something it can act on.
		if info, off := s.unavailableFor(params.Name); off {
			return &CallToolResult{
				Content: textContent(info.message(params.Name)),
				IsError: true,
				Result:  s.completeResult(ctx),
			}, nil
		}
		return &CallToolResult{
			Content: textContent(s.unknownToolMessage(params.Name)),
			IsError: true,
			Result:  s.completeResult(ctx),
		}, nil
	}
	res, err := s.runTool(ctx, params.Name, tool.handler, params.Arguments)
	if err != nil {
		return nil, err
	}
	res.Result = s.completeResult(ctx)
	return res, nil
}

// runTool calls a handler and turns a panic in it into an ordinary tool error.
// One tool hitting a nil map or an index it did not check should cost the
// caller that one call, not the whole server: on stdio a panic would take the
// process down with every session on it, and the model would see the transport
// die rather than something it could act on.
func (s *Server) runTool(ctx context.Context, name string, handler ToolHandler, args json.RawMessage) (res *CallToolResult, rpcErr *RPCError) {
	defer func() {
		if r := recover(); r != nil {
			if s.logger != nil {
				s.logger.Printf("tool %s panicked: %v\n%s", name, r, debug.Stack())
			}
			res = toolError("the %s tool failed with an internal error: %v. This is a bug in the "+
				"server, not in the call; try another approach", name, r)
			rpcErr = nil
		}
	}()
	return handler(ctx, args)
}

// toolResult builds a successful tool result from text.
func toolResult(text string) *CallToolResult {
	return &CallToolResult{Content: textContent(text)}
}

// toolResultJSON builds a tool result carrying both structured content and,
// for clients that ignore it, the same data serialised as text.
func toolResultJSON(value any) *CallToolResult {
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return toolError("could not serialise result: %v", err)
	}
	return &CallToolResult{
		Content:           textContent(string(pretty)),
		StructuredContent: value,
	}
}

// toolError reports a failure the model can act on and retry.
func toolError(format string, args ...any) *CallToolResult {
	return &CallToolResult{
		Content: textContent(fmt.Sprintf(format, args...)),
		IsError: true,
	}
}

// camelToSnake rewrites a camelCase or PascalCase key as snake_case. Runs of
// capitals are treated as one word, so "maxHTTPRetries" becomes
// "max_http_retries" rather than "max_h_t_t_p_retries".
func camelToSnake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			b.WriteRune(r)
			continue
		}
		prevIsLower := i > 0 && !unicode.IsUpper(runes[i-1]) && runes[i-1] != '_'
		nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if i > 0 && (prevIsLower || nextIsLower) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// aliasCamelKeys adds a snake_case sibling for every camelCase key, recursively.
// Models are inconsistent about which convention JSON "should" use, and a tool
// call that fails on the spelling of a key wastes a whole turn to fix something
// that carries no meaning. An explicitly supplied snake_case key always wins.
func aliasCamelKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		aliases := map[string]any{}
		for k, val := range t {
			t[k] = aliasCamelKeys(val)
			if snake := camelToSnake(k); snake != k {
				if _, exists := t[snake]; !exists {
					aliases[snake] = t[k]
				}
			}
		}
		// Applied after the walk, because adding to a map mid-range leaves it
		// unspecified whether the new keys are visited.
		for k, val := range aliases {
			t[k] = val
		}
		return t
	case []any:
		for i := range t {
			t[i] = aliasCamelKeys(t[i])
		}
		return t
	}
	return v
}

// hasUpper reports whether the payload contains a capital letter at all, which
// is the cheap check that keeps the alias round-trip off the common path.
func hasUpper(raw []byte) bool {
	for _, b := range raw {
		if b >= 'A' && b <= 'Z' {
			return true
		}
	}
	return false
}

// decodeArgs unmarshals tool arguments, reporting a bad shape as a tool error
// rather than a protocol error so the model can correct itself. camelCase keys
// are accepted as aliases for their snake_case equivalents.
func decodeArgs(raw json.RawMessage, v any) *CallToolResult {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if hasUpper(raw) {
		if aliased, ok := withCamelAliases(raw); ok {
			raw = aliased
		}
	}
	if coerced, ok := coerceScalarStrings(raw, v); ok {
		raw = coerced
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	return nil
}

// coerceScalarStrings rewrites JSON string values into the number or boolean
// the destination field expects, for clients that stringify every argument
// ("strip":"1", "dry_run":"false"). Only fields the target struct declares as
// numeric or boolean are touched, so a genuine string argument that happens to
// look like a number is left alone. The bool reports whether raw was rewritten.
func coerceScalarStrings(raw json.RawMessage, v any) (json.RawMessage, bool) {
	if !bytes.Contains(raw, []byte(`"`)) {
		return nil, false
	}
	fields := scalarFieldKinds(v)
	if len(fields) == 0 {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, false
	}
	changed := false
	for key, val := range obj {
		kind, ok := fields[key]
		if !ok || len(val) == 0 || val[0] != '"' {
			continue
		}
		var str string
		if err := json.Unmarshal(val, &str); err != nil {
			continue
		}
		lit, ok := scalarLiteral(strings.TrimSpace(str), kind)
		if !ok {
			continue
		}
		obj[key] = json.RawMessage(lit)
		changed = true
	}
	if !changed {
		return nil, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return out, true
}

// scalarLiteral renders s as a JSON literal of the given kind, or reports that
// it is not a value of that kind and should be left as the string it is.
func scalarLiteral(s string, kind reflect.Kind) (string, bool) {
	switch kind {
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return "", false
		}
		return strconv.FormatBool(b), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if _, err := strconv.ParseInt(s, 10, 64); err != nil {
			return "", false
		}
		return s, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if _, err := strconv.ParseUint(s, 10, 64); err != nil {
			return "", false
		}
		return s, true
	case reflect.Float32, reflect.Float64:
		if _, err := strconv.ParseFloat(s, 64); err != nil {
			return "", false
		}
		return s, true
	}
	return "", false
}

// scalarFieldKinds maps the JSON names of v's numeric and boolean fields to
// their kinds. Pointer fields are reported as the kind they point at.
func scalarFieldKinds(v any) map[string]reflect.Kind {
	rt := reflect.TypeOf(v)
	for rt != nil && rt.Kind() == reflect.Pointer {
		rt = rt.Elem()
	}
	if rt == nil || rt.Kind() != reflect.Struct {
		return nil
	}
	out := map[string]reflect.Kind{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		switch ft.Kind() {
		case reflect.Bool,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Float32, reflect.Float64:
			out[name] = ft.Kind()
		}
	}
	return out
}

// withCamelAliases round-trips the payload to add the snake_case aliases. A
// failure here is not reported: the original bytes are then decoded as they
// stand, and any real problem with them surfaces as the usual decode error.
func withCamelAliases(raw json.RawMessage) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exact through the round trip
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, false
	}
	out, err := json.Marshal(aliasCamelKeys(generic))
	if err != nil {
		return nil, false
	}
	return out, true
}

// schema is a small helper for building JSON Schema 2020-12 object schemas.
func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object"}
	if len(props) == 0 {
		s["additionalProperties"] = false
		return s
	}
	s["properties"] = props
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func propDefault(typ, description string, def any) map[string]any {
	return map[string]any{"type": typ, "description": description, "default": def}
}

// identity returns the server name and version under the lock, since a config
// reload can replace both while requests are in flight.
func (s *Server) identity() (string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.name, s.version
}

// instructionText returns the current instructions under the lock.
func (s *Server) instructionText() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.instructions
}

// isLegacy reports whether the legacy revisions are served, under the lock.
func (s *Server) isLegacy() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.legacy
}

// sudoAgentRef returns the current sudo agent under the lock, so a command that
// starts while the configuration is being replaced uses one consistent agent.
func (s *Server) sudoAgentRef() *sudoAgent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sudo
}

// workspace returns the current workspace under the lock.
func (s *Server) workspace() *Workspace {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ws
}

// openAPI returns the OpenAPI tool server under the lock, or nil when the
// REST face is switched off.
func (s *Server) openAPI() *openAPIServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.api
}

// downloads returns the download server under the lock.
func (s *Server) downloads() *downloadServer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dl
}

// registerAll installs the tool set described by a configuration, replacing
// whatever was registered before. It is what startup calls, and what a config
// reload calls again.
func (s *Server) registerAll(cfg Config) {
	s.mu.Lock()
	s.tools = make(map[string]registeredTool)
	s.order = nil
	s.commandToolNames = nil
	s.unavailable = map[string]unavailableTool{}
	s.mu.Unlock()

	s.registerSystemTools(cfg)
	s.registerFileTools()
	s.registerEditTools()
	s.registerMarkdownTools()
	s.registerGrepTools()
	s.registerDefinitionTools()
	s.registerDiffTools()
	s.registerLineEndingTools(cfg.Workspace)
	s.registerCommandTools(cfg.Commands)
	s.registerShellTool()
	s.registerProcessTools()
	if cfg.Git.Enabled {
		s.registerGitTools(cfg.Git)
	}
	if cfg.GitHub.Enabled {
		s.registerGitHubTools(cfg.GitHub)
	}
	if s.db != nil {
		s.registerDatabaseTools()
	}
	if cfg.Downloads.Enabled && s.downloads() != nil {
		s.registerDownloadTools(cfg.Downloads)
	}
	s.registerIssueTool(cfg.Issues)
	s.markUnavailableTools(cfg)
}

// gitToolNames, githubToolNames and the rest are the tools each section of the
// configuration governs, so a call to one that is off can say which setting
// turned it off rather than coming back as an unknown name.
var (
	gitToolNames = []string{
		"git_status", "git_diff", "git_log", "git_branch", "git_add",
		"git_commit", "git_push", "git_show", "git_blame", "git_stash", "git_restore",
	}
	githubToolNames = []string{
		"github_workflows", "github_runs", "github_run_view", "github_run_logs",
		"github_run_watch", "github_workflow_run", "github_run_rerun", "github_run_cancel",
		"github_releases", "github_pr", "github_workflow_file",
	}
	databaseToolNames = []string{"db_query", "db_tables", "db_describe_table", "db_execute"}
	downloadToolNames = []string{"get_download_link", "list_download_links", "revoke_download_link"}
)

// markUnavailableTools records why each unregistered tool is missing. It runs
// after registration, so anything that did get registered is left alone and a
// user-defined command can still shadow a built-in name.
func (s *Server) markUnavailableTools(cfg Config) {
	mark := func(names []string, reason, instead string) {
		for _, name := range names {
			if _, registered := s.lookupTool(name); registered {
				continue
			}
			s.markUnavailable(name, reason, instead)
		}
	}

	if !cfg.Git.Enabled {
		mark(gitToolNames, "git.enabled is false",
			"Shelling out to git with run_command is blocked for the same reason. "+
				"Read and change files with the file tools instead.")
	} else {
		mark([]string{"git_commit"}, "git.allow_commit is false",
			"Stage the change with git_add and leave the commit to the user.")
		mark([]string{"git_push"}, "git.allow_push is false",
			"Commit locally and let the user push; on this repository a push is a deploy.")
		mark([]string{"git_restore"}, "git.allow_restore is false",
			"Undo the change with edit_file or apply_diff, or ask the user to restore the file.")
	}
	if !cfg.GitHub.Enabled {
		mark(githubToolNames, "github.enabled is false",
			"There is no other route to GitHub Actions from here.")
	}
	if s.db == nil {
		mark(databaseToolNames, "no database is configured (the database section of config.json is empty)",
			"There is no database to query on this server.")
	} else {
		mark([]string{"db_execute"}, "database.allow_write is false",
			"db_query still answers read-only questions.")
	}
	if !cfg.Downloads.Enabled || s.downloads() == nil {
		mark(downloadToolNames, "downloads.enabled is false",
			"Return the file's contents with read_file instead of a link.")
	}
	if !cfg.Workspace.AllowWrite {
		// These are registered, and each refuses on its own with the same
		// reason; the entry is here for a name that is called before the tool
		// list is read.
		mark([]string{"write_file", "edit_file", "multi_edit", "apply_diff", "fix_line_endings"},
			"workspace.allow_write is false", "This server is read-only.")
	}
}

// lookupTool reports whether a tool is registered.
func (s *Server) lookupTool(name string) (registeredTool, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tool, ok := s.tools[name]
	return tool, ok
}

// applyConfig installs a configuration on a running server: the workspace, the
// sudo agent, the instructions and the whole tool set are rebuilt from it.
//
// Not everything can be changed under a running process. The listener, its TLS
// material, the auth token, the tunnel and the database connection are all
// fixed when the server starts, so a change to those is reported and otherwise
// ignored rather than half-applied.
func (s *Server) applyConfig(cfg Config) error {
	agent, err := newSudoAgent(cfg.Sudo)
	if err != nil {
		return err
	}

	s.mu.Lock()
	previous := s.cfg
	old := s.sudo
	s.cfg = cfg
	s.name = cfg.Server.Name
	s.version = cfg.Server.Version
	s.legacy = cfg.Server.LegacyCompatibility
	s.baseInstructions = cfg.Server.Instructions
	s.sudo = agent
	s.ws = NewWorkspace(cfg.Workspace)
	ws := s.ws
	s.mu.Unlock()

	// The old agent's shim directory is removed only once nothing can still be
	// running against it.
	old.Close()

	s.setInstructions(cfg.Server.Instructions, ws, agent)
	s.registerAll(cfg)
	s.openAPI().update(cfg)
	s.warnUnappliable(previous, cfg)
	return nil
}

// setInstructions rebuilds the instruction text from the operator's own and the
// notes this server adds, so a reload replaces them instead of appending a
// second copy.
func (s *Server) setInstructions(base string, ws *Workspace, agent *sudoAgent) {
	parts := []string{base, currentPrivileges().note(), ws.ScopeNote(), ws.LineEndingNote(), agent.note()}
	var kept []string
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.instructions = strings.Join(kept, "\n\n")
}

// warnUnappliable names the sections that changed but cannot take effect until
// the process is restarted, so an operator is not left wondering why an edit
// did nothing.
func (s *Server) warnUnappliable(previous, next Config) {
	if s.logger == nil || previous.Server.Transport == "" {
		// Nothing was in effect before: this is the first apply, not a change.
		return
	}
	for _, section := range []struct {
		name       string
		old, fresh any
	}{
		{"server.url / transport / tls / auth_token", serverListenerConfig(previous.Server), serverListenerConfig(next.Server)},
		{"tunnel", previous.Tunnel, next.Tunnel},
		{"database", previous.Database, next.Database},
		{"downloads.addr / base_url", []string{previous.Downloads.Addr, previous.Downloads.BaseURL},
			[]string{next.Downloads.Addr, next.Downloads.BaseURL}},
		// The rest of the openapi section is re-read on every request; these
		// two decide the routes, which are fixed when the mux is built.
		{"openapi.enabled / path", []any{previous.OpenAPI.Enabled, previous.OpenAPI.Path},
			[]any{next.OpenAPI.Enabled, next.OpenAPI.Path}},
	} {
		if !sameJSON(section.old, section.fresh) {
			s.logger.Printf("config reload: %s changed, which needs a restart to take effect", section.name)
		}
	}
}

// serverListenerConfig is the part of the server section that is fixed once the
// listener is up.
func serverListenerConfig(cfg ServerConfig) []string {
	return append([]string{cfg.Transport, cfg.URL, cfg.AuthToken, cfg.TLSCertFile, cfg.TLSKeyFile},
		cfg.AdditionalURLs...)
}

func sameJSON(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(left, right)
}
