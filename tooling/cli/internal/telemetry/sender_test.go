package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	otlp "go.putnami.dev/protocol/telemetry"
	"go.putnami.dev/protocol/telemetry/cliusage"
	"go.putnami.dev/tooling/cli/internal/hometest"
)

const otlpLogsGoldenDigest = "317c957b3218a63f8cfdb5adf5c21f91fad14ce7b9dc5b490955022a31d5e7fa"

func TestEncodeLogsGolden(t *testing.T) {
	request := encodeLogs([]Event{{
		Name:      cliusage.EventSessionEnd,
		Timestamp: "2026-01-02T03:04:05Z",
		Data: map[string]any{
			cliusage.AttrDuration:      int64(42),
			cliusage.AttrSuccess:       false,
			cliusage.AttrErrorCategory: cliusage.ErrorCategoryFailure,
			cliusage.AttrInteractive:   false,
		},
	}}, "device-123", "2026-01", "v1.2.3", "darwin", "arm64")

	got, err := otlp.MarshalCanonical(request)
	if err != nil {
		t.Fatalf("marshal OTLP logs: %v", err)
	}
	want := cliusage.GoldenLogsJSON
	if !bytes.Equal(got, bytes.TrimSpace(want)) {
		t.Errorf("OTLP logs golden mismatch\n--- got:\n%s\n--- want:\n%s", got, want)
	}
	if digest := otlp.Digest(got); digest != otlpLogsGoldenDigest {
		t.Errorf("OTLP logs digest = %s, want %s", digest, otlpLogsGoldenDigest)
	}
}

func TestEncodeLogsPreservesCommandList(t *testing.T) {
	request := encodeLogs([]Event{{
		Name:      cliusage.EventSessionStart,
		Timestamp: "2026-01-02T03:04:05Z",
		Data:      map[string]any{cliusage.AttrCommands: []string{"build", "test"}},
	}}, "device-123", "2026-01", "v1.2.3", "linux", "amd64")

	attributes := request.ResourceLogs[0].ScopeLogs[0].LogRecords[0].Attributes
	for _, attribute := range attributes {
		if attribute.Key != cliusage.AttrCommands {
			continue
		}
		if attribute.Value.StringValue == nil || *attribute.Value.StringValue != "build,test" {
			t.Fatalf("commands attribute = %#v, want comma-separated closed vocabulary", attribute.Value)
		}
		return
	}
	t.Fatal("commands attribute missing")
}

func TestEncodeLogsIncludesRequiredResourceMetadata(t *testing.T) {
	request := encodeLogs(nil, "device-123", "2026-01", "v1.2.3", "darwin", "arm64")
	attributes := request.ResourceLogs[0].Resource.Attributes
	if got := otlpStringAttribute(t, attributes, otlp.AttrServiceName); got != "putnami-cli" {
		t.Errorf("service.name = %q, want putnami-cli", got)
	}
	if got := otlpStringAttribute(t, attributes, otlp.AttrServiceVersion); got != "v1.2.3" {
		t.Errorf("service.version = %q, want v1.2.3", got)
	}
	if got := otlpStringAttribute(t, attributes, otlp.AttrPutnamiFramework); got != otlp.FrameworkGo {
		t.Errorf("putnami.framework = %q, want %q", got, otlp.FrameworkGo)
	}
}

func TestEncodeLogsUsesDevResourceVersionWhenUnset(t *testing.T) {
	request := encodeLogs(nil, "device-123", "2026-01", "", "darwin", "arm64")
	attributes := request.ResourceLogs[0].Resource.Attributes
	if got := otlpStringAttribute(t, attributes, otlp.AttrServiceVersion); got != "dev" {
		t.Errorf("service.version = %q, want dev", got)
	}
}

