package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// downloadSegment is appended to the MCP endpoint path to form the download
// route, so a server on /mcp serves its temporary files from /mcp/files/.
// Sharing the endpoint's listener is deliberate: the port is already open,
// already reachable through the tunnel, and already the address a client knows.
const downloadSegment = "files"

// downloadLink is one file made reachable over HTTP for a bounded time. The
// absolute path is captured when the link is minted, so moving or renaming the
// file afterwards breaks the link rather than silently serving something else.
type downloadLink struct {
	token   string
	abs     string
	rel     string
	name    string
	size    int64
	created time.Time
	expires time.Time
}

// downloadServer mints expiring links to workspace files and serves them.
//
// It normally rides on the listeners the MCP endpoint already has: Routes says
// which paths the transport should hand to Handle. On stdio there is no such
// listener, so the first link opens one of its own instead.
type downloadServer struct {
	cfg    DownloadsConfig
	logger *log.Logger

	// base is the public URL prefix a link is built from, without a trailing
	// slash - "http://127.0.0.1:8765/mcp/files". Empty until the transport
	// reports where it is serving, or the standalone listener comes up.
	mu       sync.Mutex
	base     string
	links    map[string]*downloadLink
	srv      *http.Server
	addr     string
	closed   bool
	stopped  chan struct{}
	attached bool
}

func newDownloadServer(cfg DownloadsConfig, logger *log.Logger) *downloadServer {
	return &downloadServer{
		cfg:     cfg,
		logger:  logger,
		links:   make(map[string]*downloadLink),
		stopped: make(chan struct{}),
	}
}

// downloadRoute is the path an MCP endpoint path serves downloads on.
func downloadRoute(endpointPath string) string {
	return strings.TrimSuffix(endpointPath, "/") + "/" + downloadSegment + "/"
}

// Attach records that the transport is serving downloads for this server, and
// on which public URL. base is the MCP endpoint URL a client was given, so the
// links carry the same host, scheme and port the client already reaches.
func (d *downloadServer) Attach(endpointURL string) {
	if d == nil || endpointURL == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.attached = true
	if d.base == "" {
		d.base = strings.TrimSuffix(endpointURL, "/") + "/" + downloadSegment
	}
}

// Close drops every outstanding link and shuts the standalone listener down if
// one was started. The shared MCP listener is not ours to close.
func (d *downloadServer) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return
	}
	d.closed = true
	srv := d.srv
	d.srv = nil
	d.links = make(map[string]*downloadLink)
	close(d.stopped)
	d.mu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
}

// Add publishes abs - already resolved inside the workspace - for ttl and
// returns the link together with the URL to fetch it from.
func (d *downloadServer) Add(abs, rel string, size int64, ttl time.Duration) (*downloadLink, string, error) {
	if err := d.ensureServing(); err != nil {
		return nil, "", err
	}

	token, err := newDownloadToken()
	if err != nil {
		return nil, "", err
	}
	now := time.Now()
	link := &downloadLink{
		token:   token,
		abs:     abs,
		rel:     rel,
		name:    filepath.Base(abs),
		size:    size,
		created: now,
		expires: now.Add(ttl),
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, "", errors.New("the download server is shut down")
	}
	d.purgeLocked(now)
	if max := d.cfg.MaxLinks; max > 0 && len(d.links) >= max {
		// Drop the link closest to expiry rather than refusing outright: the
		// cap bounds memory, it is not meant to make the tool fail.
		d.evictSoonestLocked()
	}
	d.links[token] = link
	d.mu.Unlock()

	return link, d.URLFor(link), nil
}

// Revoke removes a link before it expires and reports whether one was found.
func (d *downloadServer) Revoke(token string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.links[token]; !ok {
		return false
	}
	delete(d.links, token)
	return true
}

// List returns the live links, soonest to expire first.
func (d *downloadServer) List() []*downloadLink {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.purgeLocked(time.Now())
	out := make([]*downloadLink, 0, len(d.links))
	for _, link := range d.links {
		out = append(out, link)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].expires.Before(out[j].expires) })
	return out
}

// URLFor builds the public URL of a link: the file name in the path so the
// browser saves it under the right name, and the token in the query, which is
// the only thing that actually authorises the download.
func (d *downloadServer) URLFor(link *downloadLink) string {
	d.mu.Lock()
	base := d.base
	d.mu.Unlock()
	return base + "/" + url.PathEscape(link.name) + "?token=" + link.token
}

