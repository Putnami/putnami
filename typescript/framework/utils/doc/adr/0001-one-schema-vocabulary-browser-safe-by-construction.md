# ADR 0001 — Keep one schema vocabulary, browser-safe by construction

- **Status**: accepted
- **Scope**: `@putnami/utils` (`typescript/framework/utils`)

## Context

A workload describes one field shape in its configuration, its request payload,
its response body, and its OpenAPI document. Separate descriptions drift, and the
drift surfaces when a caller sends what the server claimed to accept. The same
helpers run in browser bundles, where one transitive `node:fs`, `node:os`, or
`node:path` import breaks the bundler or the user's tab. Per-request validation
is hot, which pulls toward generated code, while correctness pulls toward one
shared interpreter.

## Decision

- **One schema vocabulary, owned here.** `@putnami/runtime` re-exports it and
  defines no second one. A field is declared once (type, constraints, default,
  description, environment binding, sensitivity), and configuration, endpoint
  validation, and generated documentation consume that declaration.
- **Explicit semantics.** A `Default()` field is optional in input and always
  present in output. A field with neither `Default()` nor `Optional()` is
  required. Coercion is off unless requested, so JSON bodies are validated
  strictly and path or query segments can opt in.
- **Split by reachability.** Shared helpers import nothing from Node. The browser
  entry exposes the shared surface; the server entry adds the Node-backed
  utilities. A helper that needs a Node built-in lives on the server side.
- **The JSONL hook protocol is off the root barrel.** It is the build-time seam
  between a package's `bin/` entry and the CLI extension system, reachable only
  through `@putnami/utils/hooks`.
- **The compiled validator is an optimization, never a second contract.** It
  inlines only cases it reproduces exactly and delegates the rest (constrained
  types, regex patterns, unusual shapes) to the interpreter. A checked-in parity
  suite asserts both agree on accepted values, rejected values, defaults, and
  coercion.

## Rejected alternatives

- **An external schema library.** Its vocabulary, error shape, and releases
  become the framework contract, and it has no equivalent for `Sensitive()`,
  `Env()`, or `ProductionUnsafeDefault()`.
- **Separate configuration and request schemas.** Two validators, two doc
  generators, and a permanent translation layer.
- **Coerce everywhere.** A JSON `"5"` for a number is a client bug; accepting it
  moves the failure into the database.
- **One entry point plus tree shaking.** Shaking removes unused code, not
  unresolvable imports.
- **Generate a full validator per schema.** Any subtly wrong case is an invisible
  validation difference.

## Consequences

- A new schema capability reaches config, endpoints, OpenAPI, and contracts at
  once, so a careless addition is broad.
- A Node built-in import in a shared helper breaks every consumer's browser
  bundle.
- When unsure, the compiled validator delegates: slower is acceptable,
  disagreeing with the interpreter is not.
- A change here is a public change in `@putnami/utils` and `@putnami/runtime`.