func TestEncodeLogsPreservesDeviceIDForEachEventMonth(t *testing.T) {
	janID := "11111111111111111111111111111111"
	febID := "22222222222222222222222222222222"
	request := encodeLogs([]Event{
		{Name: cliusage.EventSessionEnd, Timestamp: "2026-01-31T23:59:59Z", DeviceID: janID},
		{Name: cliusage.EventSessionEnd, Timestamp: "2026-02-01T00:00:00Z", DeviceID: febID},
	}, "33333333333333333333333333333333", "2026-03", "v1.2.3", "darwin", "arm64")
	records := request.ResourceLogs[0].ScopeLogs[0].LogRecords
	if got := otlpStringAttribute(t, records[0].Attributes, "device.id"); got != janID {
		t.Errorf("January device ID = %q, want %q", got, janID)
	}
	if got := otlpStringAttribute(t, records[1].Attributes, "device.id"); got != febID {
		t.Errorf("February device ID = %q, want %q", got, febID)
	}
}

func TestEncodeLogsDerivesDistinctLegacyDeviceIDsByMonth(t *testing.T) {
	currentID := "33333333333333333333333333333333"
	request := encodeLogs([]Event{
		{Name: cliusage.EventSessionEnd, Timestamp: "2026-01-01T00:00:00Z"},
		{Name: cliusage.EventSessionEnd, Timestamp: "2026-01-02T00:00:00Z"},
		{Name: cliusage.EventSessionEnd, Timestamp: "2026-02-01T00:00:00Z"},
	}, currentID, "2026-03", "v1.2.3", "darwin", "arm64")
	records := request.ResourceLogs[0].ScopeLogs[0].LogRecords
	janFirst := otlpStringAttribute(t, records[0].Attributes, "device.id")
	janSecond := otlpStringAttribute(t, records[1].Attributes, "device.id")
	february := otlpStringAttribute(t, records[2].Attributes, "device.id")
	if janFirst == "" || janFirst != janSecond {
		t.Errorf("January legacy IDs = %q, %q; want one non-empty monthly ID", janFirst, janSecond)
	}
	if february == "" || february == janFirst || february == currentID {
		t.Errorf("February legacy ID = %q, want an ID distinct from January and current month", february)
	}
}

func TestDecodeBufferedEventsDropsUnapprovedAttributes(t *testing.T) {
	events := decodeBufferedEvents([]byte(`{"name":"session:end","timestamp":"2026-01-02T03:04:05Z","data":{"duration":42,"success":false,"errorCategory":"failure","interactive":false,"path":"/private/workspace"}}` + "\n"))
	if len(events) != 1 {
		t.Fatalf("decoded events = %d, want 1", len(events))
	}
	if _, found := events[0].Data["path"]; found {
		t.Fatalf("unapproved buffer attribute survived sanitization: %#v", events[0].Data)
	}
}

func TestTelemetryEndpointResolution(t *testing.T) {
	t.Run("production default", func(t *testing.T) {
		t.Setenv(telemetryEndpointEnv, "")
		if got := telemetryEndpoint(); got != "https://telemetry.putnami.dev" {
			t.Fatalf("telemetry endpoint = %q, want production default", got)
		}
	})
	t.Run("environment override", func(t *testing.T) {
		t.Setenv(telemetryEndpointEnv, "  https://collector.example.test/  ")
		if got := telemetryEndpoint(); got != "https://collector.example.test" {
			t.Fatalf("telemetry endpoint = %q, want trimmed environment override", got)
		}
	})
}

