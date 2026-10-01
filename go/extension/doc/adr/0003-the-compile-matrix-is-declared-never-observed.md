# ADR 0003 — The compile matrix is declared, never observed

- **Status**: accepted
- **Scope**: `@putnami/go` (`go/extension`)

## Context

Compiling for the host makes a developer build and a CI build produce
different trees from one source. Compiling the full distribution matrix makes
every inner-loop build pay for cross compiles, and makes a single-platform
Docker channel stage binaries it never ships. The platform set is also a
cache-key input: it must come from a declared parameter, or a hit restores the
wrong tree.

## Decision

The compiled platform set resolves from declarations only, in this order:

1. an explicit `--target` or `platforms` request, a declared task parameter
   in the cache key;
2. when packaging, the channel being produced: a Docker channel compiles its
   target, an archive channel compiles the declared archive matrix;
3. the project's declared platform matrix;
4. otherwise the host target, the inner-loop default.

A malformed or undecodable platform request is an error, never a fallback. A
library honours an explicit platform request. Packaging refuses to stage an
archive whose prepared runtime binary is missing, so the failure happens in
the producing workspace, not in a consumer's.

## Invariants

- The platform set depends on `runtime.GOOS`/`GOARCH` only as the documented
  inner-loop default.
- Every input that changes the platform set is a declared task parameter.
- An extension archive stages exactly the declared matrix.

## Rejected alternatives

- **Always cross-compile the full matrix.** It turns every inner-loop build
  into a release build.
- **Infer the matrix from the CI environment.** A hidden host dependency; the
  local build cannot reproduce CI.
- **Let the Docker channel reuse binaries an earlier step produced.** The
  image ships a binary from a different build once that matrix changes.
- **Warn on a missing prepared runtime.** Nobody reads the warning; the
  installer finds the gap.

## Consequences

- Adding a platform is a declaration change with a visible diff.
- A local cross build passes `--target` explicitly.
- Any output that varies with the platform set must derive from a declared
  parameter.
