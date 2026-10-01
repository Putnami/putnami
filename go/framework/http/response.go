package http

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	perrors "go.putnami.dev/errors"
)

// Response represents an HTTP response with status, headers, and body.
type Response struct {
	Status     int
	StatusText string
	Headers    http.Header
	body       any
	raw        []byte
	stream     *responseStream
}

type responseStream struct {
	io.Reader
	once sync.Once
	err  error
}

func (s *responseStream) Close() error {
	s.once.Do(func() {
		if closer, ok := s.Reader.(io.Closer); ok {
			s.err = closer.Close()
		}
	})
	return s.err
}

// NewResponse creates a response with the given status code.
func NewResponse(status int) *Response {
	return &Response{
		Status:  status,
		Headers: make(http.Header),
	}
}

// JSON creates a JSON response with status 200.
func JSON(data any) *Response {
	r := NewResponse(http.StatusOK)
	r.body = data
	r.Headers.Set("Content-Type", "application/json")
	return r
}

// JSONStatus creates a JSON response with a custom status code.
func JSONStatus(status int, data any) *Response {
	r := NewResponse(status)
	r.body = data
	r.Headers.Set("Content-Type", "application/json")
	return r
}

// JSONBytes creates a JSON response from an already-encoded body. Use it to
// serve an immutable, pre-marshaled document so each request writes the same
// bytes (a copy in WriteTo) instead of re-encoding the value via reflection
// every time. The caller must not mutate data after passing it in.
func JSONBytes(data []byte) *Response {
	r := NewResponse(http.StatusOK)
	r.raw = data
	r.Headers.Set("Content-Type", "application/json")
	return r
}

// Bytes creates a response that writes data verbatim under mediaType. Use it
// for a declared binary payload: the bytes reach the wire unchanged, with no
// JSON encoding, no base64 and no charset parameter appended to a media type
// that has no character set. The caller must not mutate data afterwards.
func Bytes(status int, mediaType string, data []byte) *Response {
	r := NewResponse(status)
	r.raw = data
	r.Headers.Set("Content-Type", mediaType)
	return r
}

// Stream writes the source incrementally without converting it to a byte
// buffer. WriteTo owns and closes the source when it implements io.Closer.
// A response stream is consumed once and cannot be serialized with BodyBytes.
func Stream(status int, mediaType string, source io.Reader) *Response {
	r := NewResponse(status)
	if source == nil {
		source = http.NoBody
	}
	r.stream = &responseStream{Reader: source}
	r.Headers.Set("Content-Type", mediaType)
	return r
}

// Text creates a plain text response.
func Text(text string) *Response {
	r := NewResponse(http.StatusOK)
	r.raw = []byte(text)
	r.Headers.Set("Content-Type", "text/plain; charset=utf-8")
	return r
}

// Redirect creates a redirect response.
func Redirect(url string, status int) *Response {
	if status == 0 {
		status = http.StatusFound
	}
	r := NewResponse(status)
	r.Headers.Set("Location", url)
	return r
}

// NoContent creates a 204 No Content response.
func NoContent() *Response {
	return NewResponse(http.StatusNoContent)
}

// NotFound creates a 404 Not Found JSON response.
func NotFound() *Response {
	return JSONStatus(http.StatusNotFound, map[string]string{
		"error":   "Not Found",
		"message": "The requested resource was not found",
	})
}

// Unauthorized creates a 401 Unauthorized JSON response.
func Unauthorized() *Response {
	return JSONStatus(http.StatusUnauthorized, map[string]string{
		"error":   "Unauthorized",
		"message": "Authentication required",
	})
}

// Forbidden creates a 403 Forbidden JSON response.
func Forbidden() *Response {
	return JSONStatus(http.StatusForbidden, map[string]string{
		"error":   "Forbidden",
		"message": "Insufficient permissions",
	})
}

// InternalError creates a 500 Internal Server Error JSON response.
func InternalError(message string) *Response {
	return JSONStatus(http.StatusInternalServerError, map[string]string{
		"error":   "Internal Server Error",
		"message": message,
	})
}

// ErrorResponse converts a Putnami framework error to the canonical sanitized
// HTTP envelope. It is the provider-side counterpart of generated typed client
// errors and preserves stable error codes without exposing internal causes.
func ErrorResponse(err error) *Response {
	status, body := perrors.HTTPErrorResponse(err)
	return JSONStatus(status, body)
}

// WithStatus returns a copy with a different status code.
func (r *Response) WithStatus(status int) *Response {
	copy := *r
	copy.Status = status
	return &copy
}

// WithHeader returns a copy with an added header.
func (r *Response) WithHeader(key, value string) *Response {
	copy := *r
	copy.Headers = r.Headers.Clone()
	copy.Headers.Set(key, value)
	return &copy
}

// BodyBytes serializes the response body to bytes.
// Returns the raw bytes if set, otherwise JSON-encodes the body.
func (r *Response) BodyBytes() ([]byte, error) {
	if r.stream != nil {
		return nil, fmt.Errorf("http: streamed response cannot be buffered with BodyBytes")
	}
	if r.raw != nil {
		return r.raw, nil
	}
	if r.body != nil {
		return json.Marshal(r.body)
	}
	return nil, nil
}

// WriteTo writes the response to an http.ResponseWriter.
func (r *Response) WriteTo(w http.ResponseWriter) (retErr error) {
	// Copy headers
	for key, values := range r.Headers {
		for _, v := range values {
			w.Header().Add(key, v)
		}
	}

	// Write status
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)

	// Write body
	if r.stream != nil {
		defer func() {
			if err := r.stream.Close(); retErr == nil {
				retErr = err
			}
		}()
		_, retErr = io.Copy(w, r.stream)
		return retErr
	}
	if r.raw != nil {
		_, err := w.Write(r.raw)
		return err
	}
	if r.body != nil {
		return json.NewEncoder(w).Encode(r.body)
	}
	return nil
}