func TestDrainSkipsBufferBeforeInteractiveNotice(t *testing.T) {
	dir := hometest.Temp(t)
	if err := writeConfig(filepath.Join(dir, configFileName), &Config{Enabled: boolPtr(true)}); err != nil {
		t.Fatalf("write telemetry config: %v", err)
	}
	bufferPath := filepath.Join(dir, bufferFileName)
	contents := []byte(`{"name":"session:end","timestamp":"2026-01-02T03:04:05Z","data":{"duration":42,"success":false,"errorCategory":"failure","interactive":false}}` + "\n")
	if err := os.WriteFile(bufferPath, contents, 0o644); err != nil {
		t.Fatalf("write telemetry buffer: %v", err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	if got := calls.Load(); got != 0 {
		t.Fatalf("network calls = %d, want 0 before notice", got)
	}
	assertBufferContents(t, bufferPath, contents)
}

func TestDrainContextCancellationMakesNoNetworkCall(t *testing.T) {
	bufferPath, contents := telemetryBuffer(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	DrainContext(ctx)

	if got := calls.Load(); got != 0 {
		t.Fatalf("network calls = %d, want 0 after cancellation", got)
	}
	assertBufferContents(t, bufferPath, contents)
}

func TestDrainKeepsBufferOnPreDispatchRequestFailure(t *testing.T) {
	bufferPath, contents := telemetryBuffer(t)
	// NewRequest rejects this endpoint before the atomic handoff. Local setup
	// failures must not turn into an intentional at-most-once drop.
	t.Setenv(telemetryEndpointEnv, "://not-a-valid-endpoint")

	Drain()

	assertBufferContents(t, bufferPath, contents)
}

func TestDrainHandsOffEncodedBufferBefore2xx(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	var received otlp.LogsRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != otlp.PathLogs {
			t.Errorf("path = %s, want %s", r.URL.Path, otlp.PathLogs)
		}
		if got := r.Header.Get("Content-Type"); got != otlp.ContentType {
			t.Errorf("Content-Type = %q, want %q", got, otlp.ContentType)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read drained buffer: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("buffer after successful drain = %q, want empty", data)
	}
	if got := len(received.ResourceLogs); got != 1 {
		t.Fatalf("resource logs = %d, want 1", got)
	}
	records := received.ResourceLogs[0].ScopeLogs[0].LogRecords
	if got := len(records); got != 1 {
		t.Fatalf("log records = %d, want 1", got)
	}
	if records[0].TimeUnixNano != "1767323045000000000" {
		t.Errorf("timeUnixNano = %q", records[0].TimeUnixNano)
	}
	assertOTLPIntAttribute(t, records[0].Attributes, cliusage.AttrDuration, "42")
}

func TestDrainDropsHandedOffBufferOnCollectorFailure(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	assertBufferContents(t, bufferPath, nil)
}

func TestDrainRetainsEventsAppendedDuringRequest(t *testing.T) {
	bufferPath, contents := telemetryBuffer(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A foreground session uses Flush rather than replacing the file. It can
		// append after the drain's short handoff lock is released, while the
		// request remains in flight.
		client := NewClient()
		client.events = []Event{{
			DeviceID:  "device-123",
			Name:      cliusage.EventSessionEnd,
			Timestamp: "2026-01-02T03:04:06Z",
			Data: map[string]any{
				cliusage.AttrSuccess:     true,
				cliusage.AttrDuration:    int64(1),
				cliusage.AttrInteractive: false,
			},
		}}
		if err := client.Flush(); err != nil {
			t.Errorf("append event while draining: %v", err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read retained buffer suffix: %v", err)
	}
	if bytes.Contains(data, contents) {
		t.Fatalf("handed-off prefix remained after drain: %q", data)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 1 {
		t.Fatalf("buffer lines after handoff and append = %d, want 1: %q", lines, data)
	}
}

func TestDisableWaitsForInFlightDrainRequest(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	requestStarted := make(chan struct{})
	respond := make(chan struct{})
	var releaseResponse sync.Once
	release := func() { releaseResponse.Do(func() { close(respond) }) }
	t.Cleanup(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-respond
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	drained := make(chan struct{})
	go func() {
		Drain()
		close(drained)
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not begin the collector request")
	}

	disabled := make(chan error, 1)
	go func() { disabled <- Disable() }()
	select {
	case err := <-disabled:
		t.Fatalf("Disable returned during the in-flight drain: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Disable correctly waits for the request holding the consent lock.
	}

	release()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not complete after the collector responded")
	}
	select {
	case err := <-disabled:
		if err != nil {
			t.Fatalf("Disable: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Disable did not complete after the drain released the consent lock")
	}
	if _, err := os.Stat(bufferPath); !os.IsNotExist(err) {
		t.Fatalf("buffer remains after Disable: %v", err)
	}
}

func TestDrainDoesNotBlockFlushDuringRequest(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	requestStarted := make(chan struct{})
	respond := make(chan struct{})
	var releaseResponse sync.Once
	release := func() { releaseResponse.Do(func() { close(respond) }) }
	t.Cleanup(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-respond
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	drained := make(chan struct{})
	go func() {
		Drain()
		close(drained)
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not begin the collector request")
	}

	client := NewClient()
	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: false})
	flushed := make(chan error, 1)
	go func() { flushed <- client.Flush() }()
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("flush during drain: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Flush waited for the in-flight telemetry request")
	}

	release()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not complete after the collector responded")
	}
	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read remaining buffered event: %v", err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 1 {
		t.Fatalf("buffer lines after drain and flush = %d, want 1: %q", lines, data)
	}
}

func TestBufferLockSerializesHandoffAndFlush(t *testing.T) {
	bufferPath, contents := telemetryBuffer(t)
	client := NewClient()
	client.TrackSessionEnd(SessionEnd{ExitCode: 0, DurationMS: 1, Interactive: false})

	held, err := acquireBufferLock(bufferPath)
	if err != nil {
		t.Fatalf("acquire held buffer lock: %v", err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = held.Release()
		}
	})

	handedOff := make(chan bool, 1)
	flushed := make(chan error, 1)
	go func() {
		handedOff <- handoffSentBufferContext(context.Background(), bufferPath, contents)
	}()
	go func() { flushed <- client.Flush() }()

	select {
	case <-handedOff:
		t.Fatal("handoff ran while the buffer lock was held")
	case err := <-flushed:
		t.Fatalf("flush ran while the buffer lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
		// Both operations correctly wait on the same lock.
	}

	if err := held.Release(); err != nil {
		t.Fatalf("release held buffer lock: %v", err)
	}
	released = true
	select {
	case ok := <-handedOff:
		if !ok {
			t.Fatal("handoff failed after buffer lock release")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handoff did not complete after buffer lock release")
	}
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("flush: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("flush did not complete after buffer lock release")
	}

	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read serialized buffer: %v", err)
	}
	if lines := bytes.Count(data, []byte("\n")); lines != 1 {
		t.Fatalf("buffer lines after concurrent handoff and flush = %d, want 1: %q", lines, data)
	}
}

func TestDrainDropsHandedOffBufferOnTimeout(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		<-releaseHandler
	}))
	defer server.Close()
	defer close(releaseHandler)
	t.Setenv(telemetryEndpointEnv, server.URL)

	started := time.Now()
	Drain()
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("Drain blocked for %s, want at most 2s plus scheduling overhead", elapsed)
	}
	assertBufferContents(t, bufferPath, nil)
}

