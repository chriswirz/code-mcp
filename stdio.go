package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
)

// StdioTransport speaks newline-delimited JSON-RPC on stdin and stdout, which
// is how an MCP client that launches the server as a subprocess talks to it.
// Logging goes to stderr: this revision deprecated the logging feature and
// points stdio servers at stderr instead.
//
// It is dual-era. A client that opens with an initialize handshake gets the
// legacy revision it negotiated, for the life of the process; one that sends
// per-request metadata gets the modern revision. On stdio a legacy client's
// negotiated version is process-scoped, because there is no session to hang it
// on and only one client per process.
type StdioTransport struct {
	server *Server
	in     io.Reader
	out    io.Writer
	legacy bool
	logger *log.Logger

	// negotiated is set by a legacy initialize handshake and is the version
	// every subsequent request without its own metadata is answered in.
	negotiated string
}

// NewStdioTransport builds the transport for one server.
func NewStdioTransport(s *Server, in io.Reader, out io.Writer, legacy bool, logger *log.Logger) *StdioTransport {
	return &StdioTransport{server: s, in: in, out: out, legacy: legacy, logger: logger}
}

// Serve reads requests until stdin closes or ctx is cancelled.
func (t *StdioTransport) Serve(ctx context.Context) error {
	scanner := bufio.NewScanner(t.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 32<<20)
	encoder := json.NewEncoder(t.out)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			_ = encoder.Encode(newErrorResponse(nil, Errorf(CodeParseError, "invalid JSON: %v", err)))
			continue
		}
		reqCtx := withVersion(ctx, t.versionFor(&req))

		// Cancellation on stdio arrives as a notification; there is no stream
		// to close, so it is simply acknowledged by doing nothing.
		if req.IsNotification() {
			t.server.Handle(reqCtx, &req)
			continue
		}

		var resp *Response
		if req.Method == "initialize" {
			resp = t.handleInitialize(&req)
		} else if result, rpcErr := t.server.Handle(reqCtx, &req); rpcErr != nil {
			resp = newErrorResponse(req.ID, rpcErr)
		} else {
			resp = newResponse(req.ID, result)
		}
		if err := encoder.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// versionFor returns the protocol version to answer one request in. A version
// declared in the request metadata wins; otherwise the process falls back to
// whatever a legacy initialize negotiated.
func (t *StdioTransport) versionFor(req *Request) string {
	if declared := req.MetaString(MetaProtocolVersion); declared != "" {
		return declared
	}
	return t.negotiated
}

// handleInitialize answers a legacy handshake and fixes the version for the
// rest of the process.
func (t *StdioTransport) handleInitialize(req *Request) *Response {
	if !t.legacy {
		// A modern-only server names its versions in this error: a legacy
		// client has no fall-forward mechanism, so this message may be the only
		// diagnostic its user ever sees.
		return newErrorResponse(req.ID, Errorf(CodeMethodNotFound,
			"initialize was removed in protocol version %s, and legacy compatibility is disabled on this server; "+
				"supported versions: %s", ProtocolVersion, strings.Join(SupportedVersions, ", ")))
	}
	result, _, version, rpcErr := t.server.initialize(req)
	if rpcErr != nil {
		return newErrorResponse(req.ID, rpcErr)
	}
	t.negotiated = version
	return newResponse(req.ID, result)
}
