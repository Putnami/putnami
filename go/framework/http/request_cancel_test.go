package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/logger/logtest"
	"go.putnami.dev/protocol/features/spectest"
)

// terminalRecord returns the single request log line the Logging middleware
// wrote for path.
func terminalRecord(t *testing.T, rec *logtest.Recorder, path string) map[string]any {
	t.Helper()
	lines := rec.Records("http", "[GET] "+path)
	if len(lines) != 1 {
		t.Fatalf("want 1 request record, got %d.%s", len(lines), rec.Dump())
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestHandlerReturningItsCanceledContextAnswers499BelowError(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-canceled-request-answers-499-below-error")
	rec := logtest.NewRecorder(t)
	server := NewServerPlugin(ServerConfig{})
	server.Use(Logging(LoggerOptions{Logger: rec.Named("http")}))

	reqCtx, leave := context.WithCancel(context.Background())
	server.GET("/orders", func(ctx *Context) *Response {
		leave() // the caller goes away while the handler waits on a dependency
		<-ctx.Context().Done()
		return ErrorResponse(ctx.Context().Err())
	})

	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orders", nil).WithContext(reqCtx))

	if w.Code != perrors.StatusClientClosedRequest {
		t.Fatalf("status = %d, want %d", w.Code, perrors.StatusClientClosedRequest)
	}
	record := terminalRecord(t, rec, "/orders")
	if record["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", record["severity"])
	}
	group, _ := record["http"].(map[string]any)
	if group["outcome"] != "canceled" {
		t.Errorf("outcome = %v, want canceled", group["outcome"])
	}
}

func TestStalledHandlerEndsOnDeadlineUnderWriteTimeout(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-stalled-request-ends-on-its-deadline-at-error")
	rec := logtest.NewRecorder(t)
	const writeTimeout = 400 * time.Millisecond
	server := NewServerPlugin(ServerConfig{WriteTimeout: writeTimeout})
	server.Use(Logging(LoggerOptions{Logger: rec.Named("http")}))

	var handlerErr error
	var remaining time.Duration
	server.GET("/orders", func(ctx *Context) *Response {
		deadline, ok := ctx.Context().Deadline()
		if !ok {
			return InternalError("no request deadline")
		}
		remaining = time.Until(deadline)
		<-ctx.Context().Done() // a dependency that never answers
		handlerErr = ctx.Context().Err()
		return ErrorResponse(handlerErr)
	})

	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/orders", nil))

	if remaining <= 0 || remaining >= writeTimeout {
		t.Fatalf("request deadline is %v away, want under the %v write timeout", remaining, writeTimeout)
	}
	if handlerErr != context.DeadlineExceeded {
		t.Fatalf("handler context error = %v, want context.DeadlineExceeded", handlerErr)
	}
	if w.Code < 500 {
		t.Fatalf("status = %d, want 5xx", w.Code)
	}
	record := terminalRecord(t, rec, "/orders")
	if record["severity"] != "ERROR" {
		t.Errorf("severity = %v, want ERROR", record["severity"])
	}
}

// A long-lived response clears the write timeout itself, so the request
// deadline must not end it either.
func TestLongLivedRequestsRunWithoutRequestDeadline(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "a-long-lived-request-runs-without-a-request-deadline")
	for name, headers := range map[string]map[string]string{
		"sse":       {"Accept": "text/event-stream"},
		"websocket": {"Upgrade": "websocket", "Connection": "Upgrade"},
		"grpc":      {"Content-Type": "application/grpc+proto"},
		"connect":   {"Content-Type": "application/connect+json"},
	} {
		t.Run(name, func(t *testing.T) {
			server := NewServerPlugin(ServerConfig{WriteTimeout: time.Second})
			hasDeadline := true
			server.GET("/events", func(ctx *Context) *Response {
				_, hasDeadline = ctx.Context().Deadline()
				return NoContent()
			})
			req := httptest.NewRequest(http.MethodGet, "/events", nil)
			for key, value := range headers {
				req.Header.Set(key, value)
			}
			server.Handler().ServeHTTP(httptest.NewRecorder(), req)
			if hasDeadline {
				t.Fatal("a long-lived request runs under a request deadline")
			}
		})
	}
}

// The deadline rolls the request's unit of work back, so a success the handler
// returns after it would report work that was discarded.
func TestSuccessAfterRequestDeadlineAnswersServerError(t *testing.T) {
	spectest.Proves(t, "go/http-services", "caller-withdrawal", "work-rolled-back-by-the-deadline-answers-500")
	probe := &boundaryProbe{}
	server := newBoundaryServer(t, probe, func(_ *probeFinalizer, ctx *Context) *Response {
		<-ctx.Context().Done()
		return JSON(map[string]string{"ok": "yes"})
	})
	server.config.WriteTimeout = 200 * time.Millisecond
	ts := server.TestServer()
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/probe")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if _, outcome, _ := probe.snapshot(); outcome != context.DeadlineExceeded {
		t.Fatalf("scope outcome = %v, want context.DeadlineExceeded", outcome)
	}
}
