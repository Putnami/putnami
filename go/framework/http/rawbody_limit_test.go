package http

import (
	"bytes"
	"context"
	stderrors "errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.putnami.dev/errors"
)

const (
	mebibyte          = 1 << 20
	defaultBodyLimit  = int(DefaultMaxBodySize)
	largerServerLimit = 4 * mebibyte
)

// rawBodyRoute records what RawBody returned to the handler, so a test can
// assert both the response and the bytes or error the handler saw.
type rawBodyRoute struct {
	read int
	err  error
}

func (route *rawBodyRoute) handle(ctx *Context) *Response {
	data, err := ctx.RawBody()
	route.read, route.err = len(data), err
	if err != nil {
		return JSONStatus(http.StatusBadRequest, map[string]string{"error": err.Error()})
	}
	return JSON(map[string]int{"read": len(data)})
}

func postBytes(t *testing.T, server *ServerPlugin, path string, size int) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(bytes.Repeat([]byte("a"), size)))
	server.Handler().ServeHTTP(recorder, request)
	return recorder
}

func assertRawBodyRead(t *testing.T, recorder *httptest.ResponseRecorder, route *rawBodyRoute, size int) {
	t.Helper()
	if recorder.Code != http.StatusOK {
		t.Fatalf("a %d-byte body answered %d %q, want 200", size, recorder.Code, recorder.Body.String())
	}
	if route.err != nil || route.read != size {
		t.Fatalf("RawBody read %d bytes, err %v; want %d bytes, no error", route.read, route.err, size)
	}
}

func assertRawBodyRefused(t *testing.T, recorder *httptest.ResponseRecorder, route *rawBodyRoute, size int) {
	t.Helper()
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a %d-byte body answered %d %q, want 413", size, recorder.Code, recorder.Body.String())
	}
	var maxErr *http.MaxBytesError
	if !stderrors.As(route.err, &maxErr) {
		t.Fatalf("RawBody returned %v, want the server's *http.MaxBytesError", route.err)
	}
}

// A server configured above the 1 MiB default lets RawBody read a body up to
// its own limit: RawBody used to refuse anything past 1 MiB whatever the
// server allowed.
func TestRawBodyReadsUpToTheServerBodyLimit(t *testing.T) {
	limit := largerServerLimit
	server := NewServerPlugin(ServerConfig{MaxBodySize: int64(limit)})
	route := &rawBodyRoute{}
	server.POST("/raw", route.handle)

	assertRawBodyRead(t, postBytes(t, server, "/raw", 2*mebibyte), route, 2*mebibyte)
	assertRawBodyRead(t, postBytes(t, server, "/raw", limit), route, limit)
	assertRawBodyRefused(t, postBytes(t, server, "/raw", limit+1), route, limit+1)
}

// A server with the default limit still refuses one byte past 1 MiB.
func TestRawBodyKeepsTheDefaultServerBodyLimit(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	route := &rawBodyRoute{}
	server.POST("/raw", route.handle)

	assertRawBodyRead(t, postBytes(t, server, "/raw", defaultBodyLimit), route, defaultBodyLimit)
	assertRawBodyRefused(t, postBytes(t, server, "/raw", defaultBodyLimit+1), route, defaultBodyLimit+1)
}

// A route's own limit, set with WithMaxBodySize or HandleWithBodyLimit, is the
// limit RawBody reads up to on that route only.
func TestRawBodyReadsUpToTheRouteBodyLimit(t *testing.T) {
	limit := largerServerLimit
	server := NewServerPlugin(ServerConfig{})
	option, handled, global := &rawBodyRoute{}, &rawBodyRoute{}, &rawBodyRoute{}
	server.Route(http.MethodPost, "/option", option.handle, WithMaxBodySize(int64(limit)))
	server.HandleWithBodyLimit(http.MethodPost, "/handled", handled.handle, int64(limit))
	server.POST("/global", global.handle)

	assertRawBodyRead(t, postBytes(t, server, "/option", 2*mebibyte), option, 2*mebibyte)
	assertRawBodyRefused(t, postBytes(t, server, "/option", limit+1), option, limit+1)
	assertRawBodyRead(t, postBytes(t, server, "/handled", 2*mebibyte), handled, 2*mebibyte)
	assertRawBodyRefused(t, postBytes(t, server, "/handled", limit+1), handled, limit+1)
	assertRawBodyRefused(t, postBytes(t, server, "/global", 2*mebibyte), global, 2*mebibyte)
}

