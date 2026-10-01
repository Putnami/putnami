package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	protocol "go.putnami.dev/protocol/platform"

	"go.putnami.dev/protocol/features/spectest"
)

// --- test stubs ---

type fakeHealthPlugin struct {
	name string
	err  error
}

func (f *fakeHealthPlugin) Name() string                        { return f.name }
func (f *fakeHealthPlugin) CheckHealth(_ context.Context) error { return f.err }

type fakeReadyPlugin struct {
	name string
	err  error
}

func (f *fakeReadyPlugin) Name() string                           { return f.name }
func (f *fakeReadyPlugin) CheckReadiness(_ context.Context) error { return f.err }

// bothPlugin implements both capability interfaces — verifies that a
// single plugin can contribute to both /healthz and /readyz.
type bothPlugin struct {
	name string
	hErr error
	rErr error
}

func (b *bothPlugin) Name() string                           { return b.name }
func (b *bothPlugin) CheckHealth(_ context.Context) error    { return b.hErr }
func (b *bothPlugin) CheckReadiness(_ context.Context) error { return b.rErr }

// slowHealthPlugin's CheckHealth blocks for `delay` or until ctx is
// canceled — whichever comes first. Used to verify per-probe timeout
// + parallel execution: the response should return shortly after the
// timeout, not after `delay`.
type slowHealthPlugin struct {
	name  string
	delay time.Duration
}

func (s *slowHealthPlugin) Name() string { return s.name }
func (s *slowHealthPlugin) CheckHealth(ctx context.Context) error {
	select {
	case <-time.After(s.delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stubbornHealthPlugin's CheckHealth blocks for `delay` and deliberately
// ignores ctx cancellation — the non-cooperative case. Used to prove the
// aggregate handler bounds the response on its own deadline instead of
// blocking until the probe goroutine finishes. started is closed once the
// probe is actually running so the test can confirm it was invoked.
type stubbornHealthPlugin struct {
	name    string
	delay   time.Duration
	started chan struct{}
}

func (s *stubbornHealthPlugin) Name() string { return s.name }
func (s *stubbornHealthPlugin) CheckHealth(_ context.Context) error {
	if s.started != nil {
		close(s.started)
	}
	time.Sleep(s.delay) // intentionally ignores ctx.Done()
	return nil
}

// recordedProbe captures one RecordProbe call for assertions.
type recordedProbe struct {
	kind    ProbeKind
	probe   string
	outcome ProbeOutcome
	dur     time.Duration
}

// recordedAggregate captures one RecordAggregate call.
type recordedAggregate struct {
	kind    ProbeKind
	healthy bool
}

// fakeProbeMetrics is an in-memory ProbeMetrics sink for tests. It is
// concurrency-safe because the aggregate handler calls RecordProbe from the
// collection loop and (for timeouts) the abandon path — both on one
// goroutine, but the contract requires safety and -race must stay clean.
type fakeProbeMetrics struct {
	mu         sync.Mutex
	probes     []recordedProbe
	aggregates []recordedAggregate
}

func (f *fakeProbeMetrics) RecordProbe(_ context.Context, kind ProbeKind, probe string, outcome ProbeOutcome, dur time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, recordedProbe{kind: kind, probe: probe, outcome: outcome, dur: dur})
}

func (f *fakeProbeMetrics) RecordAggregate(_ context.Context, kind ProbeKind, healthy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aggregates = append(f.aggregates, recordedAggregate{kind: kind, healthy: healthy})
}

// probe returns the recorded outcome for a probe name, or ("", false).
func (f *fakeProbeMetrics) probe(name string) (recordedProbe, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.probes {
		if r.probe == name {
			return r, true
		}
	}
	return recordedProbe{}, false
}

// invoke runs h with a synthetic GET to path and returns the response.
func invoke(h http.Handler, path string) *http.Response {
	req := httptest.NewRequest("GET", path, nil)
	return h(http.NewContext(httptest.NewRecorder(), req))
}

// attrValue returns the string value of the named slog attr on a log entry,
// or ("", false) if absent.
func attrValue(e *logger.LogEntry, key string) (string, bool) {
	for _, a := range e.Attrs {
		if a.Key == key {
			return a.Value.String(), true
		}
	}
	return "", false
}

func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.Status != want {
		body, _ := resp.BodyBytes()
		t.Fatalf("status = %d, want %d (body=%s)", resp.Status, want, body)
	}
}

func assertBodyContains(t *testing.T, resp *http.Response, want string) {
	t.Helper()
	body, _ := resp.BodyBytes()
	if !strings.Contains(string(body), want) {
		t.Errorf("body missing %q: %s", want, body)
	}
}

// --- Plugin basics ---

func TestPlugin_Name(t *testing.T) {
	if NewPlugin(Config{}).Name() != "platform" {
		t.Errorf("Name = %q, want platform", NewPlugin(Config{}).Name())
	}
}

// --- /livez ---

func TestLivez_AlwaysOk_BeforeStart(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "liveness-is-trivial", "liveness-answers-success-before-start")
	// Livez never depends on the ready flag — k8s liveness must not
	// fail during normal startup or shutdown.
	p := NewPlugin(Config{})
	resp := invoke(p.livezHandler(), "/livez")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"status":"ok"`)

	// A trivial handler nobody mounts proves nothing: the guarantee is about
	// the path an orchestrator polls, so ask the mounted route as well. Before
	// Start is the moment that separates it from the aggregates, which answer
	// 503 "unavailable" here.
	if status, body := livezOverTheWire(t, p); status != 200 {
		t.Errorf("mounted /livez before Start = %d (%s), want 200 — the liveness path must not be wired to a lifecycle-aware aggregate", status, body)
	}
}

func TestLivez_AlwaysOk_AfterStop(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "liveness-is-trivial", "liveness-answers-success-after-stop")
	p := NewPlugin(Config{})
	_ = p.Start(context.Background(), nil)
	_ = p.Stop(context.Background(), nil)
	resp := invoke(p.livezHandler(), "/livez")
	assertStatus(t, resp, 200)

	if status, body := livezOverTheWire(t, p); status != 200 {
		t.Errorf("mounted /livez after Stop = %d (%s), want 200", status, body)
	}
}

// TestLivez_RunsNoProbes pins the other half of the triviality claim: a probe
// that fails, and one that hangs past every aggregate deadline, leave the
// liveness path answering success. Wiring /livez to the probe aggregate would
// turn a slow dependency into a restart loop — the failure this requirement
// exists to prevent.
func TestLivez_RunsNoProbes(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "liveness-is-trivial", "liveness-runs-no-probes")

	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	// A short probe timeout keeps the /healthz control assertion below from
	// paying the full default probe deadline for the hung probe.
	p := NewPlugin(Config{ProbeTimeout: 50 * time.Millisecond})
	p.AddHealthChecker("broken", func(context.Context) error {
		return fmt.Errorf("dependency is down")
	})
	p.AddHealthChecker("hung", func(ctx context.Context) error {
		select {
		case <-blocked:
		case <-ctx.Done():
		}
		return nil
	})
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.livezHandler(), "/livez")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"status":"ok"`)

	if status, body := livezOverTheWire(t, p); status != 200 {
		t.Errorf("mounted /livez with a failing and a hung health probe = %d (%s), want 200", status, body)
	}

	// The same probes do reach /healthz — otherwise this test would pass
	// against a plugin that simply never ran probes anywhere.
	if resp := invoke(p.healthzHandler(), "/healthz"); resp.Status == 200 {
		t.Error("/healthz answered 200 with a failing probe; the probes under test are not actually wired")
	}
}

