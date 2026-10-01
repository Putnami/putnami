// Package platform mounts a standard set of operational HTTP endpoints —
// liveness (/healthz, /livez), readiness (/readyz), version metadata
// (/version), and (optionally) pprof (/debug/pprof/...) — and discovers
// per-plugin probes from the application's module tree.
//
// Every Putnami workload, regardless of internal shape, needs the same
// operational surface. The platform plugin makes that surface a single
// import instead of per-workload boilerplate.
//
//	import "go.putnami.dev/platform"
//
//	a := app.New("my-service").
//	    Use(http.NewServerPlugin(http.ServerConfig{Port: 8080})).
//	    Use(database.NewPlugin(database.PluginConfig{ /* ... */ })).
//	    Use(platform.NewPlugin(platform.Config{
//	        Version: platform.VersionInfo{Name: "my-service"},
//	    }))
//
// The database plugin implements app.HealthChecker, so /healthz reports
// pool connectivity without any extra wiring. Any other plugin that
// satisfies app.HealthChecker (liveness) or app.ReadinessChecker
// (readiness) is auto-discovered on the same basis.
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.putnami.dev/app"
	"go.putnami.dev/errors"
	"go.putnami.dev/http"
	"go.putnami.dev/logger"
	protocol "go.putnami.dev/protocol/platform"
)

// CodeInvalidProbeName is returned when a probe is registered — explicitly via
// Add{Health,Readiness}Checker or through auto-discovery — under a name that
// violates the cross-language probe-name contract. Kept in sync with the
// protocol's error code so the runtime never emits a /healthz or /readyz
// envelope that fails the protocol's own validator.
const CodeInvalidProbeName errors.Code = errors.Code(protocol.ErrorCodeInvalidProbeName)

// validateProbeName rejects names the protocol's envelope validator would
// reject (ValidateProbeName enforces ^[a-z0-9][a-z0-9_./-]{0,63}$).
func validateProbeName(name string) error {
	if diags := protocol.ValidateProbeName(name); len(diags) > 0 {
		return errors.New(CodeInvalidProbeName,
			"probe name does not match the canonical pattern ^[a-z0-9][a-z0-9_./-]{0,63}$",
			errors.String("probe", name))
	}
	return nil
}

// defaultProbeTimeout is the per-probe timeout the runtime applies when
// Config.ProbeTimeout is zero. Sourced from the protocol to keep the
// framework's behavior aligned with the cross-language contract.
var defaultProbeTimeout = time.Duration(protocol.DefaultContract().Probe.DefaultTimeoutMS) * time.Millisecond

// missingRequiredProbeMessage is the synthesized /readyz checks-entry
// value for a probe named in Config.Required that was never registered or
// auto-discovered. It surfaces in the degraded envelope exactly like a
// probe's error.Error() string. Kept byte-identical to the TypeScript
// runtime (typescript/framework/application platform plugin) so the same
// missing-required name yields the same envelope in both runtimes — this
// is the protocol's platform.missing_probe taxonomy entry
// (protocol.ErrorCodeMissingProbe), expressed through the existing
// envelope with no wire change.
const missingRequiredProbeMessage = "required probe not registered or discovered"

