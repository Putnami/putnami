package http

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/logger"
	runtimeproto "go.putnami.dev/protocol/runtime"

	"go.putnami.dev/protocol/features/spectest"
)

// startOnEphemeralPort starts the plugin on an ephemeral port and returns the
// port the listener actually bound, read back from the readiness marker the
// framework attaches to its own listening record.
func startOnEphemeralPort(t *testing.T, plugin *ServerPlugin) int {
	t.Helper()
	t.Setenv("PORT", "0")

	var out bytes.Buffer
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
			continue
		}
		data, ok := runtimeproto.ReadyMarkerFromLogRecord(record)
		if !ok || len(data.Endpoints) == 0 {
			continue
		}
		return data.Endpoints[0].Port
	}
	t.Fatalf("no bound port in server output:\n%s", out.String())
	return 0
}

// TestServerConfigShutdownTimeoutDefault pins the resolution of the drain
// deadline. `default:"10s"` is applied by config loading only, so a directly
// constructed ServerConfig — the shape every example and sample uses — carries
// zero and must still get the documented default. A negative value is the
// explicit opt-out that defers to the caller's context.
func TestServerConfigShutdownTimeoutDefault(t *testing.T) {
	spectest.Proves(t, "go/http-services", "graceful-shutdown", "shutdown-timeout-resolves-zero-explicit-and-negative")
	if got := (ServerConfig{}).shutdownTimeout(); got != defaultShutdownTimeout {
		t.Errorf("zero ShutdownTimeout = %v, want %v", got, defaultShutdownTimeout)
	}
	if got := (ServerConfig{ShutdownTimeout: 3 * time.Second}).shutdownTimeout(); got != 3*time.Second {
		t.Errorf("explicit ShutdownTimeout = %v, want 3s", got)
	}
	if got := (ServerConfig{ShutdownTimeout: -1}).shutdownTimeout(); got != -1 {
		t.Errorf("negative ShutdownTimeout = %v, want the caller-bounded opt-out", got)
	}
}

// TestServerConfigDefaultTagsMatchResolvedDefaults keeps the two default paths
// honest: the `default:` struct tag drives config-loaded construction and the
// package constants drive direct construction, so they must agree field by
// field.
func TestServerConfigDefaultTagsMatchResolvedDefaults(t *testing.T) {
	spectest.Proves(t, "go/http-services", "graceful-shutdown", "default-tags-match-the-resolved-defaults")
	resolved := map[string]time.Duration{
		"ShutdownTimeout":      defaultShutdownTimeout,
		"IdleTimeout":          defaultIdleTimeout,
		"WebSocketIdleTimeout": defaultWebSocketIdleTimeout,
		"StreamWriteTimeout":   defaultStreamWriteTimeout,
	}
	configType := reflect.TypeFor[ServerConfig]()
	for name, want := range resolved {
		field, ok := configType.FieldByName(name)
		if !ok {
			t.Fatalf("ServerConfig has no field %s", name)
		}
		tag, ok := field.Tag.Lookup("default")
		if !ok {
			t.Fatalf("%s carries no default tag", name)
		}
		got, err := time.ParseDuration(tag)
		if err != nil {
			t.Fatalf("%s default tag %q is not a duration: %v", name, tag, err)
		}
		if got != want {
			t.Errorf("%s default tag = %v, resolved default = %v", name, got, want)
		}
	}
}

// TestServerPlugin_StopDrainsInFlightRequestWithUnsetTimeout is the regression
// for the graceful-shutdown contract: with ShutdownTimeout left unset, Stop
// used to build an already-expired context, so Shutdown returned
// context.DeadlineExceeded immediately and the in-flight request was abandoned
// instead of drained.
func TestServerPlugin_StopDrainsInFlightRequestWithUnsetTimeout(t *testing.T) {
	spectest.Proves(t, "go/http-services", "graceful-shutdown", "stop-drains-in-flight-requests-with-an-unset-timeout")
	handlerEntered := make(chan struct{})
	release := make(chan struct{})

	plugin := NewServerPlugin(ServerConfig{})
	plugin.GET("/slow", func(_ *Context) *Response {
		close(handlerEntered)
		<-release
		return Text("drained")
	})
	port := startOnEphemeralPort(t, plugin)

	type result struct {
		status int
		err    error
	}
	responses := make(chan result, 1)
	go func() {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/slow", port)) //nolint:noctx // bounded by the test's own release channel
		if err != nil {
			responses <- result{err: err}
			return
		}
		defer resp.Body.Close() //nolint:errcheck // response body close in test
		responses <- result{status: resp.StatusCode}
	}()

	select {
	case <-handlerEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the request")
	}

	stopped := make(chan error, 1)
	go func() {
		stopped <- plugin.Stop(context.Background(), nil)
	}()

	// Give Stop a moment to reach Shutdown while the request is still in
	// flight, then let the handler finish. A zero drain deadline would have
	// already returned by now.
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("Stop with unset ShutdownTimeout must drain, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}

	select {
	case got := <-responses:
		if got.err != nil {
			t.Fatalf("in-flight request failed during graceful shutdown: %v", got.err)
		}
		if got.status != http.StatusOK {
			t.Errorf("in-flight request status = %d, want 200", got.status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request never completed")
	}
}

// TestServerPlugin_StopConcurrentWithStart runs Stop while Start may still be
// building the server. Start and Stop reach the server field through the
// plugin lock, so the race detector reports nothing in either order.
func TestServerPlugin_StopConcurrentWithStart(t *testing.T) {
	t.Setenv("PORT", "0")
	plugin := NewServerPlugin(ServerConfig{})
	plugin.log = logger.New("http", logger.LevelError, logger.NewJSONSinkWriter(io.Discard))
	started := make(chan error, 1)
	go func() { started <- plugin.Start(context.Background(), nil) }()
	if err := plugin.Stop(context.Background(), nil); err != nil {
		t.Errorf("stop during start: %v", err)
	}
	if err := <-started; err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := plugin.Stop(context.Background(), nil); err != nil {
		t.Errorf("stop after start: %v", err)
	}
}
