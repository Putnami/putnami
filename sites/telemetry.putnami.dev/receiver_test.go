package main

import (
	"go.putnami.dev/protocol/features/spectest"

	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	telemetry "go.putnami.dev/protocol/telemetry"
	cliusage "go.putnami.dev/protocol/telemetry/cliusage"
)

// fakeEmitter captures emitted resource logs for assertions.
type fakeEmitter struct {
	mu   sync.Mutex
	logs []telemetry.ResourceLogs
}

func (f *fakeEmitter) Emit(rl []telemetry.ResourceLogs) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = append(f.logs, rl...)
}

func (f *fakeEmitter) captured() []telemetry.ResourceLogs {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]telemetry.ResourceLogs, len(f.logs))
	copy(out, f.logs)
	return out
}

func permissiveConfig() receiverConfig {
	return receiverConfig{port: 0, perIPPerMinute: 1_000_000, globalQPS: 1_000_000}
}

func post(t *testing.T, ts *httptest.Server, path string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest("POST", ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", telemetry.ContentType)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEndpointStatusMatrix(t *testing.T) {
	ts := newServer(permissiveConfig(), &fakeEmitter{}, nil).TestServer()
	defer ts.Close()

	t.Run("accepted returns 202", func(t *testing.T) {
		resp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("want 202, got %d", resp.StatusCode)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		resp := post(t, ts, telemetry.PathLogs, []byte("not json{"), nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("want 400, got %d", resp.StatusCode)
		}
	})

	t.Run("oversize body returns 413", func(t *testing.T) {
		big := make([]byte, maxBodyBytes+4096)
		for i := range big {
			big[i] = 'a'
		}
		resp := post(t, ts, telemetry.PathLogs, big, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("want 413, got %d", resp.StatusCode)
		}
	})

	t.Run("wrong signal path /v1/metrics returns 404", func(t *testing.T) {
		resp := post(t, ts, telemetry.PathMetrics, cliusage.GoldenLogsJSON, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})

	t.Run("wrong signal path /v1/traces returns 404", func(t *testing.T) {
		resp := post(t, ts, telemetry.PathTraces, cliusage.GoldenLogsJSON, nil)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("want 404, got %d", resp.StatusCode)
		}
	})
}

// TestUnnormalizablePayloadDropsTo202 proves a content problem never earns a 400
// (fail-silent anonymous contract): a decodable-but-non-conforming payload is
// accepted (202) and silently dropped, emitting nothing.
func TestUnnormalizablePayloadDropsTo202(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "acceptance-is-not-an-oracle", "a-content-problem-answers-like-a-clean-accept")
	em := &fakeEmitter{}
	ts := newServer(permissiveConfig(), em, nil).TestServer()
	defer ts.Close()

	// Structurally valid OTLP logs, but not CLI usage telemetry.
	body, err := telemetry.MarshalCanonical(telemetry.LogsRequest{ResourceLogs: []telemetry.ResourceLogs{{
		Resource: telemetry.Resource{Attributes: []telemetry.KeyValue{
			telemetry.Attr(telemetry.AttrServiceName, telemetry.StringVal("some-other-service")),
		}},
		ScopeLogs: []telemetry.ScopeLogs{{LogRecords: []telemetry.LogRecord{{
			SeverityNumber: telemetry.SeverityInfo,
			Attributes:     []telemetry.KeyValue{telemetry.Attr("foo", telemetry.StringVal("bar"))},
		}}}},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	resp := post(t, ts, telemetry.PathLogs, body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	if got := em.captured(); len(got) != 0 {
		t.Fatalf("expected nothing emitted, got %+v", got)
	}
}

func TestPerIPRateLimit429(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "caller-address-is-not-retained", "the-caller-address-is-used-to-apply-a-per-caller-limit")
	cfg := receiverConfig{port: 0, perIPPerMinute: 2, globalQPS: 1_000_000}
	ts := newServer(cfg, &fakeEmitter{}, nil).TestServer()
	defer ts.Close()

	var statuses []int
	for i := 0; i < 3; i++ {
		resp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
		statuses = append(statuses, resp.StatusCode)
		resp.Body.Close()
	}
	if statuses[0] != http.StatusAccepted || statuses[1] != http.StatusAccepted {
		t.Fatalf("first two requests should be 202, got %v", statuses)
	}
	if statuses[2] != http.StatusTooManyRequests {
		t.Fatalf("third request should be 429, got %v", statuses)
	}
}

func TestGlobalRateLimit429(t *testing.T) {
	cfg := receiverConfig{port: 0, perIPPerMinute: 1_000_000, globalQPS: 2}
	ts := newServer(cfg, &fakeEmitter{}, nil).TestServer()
	defer ts.Close()

	var got429 bool
	for i := 0; i < 3; i++ {
		resp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
		if resp.StatusCode == http.StatusTooManyRequests {
			got429 = true
		}
		resp.Body.Close()
	}
	if !got429 {
		t.Fatal("expected a 429 from the global limiter within 3 requests")
	}
}

// TestCollectorDownIndistinguishableFromSuccess proves an unauthenticated caller
// gets a byte-identical 202 whether persistence succeeds or the collector is
// unreachable — no retry lever leaks.
func TestCollectorDownIndistinguishableFromSuccess(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "acceptance-is-not-an-oracle", "an-unavailable-downstream-answers-like-a-clean-accept")
	// Success path: capturing emitter.
	okServer := newServer(permissiveConfig(), &fakeEmitter{}, nil).TestServer()
	defer okServer.Close()
	okResp := post(t, okServer, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
	okStatus := okResp.StatusCode
	okBody, _ := io.ReadAll(okResp.Body)
	okResp.Body.Close()

	// Failure path: real exporter pointed at a closed port (connection refused).
	down := newOTLPExporter("http://127.0.0.1:1", 0, 0, nil, testLogger())
	defer down.Close()
	downServer := newServer(permissiveConfig(), down, nil).TestServer()
	defer downServer.Close()
	downResp := post(t, downServer, telemetry.PathLogs, cliusage.GoldenLogsJSON, nil)
	downStatus := downResp.StatusCode
	downBody, _ := io.ReadAll(downResp.Body)
	downResp.Body.Close()

	if okStatus != http.StatusAccepted || downStatus != http.StatusAccepted {
		t.Fatalf("both must be 202: ok=%d down=%d", okStatus, downStatus)
	}
	if !bytes.Equal(okBody, downBody) {
		t.Fatalf("response bodies differ: ok=%q down=%q", okBody, downBody)
	}
}

// TestNoClientIPInEmittedPayload asserts the sanitized/persisted telemetry never
// contains the client's IP (from RemoteAddr or X-Forwarded-For). The IP is used
// only as an in-memory rate-limit key, never persisted.
func TestNoClientIPInEmittedPayload(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "caller-address-is-not-retained", "no-caller-address-reaches-an-emitted-event")
	em := &fakeEmitter{}
	ts := newServer(permissiveConfig(), em, nil).TestServer()
	defer ts.Close()

	const clientIPValue = "203.0.113.77"
	resp := post(t, ts, telemetry.PathLogs, cliusage.GoldenLogsJSON, map[string]string{
		"X-Forwarded-For": clientIPValue,
	})
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	captured := em.captured()
	if len(captured) != 1 {
		t.Fatalf("expected exactly one emitted resource logs, got %d", len(captured))
	}
	body, err := telemetry.MarshalCanonical(telemetry.LogsRequest{ResourceLogs: captured})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), clientIPValue) {
		t.Fatalf("client IP leaked into emitted telemetry: %s", body)
	}
	// Sanity: the emitted payload is the sanitized CLI usage series.
	if !strings.Contains(string(body), originCLIAnon) {
		t.Fatalf("expected origin=%s stamp in emitted payload: %s", originCLIAnon, body)
	}
}
