# ADR 0001 — Registry extensions are optional, probe-gated fast paths

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/oci` (`protocols/oci`)

## Context

A release assigns several refs (version tag, channel tag, often `latest`) to
one image. The standard OCI distribution API costs a manifest `GET` plus `PUT`
per tag. A Putnami-managed registry can assign them all in one authenticated,
digest-addressed call, but Putnami must keep publishing to registries it does
not control (gcr, ghcr, Docker Hub, Harbor) without lock-in.

## Decision

Putnami registry extensions are additive fast paths, never requirements.

1. **Probe first.** A client calls `GET /v2/_putnami/capabilities` and uses an
   extension only when its exact capability string (`tag-digest/v1`) is
   advertised. A non-200, an unparseable body, or a missing capability means no
   fast path.
2. **Every failure degrades.** A probe error, auth failure, or non-2xx
   extension response falls back to the standard API, and the publish succeeds.
3. **Extensions are versioned by capability string.** A breaking change ships
   as a new capability. Unknown `apis` entries are ignored.
4. **The reserved namespace is `_putnami`.** Repository name components cannot
   start with `_`, the same reservation `/v2/_catalog` relies on.
5. **Authentication reuses the distribution token flow** with push scope on the
   repository.
6. **No Putnami-only behavior** that a standard registry cannot emulate.

## Rejected alternatives

- **Require a Putnami registry.** Turns an optimization into a dependency.
- **Detect the registry by hostname.** Proxies and private deployments
  misclassify both ways.
- **Header or `?v=` versioning on one path.** One path with two shapes makes a
  partial deployment ambiguous.
- **A vendor prefix in the repository namespace** (`putnami/_tag-digest`). A
  legal repository name that can collide.
- **Fail the publish when the fast path errors.** A new outage mode for work
  the standard API can finish.

## Consequences

- Every extension ships and tests both the fast path and its fallback.
- `tooling/extension-sdk/oci` probes once per publish, cheaper than the per-tag
  round trips it removes.
- The server lives in the cloud repository, so no cross-implementation suite
  runs here; the contract is `preview` and the fixture corpus is the shared
  authority.
