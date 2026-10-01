package security

import (
	"expvar"
	"sync"
)

// Token introspection observability.
//
// Introspection is an online, security-critical operation, so each resolver
// outcome is counted via the stdlib expvar registry (scrapable from
// /debug/vars without a metrics backend), mirroring the authorization decision
// counters in observe.go. The counters never carry a token, secret, or claim
// value — only the outcome label.
//
// Two families share one map:
//   - per-request outcomes (exactly one per resolved request): cache_hit,
//     active, inactive, audience_mismatch, client_credentials_rejected,
//     endpoint_error, breaker_open, throttled. Their sum is the request volume
//     handled by the resolver.
//   - per-call load: endpoint_call counts actual introspection HTTP requests
//     issued upstream, including the single retry after a 429.
//     requests_handled - endpoint_call is what the cache and in-flight
//     de-duplication saved the auth server, net of those retries.

type introspectOutcome string

const (
	introspectCacheHit            introspectOutcome = "cache_hit"
	introspectActive              introspectOutcome = "active"
	introspectInactive            introspectOutcome = "inactive"
	introspectAudienceMismatch    introspectOutcome = "audience_mismatch"
	introspectCredentialsRejected introspectOutcome = "client_credentials_rejected"
	introspectEndpointError       introspectOutcome = "endpoint_error"
	introspectEndpointCall        introspectOutcome = "endpoint_call"
	// introspectBreakerOpen counts requests short-circuited by an open circuit
	// breaker: an uncached token that failed closed WITHOUT an upstream call
	// because consecutive upstream failures tripped the breaker. It is a
	// per-request outcome (like endpoint_error), so the endpoint the breaker
	// spared is requests_handled - endpoint_call minus the cache hits.
	introspectBreakerOpen introspectOutcome = "breaker_open"
	// introspectThrottled counts requests that failed closed because the
	// endpoint answered 429 Too Many Requests and still throttled after the
	// single bounded retry (or the leader's budget could not cover the
	// Retry-After wait). It is a per-request outcome like endpoint_error, but it
	// never counts toward the circuit breaker.
	introspectThrottled introspectOutcome = "throttled"
)

var (
	introspectOutcomes     *expvar.Map
	introspectOutcomesOnce sync.Once
)

// introspectCounters returns the process-wide introspection counters, publishing
// them on first use. Publishing lazily (rather than in an init) keeps a second
// import of the package from panicking on a duplicate expvar registration.
func introspectCounters() *expvar.Map {
	introspectOutcomesOnce.Do(func() {
		if existing := expvar.Get("security.introspect"); existing != nil {
			if m, ok := existing.(*expvar.Map); ok {
				introspectOutcomes = m
				return
			}
		}
		introspectOutcomes = expvar.NewMap("security.introspect")
	})
	return introspectOutcomes
}

// recordIntrospect increments the counter for a single introspection outcome.
func recordIntrospect(o introspectOutcome) {
	introspectCounters().Add(string(o), 1)
}
