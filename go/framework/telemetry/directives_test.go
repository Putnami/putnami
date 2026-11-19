package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
	otlp "go.putnami.dev/protocol/telemetry"
)

const directivesFixturePath = "../../../protocols/telemetry/fixtures/conventions/export-directives.json"

// testClock is a clock a test moves by hand.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(at time.Time) {
	c.mu.Lock()
	c.now = at
	c.mu.Unlock()
}

// useDirectiveReceiver replaces the process receiver with one on clock for the
// test, and removes the level limit it installs.
func useDirectiveReceiver(t *testing.T, clock *testClock) *directiveReceiver {
	t.Helper()
	saved, savedDelays := processDirectives, processDelays
	receiver := newDirectiveReceiver(clock.Now)
	processDirectives, processDelays = receiver, &signalDelays{}
	t.Cleanup(func() {
		processDirectives, processDelays = saved, savedDelays
		logger.SetLevelLimit(nil)
	})
	return receiver
}

// collectorStub answers each export with the status and headers its answer
// function gives the request path, and counts the requests per path.
type collectorStub struct {
	*httptest.Server
	mu       sync.Mutex
	requests map[string]int
}

func newCollectorStub(t *testing.T, answer func(path string, header http.Header) int) *collectorStub {
	t.Helper()
	stub := &collectorStub{requests: map[string]int{}}
	stub.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stub.mu.Lock()
		stub.requests[r.URL.Path]++
		stub.mu.Unlock()
		w.WriteHeader(answer(r.URL.Path, w.Header()))
	}))
	t.Cleanup(stub.Close)
	return stub
}

func (s *collectorStub) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests[path]
}

func newTestClient(endpoint string, clock *testClock) *otlpClient {
	client := newOTLPClient(OTLPConfig{Endpoint: endpoint})
	client.now = clock.Now
	return client
}

// directiveEvent is a directive arriving, a root span or a log record, at a
// second of a fixture scenario.
type directiveEvent struct {
	AtSecond  int64  `json:"atSecond"`
	Directive string `json:"directive"`
	TraceID   string `json:"traceId"`
	Kept      bool   `json:"kept"`
	Record    string `json:"record"`
	Emitted   bool   `json:"emitted"`
}

type directivesFixture struct {
	TraceScenarios []struct {
		Name     string           `json:"name"`
		Settings TraceSampling    `json:"settings"`
		Events   []directiveEvent `json:"events"`
	} `json:"traceScenarios"`
	LogScenarios []struct {
		Name   string           `json:"name"`
		Level  string           `json:"level"`
		Events []directiveEvent `json:"events"`
	} `json:"logScenarios"`
}

func TestDirectives_TheGoRuntimeAppliesTheFixtureDirectives(t *testing.T) {
	spectest.Proves(t, "telemetry/otlp-push-contract", "export-directives-are-shared", "the-go-runtime-applies-the-fixture-directives")
	data, err := os.ReadFile(directivesFixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture directivesFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.TraceScenarios) == 0 || len(fixture.LogScenarios) == 0 {
		t.Fatal("the fixture holds no trace or no log scenario")
	}
	for _, scenario := range fixture.TraceScenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			clock := &testClock{}
			receiver := useDirectiveReceiver(t, clock)
			sampler := newSampler(scenario.Settings)
			sampler.now = clock.Now
			receiver.register(sampler)
			root := sdktrace.ParentBased(sampler)
			for i, event := range scenario.Events {
				clock.Set(time.Unix(event.AtSecond, 0))
				if event.Directive != "" {
					receiver.observe(http.Header{otlp.HeaderExportDirective: {event.Directive}})
					continue
				}
				var id trace.TraceID
				if _, err := hex.Decode(id[:], []byte(event.TraceID)); err != nil {
					t.Fatal(err)
				}
				result := root.ShouldSample(sdktrace.SamplingParameters{ParentContext: context.Background(), TraceID: id, Name: "root"})
				if kept := result.Decision == sdktrace.RecordAndSample; kept != event.Kept {
					t.Errorf("event %d at %ds: kept = %v, want %v", i, event.AtSecond, kept, event.Kept)
				}
			}
		})
	}
	for _, scenario := range fixture.LogScenarios {
		t.Run(scenario.Name, func(t *testing.T) {
			clock := &testClock{}
			receiver := useDirectiveReceiver(t, clock)
			var buf bytes.Buffer
			log := logger.New("directive", logger.ParseLevel(scenario.Level), logger.NewJSONSinkWriter(&buf))
			for i, event := range scenario.Events {
				clock.Set(time.Unix(event.AtSecond, 0))
				if event.Directive != "" {
					receiver.observe(http.Header{otlp.HeaderExportDirective: {event.Directive}})
					continue
				}
				buf.Reset()
				logAt(log, event.Record)
				if emitted := buf.Len() > 0; emitted != event.Emitted {
					t.Errorf("event %d at %ds: a %s record emitted = %v, want %v", i, event.AtSecond, event.Record, emitted, event.Emitted)
				}
			}
		})
	}
}

