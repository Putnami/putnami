package client

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"go.putnami.dev/errors"
)

// snapshotBindingHeaders canonicalizes names once, refusing aliases instead of
// choosing a value in map iteration order. Values never enter diagnostics.
func snapshotBindingHeaders(source map[string]string) (map[string]string, error) {
	if source == nil {
		return nil, nil
	}
	headers := make(map[string]string, len(source))
	for _, name := range slices.Sorted(maps.Keys(source)) {
		if !validBindingHeaderName(name) || reservedBindingHeader(name) {
			return nil, errors.New(CodeClientConfig, "service binding header name is invalid or reserved")
		}
		canonical := http.CanonicalHeaderKey(name)
		if _, exists := headers[canonical]; exists {
			return nil, errors.New(CodeClientConfig, "service binding headers contain duplicate names")
		}
		value := source[name]
		if !validBindingHeaderValue(value) {
			return nil, errors.New(CodeClientConfig, "service binding header value must be visible ASCII without surrounding whitespace")
		}
		headers[canonical] = value
	}
	return headers, nil
}

func validBindingHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r) {
			continue
		}
		return false
	}
	return true
}

// validBindingHeaderValue accepts visible ASCII with inner spaces or tabs.
func validBindingHeaderValue(value string) bool {
	if value != "" && (isHeaderSpace(value[0]) || isHeaderSpace(value[len(value)-1])) {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] != '\t' && (value[i] < ' ' || value[i] > '~') {
			return false
		}
	}
	return true
}

func isHeaderSpace(b byte) bool {
	return b == ' ' || b == '\t'
}

// reservedBindingHeaderNames are the lower-case header names a binding can
// never supply: they belong to the runtime or to Credentials. X-Trace-Id,
// X-Region and X-Experiments carry per-request context the TypeScript runtime
// propagates. Both runtimes reserve exactly this set, pinned by
// protocols/clientcontract/fixtures/binding/headers.json.
var reservedBindingHeaderNames = []string{
	"authorization", "cookie", "set-cookie", "host", "connection", "keep-alive", "upgrade",
	"content-type", "content-length", "content-encoding", "transfer-encoding", "te", "trailer",
	"accept", "accept-encoding", "traceparent", "tracestate", "baggage",
	"x-forwarded-for", "forwarded", "x-real-ip", "x-cloud-trace-context", "origin",
	"x-client-id", "x-request-id", "x-putnami-client-id", "x-putnami-service",
	"x-trace-id", "x-region", "x-experiments",
}

// reservedBindingHeaderPrefixes are the lower-case name prefixes a binding can
// never supply, pinned by the same corpus.
var reservedBindingHeaderPrefixes = []string{"sec-websocket-", "proxy-", "connect-", "grpc-"}

func reservedBindingHeader(name string) bool {
	name = strings.ToLower(name)
	for _, prefix := range reservedBindingHeaderPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return slices.Contains(reservedBindingHeaderNames, name)
}

// requestWithBindingHeaders applies instance defaults to a private request.
// Explicit operation headers win, including an explicitly empty value. Static
// idempotency keys would conflate distinct calls, so they are never defaults.
func (runtime *serviceRuntime) requestWithBindingHeaders(request *Request, operation Operation) (*Request, error) {
	copy := cloneRequest(request)
	var err error
	copy.Headers, err = runtime.applyBindingHeaders(copy.Headers, operation)
	if err != nil {
		return nil, err
	}
	return copy, nil
}

// applyBindingHeaders merges defaults into an owned header map. The cache uses
// a header-only projection; transport preparation owns a full private request.
func (runtime *serviceRuntime) applyBindingHeaders(headers http.Header, operation Operation) (http.Header, error) {
	if headers == nil {
		headers = make(http.Header)
	}
	for name, value := range runtime.binding.Headers {
		if strings.EqualFold(name, operation.Contract.Idempotency.KeyHeader) {
			return nil, errors.New(CodeClientConfig, "service binding header conflicts with the operation idempotency key")
		}
		present := false
		for existing := range headers {
			if strings.EqualFold(existing, name) {
				present = true
				break
			}
		}
		if !present {
			headers.Set(name, value)
		}
	}
	return headers, nil
}
