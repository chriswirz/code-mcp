package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// sessionHeader is the legacy session identifier. Revision 2026-07-28 removed
// it; earlier revisions mint it during initialize and send it on every request.
const sessionHeader = "Mcp-Session-Id"

// HTTPTransport serves the Streamable HTTP transport. The 2026-07-28 shape is a
// single POST endpoint with no sessions, no GET stream and no resumability;
// when legacy compatibility is on it also serves the 2025-03-26 through
// 2025-11-25 shape, which negotiates through initialize and carries a session.
type HTTPTransport struct {
	server              *Server
	path                string
	allowedOrigins      []string
	allowedHeaders      []string
	allowPrivateNetwork bool
	authToken           string
	legacy              bool
	sessions            *SessionStore
	logger              *log.Logger
}

// NewHTTPTransport builds the transport for one server.
func NewHTTPTransport(s *Server, cfg ServerConfig, path string, logger *log.Logger) *HTTPTransport {
	return &HTTPTransport{
		server:              s,
		path:                path,
		allowedOrigins:      cfg.AllowedOrigins,
		allowedHeaders:      cfg.AllowedHeaders,
		allowPrivateNetwork: cfg.AllowPrivateNetwork,
		authToken:           cfg.AuthToken,
		legacy:              cfg.LegacyCompatibility,
		sessions:            NewSessionStore(time.Duration(cfg.SessionTimeoutSeconds) * time.Second),
		logger:              logger,
	}
}

// Handler returns the mux serving the MCP endpoint, wrapped in the CORS and
// origin checks so that every response - including the 404 and 405 ones -
// carries the headers a browser needs.
func (t *HTTPTransport) Handler() http.Handler {
	return t.HandlerFor(t.path)
}

// HandlerFor builds the handler for one listener, which may answer on several
// paths when more than one configured URL shares its address.
func (t *HTTPTransport) HandlerFor(paths ...string) http.Handler {
	mux := http.NewServeMux()
	hasRoot := false
	for _, path := range paths {
		mux.HandleFunc(path, t.handle)
		// Temporary download links ride on this listener, one route per
		// endpoint path. They are registered outside t.handle on purpose: a
		// link is fetched by a browser, which has nowhere to put the bearer
		// token the MCP endpoint requires, and the unguessable token in the
		// URL is what authorises it instead.
		if dl := t.server.downloads(); dl != nil {
			mux.HandleFunc(downloadRoute(path), dl.Handle)
		}
		if path == "/" {
			hasRoot = true
		}
	}
	if !hasRoot {
		served := strings.Join(paths, ", ")
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "the MCP endpoint is "+served, http.StatusNotFound)
		})
	}
	return t.withCORS(mux)
}

// corsAllowHeaders is the fallback set advertised on a preflight that does not
// say which headers it wants. It covers the request metadata this revision
// mirrors into headers, plus authentication.
const corsAllowHeaders = "Content-Type, Authorization, Accept, MCP-Protocol-Version, " +
	"Mcp-Method, Mcp-Name, Mcp-Session-Id, Last-Event-ID"

