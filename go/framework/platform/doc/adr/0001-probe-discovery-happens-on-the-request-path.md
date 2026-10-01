# ADR 0001 — Discover probes on the request path, validate their names at start

- **Status**: accepted
- **Scope**: `go.putnami.dev/platform` (`go/framework/platform`)

## Context

The platform plugin finds health and readiness probes by walking the module
tree. Plugins configure in registration order, so a probe contributed during a
later plugin's `Configure` is invisible to an earlier walk, and the
application reports healthy while a dependency is unmonitored. Probe names key
the envelope's `checks` map, and the protocol validator enforces a name
pattern. The endpoints are unauthenticated, and probe errors often name hosts,
ports, and drivers.

## Decision

Discovery runs once, on the first aggregate request, when every plugin has
configured. A discovered probe never overwrites an explicitly registered probe
of the same name, so a workload can override a plugin probe.

Explicit registration validates the name immediately. Discovered names and the
configured required names are validated at `Start`, where a failure still
stops the application.

Each probe runs in parallel under its own timeout. The aggregate answers after
a bounded grace period beyond that timeout, so a probe that ignores
cancellation delays the response by that period at most.

Readiness fails closed: a required name that is never registered or discovered
yields a synthesized failing entry. Health ignores the required list; a
missing dependency is a readiness question. `/livez` aggregates no probes.

Error redaction exists but is off by default, because the protocol requires
verbatim probe errors. A workload reachable from an untrusted network turns it
on.

## Rejected alternatives

- **Discover during `Configure`.** It is the ordering trap.
- **Rediscover per request.** The graph cannot change after start.
- **Validate names at discovery.** A wiring mistake would surface as a 500 on
  a health endpoint.
- **One shared deadline.** One slow probe would time out all others.
- **Redact by default.** It breaks the protocol's verbatim-error contract.
- **Aggregate probes in `/livez`.** A slow dependency would restart a live
  process.

## Consequences

- The first health or readiness request pays one tree walk.
- A probe registered after the first aggregate request is never discovered.
- Probes must honor their context; the framework bounds the delay but cannot
  kill a goroutine.
- Health and readiness can legitimately disagree.
