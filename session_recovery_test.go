package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The specification says a server answers 404 for a session it does not know
// and the client starts a new one. That is the default here, and these tests
// hold it in place. server.session_recovery is for the client that does not do
// its half and leaves its user with a dead conversation: the id is adopted
// instead, which costs nothing, because a legacy session here holds an id, a
// version, who the client said it was and two timestamps and nothing else.
//
// The exception, which recovery does not touch: a session the client itself
// ended stays ended.

// legacyServer builds a server and its HTTP handler with legacy sessions on.
func legacyServer(t *testing.T, adjust func(*Config)) http.Handler {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.Workspace.Root = root
	cfg.Server.Transport = "http"
	if adjust != nil {
		adjust(&cfg)
	}
	if err := cfg.Normalize(root); err != nil {
		t.Fatal(err)
	}
	s := NewServer(cfg.Server.Name, "test", cfg.Server.Instructions, cfg.Server.LegacyCompatibility)
	s.cfg = cfg
	s.ws = NewWorkspace(cfg.Workspace)
	s.registerAll(cfg)
	return NewHTTPTransport(s, cfg.Server, "/mcp", nil).HandlerFor("/mcp")
}

// legacyPost sends one legacy request, optionally carrying a session id.
func legacyPost(t *testing.T, h http.Handler, session, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")
	if session != "" {
		req.Header.Set(sessionHeader, session)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const toolsListRequest = `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`

// TestForgottenSessionIsRefusedByDefault: with nothing configured, the answer
// is the specification's, and it says how to recover.
func TestForgottenSessionIsRefusedByDefault(t *testing.T) {
	h := legacyServer(t, nil)

	rec := legacyPost(t, h, "mcp-from-before-the-restart", toolsListRequest)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 unless recovery is asked for", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "send initialize") {
		t.Errorf("the 404 should say how to recover: %s", rec.Body.String())
	}
}

// TestForgottenSessionIsAdoptedWhenAsked is the failure the setting exists
// for: the client is still holding a session id from before a restart, and the
// conversation continues instead of ending.
func TestForgottenSessionIsAdoptedWhenAsked(t *testing.T) {
	h := legacyServer(t, withRecovery)

	rec := legacyPost(t, h, "mcp-a-session-this-process-never-minted", toolsListRequest)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if echoed := rec.Header().Get(sessionHeader); echoed != "mcp-a-session-this-process-never-minted" {
		t.Errorf("the client's own id should come back, got %q", echoed)
	}
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("body is not JSON: %s", rec.Body.String())
	}
	if _, isError := response["error"]; isError {
		t.Errorf("the request should have been answered: %s", rec.Body.String())
	}
	if result, _ := response["result"].(map[string]any); result["tools"] == nil {
		t.Errorf("the tool list should have come back: %s", rec.Body.String())
	}
}

// withRecovery turns the setting on, for the tests that are about it.
func withRecovery(c *Config) { c.Server.SessionRecovery = true }

// TestTerminatedSessionStaysTerminated is the exception. Forgetting a session
// is this server's failing to make good; ending one is the client's decision,
// and recovery must not undo it.
func TestTerminatedSessionStaysTerminated(t *testing.T) {
	h := legacyServer(t, withRecovery)

	// Establish a session the way a legacy client does.
	rec := legacyPost(t, h, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize failed: %d %s", rec.Code, rec.Body.String())
	}
	id := rec.Header().Get(sessionHeader)
	if id == "" {
		t.Fatal("initialize minted no session")
	}
	if got := legacyPost(t, h, id, toolsListRequest); got.Code != http.StatusOK {
		t.Fatalf("the live session should work: %d", got.Code)
	}

	// The client ends it.
	del := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	del.Header.Set(sessionHeader, id)
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, del)
	if delRec.Code != http.StatusOK && delRec.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d", delRec.Code)
	}

	after := legacyPost(t, h, id, toolsListRequest)
	if after.Code != http.StatusNotFound {
		t.Errorf("a session the client ended should stay ended, got %d: %s", after.Code, after.Body.String())
	}
	if !strings.Contains(after.Body.String(), "terminated by this client") {
		t.Errorf("the answer should say why: %s", after.Body.String())
	}
}

// TestReconnectedStreamIsAdopted covers the shape this actually fails in: a
// long-lived SSE stream drops and the client comes back with the id it has.
func TestReconnectedStreamIsAdopted(t *testing.T) {
	h := legacyServer(t, withRecovery)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(sessionHeader, "mcp-a-stream-from-before-the-restart")
	// The stream is kept open until the client goes away, so the request is
	// given a context that is already done: the handler opens the stream,
	// sees the caller gone and returns.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatalf("a reconnecting stream should not be refused: %s", rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Errorf("the stream should have been opened, content-type = %q", got)
	}
}

func TestAdoptedSessionKeepsItsVersion(t *testing.T) {
	store := NewSessionStore(0)

	session := store.Adopt("mcp-abc", "2025-06-18", Implementation{Name: "openwebui"})
	if session == nil || session.Version != "2025-06-18" || session.ID != "mcp-abc" {
		t.Fatalf("adopted session = %+v", session)
	}
	if !session.Recovered {
		t.Error("an adopted session should be marked as one this process did not mint")
	}
	// Adopting twice is the same session, not two.
	again := store.Adopt("mcp-abc", "2025-06-18", Implementation{})
	if again.Created != session.Created {
		t.Error("the second adoption should find the first")
	}
	if store.Count() != 1 {
		t.Errorf("count = %d, want 1", store.Count())
	}
	if _, ok := store.Get("mcp-abc"); !ok {
		t.Error("an adopted session should be live")
	}
}

// TestAdoptRefusesAnUnusableID: an id that this server could not have issued
// is not stored; the client is given one that it could have.
func TestAdoptRefusesAnUnusableID(t *testing.T) {
	store := NewSessionStore(0)

	session := store.Adopt("a session id with spaces and\ttabs", "2025-06-18", Implementation{})
	if session == nil {
		t.Fatal("a fresh session should have been minted instead")
	}
	if strings.ContainsAny(session.ID, " \t") {
		t.Errorf("the minted id should be a proper one, got %q", session.ID)
	}
	if !validSessionID(session.ID) {
		t.Errorf("the minted id should be valid, got %q", session.ID)
	}
}

func TestValidSessionID(t *testing.T) {
	for _, id := range []string{"mcp-abc123", "1868a90c-1234", "~!@#$%^&*()"} {
		if !validSessionID(id) {
			t.Errorf("validSessionID(%q) = false", id)
		}
	}
	for _, id := range []string{"", "has space", "tab\there", "new\nline", strings.Repeat("x", 129)} {
		if validSessionID(id) {
			t.Errorf("validSessionID(%q) = true", id)
		}
	}
}

// TestRecoveryIsOffUnlessAskedFor: an absent setting means the specification's
// behaviour, not this server's opinion of it.
func TestRecoveryIsOffUnlessAskedFor(t *testing.T) {
	if DefaultConfig().Server.SessionRecovery {
		t.Error("recovery should be off unless it is turned on")
	}
	var absent Config
	if err := json.Unmarshal([]byte(`{"server":{"url":"http://127.0.0.1:8765/mcp"}}`), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.Server.SessionRecovery {
		t.Error("a config that does not mention it should leave it off")
	}
}
