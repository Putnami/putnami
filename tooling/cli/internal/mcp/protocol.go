package mcp

import "encoding/json"

const (
	jsonRPCVersion = "2.0"

	// defaultProtocolVersion is the MCP revision the server advertises when a
	// client does not request one. When the client sends a protocolVersion in
	// initialize, the server echoes it back instead, which maximizes interop
	// across spec revisions for a read-heavy server like this one.
	defaultProtocolVersion = "2025-06-18"

	// serverName is the MCP server identity reported in initialize.
	serverName = "putnami"
)

// JSON-RPC 2.0 error codes (the subset this server emits).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// rpcMessage is the on-wire shape shared by requests and notifications. A
// request carries an id and expects exactly one response; a notification omits
// id and gets none. Params is kept raw so each method decodes its own shape.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message is a notification (no id), which
// must not produce a response per JSON-RPC 2.0.
func (m *rpcMessage) isNotification() bool {
	return len(m.ID) == 0
}

// rpcError is the JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// rpcResponse is a single JSON-RPC response (success xor error).
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// --- MCP lifecycle types ---

type initializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	ClientInfo      clientInfo      `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    serverCaps `json:"capabilities"`
	ServerInfo      serverInfo `json:"serverInfo"`
	Instructions    string     `json:"instructions,omitempty"`
}

type serverCaps struct {
	Tools     *toolsCapability     `json:"tools,omitempty"`
	Resources *resourcesCapability `json:"resources,omitempty"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged"`
}

type resourcesCapability struct {
	Subscribe   bool `json:"subscribe"`
	ListChanged bool `json:"listChanged"`
}

type serverInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// --- MCP tool types ---

// Tool is the advertised description of a callable tool (tools/list entry).
type Tool struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
	Meta        map[string]any   `json:"_meta,omitempty"`
}

type toolAnnotations struct {
	ReadOnlyHint    *bool `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool `json:"openWorldHint,omitempty"`
}

type toolsListResult struct {
	Tools []Tool `json:"tools"`
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// callToolResult is the tools/call response. A tool that fails at runtime sets
// IsError=true and reports the reason in Content, rather than returning a
// JSON-RPC error — protocol errors and tool errors are distinct in MCP.
type callToolResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// --- MCP resource types ---

type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

type resourcesListResult struct {
	Resources []Resource `json:"resources"`
}

type readResourceParams struct {
	URI string `json:"uri"`
}

type resourceContent struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Text     string `json:"text"`
}

type readResourceResult struct {
	Contents []resourceContent `json:"contents"`
}
