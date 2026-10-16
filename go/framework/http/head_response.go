package http

import (
	"net/http"
	"strconv"
)

// writeResponse writes resp to w. A HEAD request gets resp's status and
// headers and no body: the server writes no body byte and reads no streamed
// body, and closing a streamed body stays with the caller. When the status
// allows a body, the body is held in memory and no Content-Length is set, the
// HEAD answer carries the length of the body a GET answer writes.
func writeResponse(w http.ResponseWriter, r *http.Request, resp *Response) error {
	if r.Method != http.MethodHead {
		return resp.WriteTo(w)
	}
	var lengthErr error
	if resp.stream == nil && bodyAllowedForStatus(resp.Status) &&
		resp.Headers.Get("Content-Length") == "" && w.Header().Get("Content-Length") == "" {
		var length byteCounter
		if lengthErr = resp.writeBody(&length); lengthErr == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(int64(length), 10))
		}
	}
	resp.writeHead(w)
	return lengthErr
}

// bodyAllowedForStatus reports whether a response with status may carry a
// body. Status 0 is the 200 that writeHead writes.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent, status == http.StatusNotModified:
		return false
	}
	return true
}

// byteCounter is an io.Writer that counts the bytes written to it.
type byteCounter int64

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}
