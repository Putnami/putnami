package api

import (
	phttp "go.putnami.dev/http"
)

// Server is the transport contract that api.Plugin uses to dispatch endpoint definitions.
// Today only `*http.ServerPlugin` implements it (via its Handle method); a future edge
// adapter (Cloudflare Workers, Lambda) or in-memory test transport would implement Server
// directly without depending on the HTTP package.
type Server interface {
	// Handle registers a handler for the given method and path. Implementations are free to
	// pre-compose middleware or apply transport-specific options at start time.
	Handle(method, path string, handler phttp.Handler)
}

// bodyLimitServer is implemented by transports that can apply a request-body
// bound before an endpoint handler consumes the wire. Binary declarations use
// it so their own limit does not alter the server-wide JSON limit.
type bodyLimitServer interface {
	HandleWithBodyLimit(method, path string, handler phttp.Handler, maxBytes int64)
}

// StreamServer is the optional transport contract for SSE/WebSocket stream
// endpoints.
type StreamServer interface {
	HandleStream(path string, handler phttp.StreamHandler)
}