// withCORS validates the request origin and answers preflights. An Origin the
// configuration does not allow is refused outright: that check is the DNS
// rebinding defence the specification requires, and it has to happen before
// anything else looks at the request.
func (t *HTTPTransport) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !t.originAllowed(origin) {
			// No CORS headers on this path, so the browser blocks it too.
			writeJSONRPCError(w, http.StatusForbidden, nil,
				Errorf(CodeInvalidRequest, "origin %q is not allowed", origin))
			return
		}
		t.writeCORSHeaders(w, origin)

		if r.Method == http.MethodOptions {
			// A preflight carries no credentials and no body, so it is answered
			// here rather than falling through to the auth and method checks.
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Headers", t.allowHeadersFor(r.Header.Get("Access-Control-Request-Headers")))
			// Chrome's Private Network Access check: a page on a public address
			// reaching a private one (this server on 127.0.0.1) must be granted
			// permission explicitly, or the request never leaves the browser.
			// Only an already-allowed origin gets this far, so the grant is
			// bounded by allowed_origins.
			if t.allowPrivateNetwork && r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				h.Set("Access-Control-Allow-Private-Network", "true")
			}
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// writeCORSHeaders sets the headers shared by preflights and real responses.
func (t *HTTPTransport) writeCORSHeaders(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Add("Vary", "Origin")
	switch {
	case origin == "":
		// A non-browser client sends no Origin and needs no CORS headers, but
		// advertising the wildcard costs nothing and keeps probes honest.
		if t.allowAnyOrigin() {
			h.Set("Access-Control-Allow-Origin", "*")
		}
	case t.allowAnyOrigin():
		// With a wildcard configured the origin is echoed rather than starred,
		// so the response stays usable from a client that sets withCredentials;
		// credentials themselves are not granted, since any origin qualifies.
		h.Set("Access-Control-Allow-Origin", origin)
	default:
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	// A browser can only read response headers named here. Mcp-Session-Id has
	// to be among them or a legacy client in a page cannot pick up the session
	// its initialize just created, and every later request loses it.
	h.Set("Access-Control-Expose-Headers",
		strings.Join([]string{sessionHeader, "MCP-Protocol-Version", "Mcp-Method", "Mcp-Name", "WWW-Authenticate"}, ", "))
}

// allowHeadersFor decides what a preflight is told it may send. requested is
// the browser's Access-Control-Request-Headers, which may be empty.
//
// The default - an unset allowed_headers - echoes what the browser asked for.
// That is permissive, and deliberately so: a tool may mirror arguments into
// Mcp-Param-* headers whose names this server cannot know in advance, so a
// fixed list would silently break them. Naming headers in the configuration
// restricts the answer to that list; naming "*" allows anything.
func (t *HTTPTransport) allowHeadersFor(requested string) string {
	if t.allowAnyHeader() {
		// A literal "*" is ignored by browsers on a credentialed request, so
		// the concrete echo is preferred whenever there is one to give.
		if requested != "" {
			return requested
		}
		return "*"
	}
	if len(t.allowedHeaders) > 0 {
		return strings.Join(t.allowedHeaders, ", ")
	}
	if requested != "" {
		return requested
	}
	return corsAllowHeaders
}

// allowAnyHeader reports whether the configuration allows any request header.
func (t *HTTPTransport) allowAnyHeader() bool {
	for _, allowed := range t.allowedHeaders {
		if allowed == "*" {
			return true
		}
	}
	return false
}

// allowAnyOrigin reports whether the configuration contains the wildcard.
func (t *HTTPTransport) allowAnyOrigin() bool {
	for _, allowed := range t.allowedOrigins {
		if allowed == "*" {
			return true
		}
	}
	return false
}

func (t *HTTPTransport) handle(w http.ResponseWriter, r *http.Request) {
	if t.authToken != "" {
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Authorization")), "bearer "+t.authToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			writeJSONRPCError(w, http.StatusUnauthorized, nil, Errorf(CodeInvalidRequest, "authentication required"))
			return
		}
	}
	// Revision 2026-07-28 removed the GET stream and the DELETE session
	// teardown, so both are 405 unless legacy compatibility is serving an older
	// client that still expects them.
	if r.Method != http.MethodPost {
		if t.legacy && t.serveLegacyNonPost(w, r) {
			return
		}
		w.Header().Set("Allow", http.MethodPost)
		writeJSONRPCError(w, http.StatusMethodNotAllowed, nil,
			Errorf(CodeInvalidRequest, "the MCP endpoint accepts POST only in protocol version "+ProtocolVersion))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, Errorf(CodeParseError, "could not read the request body: %v", err))
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, Errorf(CodeParseError, "invalid JSON: %v", err))
		return
	}

	// Which era this request belongs to decides everything that follows: what
	// must be validated, what the result envelope looks like, and whether a
	// session is involved.
	version, session, rpcErr := t.resolveVersion(w, r, &req)
	if rpcErr != nil {
		return
	}
	ctx := withVersion(r.Context(), version)

	if req.IsNotification() {
		// Notifications carry no id, so there is nothing to respond with.
		t.server.Handle(ctx, &req)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	// The legacy handshake is the transport's business: it is what mints the
	// session that subsequent requests are carried on.
	if req.Method == "initialize" {
		t.serveInitialize(w, &req)
		return
	}

	if isModernVersion(version) {
		// The mirrored request headers arrived with 2026-07-28; a legacy client
		// neither sends them nor should be judged against them.
		if rpcErr := t.validateHeaders(r, &req); rpcErr != nil {
			writeJSONRPCError(w, http.StatusBadRequest, req.ID, rpcErr)
			return
		}
	}
	if req.Method != "server/discover" && !t.server.supports(version) {
		writeJSONRPCError(w, http.StatusBadRequest, req.ID, UnsupportedVersionError(version))
		return
	}
	if session != nil {
		// Echo the session id so a legacy client can confirm which session
		// answered, as the earlier revisions expect.
		w.Header().Set(sessionHeader, session.ID)
	}

	if req.Method == "subscriptions/listen" {
		if !isModernVersion(version) {
			writeJSONRPCError(w, http.StatusNotFound, req.ID, Errorf(CodeMethodNotFound,
				"subscriptions/listen was introduced in protocol version %s; open the GET stream instead", ProtocolVersion))
			return
		}
		t.serveListen(w, r, &req)
		return
	}

	result, rpcErr := t.server.Handle(ctx, &req)
	if rpcErr != nil {
		status := http.StatusOK
		switch rpcErr.Code {
		case CodeMethodNotFound:
			// The spec distinguishes an unknown MCP method from a legacy 404.
			status = http.StatusNotFound
		case CodeUnsupportedProtocolVersion, CodeHeaderMismatch, CodeMissingRequiredClientCapability:
			status = http.StatusBadRequest
		}
		writeJSONRPCError(w, status, req.ID, rpcErr)
		return
	}
	writeJSON(w, http.StatusOK, newResponse(req.ID, result))
}

