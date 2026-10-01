# ADR 0001 — Version 2 members are optional; a version 1 document may not carry them

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/job` (`protocols/job`)

## Context

The orchestrator writes the job execution context and a subprocess reads it.
The two release independently: an extension binary may be older or newer than
the CLI, and a recorded context may be read long after.

v2 declares members whose producers arrive at different times: the extension's
runtime executable and cache root, provider-owned project metadata, the
invocation locator, the project's resolved type and dependency closure, and
`workspace.options` (committed workspace-level extension configuration, kept
raw because the named extension owns its semantics). Declaring them up front
avoids reshaping the contract under live consumers.

A document without `protocolVersion` is v1; that absence is the only
discriminator.

## Decision

1. **A v2 member may be optional, never unconstrained.** A producer emits an
   optional member only for work that declares the matching lifecycle; a
   consumer handles absence. When present: paths are absolute; staging roots do
   not nest or coincide; `identity.key` agrees with its structured members
   (`job.invalid_key`); `project.metadata` is a JSON object under a non-empty
   extension-namespaced key (`job.invalid_metadata`); `workspace.options` is a
   non-null object of non-null object blocks (`job.invalid_value`). A later
   producer wires an existing shape instead of renegotiating it.
2. **A v1 document may not carry a v2 member** (`job.unexpected_field`).
   Otherwise a v1 consumer ignores exactly what a v2 consumer trusts. For
   example, a v1 consumer ignoring `selectedProjects[].sourceName` would rename
   the manifest the identity came from and orphan every sibling reference.
3. **Consumers are lenient; producers and conformance are strict.** `Parse`
   and `ParseFile` ignore unknown fields, so an older extension keeps working.
   `ParseStrict` and `ParseAndValidate` reject unknown fields and enforce
   required ones.
4. **The v1 corpus is frozen.** `fixtures/valid` is what other SDKs validate
   against; new members live in `fixtures/v2/` only.
   `TestV1DocumentBytesUnchanged` and `TestV1FixturesStayV1` pin this.

## Rejected alternatives

- **Require every v2 member.** Forces placeholders, hides "unknown" behind
  "none", and makes each new member breaking.
- **A version bump per member.** Version is the vocabulary generation, not a
  changelog counter.
- **v1 documents carrying v2 members.** Two consumers disagree silently about
  the same bytes.
- **New members in `fixtures/valid`.** Breaks parsers this repository does not
  own.
- **Strict consumers.** Every orchestrator addition would break every older
  extension.

## Consequences

- A member can exist with no producer, so it must be specified and testable
  before anything emits it.
- A consumer must not invent a default for an absent member; declining to act
  (as sync does without `sourceName`) is correct.
- A task acting on `workspace.options` must include the committed workspace
  file in its declared cache read set, or a restored verdict goes stale.
- Adding `protocolVersion` alone never upgrades a document; mixing members
  across versions is rejected both ways.
- Removing a v2 member is breaking even if nothing emits it.