// Body decodes JSON up to the same server limit as RawBody.
func TestBodyDecodesUpToTheServerBodyLimit(t *testing.T) {
	server := NewServerPlugin(ServerConfig{MaxBodySize: largerServerLimit})
	var decoded string
	server.POST("/json", func(ctx *Context) *Response {
		if err := ctx.Body(&decoded); err != nil {
			return JSONStatus(http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return NoContent()
	})
	value := strings.Repeat("a", 2*mebibyte)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`"`+value+`"`)))
	if recorder.Code != http.StatusNoContent || decoded != value {
		t.Fatalf("a 2 MiB JSON body answered %d %q and decoded %d bytes, want 204 and %d bytes",
			recorder.Code, recorder.Body.String(), len(decoded), len(value))
	}
}

// A Context built outside a server reads at most DefaultMaxBodySize.
func TestRawBodyOutsideAServerKeepsTheDefaultLimit(t *testing.T) {
	read := func(size int) ([]byte, error) {
		request := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(bytes.Repeat([]byte("a"), size)))
		return NewContext(httptest.NewRecorder(), request).RawBody()
	}
	if data, err := read(defaultBodyLimit); err != nil || len(data) != defaultBodyLimit {
		t.Fatalf("RawBody of a 1 MiB body read %d bytes, err %v; want 1 MiB, no error", len(data), err)
	}
	data, err := read(defaultBodyLimit + 1)
	var bodyErr *errors.Error
	if data != nil || !stderrors.As(err, &bodyErr) || bodyErr.Code() != CodeBody {
		t.Fatalf("RawBody of a 1 MiB+1 body returned %d bytes, err %v; want a %s error", len(data), err, CodeBody)
	}
}

// Wrapping Request.Body in a smaller http.MaxBytesReader lowers the limit for
// one request, as the RawBody documentation says.
func TestRawBodyHonorsASmallerWrappedReader(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Repeat("a", 200)))
	ctx := NewContext(recorder, request)
	ctx.Request.Body = http.MaxBytesReader(recorder, ctx.Request.Body, 100)
	var maxErr *http.MaxBytesError
	if _, err := ctx.RawBody(); !stderrors.As(err, &maxErr) {
		t.Fatalf("RawBody past a wrapped 100-byte reader returned %v, want *http.MaxBytesError", err)
	}
}

// The recorded limit survives WithContext, which the server calls to attach the
// request's dependency scope.
func TestRawBodyLimitSurvivesWithContext(t *testing.T) {
	ctx := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	ctx.maxBodySize = largerServerLimit
	if got := ctx.WithContext(context.Background()).bodyLimit(); got != largerServerLimit {
		t.Fatalf("bodyLimit after WithContext = %d, want %d", got, largerServerLimit)
	}
}

// A limit of math.MaxInt64 reads the whole body: one byte past it would
// overflow to a negative read limit and return an empty body.
func TestRawBodyReadsTheWholeBodyUnderTheLargestLimit(t *testing.T) {
	ctx := NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", strings.NewReader("raw body content")))
	ctx.maxBodySize = math.MaxInt64
	data, err := ctx.RawBody()
	if err != nil || string(data) != "raw body content" {
		t.Fatalf("RawBody under a math.MaxInt64 limit = %q, err %v; want the whole body", data, err)
	}
}