func TestDirectives_ADirectiveTightensTheSamplerUntilItsTTL(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "export-directives", "a-directive-tightens-the-sampler-until-its-ttl")
	clock := &testClock{now: time.Unix(1000, 0)}
	useDirectiveReceiver(t, clock)
	own := TraceSampling{Budget: 0, Ratio: 0.5, FloorPerMinute: 3}
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &own})
	collector := newCollectorStub(t, func(path string, header http.Header) int {
		if path == otlp.PathMetrics {
			header.Set(otlp.HeaderExportDirective, "trace-ratio=0.1, trace-floor=1, ttl=60")
		}
		return http.StatusOK
	})

	// A metrics response carries the directive; it applies to the traces.
	if err := newTestClient(collector.URL, clock).post(context.Background(), otlp.PathMetrics, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	budget := probe.plugin.Sampler().budget
	if got := budget.InForce(clock.Now()); got != (TraceSampling{Budget: 0, Ratio: 0.1, FloorPerMinute: 1}) {
		t.Fatalf("settings in force = %+v, want the directive's ratio and floor", got)
	}
	if probe.plugin.Sampler().Settings() != own {
		t.Fatalf("the directive changed the sampler's own settings to %+v", probe.plugin.Sampler().Settings())
	}
	clock.Set(time.Unix(1060, 0))
	if got := budget.InForce(clock.Now()); got != own {
		t.Fatalf("settings in force after the ttl = %+v, want %+v", got, own)
	}

	// A plugin configured while a directive holds follows it at once.
	clock.Set(time.Unix(2000, 0))
	if err := newTestClient(collector.URL, clock).post(context.Background(), otlp.PathMetrics, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	late := newSamplingProbe(t, Config{ServiceName: "late", TraceSampling: &own})
	if got := late.plugin.Sampler().budget.InForce(clock.Now()); got.Ratio != 0.1 {
		t.Fatalf("a plugin configured during a directive samples at %v, want 0.1", got.Ratio)
	}
}

func TestDirectives_ADirectiveRaisesTheLogLevelUntilItsTTL(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "export-directives", "a-directive-raises-the-log-level-until-its-ttl")
	clock := &testClock{now: time.Unix(1000, 0)}
	useDirectiveReceiver(t, clock)
	collector := newCollectorStub(t, func(path string, header http.Header) int {
		// Even a failed export carries a directive.
		header.Set(otlp.HeaderExportDirective, "log-level=warn, ttl=30")
		return http.StatusServiceUnavailable
	})
	sink := NewOTLPLogSink(OTLPConfig{Endpoint: collector.URL, FlushInterval: time.Hour})
	sink.client.now = clock.Now
	defer sink.Close() //nolint:errcheck // test cleanup

	var buf bytes.Buffer
	log := logger.New("app", logger.LevelDebug, logger.NewJSONSinkWriter(&buf))
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "queued"})
	if err := sink.Flush(); err != nil {
		t.Fatal(err)
	}
	log.Info("hidden")
	log.Warn("shown")
	log.Error("always", nil)
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") || !strings.Contains(buf.String(), "always") {
		t.Fatalf("under a warn directive the logger wrote %q", buf.String())
	}
	if logger.CurrentLevel() == logger.LevelWarn {
		t.Fatal("the directive changed the live level instead of limiting it")
	}
	clock.Set(time.Unix(1030, 0))
	buf.Reset()
	log.Debug("back")
	if !strings.Contains(buf.String(), "back") {
		t.Fatalf("once the ttl ended a debug record was dropped: %q", buf.String())
	}
}

func TestDirectives_ADirectiveDropIsCounted(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "export-directives", "a-directive-drop-is-counted")
	clock := &testClock{now: time.Unix(1000, 0)}
	receiver := useDirectiveReceiver(t, clock)
	probe := newSamplingProbe(t, Config{ServiceName: "test", TraceSampling: &TraceSampling{Ratio: 1}})
	probe.plugin.Sampler().now = clock.Now
	receiver.receive("trace-ratio=0, log-level=error, ttl=60")

	if got := probe.roots(t, 3); got != 0 {
		t.Fatalf("a ratio-0 directive exported %d of 3 roots", got)
	}
	if got := probe.decisions(t); got[otlp.TraceDecisionDropped] != 3 || got[otlp.TraceDecisionKept] != 0 {
		t.Fatalf("trace.decisions = %v, want 3 dropped", got)
	}

	counts := func() map[string]uint64 {
		m := map[string]uint64{}
		for _, c := range logger.RecordCounts() {
			m[c.Level+"/"+c.Outcome] = c.Count
		}
		return m
	}
	before := counts()
	logger.New("app", logger.LevelDebug, logger.NewMemorySink()).Warn("dropped by the directive")
	after := counts()
	if after["warn/dropped"]-before["warn/dropped"] != 1 {
		t.Fatalf("log.records counted warn/dropped %d times, want 1", after["warn/dropped"]-before["warn/dropped"])
	}
}