// Config configures the platform plugin.
type Config struct {
	// Prefix is prepended to every endpoint path. Empty (default) mounts
	// at the root: /healthz, /livez, /readyz, /version, /debug/pprof/*.
	// Set to e.g. "/_" to namespace under /_/healthz, /_/livez, … —
	// useful when the workload's public API also lives at root and you
	// want operational endpoints under an admin prefix.
	//
	// A single leading "/" with no trailing "/" is the canonical form
	// (the plugin normalises around that — empty, "/", and "/_/" all
	// collapse to "", "", "/_" respectively).
	Prefix string

	// EnablePprof exposes the /debug/pprof/* handlers from
	// net/http/pprof. Disabled by default: pprof reveals heap and
	// goroutine internals and is not safe to expose on a public port.
	// Gate it behind config so workloads opt-in explicitly.
	EnablePprof bool

	// Version describes the build for /version. Leave empty to fall
	// back to runtime/debug.ReadBuildInfo (which gives the module path
	// and any VCS info baked in at build time).
	Version VersionInfo

	// ProbeTimeout caps how long each probe may take. Defaults to 5s.
	ProbeTimeout time.Duration

	// Required lists readiness probe names the workload treats as
	// mandatory. Any name here that is not among the registered or
	// auto-discovered readiness probes by the time /readyz runs makes the
	// endpoint report "degraded" with a synthesized failing checks entry
	// for that name — the operator sees a missing required dependency
	// exactly as they would a failing one, and orchestrators drain traffic
	// from a workload that is wired incompletely. Names that ARE registered
	// are unaffected: their probe runs normally. /healthz ignores this
	// list; readiness is where "declared but never wired up" must fail
	// closed. Expressed through the existing degraded envelope (the
	// protocol's platform.missing_probe taxonomy entry) — no wire change,
	// no protocol bump.
	Required []string

	// ProbeMetrics, when set, receives the outcome and latency of every
	// probe the aggregate /healthz and /readyz handlers run, plus the
	// aggregate result of each evaluation. It is the seam through which
	// these endpoints' observability flows: the platform module deliberately
	// takes no direct OpenTelemetry dependency (keeping it importable by any
	// workload, exporter or not), so a workload bridges this interface to the
	// telemetry meter — see ProbeMetrics for the canonical recipe. Left nil
	// (the default) every callback is skipped, so probe execution is
	// unchanged and allocation-free.
	ProbeMetrics ProbeMetrics

	// RedactProbeErrors replaces a failing probe's verbatim error with a
	// generic "unhealthy" message in the /healthz and /readyz response,
	// logging the real cause server-side instead. Probe errors routinely
	// carry internal detail (DB driver text, "dial tcp 10.0.2.5:5432:
	// connect: connection refused", …); because these endpoints are
	// unauthenticated and mounted at root by default, an on-path caller can
	// otherwise read internal topology whenever a dependency is unhealthy.
	// Off by default to preserve the protocol's verbatim-error contract;
	// enable it when the endpoints are reachable from an untrusted network.
	RedactProbeErrors bool
}

// HealthCheckFunc is the function shape stored internally for both
// auto-discovered (app.HealthChecker) and explicitly added probes. It
// matches the existing http.HealthCheckFunc.
type HealthCheckFunc func(ctx context.Context) error

// ProbeKind distinguishes which aggregate endpoint a probe outcome came
// from, so a metrics sink can tag liveness and readiness separately.
type ProbeKind string

const (
	// ProbeKindHealth marks an outcome produced by /healthz (liveness).
	ProbeKindHealth ProbeKind = "health"
	// ProbeKindReadiness marks an outcome produced by /readyz (readiness).
	ProbeKindReadiness ProbeKind = "readiness"
)

// ProbeOutcome is the result the aggregate handler observed for a single
// probe in one evaluation.
type ProbeOutcome string

const (
	// ProbeOutcomePass means the probe returned nil within its timeout.
	ProbeOutcomePass ProbeOutcome = "pass"
	// ProbeOutcomeFail means the probe returned a non-nil error within its
	// timeout (this includes a probe returning ctx.Err() at its deadline).
	ProbeOutcomeFail ProbeOutcome = "fail"
	// ProbeOutcomeTimeout means the probe did not report before the
	// aggregate deadline and was abandoned (it ignored cancellation and is
	// still running in the background).
	ProbeOutcomeTimeout ProbeOutcome = "timeout"
)

