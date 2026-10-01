package useragent

import (
	"net/http"
	"strings"
	"sync/atomic"

	proto "go.putnami.dev/protocol/extension"
)

const product = "putnami-cli"

var (
	version       atomic.Value
	agentIdentity atomic.Value
)

func init() {
	version.Store("dev")
	agentIdentity.Store(proto.AgentIdentity{})
}

// SetVersion sets the CLI version used in outbound HTTP User-Agent headers.
func SetVersion(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		v = "dev"
	}
	version.Store(v)
}

// String returns the unified versioned User-Agent for Putnami CLI traffic.
func String() string {
	if identity := currentAgentIdentity(); identity.UserAgent != "" {
		return identity.UserAgent
	}
	v, _ := version.Load().(string)
	return product + "/" + sanitizeToken(v)
}

// Set applies Putnami request provenance without overwriting caller-selected
// headers. Outside an opted-in MCP session this preserves the historical
// putnami-cli/<version> User-Agent and adds no structured identity headers.
func Set(req *http.Request) {
	if req == nil {
		return
	}
	identity := currentAgentIdentity()
	if identity.UserAgent != "" {
		identity.ApplyHTTPHeaders(req)
		return
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", String())
	}
}

// MCPIdentity normalizes bounded client and model metadata and builds the
// stable User-Agent used by opted-in MCP-originated API requests. A value with
// an ASCII control character is rejected rather than rewritten.
func MCPIdentity(serverVersion, clientName, clientVersion, model string) proto.AgentIdentity {
	identity := proto.AgentIdentity{
		ClientName:    sanitizeIdentityValue(clientName),
		ClientVersion: sanitizeIdentityValue(clientVersion),
		Model:         sanitizeIdentityValue(model),
	}
	nameToken := sanitizeIdentityToken(identity.ClientName)
	versionToken := sanitizeIdentityToken(identity.ClientVersion)
	if nameToken != "" {
		identity.Harness = nameToken
		if versionToken != "" {
			identity.Harness += "/" + versionToken
		}
		identity.Harness = boundASCII(identity.Harness, proto.MaxAgentIdentityValueLength)
	}
	if identity.Harness == "" && identity.Model == "" {
		return identity
	}

	serverToken := sanitizeIdentityToken(sanitizeIdentityValue(serverVersion))
	if serverToken == "" {
		serverToken = "dev"
	}
	var fields []string
	if identity.Harness != "" {
		fields = append(fields, "harness="+sanitizeCommentValue(identity.Harness))
	}
	if identity.Model != "" {
		fields = append(fields, "model="+sanitizeCommentValue(identity.Model))
	}
	identity.UserAgent = "putnami-mcp/" + serverToken + " (" + strings.Join(fields, "; ") + ")"
	return identity
}

// SetAgentIdentity activates opted-in MCP provenance for outbound requests in
// this process. putnami mcp is a single-session stdio process, so one atomic
// value covers its lifetime without leaking identity between requests.
func SetAgentIdentity(identity proto.AgentIdentity) {
	agentIdentity.Store(identity)
}

// ClearAgentIdentity restores normal CLI request behavior.
func ClearAgentIdentity() {
	agentIdentity.Store(proto.AgentIdentity{})
}

func currentAgentIdentity() proto.AgentIdentity {
	identity, _ := agentIdentity.Load().(proto.AgentIdentity)
	return identity
}

// NewTransport wraps base so requests sent through an http.Client get the
// unified User-Agent even if the caller uses helpers such as Client.Get.
func NewTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return transport{base: base}
}

type transport struct {
	base http.RoundTripper
}

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	identity := currentAgentIdentity()
	needsUserAgent := req.Header.Get("User-Agent") == ""
	needsHarness := identity.Harness != "" && req.Header.Get(proto.AgentHarnessHeader) == ""
	needsModel := identity.Model != "" && req.Header.Get(proto.AgentModelHeader) == ""
	if !needsUserAgent && !needsHarness && !needsModel {
		return t.base.RoundTrip(req)
	}
	cloned := req.Clone(req.Context())
	Set(cloned)
	return t.base.RoundTrip(cloned)
}

func sanitizeToken(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isTokenChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "dev"
	}
	return b.String()
}

func sanitizeIdentityValue(raw string) string {
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		if r > 0x7e {
			b.WriteByte('-')
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(boundASCII(b.String(), proto.MaxAgentIdentityValueLength))
}

func sanitizeIdentityToken(value string) string {
	if value == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range value {
		if isTokenChar(r) {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func sanitizeCommentValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '(', ')', '\\', ';', '=':
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func boundASCII(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}

func isTokenChar(r rune) bool {
	if r >= 'a' && r <= 'z' {
		return true
	}
	if r >= 'A' && r <= 'Z' {
		return true
	}
	if r >= '0' && r <= '9' {
		return true
	}
	switch r {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	default:
		return false
	}
}
