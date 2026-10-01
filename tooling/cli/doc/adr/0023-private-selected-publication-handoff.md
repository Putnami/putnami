# ADR 0023 — Private publication handoff, and the release-set stamp as publication gate

- **Status**: accepted
- **Scope**: `@putnami/cli` (`tooling/cli/internal/jobs`: release-plan
  handoff, runner capability gates, release evidence) and
  `@putnami/extension-sdk` (`tooling/extension-sdk/releaseset` provider client)

## Context

The Framework planner owns the exact member selection: source revision,
selection fingerprints and ordered channels. An image-owned runner broker needs
that selection before it can request narrow publication authority. A
repository job must receive neither the runner credential nor a renewable
Distribution bearer.

A hosted runner hands the CLI a cloud capability and an AFTER contract
(`PUTNAMI_CLOUD_CAPABILITY_AFTER`: the commands every protected write waits
for). Holding every registry write until the last test of a workspace with
hundreds of members ends lengthens the run and bursts the writes into the
registries' rate limits. The AFTER gate is a policy, not the transport
boundary: the cloud token is captured before any repository code runs, and the
scheduler delivers it only to a protected job whose functional ancestors
succeeded.

## Decision

### 1. Release-plan callback

After attaching the final release plan and barrier, the CLI can send one
private callback to `/v1/release-plan` on an explicitly supplied loopback IP
and port. The process captures and removes
`PUTNAMI_INTERNAL_RELEASE_PLAN_CALLBACK` with its runner capability. It is an
internal runner transport, not a repository setting, job parameter or user
flag.

The JSON tuple has `protocolVersion: 1`, namespace, one immutable source
revision, ordered channels, the optional immutable tag channel, and canonically
ordered selected members with ecosystem, coordinate, version, source revision
and selection fingerprint. `planDigest` is `sha256:` plus the digest of this
JSON with that final field omitted. Provider heads, local project paths,
archive bytes and not-yet-produced artifact digests are excluded.

The callback must acknowledge exactly `{protocolVersion: 1, planDigest}`.
Redirects, extra fields, response credentials, divergent plans and oversized
responses are refused. Only a transport failure is retried, once; the broker
deduplicates by digest. A process binds to one plan digest and refuses a
second, different plan. Plan previews and dry runs make no callback. A plan
that selects nothing is handed off with an empty member list, because the
finalizer still releases the head set unchanged
([ADR 0017](0017-one-release-set-plan-against-the-channel-head.md)); the
authority the broker obtains for it is the release set's alone. An
acknowledgment never bypasses the scheduler's functional gates: only
already-authorized publication jobs receive the opaque local client token.

A callback without a non-empty `PUTNAMI_CLOUD_CAPABILITY_AFTER` is refused at
capture with a credential-free stderr diagnostic, and the token is withheld
from jobs and the ambient environment, including on build-only runs.

The runner keeps its renewable credential outside the checkout, freezes and
validates the tuple through Delivery, renews the resulting narrow registry
authority inside its broker, and routes every participating registry protocol
through that broker. A broker without OCI support must refuse selected OCI
members. No Distribution bearer is returned to the CLI or stored on disk.
Without a callback, local execution is unchanged.

### 2. The release-set stamp is the publication gate

The engine materializes the captured AFTER contract as functional DAG
dependencies before final authorization. A **release-set member publication**
waits only for its own `package` chain. The AFTER leaves gate the
**release-set stamp**: the same-session barrier `publish~release-set` when a
deploy shares the session, otherwise the finalizer, which withholds the release
when any result failed or was canceled. Every other protected write (the
barrier, a deploy, a publish outside a release set) waits for every required
gate leaf, including upstream build and describe steps.

Gating grants no capability: the independent authorizer still checks missing
or no-op gates and the exact graph, and the scheduler requires successful
terminal results. A cycle between a gate and a publication is rejected without
changing the input plan. When the stamp is withheld, no channel head moves;
versions already published stay unreferenced by any set.

### 3. Release evidence for the reserved provider

The release call also passes the reserved release-set provider two private
files, which the SDK owns, strips of ambient substitutions, and removes after
the call. The CLI grants them only to the exact reserved provider invocation.

- **Image evidence**: one entry per selected native OCI member, with its
  project and reconciled immutable digest. Inherited members are never
  reported as publications of this session. The v1 envelope holds one OCI
  member per project; a project with several carries no image evidence, so a
  Cloud hook that requires it refuses the handoff while the release itself
  proceeds.
- **Member provenance**: each selected member's frozen project, publisher,
  command and step beside its reconciled immutable tuple, so Cloud identifies a
  producer by its declared route instead of guessing from a package name.

Both carry assertions for Cloud's owner validation, never runtime credentials.
The released set, not this evidence, stays the deploy authority.

### 4. A failed provider names its reason

The provider client carries the last non-empty stderr line of a non-zero exit.
It strips control characters, then redacts every value (8 bytes or longer) of a
secret-shaped key in the child environment (`TOKEN`, `SECRET`, `CREDENTIAL`,
`CAPABILITY`, `PASSWORD`, `BEARER`, `KEY`, `AUTHORIZATION`) and every bearer
value, then bounds the line to 512 bytes. Any other secret the provider
obtained on its own is the provider's to keep out of its output.

## Consequences

- Registry writes spread over the run. A member publication does not wait for
  another project's tests, so its artifact may exist for a set that is never
  released.
- A hosted runner passes the workflow's command list as AFTER and needs no
  other change.
- Rolling back removes the runner callback configuration and restores the
  previous runner version; no durable CLI state or protocol pin migrates.
