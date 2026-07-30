package main

import (
	"encoding/json"
	"fmt"
)

// ProtocolVersion is the MCP revision this server prefers. Revision 2026-07-28
// is stateless: there is no initialize handshake and no session id; every
// request carries its own version, identity and capabilities in _meta.
const ProtocolVersion = "2026-07-28"

// The specification calls a version that conveys identity and capabilities as
// per-request metadata "modern", and one that establishes a session with an
// initialize handshake "legacy". This server is "dual-era": it serves both, and
// picks which from how the client opens.
var (
	// ModernVersions are the stateless revisions, newest first.
	ModernVersions = []string{"2026-07-28"}

	// LegacyVersions are the handshake-based revisions this server still
	// answers, newest first. All three use the Streamable HTTP transport with
	// sessions; the 2024-11-05 HTTP+SSE transport is not implemented.
	LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
)

// SupportedVersions is every version this server speaks, newest first. It is
// what server/discover advertises and what an UnsupportedProtocolVersionError
// lists.
var SupportedVersions = append(append([]string{}, ModernVersions...), LegacyVersions...)

// LatestLegacyVersion is what an initialize handshake falls back to when the
// client asks for a legacy version this server does not implement.
const LatestLegacyVersion = "2025-11-25"

// isModernVersion reports whether a version conveys its metadata per request.
func isModernVersion(version string) bool {
	for _, v := range ModernVersions {
		if v == version {
			return true
		}
	}
	return false
}

// isLegacyVersion reports whether a version uses the initialize handshake.
func isLegacyVersion(version string) bool {
	for _, v := range LegacyVersions {
		if v == version {
			return true
		}
	}
	return false
}

// Well-known _meta keys defined by the specification.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
	MetaLogLevel           = "io.modelcontextprotocol/logLevel"
	MetaSubscriptionID     = "io.modelcontextprotocol/subscriptionId"
)

// JSON-RPC and MCP error codes. -32020..-32099 is the range the specification
// reserves for itself; -32000..-32019 stays implementation-defined.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeHeaderMismatch                  = -32020
	CodeMissingRequiredClientCapability = -32021
	CodeUnsupportedProtocolVersion      = -32022
)

// Implementation is the identity of a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Request is an incoming JSON-RPC request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message carries no id and therefore
// expects no response.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// Meta returns the _meta object of the request params, or nil.
func (r *Request) Meta() map[string]json.RawMessage {
	if len(r.Params) == 0 {
		return nil
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return nil
	}
	return p.Meta
}

// MetaString returns a string-valued _meta entry.
func (r *Request) MetaString(key string) string {
	raw, ok := r.Meta()[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// ClientName returns the self-reported client identity, for logging only.
func (r *Request) ClientName() string {
	raw, ok := r.Meta()[MetaClientInfo]
	if !ok {
		return ""
	}
	var impl Implementation
	if json.Unmarshal(raw, &impl) != nil {
		return ""
	}
	if impl.Version != "" {
		return impl.Name + "/" + impl.Version
	}
	return impl.Name
}

// Bind decodes the request params into v.
func (r *Request) Bind(v any) *RPCError {
	if len(r.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return Errorf(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

// Response is an outgoing JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is an outgoing JSON-RPC notification.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Errorf builds an RPCError with a formatted message.
func Errorf(code int, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// UnsupportedVersionError is returned when a request declares a protocol
// version this server does not implement.
func UnsupportedVersionError(requested string) *RPCError {
	return &RPCError{
		Code:    CodeUnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data: map[string]any{
			"supported": SupportedVersions,
			"requested": requested,
		},
	}
}

func newResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func newErrorResponse(id json.RawMessage, err *RPCError) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: err}
}

// Result is the envelope every successful result carries. Revision 2026-07-28
// requires resultType on all results and cache hints on list-shaped ones. The
// fields are omitted when empty so the same structs serve a legacy client,
// which would not recognise them.
type Result struct {
	ResultType string         `json:"resultType,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
	TTLMs      int64          `json:"ttlMs,omitempty"`
	CacheScope string         `json:"cacheScope,omitempty"`
}

const (
	ResultComplete      = "complete"
	ResultInputRequired = "input_required"

	CacheScopePublic  = "public"
	CacheScopePrivate = "private"
)

// cacheable adds the freshness hints required on list and read results. They
// arrived with resultType in 2026-07-28, so an empty envelope - which is what a
// legacy client gets - is left alone.
func (r Result) cacheable(ttlMs int64, scope string) Result {
	if r.ResultType == "" {
		return r
	}
	r.TTLMs = ttlMs
	r.CacheScope = scope
	return r
}

// DiscoverResult answers server/discover.
type DiscoverResult struct {
	Result
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ServerCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitempty"`
}

// ServerCapabilities describes the optional features this server implements.
type ServerCapabilities struct {
	Tools      *ToolsCapability     `json:"tools,omitempty"`
	Resources  *ResourcesCapability `json:"resources,omitempty"`
	Prompts    *PromptsCapability   `json:"prompts,omitempty"`
	Extensions map[string]any       `json:"extensions,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type ResourcesCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
	Subscribe   bool `json:"subscribe,omitempty"`
}

type PromptsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// Tool is a tool definition as returned by tools/list.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are behavioural hints; clients treat them as untrusted.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

// ListToolsResult answers tools/list.
type ListToolsResult struct {
	Result
	Tools      []Tool `json:"tools"`
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams are the params of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult answers tools/call.
type CallToolResult struct {
	Result
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Content is one unstructured content block of a tool or prompt result.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

func textContent(text string) []Content {
	return []Content{{Type: "text", Text: text}}
}

// Resource is a resource definition as returned by resources/list.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ListResourcesResult answers resources/list.
type ListResourcesResult struct {
	Result
	Resources  []Resource `json:"resources"`
	NextCursor string     `json:"nextCursor,omitempty"`
}

// ListResourceTemplatesResult answers resources/templates/list.
type ListResourceTemplatesResult struct {
	Result
	ResourceTemplates []ResourceTemplate `json:"resourceTemplates"`
}

// ResourceTemplate is a parameterised resource URI.
type ResourceTemplate struct {
	URITemplate string `json:"uriTemplate"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ReadResourceResult answers resources/read.
type ReadResourceResult struct {
	Result
	Contents []ResourceContents `json:"contents"`
}

// ResourceContents is the body of one resource.
type ResourceContents struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

// Prompt is a prompt definition as returned by prompts/list.
type Prompt struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPromptsResult answers prompts/list.
type ListPromptsResult struct {
	Result
	Prompts    []Prompt `json:"prompts"`
	NextCursor string   `json:"nextCursor,omitempty"`
}

// GetPromptResult answers prompts/get.
type GetPromptResult struct {
	Result
	Description string          `json:"description,omitempty"`
	Messages    []PromptMessage `json:"messages"`
}

type PromptMessage struct {
	Role    string  `json:"role"`
	Content Content `json:"content"`
}

// SubscribeParams are the params of subscriptions/listen: the client opts in
// to the notification kinds it wants on the response stream.
type SubscribeParams struct {
	ToolsListChanged      bool     `json:"toolsListChanged,omitempty"`
	PromptsListChanged    bool     `json:"promptsListChanged,omitempty"`
	ResourcesListChanged  bool     `json:"resourcesListChanged,omitempty"`
	ResourceSubscriptions []string `json:"resourceSubscriptions,omitempty"`
}