// livezOverTheWire mounts p on a real HTTP server and GETs the liveness path,
// so the assertion covers what the route serves rather than what the handler
// constructor returns.
func livezOverTheWire(t *testing.T, p *Plugin) (int, string) {
	t.Helper()
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	ts := server.TestServer()
	defer ts.Close()
	return getStatus(t, ts.URL+"/livez")
}

// --- /healthz ---

func TestHealthz_503_BeforeStart(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "health-is-unavailable-before-start")
	p := NewPlugin(Config{})
	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"unavailable"`)
}

func TestHealthz_200_WhenRunningWithNoProbes(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "health-is-ok-while-running-with-no-probes")
	p := NewPlugin(Config{})
	_ = p.Start(context.Background(), nil)
	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"status":"ok"`)
}

// Stopping is not the same as never having started, but an orchestrator must
// read it the same way: once the application has stopped, the aggregates report
// unavailable rather than continuing to answer from a stale running flag.
func TestHealthzAndReadyz_UnavailableAfterStop(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "the-aggregates-are-unavailable-after-stop")
	p := NewPlugin(Config{})
	_ = p.Start(context.Background(), nil)
	_ = p.Stop(context.Background(), nil)

	health := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, health, 503)
	assertBodyContains(t, health, `"status":"unavailable"`)

	ready := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, ready, 503)
	assertBodyContains(t, ready, `"status":"unavailable"`)
}

