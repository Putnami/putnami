# ADR 0001 — The identity vocabulary is consumer-side and compiled from one manifest

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/identity` (`protocols/identity`)

## Context

The Putnami frameworks consume tokens: they validate bearer tokens (JWT/OIDC,
RFC 7662 introspection) and static API keys, then extract well-known claims
into a typed principal. Claim keys, principal kinds, and authorization decision
labels (`"sub"`, `"client_id"`, `"apikey"`, `"deny_scope"`) also serve as metric
and log keys. Written as literals in each runtime, they drift, and drift breaks
dashboards. A hand-written constants file per language only moves the
duplication.

## Decision

1. **`schema/contracts.json` is the single source of truth.** It is an IR of
   [`protocols/contracts`](../../../contracts/README.md), not hand-written Go.
2. **Every other artifact is generated and committed:**
   `schema/contracts.gen.go`, `schema/contracts.gen.ts`,
   `schema/contracts.schema.json`, and `schema/contracts.md`. Regenerate them
   with `putnami contracts generate`; never edit them by hand.
3. **The vocabulary is consumer-side only:** claims read, principal kinds
   produced, decision labels reported. No scopes and no grants; issuer-side
   vocabulary belongs with the issuer.
4. **Freshness and compatibility are gated.** `putnami contracts check` fails
   on drift or a breaking change. `schema/contracts_test.go` pins the manifest
   identity, the claim and principal-kind sets, and byte-identity of every
   committed artifact.
5. **The TypeScript mirror is pinned, not authoritative.** TypeScript cannot
   import across the Go project boundary (`tsc --rootDir`, TS6059), so
   `@putnami/application` mirrors the constants in
   `src/security/identity.constants.ts`, pinned to this manifest by a
   byte-identity test.

## Rejected alternatives

- **Hand-written constants per language.** Two authorities, nothing fails when
  they disagree.
- **Literals plus a documented list.** Prose has no gate.
- **Issuer vocabulary here.** The frameworks hard-code no scope, so the module
  would publish a vocabulary it does not implement.
- **Hand-written Go source of truth.** The contract has no parsing behaviour;
  the IR yields schema, docs, and both twins from one edit.
- **JSON Schema only.** Leaves the constant names unowned.

## Consequences

- A change is one manifest edit plus regeneration, reviewed together.
- Adding a claim is additive; removing or renaming one is a breaking change the
  checker refuses.
- Scope catalogs, grant types, and consent need a different home.
