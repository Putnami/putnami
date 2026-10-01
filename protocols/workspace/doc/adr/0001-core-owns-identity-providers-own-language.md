# ADR 0001: Core owns identity, providers own language knowledge

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`) probe
  contract

## Context

Recognizing a project needs language knowledge core must not acquire: a Go
module, a `package.json` workspace member, and a Python package follow different
rules on their ecosystems' schedules. Naming a project, placing it, and wiring
its dependencies is what core cannot delegate: project identity keys the
dependency graph, impacted selection, cache keys, and every `--projects`
selector. Two providers minting identity would give one workspace two answers.

The probe result is also a cache-key input. Anything in it that varies between
two runs over the same tree turns every run into a cold run.

## Decision

### 1. Providers answer, core decides

A provider answers a `ProbeRequest` with a `ProbeResult` describing the
directories it recognizes. Core alone assigns canonical project paths and IDs
(`MergeProbeResults` rule 1). A provider describes; it never names.

### 2. Everything is a repo-relative, slash-separated path

Projects, dependencies, source files and watched files are paths. Dependency
edges are project paths core resolves, never module identifiers a provider
resolved, because resolving edges decides graph membership. `sourceFile` may sit
arbitrarily deep below the project `path`.

### 3. Conflicts are errors, except where authorship decides

Explicit `putnami.json` values override probe values. Two providers reporting
different non-empty scalars for one project is a hard error unless the explicit
file decided the field. Lists are unioned and normalized, never replaced.
Provider-owned opinions go under `metadata[<extension>]`.

### 4. The digest is canonical, and content is the only oracle

`NormalizeProbeResult` collapses authoring order, path spelling, duplicates and
metadata key order; `ProbeResultDigest` hashes that form. The protocol carries
no timestamps and no file sizes: stat data may prioritize work, but only content
digests decide validity. Advisory `diagnostics` are excluded from the digest.

### 5. One document in, one document out, bounded

Core spawns `<executable> __putnami workspace-probe`, a reserved verb in the
namespace the runtime handshake owns, so no task can shadow it. Stdout carries
the result and nothing else; trailing data (a provider logging to stdout) is
rejected, not truncated. Both directions are capped at `MaxProbeDocumentBytes`.
`ServeProbe` validates the provider's answer before writing and writes nothing
on failure, so core sees "no result" rather than a half-truth.

## Rejected alternatives

- **Core detects projects itself.** Every ecosystem's rules would rot in core,
  and adding a language would be a core change.
- **Providers assign IDs or resolve edges.** Identity and graph membership would
  depend on which provider answered first.
- **Timestamps, sizes, or diagnostics in the digest.** Non-content facts would
  keep the cache cold.
- **A provider daemon or socket.** Lifecycle and version-skew costs for latency
  the one-shot exchange does not need.
- **Unbounded reads.** A provider that never terminates would hang the
  orchestrator; the cap turns a hang into a named failure.
- **Silent merge on conflict.** The graph would depend on provider ordering.

## Consequences

- Adding a language means adding a provider. Core's tests cannot cover
  discovery for a language whose provider is absent.
- A provider that logs to stdout breaks its own probe, loudly; `ServeProbe`
  lets authors hit that in their own tests.
- A new kind of fact goes under `metadata[<extension>]`; a new top-level member
  would make older strict consumers reject the result.
