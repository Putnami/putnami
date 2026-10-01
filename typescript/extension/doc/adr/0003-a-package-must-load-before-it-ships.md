# ADR 0003 — A package must load before it ships

- **Status**: accepted
- **Scope**: `@putnami/typescript` (`typescript/extension`)

## Context

A package that works in the workspace can fail in a consumer's runtime. Members
resolve through the monorepo's linked `node_modules`; a tarball resolves through
its own `exports` map and files. A missing staged file, an export pointing at an
unemitted source, or a hook manifest naming an uncopied binary all surface as
"Module not found" in someone else's project, after packaging reported success.

An extension manifest also declares which CLI contract it implements. A claim
newer than the packager knows was never validated. A stale lower claim sends the
CLI down a compatibility path the package no longer needs.

## Decision

Packaging proves loadability. It loads the staged package under the loader rules
of the CLI contract it claims and fails if the load fails.

The contract claim is checked both ways: a claim newer than the packager's
contract is rejected, and a stale lower claim is ratcheted up to the contract
the package earned. An unloadable hook-only manifest fails packaging; a loadable
one skips the full extension gate, because it declares less.

Other claims are proved the same way. Declared public assets that are missing
fail packaging. A staged runtime executable is validated before the archive is
assembled. The runtime manifest a locally prepared extension serves and the one
a packaged archive ships describe the same runtime.

## Invariants

- A staged package that does not load under its claimed contract is not shipped.
- A manifest cannot claim a CLI contract newer than the packager's.
- A manifest's contract claim is stamped to what it earned, never left stale.
- Declared public assets that are absent fail the package step.
- Local and packaged runtime manifests describe the same runtime.

## Rejected alternatives

- **Trust the build.** The consumer finds out, and is least able to diagnose it.
- **Type-check the tarball.** Type checking does not see the `exports` map, the
  staged files or the loader rules, where the failures are.
- **Verify after publishing.** A published version cannot be cleanly withdrawn.
- **Accept any declared contract number.** The CLI dispatches on it; an
  unvalidated claim is an unchecked compatibility promise.

## Consequences

- Packaging is deliberately slower than copying files.
- The packager learns a new CLI contract's loader rules before any package can
  claim it.
- A legitimate package shape the gate rejects gets an explicit carve-out, as the
  hook-only manifest did, never a bypass.
