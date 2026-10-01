# Cross-language contract

Putnami runs the same migrations from both the TypeScript and Go runtimes, and
both can share a single state store. Two small helpers keep the two languages
byte-for-byte compatible: the **canonical migration id** and the **SHA-256
hash**. A divergence in either would corrupt shared state, so both are pinned by
a shared golden fixture.

## Canonical migration id

The state store keys each migration by a canonical id of the shape
`${datasource}:${name}`:

```ts
import { canonicalMigrationId, DEFAULT_DATASOURCE } from '@putnami/migration';

canonicalMigrationId('analytics', 'iam/001'); // 'analytics:iam/001'
canonicalMigrationId(undefined, 'iam/001');    // 'default:iam/001'
canonicalMigrationId('', 'iam/001');           // 'default:iam/001'
```

An empty or undefined datasource resolves to `DEFAULT_DATASOURCE` (`'default'`).
This mirrors the Go `protocolmigration.CanonicalID` function exactly, so both
runners address the same row.

## Migration hash

Migrations are content-hashed with SHA-256 so drift detection can compare the
stored hash against the current definition. The hex digest must match the Go
runner's `sha256Hex` byte-for-byte.

```ts
import { sha256Hex, sha256HexSync } from '@putnami/migration';

await sha256Hex('');      // 'e3b0c442...b7852b855' (SHA-256 of the empty string)
await sha256Hex('hello'); // '2cf24dba...938b9824'
```

### Async vs sync, and runtime portability

- `sha256Hex(content)` is the primary, fully portable helper. It uses Bun's
  `CryptoHasher` when available and falls back to the Web Crypto API
  (`crypto.subtle`) on Node 20+, browsers, and Deno.
- `sha256HexSync(content)` is for the rare call sites that cannot `await`. It
  uses Bun's `CryptoHasher` when available and otherwise falls back to Node's
  `node:crypto` `createHash`. There is no synchronous Web Crypto API, so a
  runtime providing neither Bun nor `node:crypto` (a bare browser) cannot use
  the sync helper — `await sha256Hex` there instead.

Both helpers produce identical output for the same input, and that output equals
the Go runner's.

## How parity is enforced

- `test/types.test.ts` pins the canonical hash vectors and asserts
  `sha256HexSync` agrees with `sha256Hex`, including the `node:crypto` fallback
  path.
- `test/cross-language.test.ts` builds an infra manifest from representative
  sources and asserts the serialized bytes equal a shared golden file
  (`protocols/infra/fixtures/equivalence/migration.golden.json`). The Go
  counterpart (`go/framework/migration/cross_language_test.go`) asserts against
  the same fixture, so a change to either implementation that breaks
  byte-equivalence fails both test suites.

When you touch the hash, the canonical id, or the infra manifest shape, update
both languages and the golden fixture in lockstep.

## Migration bundle digest

A migration bundle (`migration-bundle.v1`) is content-addressed by a SHA-256
**digest** computed over the migration-meaningful subset of the manifest
(`protocol`, `appName`, and the normalized, canonically-sorted `operations`).
Release and provenance fields (`version`, `git`, `imageDigest`, `generatedAt`,
`source`) are excluded, so the same migrations produce the same digest across
rebuilds and across languages — the basis for idempotent publish-by-digest.

`computeBundleDigest` (TS, `@putnami/migration`) and `ComputeBundleDigest` (Go,
`go.putnami.dev/protocol/migration`) must agree byte-for-byte. The digest input
is serialized exactly as Go's `encoding/json` would: compact, struct field
order preserved, `omitempty` honored (note that Go always emits the
`capabilities` object, as `{}` when empty), and `<`, `>`, `&`, U+2028, U+2029
escaped. The TS port reproduces this in `goMarshal`.

### How parity is enforced

- `test/cross-language.test.ts` reads the shared golden bundle
  (`protocols/migration/fixtures/equivalence/bundle.golden.json`) — whose
  operations are deliberately out of canonical order — and asserts
  `computeBundleDigest` reproduces the pinned digest. The Go counterpart
  (`protocols/migration/bundle_equivalence_test.go`) asserts the same
  fixture and constant, so a divergence in either implementation fails both.

When you touch the digest algorithm, the bundle operation shape, or the golden
fixture, update both languages and the pinned digest in lockstep.
