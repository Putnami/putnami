# ADR 0001 — Registry credentials come from a host-keyed cloud command

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/registry` (`protocols/registry`),
  `registry-token/v1`: the seam extension processes and every path without a
  credential provider use

## Context

Go, npm, OCI and Docker publishers and installers need a bearer for a registry
host. Those registries are operated by Putnami Cloud, which lives in another
repository and is optional: a core-only install has no cloud.

1. The framework must build, test and publish to public registries with no
   cloud present.
2. The mapping from host to credential is a cloud product decision (recipes,
   tenancy, sign-in state) on the cloud's release train.
3. Several publishers need the same answer, so the mechanism must be one
   contract.

The engine's own credentials come from the workspace's credential provider
when the process enables it
([ADR 0002](0002-one-credential-call-per-purpose.md)).

## Decision

A credential is resolved by running one fixed command with the host as data:

```
putnami cloud registry-token --host <host> [--materialize]
```

- The framework stores no host list, recipe model or credential file. It knows
  only the invocation tokens (`cloud`, `registry-token`, `host`,
  `materialize`), which this module pins.
- **Stdout carries a bare bearer and nothing else.** `ValidBearer` accepts a
  non-empty token with no whitespace. A value with whitespace is a status line
  on the wrong stream and counts as "no token", never as a malformed
  `Authorization` header.
- **`--materialize`** asks the cloud to write the host's native credential
  (`.npmrc`, `.netrc`, docker config) for an installer whose package manager
  takes no bearer on a pipe. It prints nothing; exit 2 means the cloud lacks
  the shape, exit 3 means the user is not signed in.
- **Absence is a supported answer.** No cloud, an unmanaged host and a
  signed-out user all yield "no token"; the caller uses the standard floor
  (explicit token, `.npmrc`, platform keychain). The cloud's stderr is relayed
  as a hint, so the cloud owns the remediation text.
- A job of a hosted run never starts this command: the child would be a CLI
  without the run credential that loads the workspace's extensions.
- `PublishProviderCommandName` (`publish-provider`) is a reserved spelling with
  no behaviour. Both repositories assert that nothing declares it.

## Rejected alternatives

- **A load-time capability marker (`publish-provider`).** It asks whether some
  cloud is installed, not whether this host has a credential.
- **A credential file or host list in the framework.** It makes the core
  cloud-aware and puts a secret-bearing file in a tree safe to copy.
- **A JSON envelope on stdout.** It invites policy fields the framework must
  interpret; every consumer of this seam needs one string.
- **A daemon or socket.** A publish is short; a bounded 30-second subprocess
  suffices.
- **Failing the publish when no cloud answers.** The cloud would become a hard
  dependency of publishes to public registries.

## Consequences

- The producer lives in another repository; the conformance test pins the
  tokens and the bearer rule, and end-to-end proof is cross-repository.
- Returning metadata (expiry, scope, identity) on stdout is a breaking change.
- The invocation tokens are frozen for the life of `registry-token/v1`:
  renaming one breaks every framework build against every deployed cloud.
- A cloud that prints a banner on stdout degrades to "no token", quietly by
  design; the relayed stderr is the only signal.