// resolveVersion works out which protocol version answers this request, and
// which session it belongs to if any. It writes the response itself and returns
// an error only when the request cannot be served at all.
//
// The specification's rule for a dual-era server is that it selects behaviour
// from how the client opens: per-request _meta means modern, an initialize
// handshake means legacy. Everything below is that rule plus the fallbacks a
// real client needs.
func (t *HTTPTransport) resolveVersion(w http.ResponseWriter, r *http.Request, req *Request) (string, *Session, *RPCError) {
	// A version declared in _meta is the modern shape and is authoritative,
	// whether or not it names a modern revision.
	if declared := req.MetaString(MetaProtocolVersion); declared != "" {
		return declared, nil, nil
	}
	if !t.legacy {
		// Nothing else identifies a version, so let the modern path reject it
		// with the header and version errors it already produces.
		return r.Header.Get("MCP-Protocol-Version"), nil, nil
	}

	// initialize is the start of a legacy handshake; its own params name the
	// version, and serveInitialize negotiates it.
	if req.Method == "initialize" {
		return LatestLegacyVersion, nil, nil
	}

	// An established session carries the version that was negotiated for it.
	if id := r.Header.Get(sessionHeader); id != "" {
		session, ok := t.sessions.Get(id)
		if !ok {
			// The legacy specification has the server answer 404 for a session
			// it does not know, which tells the client to start a new one.
			writeJSONRPCError(w, http.StatusNotFound, req.ID,
				Errorf(CodeInvalidRequest, "unknown or expired session %q; send initialize to start a new one", id))
			return "", nil, Errorf(CodeInvalidRequest, "unknown session")
		}
		return session.Version, session, nil
	}

	// 2025-06-18 and later send the version in a header even without a session;
	// 2025-03-26 sends nothing at all, and is assumed by the specification.
	header := r.Header.Get("MCP-Protocol-Version")
	if isLegacyVersion(header) {
		return header, nil, nil
	}
	if header == "" && !hasModernMetadata(req) {
		return "2025-03-26", nil, nil
	}
	return header, nil, nil
}

