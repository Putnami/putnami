package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"go.putnami.dev/logger"
	runtimeproto "go.putnami.dev/protocol/runtime"

	"go.putnami.dev/protocol/features/spectest"
)

// startAndCaptureListeningRecord starts the plugin with its logs going to the
// production JSON sink and returns the decoded listening record — the exact
// bytes an extension's forwarder reads off the served workload's stdout.
func startAndCaptureListeningRecord(t *testing.T) map[string]any {
	t.Helper()
	t.Setenv("PORT", "0") // ephemeral: never collide with a real service

	var out bytes.Buffer
	plugin := NewServerPlugin(ServerConfig{})
	plugin.log = logger.New("http", logger.LevelDebug, logger.NewJSONSinkWriter(&out))

	if err := plugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if plugin.server != nil {
			_ = plugin.server.Close()
		}
	})

	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %v\n%s", err, line)
		}
		if message, _ := record["message"].(string); strings.Contains(message, "listening http://") {
			return record
		}
	}
	t.Fatalf("no listening record in server output:\n%s", out.String())
	return nil
}

// TestServerPlugin_ListeningLogCarriesReadinessMarker is the Go half of B6a's
// first-party emission. The framework cannot write runtime events — its stdout
// is a log stream owned by whoever spawned it — so it announces readiness by
// attaching the reserved marker to the log record it already writes.
func TestServerPlugin_ListeningLogCarriesReadinessMarker(t *testing.T) {
	spectest.Proves(t, "go/http-services", "readiness-marker", "listening-log-carries-the-readiness-marker")
	record := startAndCaptureListeningRecord(t)

	data, ok := runtimeproto.ReadyMarkerFromLogRecord(record)
	if !ok {
		t.Fatalf("listening record carries no valid readiness marker: %v", record)
	}
	if data.Target != runtimeproto.ReadyTargetServer {
		t.Errorf("target = %q, want %q", data.Target, runtimeproto.ReadyTargetServer)
	}
	if len(data.Endpoints) != 1 {
		t.Fatalf("endpoints = %+v, want exactly one", data.Endpoints)
	}
	endpoint := data.Endpoints[0]
	if endpoint.Scheme != runtimeproto.ReadySchemeHTTP || endpoint.Host != "localhost" {
		t.Errorf("endpoint = %+v, want an http://localhost address", endpoint)
	}
	// PORT=0 asks the kernel for an ephemeral port: the marker must carry the
	// BOUND port, or a consumer would be handed an address nothing listens on.
	if endpoint.Port <= 0 {
		t.Errorf("endpoint port = %d, want the bound ephemeral port", endpoint.Port)
	}
	if data.DurationMs < 0 {
		t.Errorf("durationMs = %d, want a non-negative startup duration", data.DurationMs)
	}
}

// TestServerPlugin_ListeningLogStaysHumanReadable is the additivity guard:
// typing readiness must not cost a developer the line they actually read. The
// structured marker is the only machine-facing readiness path, so nothing else
// depends on this message any more — which is precisely why it needs a guard of
// its own rather than none.
func TestServerPlugin_ListeningLogStaysHumanReadable(t *testing.T) {
	record := startAndCaptureListeningRecord(t)

	message, _ := record["message"].(string)
	if !strings.Contains(message, "listening http://") {
		t.Errorf("listening message = %q; a developer reads this line to find their server", message)
	}
	if record["severity"] != "INFO" {
		t.Errorf("severity = %v, want INFO", record["severity"])
	}
	if _, ok := record["durationMs"]; !ok {
		t.Error("the existing durationMs field must survive")
	}
}

// TestReadyMarker_DegradesToWorkloadWithoutAddressableListener pins the
// fail-closed direction: a listener whose address is not an addressable TCP
// port yields a workload claim (startup did finish) instead of a server claim
// with no address, which the protocol rejects outright.
func TestReadyMarker_DegradesToWorkloadWithoutAddressableListener(t *testing.T) {
	spectest.Proves(t, "go/http-services", "readiness-marker", "readiness-degrades-to-a-workload-claim")
	data := readyMarker(unaddressableListener{}, 12)
	if data.Target != runtimeproto.ReadyTargetWorkload {
		t.Errorf("target = %q, want %q", data.Target, runtimeproto.ReadyTargetWorkload)
	}
	if len(data.Endpoints) != 0 {
		t.Errorf("endpoints = %+v, want none", data.Endpoints)
	}
	if data.DurationMs != 12 {
		t.Errorf("durationMs = %d, want 12", data.DurationMs)
	}
}

// TestReadyMarker_IsCanonical pins determinism at the source: the marker a
// workload writes is already in canonical endpoint order, so two runs produce
// the same log bytes.
func TestReadyMarker_IsCanonical(t *testing.T) {
	spectest.Proves(t, "go/http-services", "readiness-marker", "readiness-marker-is-canonical")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	first := readyMarker(ln, 7)
	second := readyMarker(ln, 7)
	firstJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := json.Marshal(second)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstJSON) != string(secondJSON) {
		t.Errorf("marker is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(string(firstJSON), `"url"`) {
		t.Errorf("the endpoint address is derived, never emitted: %s", firstJSON)
	}
}

// unaddressableListener is a net.Listener whose address is not a *net.TCPAddr.
type unaddressableListener struct{ net.Listener }

func (unaddressableListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "/tmp/app.sock", Net: "unix"}
}