// ProbeMetrics is the optional sink for probe observability. The aggregate
// /healthz and /readyz handlers call RecordProbe once per probe per
// evaluation and RecordAggregate once per evaluation. Implementations MUST
// be safe for concurrent use (orchestrators poll these endpoints
// continuously and probes run in parallel) and MUST NOT block — they run on
// the request path.
//
// The platform module takes no OpenTelemetry dependency itself; bridge this
// interface to the telemetry meter in the workload, e.g.:
//
//	type otelProbeMetrics struct {
//	    failures metric.Int64Counter
//	    duration metric.Float64Histogram
//	    ready    metric.Int64Gauge
//	}
//
//	func (m *otelProbeMetrics) RecordProbe(ctx context.Context, k platform.ProbeKind, name string, o platform.ProbeOutcome, d time.Duration) {
//	    attrs := metric.WithAttributes(
//	        attribute.String("probe", name),
//	        attribute.String("kind", string(k)),
//	        attribute.String("outcome", string(o)),
//	    )
//	    m.duration.Record(ctx, d.Seconds(), attrs)
//	    if o != platform.ProbeOutcomePass {
//	        m.failures.Add(ctx, 1, attrs)
//	    }
//	}
//
//	func (m *otelProbeMetrics) RecordAggregate(ctx context.Context, k platform.ProbeKind, healthy bool) {
//	    v := int64(0)
//	    if healthy { v = 1 }
//	    m.ready.Record(ctx, v, metric.WithAttributes(attribute.String("kind", string(k))))
//	}
//
// Because the recorder is built on the global meter provider, the records
// are a no-op until an exporter (e.g. the telemetry plugin's OTLP reader) is
// installed.
type ProbeMetrics interface {
	// RecordProbe reports the outcome and wall-clock latency of one probe.
	// For an abandoned probe (ProbeOutcomeTimeout) the duration is the
	// aggregate deadline the handler waited, not the probe's true runtime
	// (which is unbounded — the goroutine is still running).
	RecordProbe(ctx context.Context, kind ProbeKind, probe string, outcome ProbeOutcome, dur time.Duration)
	// RecordAggregate reports whether the evaluation as a whole passed
	// (every probe healthy). Suitable for a ready/healthy gauge.
	RecordAggregate(ctx context.Context, kind ProbeKind, healthy bool)
}

// probeKindFor maps the internal checker set to its public ProbeKind.
func probeKindFor(which checkerSet) ProbeKind {
	if which == checkersReadiness {
		return ProbeKindReadiness
	}
	return ProbeKindHealth
}

// Plugin mounts the standard operational endpoints on an http server
// and aggregates probes contributed by other plugins.
//
// The probe maps are guarded by mu: AddHealthChecker / AddReadinessChecker
// and the one-shot auto-discovery write under mu.Lock(); the /healthz and
// /readyz request paths read under mu.RLock(). The lifecycle contract is
// still "register before Start," but the lock makes accidental post-Start
// registration safe rather than a data race.
type Plugin struct {
	cfg               Config
	prefix            string
	ready             atomic.Bool
	mu                sync.RWMutex
	healthCheckers    map[string]HealthCheckFunc
	readinessCheckers map[string]HealthCheckFunc
	root              *app.Module
	discoverOnce      sync.Once
	log               *logger.Logger
}

// NewPlugin creates a new platform plugin with the given config. The
// returned plugin must be registered on an *http.ServerPlugin via
// RegisterOn for the endpoints to be reachable.
func NewPlugin(cfg Config) *Plugin {
	return &Plugin{
		cfg:               cfg,
		prefix:            protocol.NormalizePrefix(cfg.Prefix),
		healthCheckers:    make(map[string]HealthCheckFunc),
		readinessCheckers: make(map[string]HealthCheckFunc),
		log:               logger.Default().Named("platform"),
	}
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "platform" }

// AddHealthChecker registers an explicit liveness probe under name. Use
// for probes not owned by a plugin (an external URL, an ad-hoc check).
// Explicit registrations win over auto-discovered probes of the same
// name, so a workload can override a plugin-provided probe.
//
// The name is validated against the protocol's probe-name contract; a
// non-conforming name is rejected with a CodeInvalidProbeName error and
// not registered.
//
// Intended use is before Start (matching Configure semantics). Calls
// after Start are still safe but the registration becomes visible to
// any /healthz request that runs strictly after the AddHealthChecker
// call returns.
func (p *Plugin) AddHealthChecker(name string, checker HealthCheckFunc) error {
	if err := validateProbeName(name); err != nil {
		return err
	}
	p.mu.Lock()
	p.healthCheckers[name] = checker
	p.mu.Unlock()
	return nil
}

// AddReadinessChecker registers an explicit readiness probe under name.
// Same precedence, validation, and lifecycle rules as AddHealthChecker.
func (p *Plugin) AddReadinessChecker(name string, checker HealthCheckFunc) error {
	if err := validateProbeName(name); err != nil {
		return err
	}
	p.mu.Lock()
	p.readinessCheckers[name] = checker
	p.mu.Unlock()
	return nil
}