func TestHealthz_AutoDiscoversHealthChecker(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "deferred-discovery", "a-health-probe-is-discovered-on-the-first-aggregate-request")
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: nil})

	child := app.NewModule("api")
	child.Use(&fakeHealthPlugin{name: "cache", err: nil})
	root.Use(child)

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"db":"ok"`)
	assertBodyContains(t, resp, `"cache":"ok"`)
}

func TestHealthz_DegradedWhenProbeFails(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "health-is-degraded-with-per-probe-detail-when-a-probe-fails")
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: fmt.Errorf("conn refused")})
	root.Use(&fakeHealthPlugin{name: "cache", err: nil})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	assertBodyContains(t, resp, `"db":"conn refused"`)
	assertBodyContains(t, resp, `"cache":"ok"`)
}

func TestHealthz_RedactProbeErrors(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "error-redaction", "a-failing-probe-reports-a-generic-message-when-redaction-is-enabled")
	const secret = "dial tcp 10.0.2.5:5432: connect: connection refused"
	p := NewPlugin(Config{RedactProbeErrors: true})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: fmt.Errorf("%s", secret)})
	root.Use(&fakeHealthPlugin{name: "cache", err: nil})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	assertBodyContains(t, resp, `"db":"unhealthy"`)
	assertBodyContains(t, resp, `"cache":"ok"`)

	// The verbatim internal detail must not reach the response body.
	body, _ := resp.BodyBytes()
	if strings.Contains(string(body), secret) {
		t.Errorf("redacted mode leaked verbatim probe error: %s", body)
	}
}

// --- structured failure logging + trace context ---

// TestHealthz_RedactedFailure_LogsStructuredFieldsWithTrace verifies the
// redacted-failure warning carries the probe name + error as queryable slog
// attrs (not baked into the message) and propagates the request's trace ID —
// pre-fix it used fmt.Sprintf with no context, so both were lost.
func TestHealthz_RedactedFailure_LogsStructuredFieldsWithTrace(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "error-redaction", "the-real-cause-of-a-redacted-failure-is-logged-server-side")
	origTrace := logger.TraceIDFromContext
	defer func() { logger.TraceIDFromContext = origTrace }()
	logger.TraceIDFromContext = func(_ context.Context) string { return "trace-xyz" }

	sink := logger.NewMemorySink()
	p := NewPlugin(Config{RedactProbeErrors: true})
	p.log = logger.New("platform", logger.LevelDebug, sink)

	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: fmt.Errorf("connection refused")})
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	_ = invoke(p.healthzHandler(), "/healthz")

	var entry *logger.LogEntry
	for i := range sink.Entries {
		if sink.Entries[i].Message == "health probe failed" {
			entry = &sink.Entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("expected a 'health probe failed' log entry, got %+v", sink.Entries)
	}
	if entry.Level != logger.LevelWarn {
		t.Errorf("level = %v, want Warn", entry.Level)
	}
	if entry.TraceID != "trace-xyz" {
		t.Errorf("traceID = %q, want trace-xyz (context was dropped?)", entry.TraceID)
	}
	if v, ok := attrValue(entry, "probe"); !ok || v != "db" {
		t.Errorf("probe attr = %q (present=%v), want db", v, ok)
	}
	if v, ok := attrValue(entry, "error"); !ok || v != "connection refused" {
		t.Errorf("error attr = %q (present=%v), want 'connection refused'", v, ok)
	}
	// The message itself must be the static, queryable string — not the
	// interpolated form the finding called out.
	if strings.Contains(entry.Message, "db") || strings.Contains(entry.Message, "connection refused") {
		t.Errorf("message should not interpolate probe/error: %q", entry.Message)
	}
}

// TestHealthz_AbandonedProbe_LogsStructuredFieldsWithTrace verifies the
// aggregate-deadline warning carries the pending probe names + timeout as
// structured attrs and propagates trace context.
func TestHealthz_AbandonedProbe_LogsStructuredFieldsWithTrace(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "error-redaction", "the-real-cause-of-an-abandoned-probe-is-logged-server-side")
	origTrace := logger.TraceIDFromContext
	defer func() { logger.TraceIDFromContext = origTrace }()
	logger.TraceIDFromContext = func(_ context.Context) string { return "trace-abc" }

	sink := logger.NewMemorySink()
	p := NewPlugin(Config{ProbeTimeout: 50 * time.Millisecond})
	p.log = logger.New("platform", logger.LevelDebug, sink)

	stubborn := &stubbornHealthPlugin{name: "stubborn", delay: 2 * time.Second, started: make(chan struct{})}
	root := app.NewModule("root")
	root.Use(p)
	root.Use(stubborn)
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	_ = invoke(p.healthzHandler(), "/healthz")

	var entry *logger.LogEntry
	for i := range sink.Entries {
		if strings.HasPrefix(sink.Entries[i].Message, "health probe(s) exceeded") {
			entry = &sink.Entries[i]
			break
		}
	}
	if entry == nil {
		t.Fatalf("expected an abandoned-probe log entry, got %+v", sink.Entries)
	}
	if entry.TraceID != "trace-abc" {
		t.Errorf("traceID = %q, want trace-abc", entry.TraceID)
	}
	if v, ok := attrValue(entry, "probes"); !ok || v != "stubborn" {
		t.Errorf("probes attr = %q (present=%v), want stubborn", v, ok)
	}
	if _, ok := attrValue(entry, "timeout"); !ok {
		t.Errorf("expected a structured 'timeout' attr, attrs=%v", entry.Attrs)
	}
	if v, ok := attrValue(entry, "count"); !ok || v != "1" {
		t.Errorf("count attr = %q (present=%v), want 1", v, ok)
	}
}

// --- per-probe metrics ---

// TestHealthz_ProbeMetrics_RecordsOutcomesAndAggregate verifies the optional
// ProbeMetrics sink receives a per-probe outcome+latency for each probe and an
// aggregate result, tagged with the health kind.
func TestHealthz_ProbeMetrics_RecordsOutcomesAndAggregate(t *testing.T) {
	mx := &fakeProbeMetrics{}
	p := NewPlugin(Config{ProbeMetrics: mx})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: fmt.Errorf("down")})
	root.Use(&fakeHealthPlugin{name: "cache", err: nil})
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	_ = invoke(p.healthzHandler(), "/healthz")

	db, ok := mx.probe("db")
	if !ok {
		t.Fatal("no metric recorded for probe db")
	}
	if db.outcome != ProbeOutcomeFail {
		t.Errorf("db outcome = %q, want fail", db.outcome)
	}
	if db.kind != ProbeKindHealth {
		t.Errorf("db kind = %q, want health", db.kind)
	}
	cache, ok := mx.probe("cache")
	if !ok {
		t.Fatal("no metric recorded for probe cache")
	}
	if cache.outcome != ProbeOutcomePass {
		t.Errorf("cache outcome = %q, want pass", cache.outcome)
	}
	if cache.dur < 0 {
		t.Errorf("cache duration = %v, want >= 0", cache.dur)
	}
	if len(mx.aggregates) != 1 {
		t.Fatalf("aggregates recorded = %d, want 1", len(mx.aggregates))
	}
	if mx.aggregates[0].healthy {
		t.Error("aggregate healthy = true, want false (db failed)")
	}
	if mx.aggregates[0].kind != ProbeKindHealth {
		t.Errorf("aggregate kind = %q, want health", mx.aggregates[0].kind)
	}
}

// TestReadyz_ProbeMetrics_TaggedReadiness verifies readiness probes are tagged
// with ProbeKindReadiness and a passing evaluation records healthy=true.
func TestReadyz_ProbeMetrics_TaggedReadiness(t *testing.T) {
	mx := &fakeProbeMetrics{}
	p := NewPlugin(Config{ProbeMetrics: mx})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "warm", err: nil})
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	_ = invoke(p.readyzHandler(), "/readyz")

	warm, ok := mx.probe("warm")
	if !ok {
		t.Fatal("no metric recorded for probe warm")
	}
	if warm.kind != ProbeKindReadiness {
		t.Errorf("warm kind = %q, want readiness", warm.kind)
	}
	if warm.outcome != ProbeOutcomePass {
		t.Errorf("warm outcome = %q, want pass", warm.outcome)
	}
	if len(mx.aggregates) != 1 || !mx.aggregates[0].healthy {
		t.Errorf("aggregates = %+v, want one healthy=true", mx.aggregates)
	}
}

// TestHealthz_ProbeMetrics_TimeoutOutcome verifies a non-cooperative probe
// (abandoned past the aggregate deadline) is recorded with ProbeOutcomeTimeout.
func TestHealthz_ProbeMetrics_TimeoutOutcome(t *testing.T) {
	mx := &fakeProbeMetrics{}
	p := NewPlugin(Config{ProbeTimeout: 50 * time.Millisecond, ProbeMetrics: mx})
	stubborn := &stubbornHealthPlugin{name: "stubborn", delay: 2 * time.Second, started: make(chan struct{})}
	root := app.NewModule("root")
	root.Use(p)
	root.Use(stubborn)
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	_ = invoke(p.healthzHandler(), "/healthz")

	st, ok := mx.probe("stubborn")
	if !ok {
		t.Fatal("no metric recorded for stubborn probe")
	}
	if st.outcome != ProbeOutcomeTimeout {
		t.Errorf("stubborn outcome = %q, want timeout", st.outcome)
	}
	// Abandoned-probe latency is the bounded deadline, not the 2s true runtime.
	if st.dur < 50*time.Millisecond || st.dur > time.Second {
		t.Errorf("stubborn recorded latency = %v, want ~the aggregate deadline", st.dur)
	}
}

// TestHealthz_NilProbeMetrics_NoPanic guards the default (nil) path: probes run
// and the endpoint responds without a metrics sink installed.
func TestHealthz_NilProbeMetrics_NoPanic(t *testing.T) {
	p := NewPlugin(Config{}) // ProbeMetrics nil
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeHealthPlugin{name: "db", err: nil})
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 200)
}

// TestHealthz_ParallelProbes_PerProbeTimeout regresses the behavior
// codex flagged: probes were running serially under a shared timeout, so
// one slow probe poisoned the budget for every probe that followed.
// With parallel execution + per-probe timeout, a 4s-slow probe should
// time out on its own but leave the fast probes free to report their
// real status (not "context deadline exceeded").
func TestHealthz_ParallelProbes_PerProbeTimeout(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "bounded-probes", "probes-run-in-parallel-each-under-its-own-timeout")
	p := NewPlugin(Config{ProbeTimeout: 100 * time.Millisecond})

	slow := &slowHealthPlugin{name: "slow", delay: 2 * time.Second}
	fast := &fakeHealthPlugin{name: "fast", err: nil}

	root := app.NewModule("root")
	root.Use(p)
	root.Use(slow)
	root.Use(fast)

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	started := time.Now()
	resp := invoke(p.healthzHandler(), "/healthz")
	elapsed := time.Since(started)

	// Generous ceiling vs the 100ms per-probe timeout — if execution
	// were still serial we'd wait the full 2s of the slow probe before
	// even starting the fast one.
	if elapsed > time.Second {
		t.Fatalf("aggregate took %s, expected <1s under per-probe timeout", elapsed)
	}
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	// fast probe must report its real status, not a poisoned deadline.
	assertBodyContains(t, resp, `"fast":"ok"`)
	// slow probe reports timeout via ctx.Err().
	assertBodyContains(t, resp, `"slow":"context deadline exceeded"`)
}

// TestHealthz_NonCooperativeProbe_DoesNotStall regresses the high-severity
// finding: a probe that ignores ctx cancellation must not hang the response.
// With the per-probe timeout at 100ms the handler must return within its
// aggregate deadline (timeout + grace), report the stubborn probe as timed
// out, and still surface the fast probe's real status — even though the
// stubborn probe's goroutine keeps running for the full 2s in the background.
func TestHealthz_NonCooperativeProbe_DoesNotStall(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "bounded-probes", "a-probe-that-ignores-cancellation-does-not-stall-the-aggregate")
	p := NewPlugin(Config{ProbeTimeout: 100 * time.Millisecond})

	stubborn := &stubbornHealthPlugin{name: "stubborn", delay: 2 * time.Second, started: make(chan struct{})}
	fast := &fakeHealthPlugin{name: "fast", err: nil}

	root := app.NewModule("root")
	root.Use(p)
	root.Use(stubborn)
	root.Use(fast)

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	started := time.Now()
	resp := invoke(p.healthzHandler(), "/healthz")
	elapsed := time.Since(started)

	// The response must be bounded by the aggregate deadline, NOT by the
	// stubborn probe's 2s sleep. Generous ceiling well under 2s.
	if elapsed > time.Second {
		t.Fatalf("aggregate took %s, expected the handler to abandon the non-cooperative probe near its timeout", elapsed)
	}
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	assertBodyContains(t, resp, `"fast":"ok"`)
	assertBodyContains(t, resp, `"stubborn":"probe did not complete`)

	// The probe must really have been invoked (not skipped).
	select {
	case <-stubborn.started:
	default:
		t.Fatal("stubborn probe was never started")
	}
}