// ensureServing makes sure there is something to serve the link. Attached to
// the MCP listeners there is nothing to do; on stdio the first link opens a
// listener of its own.
func (d *downloadServer) ensureServing() error {
	d.mu.Lock()
	switch {
	case d.closed:
		d.mu.Unlock()
		return errors.New("the download server is shut down")
	case d.attached, d.srv != nil:
		d.mu.Unlock()
		return nil
	}
	d.mu.Unlock()

	listener, err := net.Listen("tcp", d.cfg.Addr)
	if err != nil {
		return fmt.Errorf("download listener on %s: %w", d.cfg.Addr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(downloadRoute(""), d.Handle)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 15 * time.Second,
		ErrorLog:          d.logger,
	}

	d.mu.Lock()
	if d.closed || d.srv != nil || d.attached {
		// Closed, or someone else won the race, while the socket was opening.
		serving := d.srv != nil || d.attached
		d.mu.Unlock()
		_ = listener.Close()
		if serving {
			return nil
		}
		return errors.New("the download server is shut down")
	}
	d.srv = srv
	d.addr = listener.Addr().String()
	if d.base == "" {
		d.base = "http://" + reachableHost(d.addr) + "/" + downloadSegment
	}
	stopped := d.stopped
	d.mu.Unlock()

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) && d.logger != nil {
			d.logger.Printf("download server: %v", err)
		}
	}()
	go d.janitor(stopped)
	if d.logger != nil {
		d.logger.Printf("download server listening on %s", d.addr)
	}
	return nil
}

// janitor drops expired links so a long-running server does not hold paths to
// files it will never serve again.
func (d *downloadServer) janitor(stopped <-chan struct{}) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stopped:
			return
		case now := <-ticker.C:
			d.mu.Lock()
			d.purgeLocked(now)
			d.mu.Unlock()
		}
	}
}

func (d *downloadServer) purgeLocked(now time.Time) {
	for token, link := range d.links {
		if !now.Before(link.expires) {
			delete(d.links, token)
		}
	}
}

func (d *downloadServer) evictSoonestLocked() {
	var soonest string
	var at time.Time
	for token, link := range d.links {
		if soonest == "" || link.expires.Before(at) {
			soonest, at = token, link.expires
		}
	}
	if soonest != "" {
		delete(d.links, soonest)
	}
}

// lookup finds a live link by token, comparing in constant time so a near-miss
// cannot be told from a wrong guess by how long the answer took.
func (d *downloadServer) lookup(token string) *downloadLink {
	if token == "" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var found *downloadLink
	for candidate, link := range d.links {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(token)) == 1 {
			found = link
		}
	}
	if found == nil {
		return nil
	}
	if !time.Now().Before(found.expires) {
		delete(d.links, found.token)
		return nil
	}
	return found
}

// Handle serves one download. It is registered by the HTTP transport under the
// endpoint path, and answers without the bearer token the MCP endpoint
// requires: an unguessable link handed to a browser is the whole point, and a
// browser has nowhere to put an Authorization header.
//
// Everything about the response - which file, its name, its type - comes from
// the link, never from the request, so the URL carries no path to steer.
func (d *downloadServer) Handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	link := d.lookup(r.URL.Query().Get("token"))
	// The name in the path is part of the link, so a token pasted onto a
	// different file name is refused rather than served under the wrong name.
	if link != nil && !nameMatches(r.URL.Path, link.name) {
		link = nil
	}
	if link == nil {
		// One answer for an unknown token, an expired one and a mismatched
		// name: probing tokens learns nothing from the difference.
		http.Error(w, "this link has expired or does not exist", http.StatusNotFound)
		return
	}

	f, err := os.Open(link.abs)
	if err != nil {
		http.Error(w, "the file is no longer available", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "the file is no longer available", http.StatusNotFound)
		return
	}

	if ctype := mime.TypeByExtension(strings.ToLower(filepath.Ext(link.name))); ctype != "" {
		w.Header().Set("Content-Type", ctype)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(link.name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A link that lives for minutes must not outlive itself in a cache.
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, link.name, info.ModTime(), f)
}

// nameMatches reports whether the last segment of the request path is the
// file the link names.
func nameMatches(requestPath, name string) bool {
	last := requestPath
	if i := strings.LastIndex(last, "/"); i >= 0 {
		last = last[i+1:]
	}
	if decoded, err := url.PathUnescape(last); err == nil {
		last = decoded
	}
	return last == name
}

// newDownloadToken mints 128 bits of randomness in the shape of a UUID. The
// unguessable URL is the only credential a download has, so the token is
// generated from crypto/rand and never derived from the file it points at.
func newDownloadToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("could not generate a download token: %w", err)
	}
	buf[6] = buf[6]&0x0f | 0x40 // version 4
	buf[8] = buf[8]&0x3f | 0x80 // variant 1
	h := hex.EncodeToString(buf)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// reachableHost turns a listen address into one a client can dial. A wildcard