// hasModernMetadata reports whether the request carries the per-request
// metadata that identifies a modern client, which is what distinguishes it from
// a 2025-03-26 client that sends no version at all.
func hasModernMetadata(req *Request) bool {
	meta := req.Meta()
	if meta == nil {
		return false
	}
	for _, key := range []string{MetaProtocolVersion, MetaClientInfo, MetaClientCapabilities} {
		if _, ok := meta[key]; ok {
			return true
		}
	}
	return false
}

// serveInitialize answers the legacy handshake and mints a session.
func (t *HTTPTransport) serveInitialize(w http.ResponseWriter, req *Request) {
	if !t.legacy {
		writeJSONRPCError(w, http.StatusNotFound, req.ID, Errorf(CodeMethodNotFound,
			"initialize was removed in protocol version %s, and legacy compatibility is disabled on this server; "+
				"supported versions: %s", ProtocolVersion, strings.Join(SupportedVersions, ", ")))
		return
	}
	result, client, version, rpcErr := t.server.initialize(req)
	if rpcErr != nil {
		writeJSONRPCError(w, http.StatusBadRequest, req.ID, rpcErr)
		return
	}
	session := t.sessions.Create(version, client)
	w.Header().Set(sessionHeader, session.ID)
	writeJSON(w, http.StatusOK, newResponse(req.ID, result))
}

// serveLegacyNonPost answers the GET and DELETE requests the earlier revisions
// define. It reports whether it handled the request.
func (t *HTTPTransport) serveLegacyNonPost(w http.ResponseWriter, r *http.Request) bool {
	id := r.Header.Get(sessionHeader)
	switch r.Method {
	case http.MethodDelete:
		// An explicit session teardown.
		if id == "" {
			return false
		}
		if !t.sessions.Delete(id) {
			http.Error(w, "unknown session", http.StatusNotFound)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true

	case http.MethodGet:
		// The standalone SSE stream on which a legacy server could push
		// requests and notifications. This server has nothing to push, so the
		// stream is opened and kept alive: a client that waits on it is not
		// left with a failed connection, and one that does not is unaffected.
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			return false
		}
		if id != "" {
			if _, ok := t.sessions.Get(id); !ok {
				http.Error(w, "unknown session", http.StatusNotFound)
				return true
			}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if id != "" {
			w.Header().Set(sessionHeader, id)
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		keepAlive(r.Context(), w, flusher)
		return true
	}
	return false
}

// keepAlive holds an SSE stream open with periodic comment lines until the
// client goes away. A line beginning with a colon is a comment the client must
// ignore, which is what makes it a safe heartbeat.
func keepAlive(ctx context.Context, w io.Writer, flusher http.Flusher) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ":\r\n")
			flusher.Flush()
		}
	}
}

// validateHeaders enforces the request-metadata mirroring this revision
// requires: the headers an intermediary routes on must match the body.
func (t *HTTPTransport) validateHeaders(r *http.Request, req *Request) *RPCError {
	pv := r.Header.Get("MCP-Protocol-Version")
	if pv == "" {
		return Errorf(CodeHeaderMismatch, "the MCP-Protocol-Version header is required")
	}
	if declared := req.MetaString(MetaProtocolVersion); declared != "" && declared != pv {
		return Errorf(CodeHeaderMismatch,
			"MCP-Protocol-Version header %q does not match the body value %q", pv, declared)
	}
	method := r.Header.Get("Mcp-Method")
	if method == "" {
		return Errorf(CodeHeaderMismatch, "the Mcp-Method header is required")
	}
	if method != req.Method {
		return Errorf(CodeHeaderMismatch, "Mcp-Method header %q does not match the body method %q", method, req.Method)
	}

	// Mcp-Name mirrors params.name or params.uri, for the three methods that
	// address a specific tool, resource or prompt.
	var wantName string
	switch req.Method {
	case "tools/call", "prompts/get":
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		wantName = p.Name
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		_ = json.Unmarshal(req.Params, &p)
		wantName = p.URI
	default:
		return nil
	}
	raw := r.Header.Get("Mcp-Name")
	if raw == "" {
		return Errorf(CodeHeaderMismatch, "the Mcp-Name header is required for %s", req.Method)
	}
	got, err := decodeHeaderValue(raw)
	if err != nil {
		return Errorf(CodeHeaderMismatch, "Mcp-Name: %v", err)
	}
	if got != wantName {
		return Errorf(CodeHeaderMismatch, "Mcp-Name header %q does not match the body value %q", got, wantName)
	}
	if req.Method == "tools/call" {
		return t.validateParamHeaders(r, req)
	}
	return nil
}

