# Identity Contract

The Putnami frameworks' **consumer-side identity vocabulary**: the well-known
token claims they extract from a verified credential, the kinds of principal
they recognize, and the authorization decision labels they report. The module
authors that vocabulary once as a contract manifest and commits the artifacts
the contract compiler generates from it.

## Why

The Putnami frameworks are token *consumers*: they validate bearer tokens
(JWT / OIDC, RFC 7662 introspection) and static API keys, then extract a small
set of well-known claims into a typed principal. That vocabulary — which claim
keys are read, which kinds of principal exist, and which decision labels the
authorization path reports — was previously implicit in per-language auth code,
duplicated as bare string literals in Go and TypeScript. The same strings double
as metric counter keys and the `decision` log attribute, so a silent rename in
one runtime also broke shared dashboards.

This module states the vocabulary once, as a canonical contract IR
(`go.putnami.dev/protocol/contracts`), so both frameworks and the tooling agree
on it and the contract compiler generates the language twins.

Issuer-side vocabulary (application scopes, grants) lives with the issuer, not
here: this contract deliberately declares no scopes and no grants because the
framework hard-codes none. The reasoning, and the alternatives that lost, is in
[`doc/adr/0001-consumer-side-vocabulary-compiled-from-one-manifest.md`](doc/adr/0001-consumer-side-vocabulary-compiled-from-one-manifest.md).

## What

- `schema/contracts.json` — the authored manifest (also the canonical IR; the
  compiler re-canonicalizes it in place). It declares:
  - **claims** — the well-known keys the frameworks consume: `sub`, `iss`,
    `client_id`, `roles`, `scope`, plus the validation-time claims `aud` and
    `exp`.
  - **principalKinds** — `user` (bearer/OIDC) and `apikey` (static API key).
  - **enums** — `ClaimName` and `PrincipalKind` (the constant twins of the two
    vocabularies above) and `AuthDecision`, the outcome label of an
    authorization check (`allow`, `deny_unauthenticated`, `deny_client`,
    `deny_scope`, `deny_role`, `deny_guard`), whose values double as metric
    counter keys and the `decision` log attribute.
  - **structs** — `Claims` (`sub`, `iss`, `client_id`, `roles`, `scope`), the
    typed shape the Go framework extracts from a verified token payload with
    `go.putnami.dev/http`'s `ClaimsFromMap`.
  - **discovery** — the human title, summary, and tags carried with the contract.
- `schema/contracts.gen.go` — the generated Go type twin, package
  `go.putnami.dev/protocol/identity/schema`. **Do not edit.**
- `schema/contracts.gen.ts` — the generated TypeScript twin. **Do not edit.**
- `schema/contracts.schema.json` — the generated JSON Schema over the DTO
  vocabulary. **Do not edit.**
- `schema/contracts.md` — generated reference tables checked against the
  canonical manifest. **Do not edit.**

The root package (`doc.go`) carries no types; everything consumable lives in the
generated `schema` package.

## How

Regenerate the committed artifacts after editing the manifest:

```sh
putnami contracts generate --project /protocols/identity
```

Verify the committed artifacts are fresh and the change is non-breaking
(drift or a breaking vocabulary change exits 2):

```sh
putnami contracts check --project /protocols/identity
```

`schema/contracts_test.go` guards the same invariants in the regular test
gate: the manifest strict-parses and validates cleanly, its identity, claim set,
and principal-kind set are pinned, and each committed artifact is byte-identical
to a fresh generation.

## Producers and consumers

| Artifact | Produced by | Consumed by |
| --- | --- | --- |
| `schema/contracts.json` | authored by hand (the only hand-edited file here) | the contract compiler; `putnami contracts generate` / `check` |
| `schema/contracts.gen.go` | `putnami contracts generate` | `go/framework/security` (`jwt.go`, `apikey.go`, `introspect.go`, `observe.go`) and `go/framework/http` (`claims.go`), which use the generated constants instead of claim-key literals |
| `schema/contracts.gen.ts` | the same generation, driven from `@putnami/application`'s contract-twin plugin | pinned by `typescript/framework/application/test/contracts/identity-twin.test.ts`; the framework itself reads the mirrored constants in `src/security/identity.constants.ts` |
| `schema/contracts.schema.json`, `schema/contracts.md` | `putnami contracts generate` | schema consumers and human reference |

TypeScript cannot import the generated twin across the Go project boundary
(`tsc --rootDir`, TS6059), so `@putnami/application` mirrors the constants and a
byte-identity test fails on any drift. That mirror is a build-system
consequence, not a second source of truth.

## Versioning and compatibility

The manifest declares `protocolVersion`, and the guard test pins it to
`contracts.ProtocolVersion`: the wire version belongs to the contracts protocol,
not to this vocabulary. Compatibility of the vocabulary itself is enforced by
`putnami contracts check`, which exits 2 on drift *and* on a breaking vocabulary
change. Adding a claim, principal kind, or enum value is additive; removing or
renaming one is breaking and must be a deliberate, reviewed act in the manifest
and in both runtimes at once.

Unlike its sibling protocol modules this module ships no `fixtures/` corpus and
no strict parser of its own: the wire shapes it describes are validated by
`go.putnami.dev/protocol/contracts`, and the committed generated artifacts are
the conformance surface — drift is detected by regenerating and comparing bytes.

## Durable decisions

- [`doc/adr/0001-consumer-side-vocabulary-compiled-from-one-manifest.md`](doc/adr/0001-consumer-side-vocabulary-compiled-from-one-manifest.md)
  — why the vocabulary is authored once as a contract manifest, why every other
  artifact is generated and committed, and why no issuer-side vocabulary lives
  here.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is an internal vocabulary contract consumed by the two frameworks' auth
code; what a developer experiences is authentication and authorization in the
framework packages, not this manifest. Per the spec contract in
[`protocols/features`](../features/README.md) a spec details an already-authored
feature and never mints one, so the durable design intent lives in
[`doc/adr/`](doc/adr) instead. A product feature that later owns authentication
end to end links to that record rather than restating it.

## Support

- **Status:** `preview`, recorded as
  `{"id": "go.putnami.dev/protocol/identity", "kind": "protocol", "status": "preview"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** the Go framework imports the generated `identity/schema` package
  in shipped auth code, and the TypeScript framework mirrors the same vocabulary
  under a byte-identity test against the committed twin; freshness and
  non-breaking evolution are gated by `putnami contracts check` and by
  `schema/contracts_test.go`.
- **Why not `stable`:** the vocabulary is deliberately thin and still growing
  (`AuthDecision` was added after the first cut), TypeScript consumes a mirror
  rather than the generated twin, and the module has no independent fixture
  corpus of its own. Those are the gaps a stable commitment would have to close.
