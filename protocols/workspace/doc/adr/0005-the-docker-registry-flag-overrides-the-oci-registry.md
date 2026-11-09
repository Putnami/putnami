# ADR 0005: The docker-registry flag overrides the OCI registry

- **Status**: accepted
- **Date**: 2026-11-09
- **Scope**: `go.putnami.dev/protocol/workspace` (`protocols/workspace`): the
  `registries.oci` entry of `putnami.workspace.json`; the Docker publisher in
  `go.putnami.dev/sdk/extension/dockerpublish`
- **Amends**: [ADR 0002](0002-lines-are-scopes-registries-by-ecosystem.md),
  the consequence on registry endpoints

## Context

ADR 0002 makes `registries.<ecosystem>` the place a workspace declares its
registry endpoints. The `publish` command of the Go and TypeScript extensions
also declares a `--docker-registry` flag, and the documentation describes it as
an override for one push.

The CLI delivers a command flag to a job in the job parameters, and a job
cannot tell a flag from a configuration option: `options.publish`,
`options.<extension>` and `options.<extension>:publish` set the same
parameter. Any reader of the flag therefore also reads those options.

## Decision

1. **The `docker-registry` parameter overrides `registries.oci.publish`.** The
   Docker publisher resolves the target from the `--docker-registry` flag or
   the `docker-registry` option, then `registries.oci.publish`, then
   `DOCKER_REGISTRY`, then the managed registry.
2. **Only the flag's own name is read.** The camelCase `dockerRegistry`
   parameter is not a source, and a flag given without a value is refused.

## Consequences

- `registries.oci.publish` stays the workspace's declared registry. The flag
  and the option change the target of the projects they reach, and nothing
  else.
- A project that sets `options.publish["docker-registry"]` publishes there,
  whatever the workspace declares.