// validateParamHeaders checks the Mcp-Param-* headers a client mirrors from
// tool arguments annotated with x-mcp-header.
func (t *HTTPTransport) validateParamHeaders(r *http.Request, req *Request) *RPCError {
	var p CallToolParams
	if json.Unmarshal(req.Params, &p) != nil {
		return nil
	}
	t.server.mu.RLock()
	tool, ok := t.server.tools[p.Name]
	t.server.mu.RUnlock()
	if !ok {
		return nil
	}
	annotated := headerAnnotatedProps(tool.def.InputSchema, nil)
	if len(annotated) == 0 {
		return nil
	}
	var args map[string]any
	if len(p.Arguments) > 0 {
		_ = json.Unmarshal(p.Arguments, &args)
	}
	for headerName, path := range annotated {
		value, present := lookupPath(args, path)
		header := r.Header.Get("Mcp-Param-" + headerName)
		if !present || value == nil {
			if header != "" {
				return Errorf(CodeHeaderMismatch, "Mcp-Param-%s was sent but %s is not in the arguments",
					headerName, strings.Join(path, "."))
			}
			continue
		}
		if header == "" {
			return Errorf(CodeHeaderMismatch, "the Mcp-Param-%s header is required because %s is in the arguments",
				headerName, strings.Join(path, "."))
		}
		decoded, err := decodeHeaderValue(header)
		if err != nil {
			return Errorf(CodeHeaderMismatch, "Mcp-Param-%s: %v", headerName, err)
		}
		if !headerValueMatches(decoded, value) {
			return Errorf(CodeHeaderMismatch, "Mcp-Param-%s header %q does not match the argument value",
				headerName, decoded)
		}
	}
	return nil
}

// headerAnnotatedProps collects x-mcp-header annotations that are statically
// reachable through properties chains, which is the only place the spec allows
// them.
func headerAnnotatedProps(schema map[string]any, prefix []string) map[string][]string {
	out := map[string][]string{}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return out
	}
	for name, rawProp := range props {
		p, ok := rawProp.(map[string]any)
		if !ok {
			continue
		}
		path := append(append([]string(nil), prefix...), name)
		if header, ok := p["x-mcp-header"].(string); ok && header != "" {
			out[header] = path
		}
		for nested, nestedPath := range headerAnnotatedProps(p, path) {
			out[nested] = nestedPath
		}
	}
	return out
}

func lookupPath(args map[string]any, path []string) (any, bool) {
	var current any = args
	for _, step := range path {
		m, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = m[step]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func headerValueMatches(header string, value any) bool {
	switch typed := value.(type) {
	case string:
		return header == typed
	case bool:
		return header == strconv.FormatBool(typed)
	case float64:
		// Integers are compared numerically, so 42 and 42.0 agree.
		got, err := strconv.ParseFloat(header, 64)
		return err == nil && got == typed
	default:
		return header == fmt.Sprint(value)
	}
}

// decodeHeaderValue unwraps the =?base64?...?= sentinel the spec defines for
// values that cannot be carried as plain ASCII.
func decodeHeaderValue(value string) (string, error) {
	const prefix, suffix = "=?base64?", "?="
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
		return value, nil
	}
	payload := value[len(prefix) : len(value)-len(suffix)]
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("malformed base64 sentinel value")
	}
	return string(decoded), nil
}

