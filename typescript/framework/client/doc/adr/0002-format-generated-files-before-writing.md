# ADR 0002 — Format generated files before writing

- **Status**: accepted
- **Scope**: `@putnami/client` generation

## Context

The TypeScript renderer is deterministic, but `putnami lint` can rewrite its
output, so comparing a committed client with raw renderer output reports drift
when the contract has not changed.

## Decision

`generateProjectClients()` applies Biome's format and lint writer phases
before replacing client files. The formatter receives the final output path and
the provider's project or workspace configuration, so path overrides,
configuration inheritance and EditorConfig apply.

- Biome resolves from the provider's installed packages first, then the
  client's declared dependency. Installed package paths are inspected before
  resolving the launcher, so a provider without dependencies cannot trigger
  runtime auto-installation.
- `generateTypeScriptClient()` stays a pure renderer for custom pipelines.
- Every file is formatted before any is written; a formatter failure leaves
  existing client files intact.
- Error messages omit source and formatter diagnostics, which may contain
  provider data.
- The lint gate enforces remaining rule diagnostics, because Biome's stdin
  writer does not report them.

## Consequences

- Reproducibility depends on the formatter version and provider configuration
  as well as the contract. A configuration change can require regenerating
  committed clients.
- Generation needs installed dependencies and a Biome configuration.
- The `clientgen-ts` task keys its cache on `biome.json`, `.editorconfig` and
  `package.json` (project and workspace), so a formatting change moves the key.
