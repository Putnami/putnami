# ADR 0006: A host with no archive keeps the committed pin

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  toolchain-resolve answer; the install provisioning of `@putnami/cli`
- **Amends**: [ADR 0003](0003-extensions-answer-toolchain-pins.md), decision 2;
  [CLI ADR 0077](../../../../tooling/cli/doc/adr/0077-the-core-provisions-the-toolchains-extensions-pin.md),
  decision 4

## Context

ADR 0003 holds every pin of a toolchain-resolve answer to cover the host
platform. An extension that ships no archive for the host therefore answers a
problem or no pin. A problem stops the install, even on a machine that already
holds the toolchain. No pin is worse: an explicit install records exactly the
answered pins, so the identity leaves the lock for every platform, and the
next commit carries that removal.

`@putnami/php` ships no `windows/arm64` archive. A contributor on that
platform who runs `putnami install` removed the PHP pin the other five
platforms use. PHP is an optional toolchain: a machine without it still
installs the workspace.

## Decision

1. **A kept pin may leave the host out.** A pin that is the identity's locked
   entry, exactly as the request sent it, is a valid answer even when it
   records no integrity or archive for the host platform. Equality is the
   canonical JSON form of the entry. Any other pin still covers the host. A
   kept pin still follows the lock's version 5 entry rules, so a committed
   entry below version 5 cannot be kept this way.
2. **The core installs nothing from it on that host.** Install provisioning
   skips a pin with no host archive when the answering extension's runtime
   toolchain that locks the identity is optional. When that toolchain is
   required, or the answering extension runs no toolchain on the identity,
   the install fails and names the host platform, as before.

## Consequences

- The lock is no longer rewritten by the platform that runs the install: a
  host an extension cannot serve leaves the committed pin to the hosts it
  serves.
- A job on such a host resolves the optional toolchain from its other
  candidates, such as PATH, or finds it unavailable, as with no pin.
- An extension decides when to keep a pin. `@putnami/php` keeps the committed
  pin of the release it would choose; a release it would not choose is still
  answered with no pin.
