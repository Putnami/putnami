# ADR 0005: The invocation names its credential purposes and its publication barrier

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/runner` invocation block

## Context

An executing engine must use the credentials the caller allowed, and nothing
else. A request that publishes must state what publication waits for, so the
executing engine can refuse a plan that would publish before its gates pass.

## Decision

### 1. `invocation.providers`

The caller allows credential purposes with `--providers`; the request carries
that choice as `invocation.providers`, the sorted unique subset of `install` and
`publish`. The executing engine consults the workspace credential provider
(`protocols/registry`, `credential-provider/v1`) for exactly those purposes:
`install` enables the `read` credential, `publish` the `publish` credential. It
never reads `PUTNAMI_PROVIDERS` for a bound request. Every CLI process reads
`PUTNAMI_PROVIDERS` once and removes it from its environment, so a nested CLI
enables purposes only through its own command line.

A strict provider refuses an unknown member, so the member is negotiated with
the capability `invocation-providers-v1`. A client offers it at `initialize`
only for a request that carries providers, and submits that request only to a
provider that echoed it. The CLI refuses `--where remote` with `--providers`
before submission when the provider did not echo it, and names the capability.

### 2. `invocation.publication`

`invocation.publication.barrier` lists the sorted unique invocation commands
whose every task is a transitive `dependsOn` predecessor of every publication
task; none of them is a publication command. The protocol classifies a
publication task by its command (`deploy`, `publish`). An executing engine that
knows each task's declared traits and effects classifies by those, so a registry
or cloud write under another command name is held to the same rule.

Each side enforces the direction its classifier sees. The protocol refuses a
publication task without the block, and accepts the block over a plan with no
publication command, because a registry write under `build` is invisible to it.
The executing engine also refuses the block over a plan with no task that has
registry or cloud effects. At execution, the block is present exactly when the
plan publishes.

### 3. Canonical form

Both members are optional and omitted when empty; an explicit empty list is
non-canonical. A request without them keeps its canonical bytes and its
execution-input digest.

## Consequences

The CLI submitter sets `providers` and never sets `publication`: remote
placement refuses every task with registry or cloud effects before a request is
built.
