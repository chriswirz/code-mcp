package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

// Backwards compatibility with the initialize-based ("legacy") protocol
// revisions, 2025-03-26 through 2025-11-25. Those revisions negotiate a version
// once in an initialize handshake and then carry it implicitly, on a session
// for Streamable HTTP and for the life of the process on stdio.
//
// The specification calls a server that serves both eras "dual-era", and says
// it selects its behaviour from how the client opens: a request carrying modern
// per-request _meta is served statelessly, while an initialize request selects
// legacy semantics for the session or process.

// InitializeParams are the params of a legacy initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      Implementation `json:"clientInfo"`
}

// InitializeResult answers a legacy initialize request. It deliberately does
// not embed Result: a legacy client knows nothing of resultType or cache hints.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// negotiateVersion picks the version to answer an initialize with. A version
// this server implements is echoed back; anything else gets the newest legacy
// version, which the legacy specification allows and which lets a client
// decide for itself whether it can proceed.
func negotiateVersion(requested string) string {
	if isLegacyVersion(requested) {
		return requested
	}
	// A client that asks for a modern version through an initialize handshake
	// is confused about the era, but the handshake itself is legacy, so it is
	// answered with the newest legacy version rather than a modern one.
	return LatestLegacyVersion
}

// Session is one legacy Streamable HTTP session, created by initialize and
// addressed afterwards by the Mcp-Session-Id header.
type Session struct {
	ID       string
	Version  string
	Client   Implementation
	Created  time.Time
	LastSeen time.Time
	// Recovered marks a session this server did not mint in this process: the
	// client presented an id from before a restart or an idle timeout, and it
	// was adopted rather than refused.
	Recovered bool
}

// SessionStore holds the legacy sessions. Modern requests never touch it:
// revision 2026-07-28 removed protocol-level sessions entirely.
type SessionStore struct {
	mu       sync.Mutex
	sessions map[string]*Session
	ttl      time.Duration
	// terminated remembers the ids a client explicitly ended, so recovery
	// does not undo a teardown the client asked for. Forgetting a session is
	// this server's failing to make good; ending one is the client's decision
	// to keep.
	terminated map[string]time.Time
}

// NewSessionStore builds an empty store. Sessions idle for longer than ttl are
// dropped, so a client that disappears without a DELETE does not leak one.
func NewSessionStore(ttl time.Duration) *SessionStore {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &SessionStore{
		sessions:   make(map[string]*Session),
		terminated: make(map[string]time.Time),
		ttl:        ttl,
	}
}

// Create mints a session for a completed initialize handshake.
func (s *SessionStore) Create(version string, client Implementation) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	now := time.Now()
	session := &Session{
		ID:       newSessionID(),
		Version:  version,
		Client:   client,
		Created:  now,
		LastSeen: now,
	}
	s.sessions[session.ID] = session
	return session
}

// Get returns a live session and marks it as used.
func (s *SessionStore) Get(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	session, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	session.LastSeen = time.Now()
	return session, true
}

// Delete terminates a session, as a legacy client's HTTP DELETE asks.
func (s *SessionStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	s.terminated[id] = time.Now()
	return true
}

// Count reports how many sessions are live, for the startup banner and tests.
func (s *SessionStore) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	return len(s.sessions)
}

func (s *SessionStore) evictLocked() {
	cutoff := time.Now().Add(-s.ttl)
	for id, session := range s.sessions {
		if session.LastSeen.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
	// A tombstone outlives the session it replaced by one ttl, which is long
	// past the point where a client would still be using the id.
	for id, at := range s.terminated {
		if at.Before(cutoff) {
			delete(s.terminated, id)
		}
	}
}

// newSessionID returns a globally unique, cryptographically random id, which is
// what the legacy specification requires of a session id.
func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to the clock rather
		// than handing out a predictable or empty id.
		return "mcp-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "mcp-" + hex.EncodeToString(buf[:])
}

// initialize answers a legacy initialize request.
func (s *Server) initialize(req *Request) (*InitializeResult, Implementation, string, *RPCError) {
	var params InitializeParams
	if err := req.Bind(&params); err != nil {
		return nil, Implementation{}, "", err
	}
	version := negotiateVersion(params.ProtocolVersion)
	name, serverVersion := s.identity()
	return &InitializeResult{
		ProtocolVersion: version,
		Capabilities:    s.legacyCapabilities(),
		ServerInfo:      Implementation{Name: name, Version: serverVersion},
		Instructions:    s.instructionText(),
	}, params.ClientInfo, version, nil
}

// legacyCapabilities is what a legacy client is told this server can do. It
// omits the extensions field, which only exists from 2026-07-28, and declares
// resource subscription support because the legacy methods are accepted.
func (s *Server) legacyCapabilities() ServerCapabilities {
	return ServerCapabilities{
		Tools:     &ToolsCapability{},
		Resources: &ResourcesCapability{Subscribe: true},
		Prompts:   &PromptsCapability{},
	}
}

// emptyResult is the {} that several legacy methods answer with.
var emptyResult = json.RawMessage(`{}`)

// Adopt takes a session id a client is still using and makes it live again.
//
// A session here holds nothing: an id, the version that was negotiated, who
// the client said it was, and two timestamps. Revision 2026-07-28 removed
// protocol sessions precisely because they carried no state worth keeping, and
// this server's legacy sessions carry none either - no workspace position, no
// open handles, no authorisation, which is the bearer token's job and is
// checked on every request regardless.
//
// So a client whose session the server has forgotten - because the process
// restarted, or because it sat idle past the timeout - has lost nothing except
// a row in a map. Refusing it costs a working conversation to protect
// bookkeeping. Adopting the id costs nothing and the conversation continues.
func (s *SessionStore) Adopt(id, version string, client Implementation) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	if existing, ok := s.sessions[id]; ok {
		existing.LastSeen = time.Now()
		return existing
	}
	if _, ended := s.terminated[id]; ended {
		// The client ended this one itself. Bringing it back would undo a
		// decision rather than repair an accident.
		return nil
	}
	if !validSessionID(id) {
		// Not an id this server would have minted: give the client a fresh
		// one rather than storing whatever arrived in the header.
		id = newSessionID()
	}
	now := time.Now()
	session := &Session{
		ID:        id,
		Version:   version,
		Client:    client,
		Created:   now,
		LastSeen:  now,
		Recovered: true,
	}
	s.sessions[id] = session
	return session
}

// validSessionID reports whether an id is one this server could have issued:
// the specification's visible ASCII, and short enough not to be a payload.
func validSessionID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}