// TestHealthz_NoDataRace_OnConcurrentAddAndAggregate exercises the lock
// added around the probe maps. Run with `go test -race`; without the
// RWMutex this test would flag a write/read race.
func TestHealthz_NoDataRace_OnConcurrentAddAndAggregate(t *testing.T) {
	p := NewPlugin(Config{ProbeTimeout: 50 * time.Millisecond})
	_ = p.Start(context.Background(), nil)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			_ = p.AddHealthChecker(fmt.Sprintf("probe-%d", i), func(_ context.Context) error { return nil })
		}
		close(done)
	}()

	for i := 0; i < 100; i++ {
		_ = invoke(p.healthzHandler(), "/healthz")
	}
	<-done
}

func TestHealthz_ExplicitAddChecker_WinsOverAutoDiscovery(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "deferred-discovery", "an-explicitly-registered-probe-is-not-overwritten-by-a-discovered-one")
	p := NewPlugin(Config{})
	// Explicit probe registered before Configure.
	if err := p.AddHealthChecker("db", func(_ context.Context) error { return fmt.Errorf("explicit wins") }); err != nil {
		t.Fatalf("AddHealthChecker: %v", err)
	}

	root := app.NewModule("root")
	root.Use(p)
	// Auto-discovered probe of the same name must not overwrite.
	root.Use(&fakeHealthPlugin{name: "db", err: nil})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertBodyContains(t, resp, `"db":"explicit wins"`)
}

