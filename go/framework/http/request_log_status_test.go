package http

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/logger/logtest"
	"go.putnami.dev/protocol/features/spectest"
)

// A handler that writes its answer to ctx.Writer and returns nil is logged with
// the status it wrote, and the record's level and outcome follow that status.
func TestRequestLogRecordsTheStatusWrittenToTheWire(t *testing.T) {
	cases := []struct {
		name     string
		check    string
		write    func(w http.ResponseWriter)
		status   int
		severity string
		outcome  string
	}{
		{
			name:     "server error",
			check:    "a-directly-written-5xx-logs-at-error",
			write:    func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
			status:   http.StatusInternalServerError,
			severity: "ERROR",
			outcome:  "failure",
		},
		{
			name:     "success",
			check:    "a-directly-written-success-logs-at-info",
			write:    func(w http.ResponseWriter) { w.WriteHeader(http.StatusOK) },
			status:   http.StatusOK,
			severity: "INFO",
			outcome:  "success",
		},
		{
			name:     "client error",
			check:    "a-directly-written-success-logs-at-info",
			write:    func(w http.ResponseWriter) { w.WriteHeader(http.StatusNotFound) },
			status:   http.StatusNotFound,
			severity: "INFO",
			outcome:  "success",
		},
		{
			name:     "body without a status",
			check:    "a-directly-written-success-logs-at-info",
			write:    func(w http.ResponseWriter) { _, _ = w.Write([]byte("ok")) },
			status:   http.StatusOK,
			severity: "INFO",
			outcome:  "success",
		},
		{
			name: "informational status before the final one",
			write: func(w http.ResponseWriter) {
				w.WriteHeader(http.StatusEarlyHints)
				w.WriteHeader(http.StatusServiceUnavailable)
			},
			check:    "a-directly-written-5xx-logs-at-error",
			status:   http.StatusServiceUnavailable,
			severity: "ERROR",
			outcome:  "failure",
		},
		{
			name:     "caller withdrawal",
			check:    "a-directly-written-499-logs-as-canceled",
			write:    func(w http.ResponseWriter) { w.WriteHeader(perrors.StatusClientClosedRequest) },
			status:   perrors.StatusClientClosedRequest,
			severity: "INFO",
			outcome:  "canceled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spectest.Proves(t, "go/http-services", "request-log-status", tc.check)
			rec := logtest.NewRecorder(t)
			server := NewServerPlugin(ServerConfig{})
			server.Use(Logging(LoggerOptions{Logger: rec.Named("http")}))
			server.GET("/orders", func(ctx *Context) *Response {
				tc.write(ctx.Writer)
				return nil
			})

			ts := server.TestServer()
			defer ts.Close()
			resp, err := http.Get(ts.URL + "/orders")
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()

			if resp.StatusCode != tc.status {
				t.Fatalf("wire status = %d, want %d", resp.StatusCode, tc.status)
			}
			record := terminalRecord(t, rec, "/orders")
			if record["severity"] != tc.severity {
				t.Errorf("severity = %v, want %s", record["severity"], tc.severity)
			}
			group, _ := record["http"].(map[string]any)
			if group["status"] != float64(tc.status) {
				t.Errorf("logged status = %v, want %d", group["status"], tc.status)
			}
			if group["outcome"] != tc.outcome {
				t.Errorf("outcome = %v, want %s", group["outcome"], tc.outcome)
			}
		})
	}
}

// When a handler wrote to the wire and also returned a response, the server's
// later write of that response cannot change the status, so the log keeps the
// written one.
func TestRequestLogPrefersTheWrittenStatusOverTheReturnedResponse(t *testing.T) {
	spectest.Proves(t, "go/http-services", "request-log-status", "a-directly-written-5xx-logs-at-error")
	rec := logtest.NewRecorder(t)
	server := NewServerPlugin(ServerConfig{})
	server.Use(Logging(LoggerOptions{Logger: rec.Named("http")}))
	server.GET("/payments", func(ctx *Context) *Response {
		ctx.Writer.WriteHeader(http.StatusBadGateway)
		return JSON("ok")
	})

	w := httptest.NewRecorder()
	server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/payments", nil))

	record := terminalRecord(t, rec, "/payments")
	group, _ := record["http"].(map[string]any)
	if group["status"] != float64(http.StatusBadGateway) || record["severity"] != "ERROR" {
		t.Errorf("logged status/severity = %v/%v, want 502/ERROR", group["status"], record["severity"])
	}
}

type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	return nil, nil, nil
}

// The writer the Logging middleware hands down keeps the optional interfaces of
// the writer it wraps, so streams flush and upgrades hijack through it.
func TestLoggingWriterKeepsFlusherHijackerAndUnwrap(t *testing.T) {
	inner := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	var seen http.ResponseWriter
	handler := Chain(Logging(LoggerOptions{Logger: logtest.NewRecorder(t).Named("http")}))(func(ctx *Context) *Response {
		seen = ctx.Writer
		return nil
	})
	handler(NewContext(inner, httptest.NewRequest(http.MethodGet, "/", nil)))

	flusher, ok := seen.(http.Flusher)
	if !ok {
		t.Fatal("the logged writer does not implement http.Flusher")
	}
	flusher.Flush()
	if !inner.Flushed {
		t.Error("Flush did not reach the wrapped writer")
	}
	hijacker, ok := seen.(http.Hijacker)
	if !ok {
		t.Fatal("the logged writer does not implement http.Hijacker over a hijackable writer")
	}
	if _, _, err := hijacker.Hijack(); err != nil || !inner.hijacked {
		t.Errorf("Hijack did not reach the wrapped writer: %v", err)
	}
	if unwrapper, ok := seen.(interface{ Unwrap() http.ResponseWriter }); !ok || unwrapper.Unwrap() != inner {
		t.Error("Unwrap does not return the wrapped writer")
	}

	var plain http.ResponseWriter
	handler = Chain(Logging(LoggerOptions{Logger: logtest.NewRecorder(t).Named("http")}))(func(ctx *Context) *Response {
		plain = ctx.Writer
		return nil
	})
	handler(NewContext(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil)))
	if _, ok := plain.(http.Hijacker); ok {
		t.Error("the logged writer claims http.Hijacker over a writer that cannot hijack")
	}
}
