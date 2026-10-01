package security

import (
	"expvar"
	"log/slog"
	"sync"

	phttp "go.putnami.dev/http"
	"go.putnami.dev/logger"
	identity "go.putnami.dev/protocol/identity/schema"
)

// Per-request authorization observability.
//
// Authorization is a security-critical operation, so every allow/deny decision
// is both logged (structured, via the framework logger) and counted (via the
// stdlib expvar registry so the totals are scrapable without a metrics backend).
// Grants log at Debug to keep the steady-state log quiet; denials log at Warn so
// an operator can spot 401/403 spikes (credential stuffing, brute force) and
// attribute them to the failing rule. The correlation id (ctx.RequestID) is
// included so an auth outcome can be tied to the access log and downstream logs.
//
// The log carries the subject ("anonymous" when unauthenticated), the HTTP
// decision (401/403) and the failing dimension (the reason), but never the
// bearer token, secret, or the exact scope/role that was missing — that detail
// stays server-side and is never returned to the client.

// decision is the outcome label for an authorization check. The values double
// as the expvar counter keys and the "decision" log attribute, so dashboards and
// log queries share one vocabulary.
type decision string

// Alias-first adoption: each decision constant's underlying value is the
// generated identity.AuthDecision* constant, so the authorization vocabulary is
// single-sourced from protocols/identity (the same manifest the TypeScript
// framework adopts) while behavior stays byte-identical. A rename or wire-value
// change in the manifest is a deliberate, reviewed edit here rather than silent
// drift between the two frameworks.
const (
	decisionAllow               decision = decision(identity.AuthDecisionAllow)
	decisionDenyUnauthenticated decision = decision(identity.AuthDecisionDenyUnauthenticated)
	decisionDenyClient          decision = decision(identity.AuthDecisionDenyClient)
	decisionDenyScope           decision = decision(identity.AuthDecisionDenyScope)
	decisionDenyRole            decision = decision(identity.AuthDecisionDenyRole)
	decisionDenyGuard           decision = decision(identity.AuthDecisionDenyGuard)
)

// status returns the HTTP status a decision maps to: 401 for the
// unauthenticated case, 200 for a grant, 403 for any rule failure.
func (d decision) status() int {
	switch d {
	case decisionAllow:
		return 200
	case decisionDenyUnauthenticated:
		return 401
	default:
		return 403
	}
}

// authDecisions counts authorization outcomes keyed by decision label. It is
// published once under "security.auth_decisions" so every middleware instance in
// the process shares a single set of totals (e.g. expvar / /debug/vars exposes
// {"allow": N, "deny_role": M, ...}).
var (
	authDecisions     *expvar.Map
	authDecisionsOnce sync.Once
	authLog           *logger.Logger
	authLogOnce       sync.Once
)

// decisionCounters returns the process-wide auth decision counters, publishing
// them on first use.
func decisionCounters() *expvar.Map {
	authDecisionsOnce.Do(func() {
		authDecisions = expvar.NewMap("security.auth_decisions")
	})
	return authDecisions
}

// decisionLogger returns the security logger, resolved lazily so it picks up the
// process default configured at startup (level, sinks) rather than a snapshot
// taken at package-init time.
func decisionLogger() *logger.Logger {
	authLogOnce.Do(func() {
		authLog = logger.Default().Named("security")
	})
	return authLog
}

// recordDecision counts and logs a single authorization outcome. reason names the
// failing dimension (role/scope/client/guard) for denials and is empty for grants.
// It must not contain a token, secret, or the precise missing scope/role value.
func recordDecision(ctx *phttp.Context, d decision, reason string) {
	decisionCounters().Add(string(d), 1)

	subject := "anonymous"
	if ctx.User != nil && ctx.User.Subject != "" {
		subject = ctx.User.Subject
	}

	attrs := []slog.Attr{
		slog.String("decision", string(d)),
		slog.Int("status", d.status()),
		slog.String("subject", subject),
		slog.String("method", ctx.Method),
		slog.String("path", ctx.Path),
	}
	if reason != "" {
		attrs = append(attrs, slog.String("reason", reason))
	}
	if ctx.RequestID != "" {
		attrs = append(attrs, slog.String("requestId", ctx.RequestID))
	}

	log := decisionLogger()
	// Use the context-aware variants so a configured trace-id extractor can
	// correlate the decision, and so a canceled request still records its
	// outcome deterministically (logging is non-blocking, not gated on ctx).
	if d == decisionAllow {
		log.DebugCtx(ctx.Context(), "authorization granted", attrs...)
		return
	}
	log.WarnCtx(ctx.Context(), "authorization denied", attrs...)
}