// bind answers on every interface, but "0.0.0.0:8080" is not an address anyone
// can fetch, so it is reported as loopback.
func reachableHost(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	switch host {
	case "", "0.0.0.0", "::":
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// downloadLinkResult is what the download tools report back.
type downloadLinkResult struct {
	URL          string `json:"url"`
	Path         string `json:"path"`
	FileName     string `json:"file_name"`
	SizeBytes    int64  `json:"size_bytes"`
	ExpiresAt    string `json:"expires_at"`
	ExpiresInMin int    `json:"expires_in_minutes"`
	Token        string `json:"token"`
}

// registerDownloadTools adds the temporary-hosting tools. They are registered
// only when the feature is enabled, so a locked-down deployment does not
// advertise a way to publish files at all.
func (s *Server) registerDownloadTools(cfg DownloadsConfig) {
	s.RegisterTool(Tool{
		Name:  "get_download_link",
		Title: "Get a temporary download link",
		Description: fmt.Sprintf(
			"Host a workspace file over HTTP at an unguessable URL and return the link. "+
				"The link expires after %d minutes by default (maximum %d) and stops working immediately afterwards. "+
				"Use it to hand a file to a person or a browser; use read_file to look at one yourself.",
			cfg.DefaultTTLMinutes, cfg.MaxTTLMinutes),
		Annotations: &ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schema([]string{"path"}, map[string]any{
			"path": prop("string", "File to host, relative to the workspace root."),
			"minutes": propDefault("number",
				fmt.Sprintf("How long the link stays valid, in minutes (maximum %d).", cfg.MaxTTLMinutes),
				cfg.DefaultTTLMinutes),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Path    string   `json:"path"`
			Minutes *float64 `json:"minutes"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.Path) == "" {
			return toolError("path is required"), nil
		}

		ws := s.workspace()
		abs, err := ws.Resolve(args.Path)
		if err != nil {
			return toolError("%v", err), nil
		}
		if ws.IsExcluded(abs) {
			return toolError("%s is excluded from the workspace", ws.Rel(abs)), nil
		}
		info, err := os.Stat(abs)
		if err != nil {
			return toolError("%v", err), nil
		}
		if info.IsDir() {
			return toolError("%s is a directory; only files can be hosted", ws.Rel(abs)), nil
		}
		if max := cfg.MaxFileBytes; max > 0 && info.Size() > max {
			return toolError("%s is %d bytes, over the %d byte limit for a download link",
				ws.Rel(abs), info.Size(), max), nil
		}

		minutes := float64(cfg.DefaultTTLMinutes)
		if args.Minutes != nil {
			minutes = *args.Minutes
		}
		if minutes <= 0 {
			return toolError("minutes must be greater than zero"), nil
		}
		if minutes > float64(cfg.MaxTTLMinutes) {
			return toolError("minutes is %g, over the configured maximum of %d",
				minutes, cfg.MaxTTLMinutes), nil
		}

		downloads := s.downloads()
		if downloads == nil {
			return toolError("temporary downloads are not enabled on this server"), nil
		}
		link, publicURL, err := downloads.Add(abs, ws.Rel(abs), info.Size(),
			time.Duration(minutes*float64(time.Minute)))
		if err != nil {
			return toolError("%v", err), nil
		}
		return toolResultJSON(downloadLinkResult{
			URL:          publicURL,
			Path:         link.rel,
			FileName:     link.name,
			SizeBytes:    link.size,
			ExpiresAt:    link.expires.Format(time.RFC3339),
			ExpiresInMin: int(minutes),
			Token:        link.token,
		}), nil
	})

	s.RegisterTool(Tool{
		Name:        "list_download_links",
		Title:       "List active download links",
		Description: "List the temporary download links that have not expired yet.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema(nil, nil),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		downloads := s.downloads()
		if downloads == nil {
			return toolError("temporary downloads are not enabled on this server"), nil
		}
		links := downloads.List()
		if len(links) == 0 {
			return toolResult("(no active download links)"), nil
		}
		now := time.Now()
		out := make([]downloadLinkResult, 0, len(links))
		for _, link := range links {
			out = append(out, downloadLinkResult{
				URL:          downloads.URLFor(link),
				Path:         link.rel,
				FileName:     link.name,
				SizeBytes:    link.size,
				ExpiresAt:    link.expires.Format(time.RFC3339),
				ExpiresInMin: int(link.expires.Sub(now).Round(time.Minute) / time.Minute),
				Token:        link.token,
			})
		}
		return toolResultJSON(out), nil
	})

	s.RegisterTool(Tool{
		Name:        "revoke_download_link",
		Title:       "Revoke a download link",
		Description: "Stop serving a download link before it expires, by the token get_download_link returned.",
		Annotations: &ToolAnnotations{IdempotentHint: true},
		InputSchema: schema([]string{"token"}, map[string]any{
			"token": prop("string", "The token from get_download_link, or the whole link URL."),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Token string `json:"token"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		downloads := s.downloads()
		if downloads == nil {
			return toolError("temporary downloads are not enabled on this server"), nil
		}
		token := tokenFromArg(args.Token)
		if token == "" {
			return toolError("token is required"), nil
		}
		if !downloads.Revoke(token) {
			return toolError("no active link with that token; it may have expired already"), nil
		}
		return toolResult("Revoked. The link no longer serves anything."), nil
	})
}

// tokenFromArg accepts either the bare token or the whole URL it appears in,
// since pasting back what the tool printed is the common case.
func tokenFromArg(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	if u, err := url.Parse(arg); err == nil && u.Query().Get("token") != "" {
		return u.Query().Get("token")
	}
	return path.Base(arg)
}
