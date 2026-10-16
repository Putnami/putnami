package http

import (
	"net/http"
	"strings"
	"time"
)

// defaultWriteTimeout bounds a response when ServerConfig.WriteTimeout is left
// unset, mirroring the struct tag's documented default.
const defaultWriteTimeout = 30 * time.Second

// writeTimeout resolves the server write timeout. Zero means the documented
// default; a negative value disables it.
func (c ServerConfig) writeTimeout() time.Duration {
	if c.WriteTimeout == 0 {
		return defaultWriteTimeout
	}
	return c.WriteTimeout
}

// requestDeadline is how long a request runs before its context ends with
// context.DeadlineExceeded: nine tenths of the write timeout. Over HTTP/2, a
// write timeout that fires first resets the stream and cancels the request
// context, so a stalled handler would read context.Canceled, the error a caller
// that left produces. The margin also leaves the handler time to write its 5xx
// answer. A disabled write timeout sets no deadline.
func (c ServerConfig) requestDeadline() time.Duration {
	timeout := c.writeTimeout()
	return timeout - timeout/10
}

// longLivedRequest reports a request whose response outlives the write timeout
// on purpose: a WebSocket upgrade, a Server-Sent Events stream, or a gRPC or
// Connect stream. Each clears the server write deadline and bounds its own
// writes, so a request deadline would cut it.
func longLivedRequest(r *http.Request) bool {
	if isWebSocketRequest(r) || acceptsEventStream(r) {
		return true
	}
	contentType := r.Header.Get("Content-Type")
	return hasPrefixFold(contentType, "application/grpc") ||
		hasPrefixFold(contentType, "application/connect+")
}

func hasPrefixFold(value, prefix string) bool {
	return len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix)
}