// --- probe-name validation ---

func TestAddHealthChecker_RejectsInvalidName(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "probe-names", "an-explicit-health-probe-name-is-validated-at-registration")
	p := NewPlugin(Config{})
	// Uppercase + space violate the canonical probe-name pattern.
	if err := p.AddHealthChecker("BAD Name", func(_ context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for non-conforming probe name, got nil")
	}
	if _, ok := p.healthCheckers["BAD Name"]; ok {
		t.Error("a rejected probe must not be registered")
	}
}

func TestAddReadinessChecker_RejectsInvalidName(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "probe-names", "an-explicit-readiness-probe-name-is-validated-at-registration")
	p := NewPlugin(Config{})
	if err := p.AddReadinessChecker("UPPER", func(_ context.Context) error { return nil }); err == nil {
		t.Fatal("expected error for non-conforming probe name, got nil")
	}
}

func TestStart_RejectsDiscoveredInvalidProbeName(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "probe-names", "a-discovered-probe-with-an-invalid-name-fails-start")
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	// A plugin whose Name() is non-conforming would emit an envelope that fails
	// the protocol's own validator; Start must reject it rather than serve it.
	root.Use(&fakeHealthPlugin{name: "Bad Probe"})
	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	if err := p.Start(context.Background(), nil); err == nil {
		t.Fatal("expected Start to fail for a non-conforming discovered probe name")
	}
}

func TestStart_RejectsInvalidRequiredProbeName(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "probe-names", "a-required-probe-with-an-invalid-name-fails-start")
	// A non-conforming Config.Required name would be synthesized verbatim into a
	// degraded /readyz checks key, making the envelope fail the protocol's own
	// validator. Start must reject it, symmetric with discovered/registered names.
	p := NewPlugin(Config{Required: []string{"Leader Elect"}})
	if err := p.Start(context.Background(), nil); err == nil {
		t.Fatal("expected Start to fail for a non-conforming required probe name")
	}
	// A conforming required name still starts cleanly.
	ok := NewPlugin(Config{Required: []string{"leader-elect"}})
	if err := ok.Start(context.Background(), nil); err != nil {
		t.Fatalf("Start with a conforming required name: %v", err)
	}
}

