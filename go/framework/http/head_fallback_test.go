package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"go.putnami.dev/logger/logtest"
	"go.putnami.dev/protocol/features/spectest"
)

// countingSource is a stream source that counts the bytes read from it and
// whether it was closed.
type countingSource struct {
	reader io.Reader
	read   atomic.Int64
	closed atomic.Bool
}

func (s *countingSource) Read(p []byte) (int, error) {
	n, err := s.reader.Read(p)
	s.read.Add(int64(n))
	return n, err
}

func (s *countingSource) Close() error {
	s.closed.Store(true)
	return nil
}

// A HEAD request to a GET-only route answers the GET route's status and
// headers with no body over HTTP/1.1 and HTTP/2, reads no streamed body, and
// logs no ERROR, with the Logging middleware's recording writer in the chain.
// A body held in memory gives the HEAD answer the GET body's Content-Length.
func TestHeadFallbackAnswersTheGetHeadWithoutABody(t *testing.T) {
	spectest.Proves(t, "go/http-services", "method-fallbacks", "a-head-fallback-answers-the-get-status-and-headers-without-a-body")
	large := bytes.Repeat([]byte("x"), 64<<10)
	for _, major := range []int{1, 2} {
		for name, route := range map[string]struct {
			status int
			// inMemory marks a body the server holds, whose length the HEAD
			// answer carries.
			inMemory bool
			handler  func(ctx *Context, source *countingSource) *Response
		}{
			"large bytes": {status: http.StatusOK, inMemory: true, handler: func(*Context, *countingSource) *Response {
				return Bytes(http.StatusOK, "application/octet-stream", large)
			}},
			"small text": {status: http.StatusOK, inMemory: true, handler: func(*Context, *countingSource) *Response {
				return Text("ok")
			}},
			"large json": {status: http.StatusCreated, inMemory: true, handler: func(*Context, *countingSource) *Response {
				return JSONStatus(http.StatusCreated, map[string]string{"data": string(large)})
			}},
			"stream": {status: http.StatusOK, handler: func(_ *Context, source *countingSource) *Response {
				return Stream(http.StatusOK, "application/octet-stream", source)
			}},
			"direct write": {status: http.StatusAccepted, handler: func(ctx *Context, _ *countingSource) *Response {
				ctx.SetHeader("Content-Type", "application/octet-stream")
				ctx.Writer.WriteHeader(http.StatusAccepted)
				_, _ = ctx.Writer.Write(large)
				return nil
			}},
		} {
			t.Run(fmt.Sprintf("%s over HTTP/%d", name, major), func(t *testing.T) {
				rec := logtest.NewRecorder(t)
				server := NewServerPlugin(ServerConfig{})
				server.log = rec.Named("http")
				server.Use(Logging(LoggerOptions{Logger: rec.Named("http")}))
				var source *countingSource
				server.GET("/download", func(ctx *Context) *Response {
					source = &countingSource{reader: bytes.NewReader(large)}
					return route.handler(ctx, source)
				})

				ts := httptest.NewUnstartedServer(server.Handler())
				if major == 2 {
					ts.EnableHTTP2 = true
					ts.StartTLS()
				} else {
					ts.Start()
				}
				defer ts.Close()

				get, err := ts.Client().Get(ts.URL + "/download")
				if err != nil {
					t.Fatal(err)
				}
				getBody, err := io.ReadAll(get.Body)
				_ = get.Body.Close()
				if err != nil {
					t.Fatal(err)
				}

				req, err := http.NewRequest(http.MethodHead, ts.URL+"/download", nil)
				if err != nil {
					t.Fatal(err)
				}
				head, err := ts.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(head.Body)
				_ = head.Body.Close()
				ts.Close() // waits for the handler, so every record it writes is in

				if head.ProtoMajor != major {
					t.Fatalf("protocol = %s, want HTTP/%d", head.Proto, major)
				}
				if head.StatusCode != route.status || get.StatusCode != route.status {
					t.Errorf("HEAD status = %d, GET status = %d, want %d", head.StatusCode, get.StatusCode, route.status)
				}
				if got, want := head.Header.Get("Content-Type"), get.Header.Get("Content-Type"); got != want {
					t.Errorf("HEAD Content-Type = %q, want the GET value %q", got, want)
				}
				if got, want := head.Header.Get("X-Request-ID") != "", get.Header.Get("X-Request-ID") != ""; got != want {
					t.Errorf("HEAD X-Request-ID present = %v, want %v", got, want)
				}
				if route.inMemory {
					if got, want := head.Header.Get("Content-Length"), strconv.Itoa(len(getBody)); got != want {
						t.Errorf("HEAD Content-Length = %q, want the GET body length %s", got, want)
					}
					if gotGet := get.Header.Get("Content-Length"); gotGet != "" && gotGet != head.Header.Get("Content-Length") {
						t.Errorf("HEAD Content-Length = %q, want the GET value %q", head.Header.Get("Content-Length"), gotGet)
					}
				}
				if len(body) != 0 {
					t.Errorf("HEAD body = %d bytes, want none", len(body))
				}
				if name == "stream" {
					if read := source.read.Load(); read != 0 {
						t.Errorf("HEAD read %d bytes from the stream source, want none", read)
					}
					if !source.closed.Load() {
						t.Error("HEAD left the stream source open")
					}
				}
				if records := writeResponseRecords(t, rec); len(records) != 0 {
					t.Errorf("want no write response record, got %d.%s", len(records), rec.Dump())
				}
				assertNoErrorRecord(t, rec)
				lines := rec.Records("http", "[HEAD] /download")
				if len(lines) != 1 {
					t.Fatalf("want 1 HEAD request record, got %d.%s", len(lines), rec.Dump())
				}
				var record map[string]any
				if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
					t.Fatal(err)
				}
				if group, _ := record["http"].(map[string]any); group["status"] != float64(route.status) {
					t.Errorf("request log http = %v, want status %d", record["http"], route.status)
				}
			})
		}
	}
}

// An explicit HEAD route and a HEAD request to an unknown path also answer
// their status and headers with no body, and log no ERROR.
func TestHeadRequestWritesNoBody(t *testing.T) {
	spectest.Proves(t, "go/http-services", "method-fallbacks", "a-head-fallback-answers-the-get-status-and-headers-without-a-body")
	rec := logtest.NewRecorder(t)
	server := NewServerPlugin(ServerConfig{})
	server.log = rec.Named("http")
	server.Route(http.MethodHead, "/explicit", func(*Context) *Response {
		return Bytes(http.StatusOK, "application/octet-stream", bytes.Repeat([]byte("x"), 64<<10))
	})

	ts := httptest.NewUnstartedServer(server.Handler())
	ts.EnableHTTP2 = true
	ts.StartTLS()
	defer ts.Close()

	for path, status := range map[string]int{"/explicit": http.StatusOK, "/missing": http.StatusNotFound} {
		req, err := http.NewRequest(http.MethodHead, ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.ProtoMajor != 2 || resp.StatusCode != status || len(body) != 0 {
			t.Errorf("HEAD %s = %s %d with %d body bytes, want HTTP/2 %d with none", path, resp.Proto, resp.StatusCode, len(body), status)
		}
	}
	ts.Close() // waits for the handlers, so every record they write is in
	if records := writeResponseRecords(t, rec); len(records) != 0 {
		t.Errorf("want no write response record, got %d.%s", len(records), rec.Dump())
	}
	assertNoErrorRecord(t, rec)
}
