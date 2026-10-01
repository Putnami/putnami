package security

import (
	"expvar"
	"sync"
)

// Signing-keyring lifecycle observability.
//
// Rotating or revoking a signing key is a security-critical, low-frequency
// event an operator wants to watch (a rotation invalidates nothing in-flight
// thanks to the overlap window; an unexpected rotation cadence is worth an
// alert). Following the same stdlib expvar convention as observe.go's
// authorization counters and introspect_observe.go's introspection counters,
// each successful Rotate/Revoke is counted under a stable, scrapable name
// ("security.keyring_events", exposed via /debug/vars) so the cadence is
// visible without a metrics backend.
//
// SECRET-SAFETY INVARIANT: the counter carries ONLY an event-type label
// ("rotate"/"revoke") and an integer count. It never carries key material and
// never carries a kid. The kid is public (it travels in every JWS header and
// the published JWKS), but it is deliberately kept OFF the metric to keep the
// label cardinality bounded and to make the "no secret in a metric" guarantee
// trivially checkable on the serialized bytes (see keyring_observe_test.go).

// keyringEvent is the outcome label for a keyring lifecycle event. The values
// double as the expvar counter keys so a dashboard and a log query share one
// vocabulary.
type keyringEvent string

const (
	// keyringEventRotate counts a successful Rotate: a new active key installed
	// and the previous active key demoted to retiring for the overlap window.
	keyringEventRotate keyringEvent = "rotate"
	// keyringEventRevoke counts a successful Revoke: a non-active key marked
	// revoked and dropped from the published JWKS.
	keyringEventRevoke keyringEvent = "revoke"
)

// keyringEvents counts signing-keyring lifecycle events keyed by event label.
// It is published once under "security.keyring_events" so every provider in the
// process shares a single set of totals.
var (
	keyringEvents     *expvar.Map
	keyringEventsOnce sync.Once
)

// keyringEventCounters returns the process-wide keyring event counters,
// publishing them on first use.
func keyringEventCounters() *expvar.Map {
	keyringEventsOnce.Do(func() {
		keyringEvents = expvar.NewMap("security.keyring_events")
	})
	return keyringEvents
}

// recordKeyringEvent increments the counter for a single keyring lifecycle
// event. The label is a fixed event type; no key material or kid is ever passed.
func recordKeyringEvent(e keyringEvent) {
	keyringEventCounters().Add(string(e), 1)
}