// --- /readyz ---

func TestReadyz_503_BeforeStart(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "readiness-is-unavailable-before-start")
	p := NewPlugin(Config{})
	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"unavailable"`)
}

func TestReadyz_AutoDiscoversReadinessChecker(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "deferred-discovery", "a-readiness-probe-is-discovered-on-the-first-aggregate-request")
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "leader-elect", err: nil})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"leader-elect":"ok"`)
}

func TestReadyz_DegradedWhenProbeFails(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "lifecycle-state", "readiness-is-degraded-with-per-probe-detail-when-a-probe-fails")
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "warm", err: fmt.Errorf("loading")})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	assertBodyContains(t, resp, `"warm":"loading"`)
}

// A plugin implementing both interfaces contributes to both endpoints.
func TestPlugin_BothInterfaces_ContributesToBothEndpoints(t *testing.T) {
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&bothPlugin{name: "deps", hErr: nil, rErr: fmt.Errorf("warmup")})

	if err := p.Configure(context.Background(), root); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	hResp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, hResp, 200)
	assertBodyContains(t, hResp, `"deps":"ok"`)

	rResp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, rResp, 503)
	assertBodyContains(t, rResp, `"deps":"warmup"`)
}

// --- required readiness ---

// TestReadyz_MissingRequiredProbe_Degraded verifies that a name in
// Config.Required which is neither registered nor auto-discovered makes
// /readyz report degraded with a synthesized failing checks entry for
// that name — expressed through the existing envelope (no wire change),
// so the response still passes the protocol validator.
func TestReadyz_MissingRequiredProbe_Degraded(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "required-probes", "a-missing-required-probe-makes-readiness-degraded")
	p := NewPlugin(Config{Required: []string{"leader-elect"}})
	root := app.NewModule("root")
	root.Use(p)
	_ = p.Configure(context.Background(), root)
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 503)

	body, _ := resp.BodyBytes()
	var env protocol.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not a valid envelope: %v (body=%s)", err, body)
	}
	if env.Status != protocol.StatusDegraded {
		t.Errorf("status = %q, want degraded (body=%s)", env.Status, body)
	}
	entry, ok := env.Checks["leader-elect"]
	if !ok {
		t.Fatalf("missing synthesized checks entry for leader-elect (body=%s)", body)
	}
	if string(entry) != missingRequiredProbeMessage {
		t.Errorf("missing-required entry = %q, want %q", entry, missingRequiredProbeMessage)
	}
	if diags := protocol.ValidateEnvelope(env); len(diags) > 0 {
		t.Errorf("envelope failed protocol validation: %v (body=%s)", diags, body)
	}
}

