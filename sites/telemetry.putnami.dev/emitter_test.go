package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/logger"
	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

type staticTokenSource struct {
	token string
	err   error
}

func (s staticTokenSource) source() bearerTokenSource {
	return func(context.Context) (string, error) { return s.token, s.err }
}

func testLogger() *logger.Logger { return logger.Default().Named("test") }

func goldenResourceLogs(t *testing.T) []telemetry.ResourceLogs {
	t.Helper()
	req, diags := telemetry.ParseLogsRequest(cliusage.GoldenLogsJSON)
	if len(diags) != 0 {
		t.Fatalf("parse golden: %v", diags)
	}
	return sanitizeLogs(req).ResourceLogs
}

// TestExporterFlushToCollector proves the exporter POSTs OTLP/JSON to the
// configured collector endpoint at /v1/logs.
func TestExporterFlushToCollector(t *testing.T) {
	var (
		mu       sync.Mutex
		gotPath  string
		gotCT    string
		gotBody  []byte
		received bool
	)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotPath = r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody = body
		received = true
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()

	exp := newOTLPExporter(collector.URL, 0, 0, nil, testLogger())
	defer exp.Close()

	exp.Emit(goldenResourceLogs(t))
	if err := exp.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !received {
		t.Fatal("collector received nothing")
	}
	if gotPath != telemetry.PathLogs {
		t.Fatalf("collector path = %q, want %q", gotPath, telemetry.PathLogs)
	}
	if gotCT != telemetry.ContentType {
		t.Fatalf("content-type = %q, want %q", gotCT, telemetry.ContentType)
	}
	if !strings.Contains(string(gotBody), cliusage.ServiceName) || !strings.Contains(string(gotBody), originCLIAnon) {
		t.Fatalf("collector body missing expected content: %s", gotBody)
	}
}

func TestExporterAuthenticatesCollectorRequest(t *testing.T) {
	var gotAuthorization string
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer collector.Close()

	exp := newOTLPExporter(
		collector.URL,
		time.Hour,
		time.Second,
		staticTokenSource{token: "identity-token"}.source(),
		testLogger(),
	)
	defer exp.Close()

	exp.Emit(goldenResourceLogs(t))
	if err := exp.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if gotAuthorization != "Bearer identity-token" {
		t.Fatalf("Authorization = %q, want bearer identity token", gotAuthorization)
	}
}

func TestExporterTokenFailureDropsWithoutRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source staticTokenSource
	}{
		{name: "source error", source: staticTokenSource{err: errors.New("metadata unavailable")}},
		{name: "empty token", source: staticTokenSource{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests int
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(http.StatusAccepted)
			}))
			defer collector.Close()

			exp := newOTLPExporter(collector.URL, time.Hour, time.Second, tc.source.source(), testLogger())
			defer exp.Close()

			exp.Emit(goldenResourceLogs(t))
			if err := exp.Flush(); err != nil {
				t.Fatalf("flush must swallow token errors, got %v", err)
			}
			if requests != 0 {
				t.Fatalf("collector requests = %d, want 0", requests)
			}
		})
	}
}

// TestExporterCollectorDownDrops proves a wedged/unreachable collector never
// surfaces an error and the batch is dropped.
func TestExporterCollectorDownDrops(t *testing.T) {
	exp := newOTLPExporter("http://127.0.0.1:1", 0, 50*time.Millisecond, nil, testLogger())
	defer exp.Close()

	exp.Emit(goldenResourceLogs(t))
	if err := exp.Flush(); err != nil {
		t.Fatalf("flush must swallow errors, got %v", err)
	}
	// Buffer drained regardless of the transport failure.
	exp.mu.Lock()
	n := len(exp.buf)
	exp.mu.Unlock()
	if n != 0 {
		t.Fatalf("buffer not drained on failure: %d", n)
	}
}

// TestExporterBoundedQueueDropsOldest proves the queue is bounded (drop-oldest)
// so a wedged collector cannot grow memory without limit.
func TestExporterBoundedQueueDropsOldest(t *testing.T) {
	exp := newOTLPExporter("http://127.0.0.1:1", time.Hour, 50*time.Millisecond, nil, testLogger())
	defer exp.Close()
	exp.mu.Lock()
	exp.maxQueue = 2
	exp.mu.Unlock()

	for i := 0; i < 5; i++ {
		exp.Emit([]telemetry.ResourceLogs{{
			Resource: telemetry.Resource{Attributes: []telemetry.KeyValue{
				telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal(cliusage.ServiceName)),
			}},
		}})
	}

	exp.mu.Lock()
	n := len(exp.buf)
	exp.mu.Unlock()
	if n != 2 {
		t.Fatalf("bounded queue kept %d entries, want 2", n)
	}
}

func TestNoopEmitterDoesNothing(t *testing.T) {
	// Compiles-and-runs guard: the noop emitter must accept and drop silently.
	noopEmitter{}.Emit(goldenResourceLogs(t))
}
