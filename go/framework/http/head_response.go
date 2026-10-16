package http

import "net/http"

// headWriter is the handler's writer for a HEAD request: it passes headers and
// the status through and discards every body byte, so a handler that writes a
// body to ctx.Writer answers the same head with no body on the wire.
type headWriter struct {
	http.ResponseWriter
}

func (w headWriter) Write(p []byte) (int, error) {
	return len(p), nil
}

// Flush sends the head when the wrapped writer can flush.
func (w headWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap returns the wrapped writer, so http.ResponseController reaches it.
func (w headWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// writeResponse writes resp to w. A HEAD request gets resp's status and
// headers and no body: the server neither encodes nor reads the body, and
// closing a streamed body stays with the caller.
func writeResponse(w http.ResponseWriter, r *http.Request, resp *Response) error {
	if r.Method != http.MethodHead {
		return resp.WriteTo(w)
	}
	for key, values := range resp.Headers {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	return nil
}
