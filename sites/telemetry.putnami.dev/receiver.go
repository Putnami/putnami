package main

import (
	stderrors "errors"
	stdhttp "net/http"
	"strings"
	"time"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	diag "go.putnami.dev/protocol/diagnostic"
	telemetry "go.putnami.dev/protocol/telemetry"
)

// Fixed request-handling limits (Task 2). The body cap matches the OTLP sender's
// own 1 MiB ceiling and the read timeout matches the sender's request deadline.
const (
	maxBodyBytes = 1 << 20 // 1 MiB
	readTimeout  = 2 * time.Second
)

// Admission limits (Task 4). Applied before any body read.
const (
	defaultPerIPPerMinute = 600 // per client IP, sliding 60s window
	defaultGlobalQPS      = 100 // per instance, sliding 1s window
)

// receiver is the anonymous OTLP/JSON logs endpoint. It parses, sanitizes, and
// best-effort persists CLI usage telemetry. It carries no session, API key, or
// cookie state — the surface is anonymous by design.
type receiver struct {
	emitter Emitter
	log     *logger.Logger
}

// handleLogs serves POST /v1/logs. Status contract:
//
//   - oversize body            → 413 (before parse; caught by the body cap)
//   - unreadable body          → 400
//   - malformed OTLP/JSON      → 400
//   - accepted (or drop)       → 202, byte-identical regardless of whether the
//     payload had content problems or the collector is down
//
// Wrong-signal paths (/v1/metrics, /v1/traces) are not mounted, so they 404 at
// the router before reaching here.
func (rc *receiver) handleLogs(ctx *phttp.Context) *phttp.Response {
	data, err := ctx.RawBody()
	if err != nil {
		if isBodyTooLarge(err) {
			return phttp.NewResponse(stdhttp.StatusRequestEntityTooLarge) // 413
		}
		return phttp.NewResponse(stdhttp.StatusBadRequest) // 400
	}

	// Transport-level malformed (undecodable JSON, unknown envelope fields) earns
	// a 400. Content problems past this point never do — they drop to 202.
	req, diags := telemetry.ParseLogsRequest(data)
	if diag.HasErrors(diags) {
		return phttp.NewResponse(stdhttp.StatusBadRequest) // 400
	}

	clean := sanitizeLogs(req)
	if len(clean.ResourceLogs) > 0 {
		rc.emitter.Emit(clean.ResourceLogs)
	}

	// Fail-silent anonymous contract: a content problem is silently dropped, and
	// the response is indistinguishable from a fully accepted payload.
	return phttp.NewResponse(stdhttp.StatusAccepted) // 202
}

// isBodyTooLarge reports whether a body-read error is the 1 MiB cap tripping,
// so it maps to 413 rather than 400.
func isBodyTooLarge(err error) bool {
	var maxErr *stdhttp.MaxBytesError
	if stderrors.As(err, &maxErr) {
		return true
	}
	return strings.Contains(err.Error(), "too large")
}

// receiverConfig is the workload's runtime configuration, sourced from the
// environment so the workload configures its own collector and trust posture.
type receiverConfig struct {
	port              int
	collectorEndpoint string
	trustedProxies    []string
	perIPPerMinute    int
	globalQPS         int
	aggregateAuth     aggregateAuth
}

// newServer wires the anonymous receiver: recovery, request-id, access logging
// (which records method/path/status/duration/requestId — never a client IP),
// the global and per-IP admission limiters (before the handler, hence before any
// body read), the single anonymous POST /v1/logs route, and the private
// aggregate read route. No other OTLP signal path is mounted.
//
// The aggregate route's identity resolver and guard are route-scoped: the
// anonymous ingest path keeps running with no authentication middleware at all,
// exactly as before this route existed.
func newServer(cfg receiverConfig, emitter Emitter, aggregate *aggregateEndpoint) *phttp.ServerPlugin {
	server := phttp.NewServerPlugin(phttp.ServerConfig{
		Port:        cfg.port,
		ReadTimeout: readTimeout,
		MaxBodySize: maxBodyBytes,
	})

	server.Use(phttp.Recovery())
	server.Use(phttp.RequestID())
	server.Use(phttp.Logging(phttp.LoggerOptions{Exclude: []string{"/_/health"}}))
	server.Use(globalRateLimit(cfg.globalQPS))
	server.Use(perIPRateLimit(cfg.perIPPerMinute, cfg.trustedProxies))

	// A nil endpoint still mounts the route behind the fail-closed deny, so the
	// described route inventory never depends on how the workload was built.
	if aggregate == nil {
		aggregate = &aggregateEndpoint{}
	}
	for _, mw := range aggregate.middlewares() {
		server.Use(mw)
	}

	rc := &receiver{
		emitter: emitter,
		log:     logger.Default().Named("receiver"),
	}
	server.POST(telemetry.PathLogs, rc.handleLogs)
	server.GET(aggregatePath, aggregate.handle)
	return server
}

// globalRateLimit caps total instance throughput (~globalQPS req/s) so a single
// instance cannot be driven past its capacity, keying every request onto one
// counter.
func globalRateLimit(qps int) phttp.Middleware {
	return phttp.RateLimit(phttp.RateLimitOptions{
		WindowMs: 1000,
		Max:      qps,
		KeyFunc:  func(*phttp.Context) string { return "global" },
		Headers:  boolPtr(false),
	})
}

// perIPRateLimit caps per-client throughput (perMinute req/min) using the
// trusted-proxy-aware client-IP derivation. Bookkeeping is in-memory with a
// short TTL (entries expire after the window and are cleaned inline); no IP is
// persisted.
func perIPRateLimit(perMinute int, trustedProxies []string) phttp.Middleware {
	ts := parseTrustSet(trustedProxies)
	return phttp.RateLimit(phttp.RateLimitOptions{
		WindowMs: 60_000,
		Max:      perMinute,
		KeyFunc:  func(ctx *phttp.Context) string { return clientIP(ctx, ts) },
		Headers:  boolPtr(false),
	})
}

func boolPtr(b bool) *bool { return &b }
