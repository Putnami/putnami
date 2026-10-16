package http

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// writeResponse writes resp to w. A HEAD request gets resp's status and
// headers and no body: the server writes no body byte and reads no streamed
// body, and closing a streamed body stays with the caller. When the body is
// held in memory and resp sets no Content-Length, the HEAD answer carries the
// length a GET answer's body has.
func writeResponse(w http.ResponseWriter, r *http.Request, resp *Response) error {
	if r.Method != http.MethodHead {
		return resp.WriteTo(w)
	}
	if resp.stream == nil && resp.Headers.Get("Content-Length") == "" && w.Header().Get("Content-Length") == "" {
		length, err := resp.bodyLength()
		if err != nil {
			return err
		}
		if length > 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		}
	}
	resp.writeHead(w)
	return nil
}

// bodyLength returns the number of body bytes WriteTo writes for a body held
// in memory.
func (r *Response) bodyLength() (int64, error) {
	if r.raw != nil {
		return int64(len(r.raw)), nil
	}
	if r.body == nil {
		return 0, nil
	}
	var counter byteCounter
	if err := json.NewEncoder(&counter).Encode(r.body); err != nil {
		return 0, err
	}
	return int64(counter), nil
}

// byteCounter is an io.Writer that counts the bytes written to it.
type byteCounter int64

func (c *byteCounter) Write(p []byte) (int, error) {
	*c += byteCounter(len(p))
	return len(p), nil
}