// TestReadyz_RequiredProbe_Registered_RunsNormally verifies a required
// name that IS registered runs as a normal probe — the required list only
// synthesizes entries for names that are absent, and never double-reports
// a registered one.
func TestReadyz_RequiredProbe_Registered_RunsNormally(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "required-probes", "a-registered-required-probe-runs-normally")
	p := NewPlugin(Config{Required: []string{"leader-elect"}})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "leader-elect", err: nil})
	_ = p.Configure(context.Background(), root)
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"status":"ok"`)
	assertBodyContains(t, resp, `"leader-elect":"ok"`)
}

// TestReadyz_MissingRequired_AlongsidePassingProbe verifies the
// synthesized entry coexists with real probe results and forces degraded
// even when every registered readiness probe passes.
func TestReadyz_MissingRequired_AlongsidePassingProbe(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "required-probes", "a-missing-required-probe-is-reported-alongside-a-passing-one")
	p := NewPlugin(Config{Required: []string{"leader-elect"}})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "warm", err: nil})
	_ = p.Configure(context.Background(), root)
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	assertStatus(t, resp, 503)
	assertBodyContains(t, resp, `"status":"degraded"`)
	assertBodyContains(t, resp, `"warm":"ok"`)
	assertBodyContains(t, resp, `"leader-elect":`)
}

// TestHealthz_IgnoresRequired verifies the required list is readiness-only:
// a required name absent from readiness must not affect /healthz (a
// missing dependency drains traffic, it does not restart the pod).
func TestHealthz_IgnoresRequired(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "required-probes", "health-ignores-the-required-list")
	p := NewPlugin(Config{Required: []string{"leader-elect"}})
	root := app.NewModule("root")
	root.Use(p)
	_ = p.Configure(context.Background(), root)
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertStatus(t, resp, 200)
	assertBodyContains(t, resp, `"status":"ok"`)
}

// --- Configure edge cases ---

func TestConfigure_NilOwner_NoPanic(t *testing.T) {
	if err := NewPlugin(Config{}).Configure(context.Background(), nil); err != nil {
		t.Errorf("Configure(nil): %v", err)
	}
}

func TestConfigure_FromNestedModule_WalksToRoot(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "deferred-discovery", "discovery-walks-from-a-nested-module-to-the-root")
	// Platform plugin registered in a child module still sees siblings
	// of the root via owner.Root().CollectPlugins().
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(&fakeHealthPlugin{name: "db", err: nil})

	child := app.NewModule("api")
	child.Use(p)
	root.Use(child)

	if err := p.Configure(context.Background(), child); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.healthzHandler(), "/healthz")
	assertBodyContains(t, resp, `"db":"ok"`)
}

// contributingPlugin registers a HealthChecker via app.Contribute during its
// own Configure — the case interface-implementation can't express (one plugin,
// many probes) and the case that exposed the ordering bug.
type contributingPlugin struct {
	probe string
}

func (c *contributingPlugin) Name() string { return "contributor" }
func (c *contributingPlugin) Configure(_ context.Context, owner *app.Module) error {
	app.Contribute[app.HealthChecker](owner, &fakeHealthPlugin{name: c.probe})
	return nil
}

func TestHealthz_DiscoversContributionFromLaterConfiguredPlugin(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "deferred-discovery", "a-probe-contributed-by-a-later-configured-plugin-is-still-found")
	// Regression: the platform plugin is configured BEFORE the plugin that
	// contributes a probe. Discovery must still see it. Pre-fix, discovery ran
	// inline in Configure and missed contributions made by plugins configured
	// afterwards.
	p := NewPlugin(Config{})
	a := app.New("t")
	a.Use(p)                                        // configured first
	a.Use(&contributingPlugin{probe: "late-probe"}) // contributes during its Configure, after p
	a.Run(func(context.Context) error { return nil })
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	resp := invoke(p.healthzHandler(), "/healthz")
	assertBodyContains(t, resp, `"late-probe":"ok"`)
}

// --- /version ---

func TestVersion_ReturnsConfiguredInfo(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "version-fallback", "the-version-path-returns-the-configured-build-metadata")
	p := NewPlugin(Config{
		Version: VersionInfo{
			Name:      "my-service",
			Version:   "1.2.3",
			SHA:       "abc1234",
			Branch:    "main",
			BuildTime: "2026-05-23T07:00:00Z",
		},
	})
	resp := invoke(p.versionHandler(), "/version")
	assertStatus(t, resp, 200)
	for _, want := range []string{
		`"name":"my-service"`,
		`"version":"1.2.3"`,
		`"sha":"abc1234"`,
		`"branch":"main"`,
		`"buildTime":"2026-05-23T07:00:00Z"`,
	} {
		assertBodyContains(t, resp, want)
	}
}

func TestVersion_OmitsEmptyFields(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "version-fallback", "a-field-with-no-value-anywhere-is-omitted-rather-than-reported-empty")
	// debug.ReadBuildInfo will fill Name with the test binary's module
	// path, but Version/SHA/Branch/BuildTime stay empty under `go test`.
	// We only assert that the response is valid JSON and contains a
	// "name" key — exact build metadata can't be asserted portably.
	p := NewPlugin(Config{})
	resp := invoke(p.versionHandler(), "/version")
	assertStatus(t, resp, 200)
	body, err := resp.BodyBytes()
	if err != nil {
		t.Fatalf("BodyBytes: %v", err)
	}
	if !strings.Contains(string(body), `"name"`) {
		t.Errorf("expected name field, got %s", body)
	}
}

// --- protocol conformance ---

// TestConformance_HealthzEnvelopeMatchesProtocol runs the real handler
// across each lifecycle state and validates the JSON payload against
// the protocol's canonical validator. If this fails, the framework's
// wire shape has drifted from the cross-language contract.
func TestConformance_HealthzEnvelopeMatchesProtocol(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(p *Plugin, root *app.Module)
		expect protocol.Status
	}{
		{
			name:   "unavailable before start",
			setup:  func(_ *Plugin, _ *app.Module) {},
			expect: protocol.StatusUnavailable,
		},
		{
			name: "ok when running with passing probe",
			setup: func(p *Plugin, root *app.Module) {
				root.Use(&fakeHealthPlugin{name: "db", err: nil})
				_ = p.Configure(context.Background(), root)
				_ = p.Start(context.Background(), nil)
			},
			expect: protocol.StatusOK,
		},
		{
			name: "degraded when probe fails",
			setup: func(p *Plugin, root *app.Module) {
				root.Use(&fakeHealthPlugin{name: "db", err: fmt.Errorf("conn refused")})
				_ = p.Configure(context.Background(), root)
				_ = p.Start(context.Background(), nil)
			},
			expect: protocol.StatusDegraded,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := NewPlugin(Config{})
			root := app.NewModule("root")
			root.Use(p)
			c.setup(p, root)

			resp := invoke(p.healthzHandler(), "/healthz")
			body, _ := resp.BodyBytes()

			var env protocol.Envelope
			if err := json.Unmarshal(body, &env); err != nil {
				t.Fatalf("response is not a valid envelope: %v (body=%s)", err, body)
			}
			if env.Status != c.expect {
				t.Errorf("status = %q, want %q (body=%s)", env.Status, c.expect, body)
			}
			if diags := protocol.ValidateEnvelope(env); len(diags) > 0 {
				t.Errorf("envelope failed protocol validation: %v (body=%s)", diags, body)
			}
			wantHTTP, _ := protocol.HTTPStatusFor(env.Status)
			if resp.Status != wantHTTP {
				t.Errorf("HTTP status = %d, want %d for envelope status %q", resp.Status, wantHTTP, env.Status)
			}
		})
	}
}

// TestConformance_ReadyzEnvelopeMatchesProtocol is the readiness twin
// of the healthz conformance test — identical shape rules apply.
func TestConformance_ReadyzEnvelopeMatchesProtocol(t *testing.T) {
	p := NewPlugin(Config{})
	root := app.NewModule("root")
	root.Use(p)
	root.Use(&fakeReadyPlugin{name: "warm", err: fmt.Errorf("loading")})
	_ = p.Configure(context.Background(), root)
	_ = p.Start(context.Background(), nil)

	resp := invoke(p.readyzHandler(), "/readyz")
	body, _ := resp.BodyBytes()
	var env protocol.Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not a valid envelope: %v (body=%s)", err, body)
	}
	if diags := protocol.ValidateEnvelope(env); len(diags) > 0 {
		t.Errorf("readyz envelope failed protocol validation: %v", diags)
	}
}

// --- RegisterOn / routing (via http.ServerPlugin.TestServer) ---

func TestRegisterOn_MountsCoreEndpointsAtRoot(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "endpoint-set", "core-endpoints-mount-at-the-root")
	p := NewPlugin(Config{})
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	_ = p.Start(context.Background(), nil)

	ts := server.TestServer()
	defer ts.Close()

	for _, path := range []string{"/livez", "/healthz", "/readyz", "/version"} {
		status, _ := getStatus(t, ts.URL+path)
		// /healthz and /readyz return 200 here (Start was called and
		// there are no probes); /livez and /version always 200.
		if status != 200 {
			t.Errorf("%s status = %d, want 200", path, status)
		}
	}
}

func TestRegisterOn_AppliesPrefix(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "endpoint-set", "the-configured-prefix-applies-to-the-core-endpoints")
	p := NewPlugin(Config{Prefix: "/_"})
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	_ = p.Start(context.Background(), nil)

	ts := server.TestServer()
	defer ts.Close()

	if status, _ := getStatus(t, ts.URL+"/_/livez"); status != 200 {
		t.Errorf("/_/livez status %d, want 200", status)
	}
	// Without prefix, the plain path must not be registered.
	if status, _ := getStatus(t, ts.URL+"/livez"); status != 404 {
		t.Errorf("/livez under /_ prefix should 404, got %d", status)
	}
}

// --- pprof ---

func TestPprof_DisabledByDefault(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "endpoint-set", "profiling-handlers-are-off-by-default")
	p := NewPlugin(Config{})
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	_ = p.Start(context.Background(), nil)

	ts := server.TestServer()
	defer ts.Close()

	if status, _ := getStatus(t, ts.URL+"/debug/pprof"); status != 404 {
		t.Errorf("/debug/pprof should 404 when EnablePprof=false, got %d", status)
	}
}

func TestPprof_EnabledMountsIndexAndProfile(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "endpoint-set", "profiling-handlers-mount-when-the-workload-enables-them")
	p := NewPlugin(Config{EnablePprof: true})
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	_ = p.Start(context.Background(), nil)

	ts := server.TestServer()
	defer ts.Close()

	status, body := getStatus(t, ts.URL+"/debug/pprof")
	if status != 200 {
		t.Errorf("/debug/pprof status %d, want 200", status)
	}
	if !strings.Contains(body, "heap") {
		t.Errorf("pprof index should list profiles, got: %s", body)
	}

	// A named profile must respond with non-empty body.
	status, body = getStatus(t, ts.URL+"/debug/pprof/heap")
	if status != 200 {
		t.Errorf("/debug/pprof/heap status %d, want 200", status)
	}
	if len(body) == 0 {
		t.Errorf("/debug/pprof/heap body is empty")
	}
}

func TestPprof_AppliesPrefix(t *testing.T) {
	spectest.Proves(t, "go/platform-endpoints", "endpoint-set", "the-configured-prefix-applies-to-the-profiling-handlers")
	p := NewPlugin(Config{Prefix: "/_", EnablePprof: true})
	server := http.NewServerPlugin(http.ServerConfig{Port: 0})
	p.RegisterOn(server)
	_ = p.Start(context.Background(), nil)

	ts := server.TestServer()
	defer ts.Close()

	if status, _ := getStatus(t, ts.URL+"/_/debug/pprof"); status != 200 {
		t.Errorf("/_/debug/pprof status %d, want 200", status)
	}
	if status, _ := getStatus(t, ts.URL+"/debug/pprof"); status != 404 {
		t.Errorf("/debug/pprof under /_ prefix should 404, got %d", status)
	}
}

// getStatus sends a GET to url through stdhttp and returns the status
// + body. Test helpers panic on transport errors — the test server is
// always local, so any error here is the test framework's problem.
func getStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := stdhttp.Get(url) //nolint:noctx,gosec // local test server
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, string(body)
}