// TestDrainHandoffDoesNotReplayAcceptedBatchAfterCancellation pins a failure
// window. The handler's counter stands in for cli_daily_counter: it
// increments as soon as the receiver accepts the POST, then withholds a
// response to model a CLI process exiting before acknowledgement. The next
// drain must make no second request, or an additive durable counter would be
// inflated.
func TestDrainHandoffDoesNotReplayAcceptedBatchAfterCancellation(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	var durableCounter atomic.Int32
	accepted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseHandler) }) }
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		durableCounter.Add(1)
		select {
		case accepted <- struct{}{}:
		default:
		}
		<-releaseHandler
	}))
	// Cleanup runs LIFO: release the blocked handler before asking httptest to
	// close its active connection, including on a fatal assertion.
	t.Cleanup(server.Close)
	t.Cleanup(release)
	t.Setenv(telemetryEndpointEnv, server.URL)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	drained := make(chan struct{})
	go func() {
		DrainContext(ctx)
		close(drained)
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not accept the handed-off batch")
	}
	cancel()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not stop after cancellation")
	}

	assertBufferContents(t, bufferPath, nil)
	Drain() // A later process sees no batch to replay.
	if got := durableCounter.Load(); got != 1 {
		t.Fatalf("durable counter = %d, want 1 after canceled response and later drain", got)
	}
	release()
}

