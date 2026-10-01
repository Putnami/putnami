# ADR 0001 — Ecosystem profiles are declared by extensions

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/extension` (`protocols/extension`), the
  `ecosystems` and `uses` members of `putnami.extension.json`

## Context

Adding a language must not require a change to the release-set protocol, the
CLI, or the distribution backend. Extensions already own the publish jobs of
their ecosystems and the metadata they emit for a release set, so they are the
right owner of everything an ecosystem is, including the shape of its registry
configuration.

## Decision

1. **An extension declares ecosystem profiles** under `ecosystems`. A profile
   carries: `id`, matching `^[a-z][a-z0-9-]{0,31}$`; `coordinate` and
   `version` rules (RE2 pattern, and `semver` or `string` ordering) enforced
   at plan time; `channel`, `native` or `none`; an optional `channelEncoding`
   that may only narrow the portable channel alphabet; `registries`, the JSON
   schema (an object schema) of the ecosystem's entry in the workspace
   `registries` section; and `publish`, a command of the same manifest.
2. **A profile has exactly one owner.** Another extension that publishes to
   the ecosystem lists it under `uses` and never redeclares it, even
   byte-identically. Resolution refuses a profile declared twice and a `uses`
   entry no installed extension declares.
3. **Owners**: `npm` is `@putnami/typescript`, `go` is `@putnami/go`, `oci` is
   the shared OCI publisher of the extension SDK (a built-in profile), and
   `archive` and `put` are `@putnami/cloud`. `@putnami/typescript` and
   `@putnami/go` declare `uses: ["oci"]`.
4. **The CLI builds its ecosystem list from the installed extensions** and the
   built-in profiles. No ecosystem name appears in the CLI. A publish job
   emits one `published-member` event per artifact it produced (coordinate,
   version, artifact digest); a project may produce several members in one
   ecosystem.
5. **An ecosystem without a native channel** is followed by `--release` or
   `--version` only; `upgrade --channel` skips it and says so.

See the [distribution protocol](../../../distribution/README.md#the-protocol-keeps-only-the-generic-part-of-an-ecosystem)
for how the release set consumes profiles.

## Consequences

- Adding Python is a manifest, a publish job, and a registry on the backend,
  with no change in `protocols/distribution`, `protocols/workspace`, or the
  CLI.
- The release-set provider and the registries validate coordinates and
  versions with the same profile rules.