func TestDirectives_ARetryAfterDelaysOnlyItsSignal(t *testing.T) {
	spectest.Proves(t, "go/service-telemetry", "export-directives", "a-retry-after-delays-only-its-signal")
	spectest.Proves(t, "telemetry/otlp-push-contract", "too-many-requests-delays-a-signal", "the-go-exporters-wait-out-a-retry-after")
	clock := &testClock{now: time.Unix(1000, 0)}
	useDirectiveReceiver(t, clock)
	var throttle atomic.Bool
	throttle.Store(true)
	collector := newCollectorStub(t, func(path string, header http.Header) int {
		if throttle.Load() && (path == otlp.PathTraces || path == otlp.PathLogs) {
			header.Set("Retry-After", "120")
			return http.StatusTooManyRequests
		}
		return http.StatusOK
	})
	ctx := context.Background()
	traces, metrics := newTestClient(collector.URL, clock), newTestClient(collector.URL, clock)

	_ = traces.post(ctx, otlp.PathTraces, []byte("{}"))
	if err := traces.post(ctx, otlp.PathTraces, []byte("{}")); err != errExportDelayed {
		t.Fatalf("an export during the delay returned %v, want errExportDelayed", err)
	}
	if got := collector.count(otlp.PathTraces); got != 1 {
		t.Fatalf("the collector got %d trace exports during the delay, want 1", got)
	}
	if err := metrics.post(ctx, otlp.PathMetrics, []byte("{}")); err != nil {
		t.Fatalf("a 429 on traces delayed metrics: %v", err)
	}

	// The log sink keeps its records through the delay and sends them after.
	sink := NewOTLPLogSink(OTLPConfig{Endpoint: collector.URL, FlushInterval: time.Hour})
	sink.client.now = clock.Now
	defer sink.Close() //nolint:errcheck // test cleanup
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "first"})
	_ = sink.Flush()
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "second"})
	_ = sink.Flush()
	if got := collector.count(otlp.PathLogs); got != 1 {
		t.Fatalf("the collector got %d log exports during the delay, want 1", got)
	}
	throttle.Store(false)
	clock.Set(time.Unix(1120, 0))
	_ = sink.Flush()
	if got := collector.count(otlp.PathLogs); got != 2 {
		t.Fatalf("the collector got %d log exports after the delay, want 2", got)
	}
	sink.mu.Lock()
	queued := len(sink.buf)
	sink.mu.Unlock()
	if queued != 0 {
		t.Fatalf("%d records stayed queued after the delay", queued)
	}
	if err := traces.post(ctx, otlp.PathTraces, []byte("{}")); err != nil {
		t.Fatalf("an export after the delay returned %v", err)
	}
}

// logAt writes one record at level.
func logAt(log *logger.Logger, level string) {
	switch level {
	case "debug":
		log.Debug("record")
	case "info":
		log.Info("record")
	case "warn":
		log.Warn("record")
	default:
		log.Error("record", nil)
	}
}

func TestDirectives_AShutdownDuringTheDelayDropsTheQueueAndWarns(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	useDirectiveReceiver(t, clock)
	collector := newCollectorStub(t, func(path string, header http.Header) int {
		header.Set("Retry-After", "120")
		return http.StatusTooManyRequests
	})
	sink := NewOTLPLogSink(OTLPConfig{Endpoint: collector.URL, FlushInterval: time.Hour})
	sink.client.now = clock.Now
	warnings := logger.NewMemorySink()
	sink.log = logger.New("telemetry.otlp.logs", logger.LevelDebug, warnings)
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "first"})
	_ = sink.Flush()
	sink.Write(logger.LogEntry{Level: logger.LevelInfo, Message: "left"})
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	if got := collector.count(otlp.PathLogs); got != 1 {
		t.Fatalf("the collector got %d log exports, want only the first", got)
	}
	if warnings.Len() == 0 || !strings.Contains(warnings.Last().Message, "asked to wait") {
		t.Fatalf("a shutdown during the delay did not warn of the records it left (%d warnings)", warnings.Len())
	}
}

func TestDirectives_ARemovedLevelLimitComesBackWithTheNextDirective(t *testing.T) {
	clock := &testClock{now: time.Unix(1000, 0)}
	receiver := useDirectiveReceiver(t, clock)
	var buf bytes.Buffer
	log := logger.New("app", logger.LevelDebug, logger.NewJSONSinkWriter(&buf))
	receiver.receive("log-level=warn, ttl=60")
	logger.SetLevelLimit(nil)
	receiver.receive("log-level=warn, ttl=60")
	log.Info("hidden")
	if buf.Len() != 0 {
		t.Fatalf("a directive after SetLevelLimit(nil) did not limit the level: %q", buf.String())
	}
}
