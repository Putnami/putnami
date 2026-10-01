package http

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"go.putnami.dev/logger"
	"go.putnami.dev/logger/logtest"
)

// This file executes the HTTP boundary's cases of the canonical cross-runtime log
// corpus (protocols/logging/conformance) against the records the REAL middleware
// chain emits through the REAL JSON sink. Its TypeScript twin is
// typescript/framework/application/test/http/logging-cross-language.test.ts; a
// divergence here means either the change forgot the other runtime or it forgot
// the corpus.
//
// The accumulation case (http.terminal.success-with-publishes) is driven from
// go/framework/events/logging_cross_language_test.go: publishing is what
// accumulates onto this boundary's terminal record, and go.putnami.dev/events is
// the module that depends on this one (not the reverse).
const logConformanceManifest = "../../../protocols/logging/conformance/manifest.json"

// driveRequest runs one request through the real RequestID + Logging chain with
// the recorder's logger installed, so the captured line is exactly what a log
// aggregator would receive.
func driveRequest(t *testing.T, rec *logtest.Recorder, method, path string, h Handler) {
	t.Helper()
	handler := Chain(RequestID(), Logging(LoggerOptions{Logger: rec.Named("http")}))(h)
	req := httptest.NewRequest(method, path, nil)
	handler(NewContext(httptest.NewRecorder(), req))
}

func TestLoggingConformanceHTTPTerminalSuccess(t *testing.T) {
	suite := logtest.LoadCases(t, logConformanceManifest, "http")
	want := suite.Case(t, "http.terminal.success")

	rec := logtest.NewRecorder(t)
	driveRequest(t, rec, "GET", "/conformance/orders", func(*Context) *Response { return JSON("ok") })

	logtest.AssertRecord(t, rec.Record(t, want), want)
}

func TestLoggingConformanceHTTPTerminal5xx(t *testing.T) {
	suite := logtest.LoadCases(t, logConformanceManifest, "http")
	want := suite.Case(t, "http.terminal.5xx")

	rec := logtest.NewRecorder(t)
	driveRequest(t, rec, "GET", "/conformance/orders", func(ctx *Context) *Response {
		// The framework records the real handler error here (api/endpoint.go and
		// middleware_recovery.go do the same), and the terminal record reads it
		// back from the request-scoped field bag — never from the message.
		logger.SetRequestError(ctx.Context(), fmt.Errorf("boom"))
		return InternalError("Internal Server Error")
	})

	logtest.AssertRecord(t, rec.Record(t, want), want)
}
