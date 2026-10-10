package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/app"
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

// TestListening_AnUnaddressableListenerWritesNoMarker pins the fail-closed
// direction: a listener whose address is not an addressable TCP port writes
// its listening record without a readiness marker and reports no endpoint. The
// application's own ready record announces completed startup.
func TestListening_AnUnaddressableListenerWritesNoMarker(t *testing.T) {
	spectest.Proves(t, "go/http-services", "readiness-marker", "an-unaddressable-listener-writes-no-marker")
	attrs, endpoints := listening(unaddressableListener{}, 12)
	for _, attr := range attrs {
		if attr.Key == runtimeproto.ReadyLogKey {
			t.Errorf("the listening record carries a readiness marker: %v", attr.Value)
		}
	}
	if len(attrs) != 1 || attrs[0].Key != "durationMs" || attrs[0].Value.Int64() != 12 {
		t.Errorf("attrs = %v, want durationMs alone", attrs)
	}
	if len(endpoints) != 0 {
		t.Errorf("endpoints = %+v, want none", endpoints)
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

	firstJSON := listeningMarkerJSON(t, ln)
	secondJSON := listeningMarkerJSON(t, ln)
	if firstJSON != secondJSON {
		t.Errorf("marker is not deterministic:\n%s\n%s", firstJSON, secondJSON)
	}
	if strings.Contains(firstJSON, `"url"`) {
		t.Errorf("the endpoint address is derived, never emitted: %s", firstJSON)
	}
}

// listeningMarkerJSON returns the encoded readiness marker of ln's listening
// record.
func listeningMarkerJSON(t *testing.T, ln net.Listener) string {
	t.Helper()
	attrs, _ := listening(ln, 7)
	for _, attr := range attrs {
		if attr.Key == runtimeproto.ReadyLogKey {
			encoded, err := json.Marshal(attr.Value.Any())
			if err != nil {
				t.Fatal(err)
			}
			return string(encoded)
		}
	}
	t.Fatalf("the listening record of an addressable listener carries no marker: %v", attrs)
	return ""
}

// TestServerPlugin_ReportsTheEndpointItBound pins the server's half of the
// application's workload claim: from Start until Stop, ReadyEndpoints is
// exactly the address the server claim announced.
func TestServerPlugin_ReportsTheEndpointItBound(t *testing.T) {
	spectest.Proves(t, "go/http-services", "readiness-marker", "the-server-reports-the-endpoint-it-bound")
	t.Setenv("PORT", "0")
	var out bytes.Buffer
	plugin := NewServerPlugin(ServerConfig{})
	plugin.log = logger.New("http", logger.LevelDebug, logger.NewJSONSinkWriter(&out))
	if endpoints := plugin.ReadyEndpoints(); len(endpoints) != 0 {
		t.Errorf("endpoints before Start = %+v, want none", endpoints)
	}

	if err := plugin.Start(context.Background(), nil); err != nil {
		t.Fatalf("start server: %v", err)
	}
	var claim *runtimeproto.ReadyData
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		if data, ok := runtimeproto.ReadyMarkerFromLogRecord(record); ok {
			claim = data
		}
	}
	if claim == nil {
		t.Fatalf("no server claim in the server output:\n%s", out.String())
	}
	if endpoints := plugin.ReadyEndpoints(); !slices.Equal(endpoints, claim.Endpoints) {
		t.Errorf("endpoints after Start = %+v, want the server claim's %+v", endpoints, claim.Endpoints)
	}

	if err := plugin.Stop(context.Background(), nil); err != nil {
		t.Fatalf("stop server: %v", err)
	}
	if endpoints := plugin.ReadyEndpoints(); len(endpoints) != 0 {
		t.Errorf("endpoints after Stop = %+v, want none", endpoints)
	}
}

// readyChildEnv makes the test binary run one application that holds a server,
// with the production log sink on its stdout, instead of the test.
const readyChildEnv = "PUTNAMI_HTTP_READY_CHILD"

// readyChildHangDetector bounds the child application. It detects a hang; it
// is never a latency assertion.
const readyChildHangDetector = 60 * time.Second

// TestApplication_ReadyRecordCarriesTheServerEndpoint runs an application with
// a server in a child process and reads its stdout, the stream an extension's
// forwarder reads: the application's workload claim carries the address the
// server claim announced.
func TestApplication_ReadyRecordCarriesTheServerEndpoint(t *testing.T) {
	if os.Getenv(readyChildEnv) == "1" {
		runReadyChild(t)
		return
	}
	spectest.Proves(t, "go/http-services", "readiness-marker", "the-application-ready-record-carries-the-bound-endpoint")
	ctx, cancel := context.WithTimeout(context.Background(), readyChildHangDetector)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestApplication_ReadyRecordCarriesTheServerEndpoint$")
	cmd.Env = append(os.Environ(), readyChildEnv+"=1", "PORT=0", "LOG_LEVEL=info")
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("child application: %v\n%s", err, stdout)
	}

	claims := map[string]*runtimeproto.ReadyData{}
	for _, line := range bytes.Split(stdout, []byte("\n")) {
		var record map[string]any
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if data, ok := runtimeproto.ReadyMarkerFromLogRecord(record); ok {
			if _, seen := claims[data.Target]; seen {
				t.Errorf("the child wrote more than one %s claim:\n%s", data.Target, stdout)
			}
			claims[data.Target] = data
		}
	}
	server, workload := claims[runtimeproto.ReadyTargetServer], claims[runtimeproto.ReadyTargetWorkload]
	if server == nil || workload == nil {
		t.Fatalf("claims = %v, want a server and a workload claim:\n%s", claims, stdout)
	}
	if len(workload.Endpoints) != 1 || !slices.Equal(workload.Endpoints, server.Endpoints) {
		t.Errorf("workload claim endpoints = %+v, want the server claim's %+v", workload.Endpoints, server.Endpoints)
	}
}

// runReadyChild starts and stops an application that holds one server.
func runReadyChild(t *testing.T) {
	a := app.New("ready-child")
	a.Use(NewServerPlugin(ServerConfig{}))
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// unaddressableListener is a net.Listener whose address is not a *net.TCPAddr.
type unaddressableListener struct{ net.Listener }

func (unaddressableListener) Addr() net.Addr {
	return &net.UnixAddr{Name: "/tmp/app.sock", Net: "unix"}
}