// Configure captures the module tree root so probe discovery can run
// later, on the first /healthz or /readyz request. Discovery is NOT done
// here: plugin Configure runs sequentially in registration order, so a
// plugin that registers a probe via app.Contribute in its own Configure
// would be invisible to a platform plugin configured before it. Deferring
// to the request path makes the order irrelevant — by the time any probe
// is served, every plugin has finished configuring.
func (p *Plugin) Configure(_ context.Context, owner *app.Module) error {
	if owner != nil {
		p.root = owner.Root()
	}
	return nil
}

// ensureDiscovered runs probe auto-discovery exactly once, walking the
// captured module tree for app.HealthChecker / app.ReadinessChecker
// implementers (and values registered via app.Contribute). Explicit
// AddHealthChecker / AddReadinessChecker entries are preserved —
// auto-discovery never overwrites them. Safe to call from the request
// path: sync.Once serializes the one-time write against concurrent reads.
func (p *Plugin) ensureDiscovered() {
	p.discoverOnce.Do(func() {
		if p.root == nil {
			return
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		for _, hc := range app.Collect[app.HealthChecker](p.root) {
			if _, exists := p.healthCheckers[hc.Name()]; !exists {
				p.healthCheckers[hc.Name()] = hc.CheckHealth
			}
		}
		for _, rc := range app.Collect[app.ReadinessChecker](p.root) {
			if _, exists := p.readinessCheckers[rc.Name()]; !exists {
				p.readinessCheckers[rc.Name()] = rc.CheckReadiness
			}
		}
	})
}

// Start marks the application as running. /healthz and /readyz return
// 503 until Start is called and 503 again after Stop.
//
// Before flipping the running flag it validates auto-discovered probe
// names and Config.Required names against the protocol contract. Probe
// discovery is deferred to the request path, but by Start every plugin has
// configured, so the module tree is complete enough to name-check: a probe
// whose Name() (or a required name) is non-conforming would make /healthz or
// /readyz emit an envelope that fails the protocol's own validator, so Start
// fails fast instead.
func (p *Plugin) Start(_ context.Context, _ *app.Module) error {
	if p.root != nil {
		for _, hc := range app.Collect[app.HealthChecker](p.root) {
			if err := validateProbeName(hc.Name()); err != nil {
				return err
			}
		}
		for _, rc := range app.Collect[app.ReadinessChecker](p.root) {
			if err := validateProbeName(rc.Name()); err != nil {
				return err
			}
		}
	}
	// Config.Required names surface as synthesized /readyz checks-entry keys when
	// the probe is missing, so a non-conforming required name would make /readyz
	// emit an envelope that fails the protocol's own validator. Name-check it here
	// too, symmetric with the discovered/registered probe names above, so a
	// misconfigured Required fails fast at Start instead of at request time.
	for _, name := range p.cfg.Required {
		if err := validateProbeName(name); err != nil {
			return err
		}
	}
	p.ready.Store(true)
	return nil
}

// Stop flips the running flag back off.
func (p *Plugin) Stop(_ context.Context, _ *app.Module) error {
	p.ready.Store(false)
	return nil
}

// RegisterOn mounts the platform endpoints on the given http server
// plugin. Call this after constructing both plugins and before
// app.ListenAndServe so route registration happens before the http
// server's Start phase.
func (p *Plugin) RegisterOn(server *http.ServerPlugin) {
	server.GET(p.prefix+protocol.PathLivez, p.livezHandler())
	server.GET(p.prefix+protocol.PathHealthz, p.healthzHandler())
	server.GET(p.prefix+protocol.PathReadyz, p.readyzHandler())
	server.GET(p.prefix+protocol.PathVersion, p.versionHandler())
	if p.cfg.EnablePprof {
		p.registerPprof(server)
	}
}

// livezHandler is intentionally minimal: if the handler can run, the
// process is alive. No probes, no flags. This is the endpoint a
// Kubernetes liveness probe should call — a slow probe here causes pod
// restarts.
func (p *Plugin) livezHandler() http.Handler {
	return func(_ *http.Context) *http.Response {
		return http.JSON(map[string]string{"status": "ok"})
	}
}

// healthzHandler aggregates the running flag and every HealthChecker
// probe. Returns 503 with status="unavailable" before Start / after
// Stop, or 503 with status="degraded" and per-probe detail when any
// probe fails.
func (p *Plugin) healthzHandler() http.Handler {
	return p.aggregateHandler(checkersHealth)
}

// readyzHandler aggregates the running flag and every ReadinessChecker
// probe. Returns 503 with status="unavailable" before Start / after
// Stop, or 503 with status="degraded" and per-probe detail when any
// probe fails.
func (p *Plugin) readyzHandler() http.Handler {
	return p.aggregateHandler(checkersReadiness)
}

// pickCheckers chooses the right registry (healthz vs readyz) without
// the handler closing over a map reference that bypasses the mutex.
type checkerSet int

const (
	checkersHealth checkerSet = iota
	checkersReadiness
)

// aggregateGrace is the slack added on top of the per-probe timeout when
// bounding the aggregate handler. A cooperative probe returns the moment
// its own ctx deadline fires (at the per-probe timeout); the grace gives
// that real result time to land before the handler gives up and reports
// the probe as timed out. It also bounds how long a non-cooperative probe
// (one that ignores ctx) can delay the response.
const aggregateGrace = 250 * time.Millisecond

// probeResult carries a single probe's outcome back to the aggregate
// handler over a buffered channel, keyed by the probe's index in the
// snapshot. Passing results by channel (rather than writing a shared
// slice from each goroutine) keeps the handler race-free even when it
// stops reading at the deadline while a runaway probe is still running.
type probeResult struct {
	idx  int
	msg  string
	pass bool
	dur  time.Duration
}

func (p *Plugin) aggregateHandler(which checkerSet) http.Handler {
	return func(ctx *http.Context) *http.Response {
		if !p.ready.Load() {
			return http.JSONStatus(protocol.HTTPStatusUnavailable, map[string]string{
				"status": string(protocol.StatusUnavailable),
			})
		}

		p.ensureDiscovered()

		// Snapshot the names + funcs under RLock so we don't hold the
		// lock during probe execution. Sort by name so the response
		// ordering is deterministic (helps tests; harmless otherwise).
		names, probes := p.snapshot(which)

		// Readiness treats Config.Required names that were never registered
		// or discovered as failing: a workload that declares a dependency
		// required but never wires it up must not report ready. Computed
		// against the same snapshot so the check is consistent with what
		// the probes below observe.
		missing := p.missingRequired(which, names)

		if len(names) == 0 && len(missing) == 0 {
			return http.JSON(map[string]string{"status": string(protocol.StatusOK)})
		}

		// Run probes in parallel. Each probe gets its OWN per-probe
		// timeout — a shared timeout was an earlier bug: if probe #1
		// burned 80% of the budget, probes #2..N would all report
		// "context deadline exceeded" instead of their real status,
		// breaking the protocol's "verbatim error" contract.
		//
		// Results arrive on a buffered channel sized to len(names) so a
		// probe that ignores ctx and runs past its deadline can still
		// deliver its result and exit (no goroutine leak on the send) —
		// the handler simply stops waiting at the aggregate deadline.
		timeout := p.probeTimeout()
		resultCh := make(chan probeResult, len(names))
		for i, probe := range probes {
			go func(idx int, fn HealthCheckFunc) {
				probeCtx, cancel := context.WithTimeout(ctx.Context(), timeout)
				defer cancel()
				started := time.Now()
				msg := "ok"
				pass := true
				if err := fn(probeCtx); err != nil {
					msg = err.Error()
					pass = false
				}
				resultCh <- probeResult{idx: idx, msg: msg, pass: pass, dur: time.Since(started)}
			}(i, probe)
		}

		// Bound the wait on the aggregate deadline, independent of probe
		// goroutine completion. context.WithTimeout cancels the ctx handed
		// to a probe but cannot preempt a goroutine that ignores it, so
		// blocking until every goroutine finishes would let one runaway
		// probe stall the response (and leak goroutines under repeated
		// orchestrator polling). Instead, collect results until the
		// deadline; any probe that has not reported by then is abandoned
		// and recorded as timed out.
		//
		// Probe observability flows through the optional ProbeMetrics sink
		// (nil by default, then every record below is skipped). kind tags
		// liveness vs readiness so a single sink can serve both endpoints;
		// the request ctx carries the trace span for exemplars.
		mx := p.cfg.ProbeMetrics
		kind := probeKindFor(which)

		results := make([]string, len(names))
		reported := make([]bool, len(names))
		deadline := time.NewTimer(timeout + aggregateGrace)
		defer deadline.Stop()
		collected := 0
	collect:
		for collected < len(names) {
			select {
			case r := <-resultCh:
				reported[r.idx] = true
				results[r.idx] = r.msg
				collected++
				if mx != nil {
					outcome := ProbeOutcomePass
					if !r.pass {
						outcome = ProbeOutcomeFail
					}
					mx.RecordProbe(ctx.Context(), kind, names[r.idx], outcome, r.dur)
				}
			case <-deadline.C:
				break collect
			}
		}

		if collected < len(names) {
			pending := make([]string, 0, len(names)-collected)
			for i, name := range names {
				if !reported[i] {
					results[i] = fmt.Sprintf("probe did not complete within %s", timeout)
					pending = append(pending, name)
					if mx != nil {
						// The probe is still running; its true latency is
						// unbounded. Record the deadline we actually waited.
						mx.RecordProbe(ctx.Context(), kind, name, ProbeOutcomeTimeout, timeout+aggregateGrace)
					}
				}
			}
			p.log.WarnCtx(ctx.Context(),
				"health probe(s) exceeded the aggregate deadline and were abandoned (still running in the background)",
				slog.String("probes", strings.Join(pending, ", ")),
				slog.Int("count", len(pending)),
				slog.Duration("timeout", timeout),
			)
		}

		checks := make(map[string]string, len(names))
		allHealthy := true
		for i, name := range names {
			msg := results[i]
			if msg != "ok" {
				allHealthy = false
				// Only reported probes carry a verbatim err.Error(); the
				// abandoned-probe placeholder is already generic. Redact the
				// real cause from the response (logging it) when configured,
				// so an unauthenticated caller can't read internal detail.
				if reported[i] && p.cfg.RedactProbeErrors {
					p.log.WarnCtx(ctx.Context(), "health probe failed",
						slog.String("probe", name),
						slog.String("error", msg),
					)
					msg = "unhealthy"
				}
			}
			checks[name] = msg
		}

		// Synthesize a failing entry for every required readiness probe that
		// was absent from the snapshot. Added after the probe results (and
		// after redaction) so it never collides with a real probe of the
		// same name — a required name that IS registered lands in `names`
		// above and is not reported here. The message is deliberately not a
		// probe error (it carries no internal detail), so redaction skips it.
		for _, name := range missing {
			checks[name] = missingRequiredProbeMessage
			allHealthy = false
		}

		if mx != nil {
			mx.RecordAggregate(ctx.Context(), kind, allHealthy)
		}

		status := protocol.StatusOK
		httpStatus := protocol.HTTPStatusOK
		if !allHealthy {
			status = protocol.StatusDegraded
			httpStatus = protocol.HTTPStatusUnavailable
		}
		return http.JSONStatus(httpStatus, map[string]any{
			"status": string(status),
			"checks": checks,
		})
	}
}

// snapshot returns a (names, probes) parallel-array pair under RLock.
// Names are sorted for deterministic response ordering.
func (p *Plugin) snapshot(which checkerSet) (names []string, probes []HealthCheckFunc) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	source := p.healthCheckers
	if which == checkersReadiness {
		source = p.readinessCheckers
	}
	names = make([]string, 0, len(source))
	for n := range source {
		names = append(names, n)
	}
	sort.Strings(names)
	probes = make([]HealthCheckFunc, len(names))
	for i, n := range names {
		probes[i] = source[n]
	}
	return names, probes
}

// missingRequired returns the Config.Required names that are absent from
// present (the snapshot's sorted probe-name set). It applies only to
// readiness — /healthz ignores the required list because a missing
// dependency should drain traffic (readiness), not restart the pod
// (liveness). The result is de-duplicated and preserves Config.Required
// order for a stable, deterministic response.
func (p *Plugin) missingRequired(which checkerSet, present []string) []string {
	if which != checkersReadiness || len(p.cfg.Required) == 0 {
		return nil
	}
	have := make(map[string]bool, len(present))
	for _, n := range present {
		have[n] = true
	}
	var missing []string
	seen := make(map[string]bool, len(p.cfg.Required))
	for _, req := range p.cfg.Required {
		if have[req] || seen[req] {
			continue
		}
		seen[req] = true
		missing = append(missing, req)
	}
	return missing
}

func (p *Plugin) probeTimeout() time.Duration {
	if p.cfg.ProbeTimeout > 0 {
		return p.cfg.ProbeTimeout
	}
	return defaultProbeTimeout
}