func TestDrainDiscardsStalePreHandoffTemporary(t *testing.T) {
	bufferPath, _ := telemetryBuffer(t)
	if err := os.WriteFile(handoffTempPath(bufferPath), []byte("stale raw telemetry\n"), 0o644); err != nil {
		t.Fatalf("write stale handoff temporary: %v", err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	if got := calls.Load(); got != 1 {
		t.Fatalf("requests = %d, want retained buffer sent once", got)
	}
	if _, err := os.Stat(handoffTempPath(bufferPath)); !os.IsNotExist(err) {
		t.Fatalf("stale handoff temporary survived drain: %v", err)
	}
}

func TestDrainSkipsMalformedBufferLines(t *testing.T) {
	dir := hometest.Temp(t)
	if err := writeConfig(filepath.Join(dir, configFileName), &Config{
		Enabled:       boolPtr(true),
		NoticeShownAt: "2026-01-01T00:00:00Z",
		DeviceID:      "device-123",
		DeviceIDMonth: time.Now().UTC().Format("2006-01"),
	}); err != nil {
		t.Fatalf("write telemetry config: %v", err)
	}
	validOne := `{"name":"session:start","timestamp":"2026-01-02T03:04:05Z","data":{"commands":["build"],"projects":2,"jobs":1,"interactive":false,"flag.impacted":false,"flag.coverage":false,"flag.output":false,"flag.no-cache":false,"flag.projects":false,"flag.watch":false}}`
	validTwo := `{"name":"session:end","timestamp":"2026-01-02T03:04:06Z","data":{"success":true,"duration":1,"interactive":false}}`
	bufferPath := filepath.Join(dir, bufferFileName)
	if err := os.WriteFile(bufferPath, []byte(validOne+"\nnot-json\n"+validTwo+"\n"), 0o644); err != nil {
		t.Fatalf("write buffer: %v", err)
	}

	var records int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request otlp.LogsRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		records = len(request.ResourceLogs[0].ScopeLogs[0].LogRecords)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(telemetryEndpointEnv, server.URL)

	Drain()

	if records != 2 {
		t.Fatalf("sent records = %d, want 2 valid buffer lines", records)
	}
	data, err := os.ReadFile(bufferPath)
	if err != nil {
		t.Fatalf("read drained buffer: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("buffer after successful drain = %q, want empty", data)
	}
}

func telemetryBuffer(t *testing.T) (string, []byte) {
	t.Helper()
	dir := hometest.Temp(t)
	if err := writeConfig(filepath.Join(dir, configFileName), &Config{
		Enabled:       boolPtr(true),
		NoticeShownAt: "2026-01-01T00:00:00Z",
		DeviceID:      "device-123",
		DeviceIDMonth: time.Now().UTC().Format("2006-01"),
	}); err != nil {
		t.Fatalf("write telemetry config: %v", err)
	}
	contents := []byte(`{"name":"session:end","timestamp":"2026-01-02T03:04:05Z","data":{"duration":42,"success":false,"errorCategory":"failure","interactive":false}}` + "\n")
	path := filepath.Join(dir, bufferFileName)
	if err := os.WriteFile(path, contents, 0o644); err != nil {
		t.Fatalf("write telemetry buffer: %v", err)
	}
	return path, contents
}

func assertBufferContents(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read telemetry buffer: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("buffer changed\n--- got:\n%s\n--- want:\n%s", got, want)
	}
}

func otlpStringAttribute(t *testing.T, attrs []otlp.KeyValue, key string) string {
	t.Helper()
	for _, attribute := range attrs {
		if attribute.Key != key {
			continue
		}
		if attribute.Value.StringValue == nil {
			t.Fatalf("attribute %q = %#v, want string value", key, attribute.Value)
		}
		return *attribute.Value.StringValue
	}
	t.Fatalf("attribute %q missing", key)
	return ""
}

func assertOTLPIntAttribute(t *testing.T, attrs []otlp.KeyValue, key, want string) {
	t.Helper()
	for _, attr := range attrs {
		if attr.Key != key {
			continue
		}
		if attr.Value.IntValue == nil || *attr.Value.IntValue != want {
			t.Fatalf("attribute %q = %#v, want int value %q", key, attr.Value, want)
		}
		return
	}
	t.Fatalf("attribute %q missing", key)
}
