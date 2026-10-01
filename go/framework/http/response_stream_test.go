package http

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.putnami.dev/inject"
	"go.putnami.dev/protocol/features/spectest"
)

type responseStreamSource struct {
	io.Reader
	reads    int
	closed   bool
	closeErr error
}

func (s *responseStreamSource) Read(p []byte) (int, error) {
	s.reads++
	return s.Reader.Read(p)
}
func (s *responseStreamSource) Close() error { s.closed = true; return s.closeErr }

type failedStreamWriter struct{ *httptest.ResponseRecorder }

func (failedStreamWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStreamResponseTransfersOwnershipWithoutBuffering(t *testing.T) {
	spectest.Proves(t, "go/http-services", "streamed-responses", "a-streamed-response-is-copied-without-buffering-and-closed-on-success-or-failure")
	for _, fail := range []bool{false, true} {
		source := &responseStreamSource{Reader: strings.NewReader("\x00\xffstream")}
		response := Stream(201, "application/gzip; version=1", source)
		if _, err := response.BodyBytes(); err == nil || source.reads != 0 {
			t.Fatal("BodyBytes must refuse without consuming the stream")
		}
		recorder := httptest.NewRecorder()
		var writer http.ResponseWriter = recorder
		if fail {
			writer = failedStreamWriter{recorder}
		}
		err := response.WriteTo(writer)
		if !source.closed || (fail && !errors.Is(err, io.ErrClosedPipe)) || (!fail && err != nil) {
			t.Fatalf("fail=%v closed=%v error=%v", fail, source.closed, err)
		}
		if !fail && (recorder.Body.String() != "\x00\xffstream" || recorder.Code != 201 || recorder.Header().Get("Content-Type") != "application/gzip; version=1") {
			t.Fatalf("response = %#v", recorder)
		}
	}
	closeErr := errors.New("close failed")
	if err := Stream(200, "application/octet-stream", &responseStreamSource{Reader: strings.NewReader(""), closeErr: closeErr}).WriteTo(httptest.NewRecorder()); !errors.Is(err, closeErr) {
		t.Fatalf("close error = %v", err)
	}
	if err := Stream(200, "application/octet-stream", nil).WriteTo(httptest.NewRecorder()); err != nil {
		t.Fatal(err)
	}
}

func TestCompressionDoesNotConsumeAnOctetStream(t *testing.T) {
	source := &responseStreamSource{Reader: strings.NewReader(strings.Repeat("x", 2048))}
	request := httptest.NewRequest("GET", "/", nil)
	request.Header.Set("Accept-Encoding", "gzip")
	response := Compression(CompressionOptions{})(NewContext(httptest.NewRecorder(), request), func() *Response {
		return Stream(200, "application/json", source)
	})
	if source.reads != 0 || response.Headers.Get("Content-Encoding") != "" {
		t.Fatal("compression eagerly consumed the stream")
	}
	if err := response.WriteTo(httptest.NewRecorder()); err != nil || !source.closed {
		t.Fatalf("write = %v, closed=%v", err, source.closed)
	}
}

type streamScopeReader struct {
	io.Reader
	closed *bool
	probe  *boundaryProbe
}

func (r *streamScopeReader) Read(p []byte) (int, error) {
	finalized, _, _ := r.probe.snapshot()
	if *r.closed || finalized != 1 {
		return 0, errors.New("stream scope closed early or transaction was not finalized")
	}
	return r.Reader.Read(p)
}

func TestStreamResponseKeepsResourcesAfterCommitUntilTheCopyEnds(t *testing.T) {
	for _, failCommit := range []bool{false, true} {
		probe := &boundaryProbe{}
		if failCommit {
			probe.finalizeErr = errors.New("private commit failure")
		}
		closed := false
		cc := inject.NewContainerContext("stream-scope")
		if err := cc.Register(inject.Provide(inject.TokenOf[*probeFinalizer](),
			func(inject.Resolver) (any, error) { return &probeFinalizer{probe: probe}, nil },
			inject.WithScope(inject.Scoped), inject.WithOnClose(func() error { closed = true; return nil }))); err != nil {
			t.Fatal(err)
		}
		if err := cc.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cc.Close() })
		server := NewServerPlugin(ServerConfig{})
		server.setContainerContext(cc)
		source := &responseStreamSource{Reader: &streamScopeReader{Reader: strings.NewReader("streamed"), closed: &closed, probe: probe}}
		handler := Inject(func(*probeFinalizer, *Context) *Response { return Stream(200, "application/octet-stream", source) })
		if err := handler.Finalize(cc); err != nil {
			t.Fatal(err)
		}
		server.GET("/stream", handler)
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest("GET", "/stream", nil))
		if !closed || !source.closed {
			t.Fatalf("scope=%v body=%v", closed, source.closed)
		}
		if failCommit {
			if recorder.Code != 500 || source.reads != 0 || strings.Contains(recorder.Body.String(), "private") {
				t.Fatalf("failed commit response = %d %q, reads=%d", recorder.Code, recorder.Body.String(), source.reads)
			}
		} else if recorder.Code != 200 || recorder.Body.String() != "streamed" {
			t.Fatalf("response = %d %q", recorder.Code, recorder.Body.String())
		}
	}
}

type cancellationReader struct {
	ctx         context.Context
	entered     chan struct{}
	release     chan struct{}
	closeCalled chan struct{}
	reading     atomic.Bool
	concurrent  atomic.Bool
	closed      atomic.Int32
}

func (reader *cancellationReader) Read([]byte) (int, error) {
	reader.reading.Store(true)
	close(reader.entered)
	<-reader.ctx.Done()
	<-reader.release
	reader.reading.Store(false)
	return 0, reader.ctx.Err()
}

func (reader *cancellationReader) Close() error {
	if reader.reading.Load() {
		reader.concurrent.Store(true)
	}
	if reader.closed.Add(1) == 1 {
		close(reader.closeCalled)
	}
	return nil
}

func TestRequestCancellationDoesNotCloseTheProviderStreamDuringRead(t *testing.T) {
	server := NewServerPlugin(ServerConfig{})
	ctx, cancel := context.WithCancel(t.Context())
	reader := &cancellationReader{
		ctx: ctx, entered: make(chan struct{}), release: make(chan struct{}), closeCalled: make(chan struct{}),
	}
	server.GET("/stream", func(*Context) *Response {
		return Stream(200, "application/octet-stream", reader)
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		server.Handler().ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/stream", nil).WithContext(ctx))
	}()
	<-reader.entered
	cancel()
	select {
	case <-reader.closeCalled:
		t.Fatal("request cancellation closed the source concurrently with Read")
	case <-time.After(50 * time.Millisecond):
	}
	close(reader.release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("request cancellation did not finish after the context-aware source returned")
	}
	if reader.concurrent.Load() || reader.closed.Load() != 1 {
		t.Fatalf("concurrent=%v closes=%d", reader.concurrent.Load(), reader.closed.Load())
	}
}
