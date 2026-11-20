# ADR 0002 — An export directive sets the trace settings, and a service may decide behind an outside parent

- **Status**: accepted
- **Date**: 2026-11-20
- **Scope**: `go.putnami.dev/protocol/telemetry` (`protocols/telemetry`)

## Context

Putnami Cloud stores telemetry settings per workspace and per service, and
the workspace collector returns them on each export as the
`Putnami-Export-Directive` header. Until now a directive could only tighten:
a service kept the lower trace ratio and floor of the directive and its own
settings, and its own budget. A person who wanted to see more traces of a
running service had no way to raise its sampling without a redeploy.

Behind a load balancer such as Cloud Run's, the edge sends a `traceparent`
whose sampled flag is usually `00`. A server span with a parent followed that
flag, so almost every server span of a web app behind the edge was dropped,
with its children, whatever `telemetry.traces` said.

## Decision

1. **A directive's trace settings win.** While a directive holds, each trace
   setting it carries, `trace-budget`, `trace-ratio`, `trace-floor` and
   `trace-parent`, replaces the service's own, higher or lower. A trace
   setting it leaves out keeps the service's own. The only producer of
   directives is Putnami Cloud, which renders the settings a person stored, so
   the person's setting wins for its ttl.
2. **The log level only tightens.** The service applies the higher of the
   directive's level and its own, and a record at error always passes.
   Lowering the level from Cloud would need each logger to emit below its own
   level; that is not part of this decision.
3. **The parent policy.** `telemetry.traces.parent`, and `trace-parent` in a
   directive, is `follow` (the default) or `decide`. Under `decide`, a server
   span whose remote parent comes from outside takes a root decision by the
   sampling rule, and keeps the parent's trace ID, parent span ID and
   tracestate.
4. **Outside means no Putnami client sent it.** Every Putnami client writes
   its span context to `traceparent` and to `x-putnami-traceparent`. A request
   with a valid `traceparent` and no valid `x-putnami-traceparent` on the same
   trace came from outside: a load balancer's edge, a browser, a service that
   does not run a Putnami runtime. A parent a Putnami client sent, a local
   parent and an event's parent are followed under both policies, so one
   decision still holds across the services of a trace.

## Rejected alternatives

- **A separate key that marks a directive as a raise.** Two meanings for the
  same keys, for no producer that needs the tightening one.
- **Decide behind every remote parent.** A call between two Putnami services
  would decide twice, and a trace would lose the spans of one side.
- **Keep an outside parent's sampled flag when it is set.** An outside caller
  could then force sampling, and its cost, on any service.

## Consequences

- A runtime released before this decision keeps tightening only: a raise
  reaches it as no change, a lowering still applies.
- A raise multiplies a service's trace volume for its ttl; Cloud bounds each
  field it stores, and the ttl is at most 3600 seconds.
- Under `decide`, a caller that sends `traceparent` alone, such as a browser
  or a load balancer's edge, can no longer force sampling, as it can under
  `follow`. A caller that also sends a matching `x-putnami-traceparent` is
  followed, as a Putnami client is: the header proves no identity, so the
  parent policy bounds the edge's influence, not a deliberate caller's.