// serveListen answers subscriptions/listen with a long-lived SSE stream. This
// server publishes no list-changed notifications, so the stream acknowledges
// the subscription and then only keeps itself alive; it exists so clients that
// open one are not left hanging.
func (t *HTTPTransport) serveListen(w http.ResponseWriter, r *http.Request, req *Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONRPCError(w, http.StatusInternalServerError, req.ID, Errorf(CodeInternalError, "streaming is not supported"))
		return
	}
	var params SubscribeParams
	_ = json.Unmarshal(req.Params, &params)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	subscriptionID := fmt.Sprintf("sub-%d", time.Now().UnixNano())
	writeSSE(w, Notification{
		JSONRPC: "2.0",
		Method:  "notifications/subscriptions/acknowledged",
		Params: map[string]any{
			"_meta": map[string]any{MetaSubscriptionID: subscriptionID},
		},
	})
	flusher.Flush()

	// An SSE comment keeps intermediaries from closing a quiet stream.
	keepAlive(r.Context(), w, flusher)
}

func writeSSE(w io.Writer, message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, rpcErr *RPCError) {
	writeJSON(w, status, newErrorResponse(id, rpcErr))
}

func (t *HTTPTransport) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, allowed := range t.allowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
		a, err := url.Parse(allowed)
		if err != nil {
			continue
		}
		// An entry without a port matches any port on that host, which is what
		// makes "http://localhost" a useful thing to configure.
		if a.Scheme == u.Scheme && a.Hostname() == u.Hostname() &&
			(a.Port() == "" || a.Port() == u.Port()) {
			return true
		}
	}
	return false
}

// Serve runs the HTTP server until ctx is cancelled. When tls is non-nil the
// listener is wrapped, which is what an https:// endpoint URL asks for.
func (t *HTTPTransport) Serve(ctx context.Context, addr string, tlsConfig *tls.Config) error {
	return t.ServeOne(ctx, Listener{Addr: addr, Paths: []string{t.path}, TLS: tlsConfig != nil}, tlsConfig)
}

// ServeAll runs every configured listener at once and returns when the first
// one fails or ctx is cancelled. Serving an http and an https endpoint side by
// side is the usual reason to have more than one.
func (t *HTTPTransport) ServeAll(ctx context.Context, listeners []Listener, tlsConfig *tls.Config) error {
	if len(listeners) == 0 {
		return errors.New("no listeners configured")
	}
	// A failure on any listener takes the others down with it: a server that is
	// half up is worse than one that refuses to start, because a client
	// connecting to the surviving half has no way to know.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errs := make(chan error, len(listeners))
	for _, l := range listeners {
		go func(l Listener) {
			err := t.ServeOne(ctx, l, tlsConfig)
			if err != nil {
				err = fmt.Errorf("%s: %w", l.Addr, err)
			}
			errs <- err
		}(l)
	}

	var first error
	for range listeners {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// ServeOne runs a single listener until ctx is cancelled. When the listener is
// a TLS one the socket is wrapped, which is what an https:// URL asks for.
func (t *HTTPTransport) ServeOne(ctx context.Context, l Listener, tlsConfig *tls.Config) error {
	listener, err := net.Listen("tcp", l.Addr)
	if err != nil {
		return err
	}
	if l.TLS {
		if tlsConfig == nil {
			return fmt.Errorf("%s is an https endpoint but no certificate is configured", l.Addr)
		}
		listener = tls.NewListener(listener, tlsConfig)
	}
	srv := &http.Server{
		Handler:           t.HandlerFor(l.Paths...),
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          t.logger,
		// net/http would otherwise answer a server-wide "OPTIONS *" itself,
		// with a bare 200 and no CORS headers. Let it reach our handler so a
		// browser probing the origin gets a real preflight response.
		DisableGeneralOptionsHandler: true,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
