# `go.putnami.dev/protocol/keyring`

The Putnami **canonical keyring vocabulary**: the single cross-language contract
for a signing keyring's **key-state lifecycle**, its **private-keyring document**
and **public JWKS projection**, and the **PHC-style versioned credential-digest
grammar**.

The point of this package is that `keyring` owns the shared vocabulary. Without
it each consumer hand-rolls all three concerns — unversioned PBKDF2 strings, a
single-key JWK, and an implicit key lifecycle. This package makes each one an
explicit, reviewable, testable contract so Go, TypeScript, and (later) Python
consumers agree on the *same* shapes, the *same* closed key-state machine, and
the *same* digest string format.

This package is **vocabulary + declaration + strict parse/validate only**. It
deliberately performs **no cryptography**: it does not sign, verify, hash, or
derive keys. It defines the shapes, the closed enums, the legal key-state
transition table, the private→public projection rule, and the digest grammar's
parser/encoder. The signing, verifying, and hashing implementations that consume
this vocabulary live in the frameworks (see
[Producers and consumers](#producers-and-consumers)).

## Three vocabularies

### 1. Key-state lifecycle — `KeyState`, `Transition`

A closed key-state machine with an explicit **legal-transition table**:

| State | Meaning | Publishable? | Terminal? |
| --- | --- | --- | --- |
| `active` | Usable for signing new material **and** verifying. | yes | no |
| `retiring` | No longer signs new material; still trusted for verification during rollover grace. | yes | no |
| `revoked` | Withdrawn (typically compromised). Never trusted, never published. | **no** | **yes** |
| `expired` | Validity window elapsed naturally (not compromised). Never published. | **no** | no |

Legal transitions (the table enforced by `CanTransition` / `Transition`):

```
active   → retiring | expired | revoked
retiring →            expired | revoked
expired  →                      revoked
revoked  → ∅   (terminal)
```

Design rules the table encodes (recorded in
[`doc/adr/0001-closed-key-state-machine-and-fail-closed-publication.md`](doc/adr/0001-closed-key-state-machine-and-fail-closed-publication.md)):

- **Revocation is always reachable** from any non-revoked state — a compromise
  can be discovered at any time, including for an already-expired key
  (retroactive disclosure).
- **`revoked` is the only terminal state.** A key is never un-revoked,
  re-activated, or un-expired; there is no backward edge.
- **A transition must change state.** A same-state edge (e.g. `active → active`)
  is rejected.

`Transition(from, to)` returns diagnostics (empty when legal); `CanTransition`
is the boolean form; `KeyState.String()` gives the `KeyState(s.String()) == s`
round-trip; `KeyState.AllowedTransitions()` returns the legal targets **sorted**
for deterministic output.

### 2. Private keyring + public JWKS projection — `PrivateKeyring`, `PublicJWKS`

A `PrivateKeyring` is the owner-side document: a versioned set of signing keys,
each of which may carry private material (`d, p, q, dp, dq, qi, k`) and a
lifecycle `state`. A verifier never sees it — it receives only the **public JWKS
projection** produced by `PublicJWKS`.

`PublicJWKS` applies two rules, per key, then projects with `PrivateJWK.Public`:

1. **Publishability filter** — only `active` and `retiring` keys are published;
   `revoked` and `expired` keys are dropped so a verifier never sees a distrusted
   key. *(This is the fail-closed rule: a projection that advertised a revoked key
   would let a verifier accept a signature from a compromised key.)*
2. **Asymmetry filter** — symmetric `oct` keys are dropped; a symmetric key has
   no public form, and publishing its material would leak the secret.

**Structural invariant (enforced two-sided + tested):** the private-material
fields `d, p, q, dp, dq, qi, k` **NEVER** appear in the public projection.

- *Type side:* the public `JWK` type has **no** private-material fields, so
  `PublicJWKS` structurally cannot emit them.
- *Parse side:* `ParseJWKS` strict-parses with `DisallowUnknownFields`, so a JWKS
  document carrying any private field is **rejected** as an unknown field.

The `JWK` / `JWKS` shapes match `go/framework/security` (`JWK`, `JWKS`,
`ParseJWKS`, `jwt.go`) field-for-field on standard JOSE tags (`kty`, `crv`, `x`,
`y`, `n`, `e`, `kid`, `alg`, `use`), so the verify side can adopt this package as
its canonical vocabulary. A projected JWKS is a plain `{"keys":[...]}` document
`security.ParseJWKS` consumes directly.

### 3. PHC-style credential-digest grammar — `Digest`, `ParseDigest`

A versioned, self-describing digest string for stored credentials (passwords,
device codes, keyed MACs); the grammar's rationale, including the two base64
alphabets, is in
[`doc/adr/0002-phc-style-versioned-digest-grammar.md`](doc/adr/0002-phc-style-versioned-digest-grammar.md).
Two algorithms:

```
$pbkdf2-sha256$v=1$i=<iterations>$<b64-salt>$<b64-hash>
$hmac-sha256$v=1$<b64-salt>$<b64-hash>
```

EBNF:

```ebnf
digest      = pbkdf2 | hmac ;
pbkdf2      = "$" "pbkdf2-sha256" "$" version "$" "i=" iterations "$" b64 "$" b64 ;
hmac        = "$" "hmac-sha256"   "$" version "$"                    b64 "$" b64 ;
version     = "v=" "1" ;                       (* pins DigestVersion *)
iterations  = digit , { digit } ;              (* canonical unsigned decimal, 1..10000000 *)
b64         = b64char , { b64char } ;          (* RFC 4648 §4 base64, NO padding *)
b64char     = "A".."Z" | "a".."z" | "0".."9" | "+" | "/" ;
digit       = "0".."9" ;
```

`ParseDigest` is strict — it rejects, each with a specific diagnostic code:

| Failure | Code |
| --- | --- |
| Missing leading `$`, wrong segment count | `keyring.digest_structure` |
| Unknown algorithm | `keyring.digest_algorithm` |
| Unsupported `v=` | `keyring.digest_version` |
| Malformed/out-of-range `i=`, empty salt/hash, non-`v=`/`i=` param | `keyring.digest_params` |
| Salt/hash not canonical unpadded standard base64 | `keyring.digest_encoding` |

**Note the base64 alphabet.** The digest salt/hash use RFC 4648 **§4 standard
base64 without padding** (`+`/`/`), per the PHC string format — *not* base64url.
The JWK parameters, by contrast, use base64**url** (`-`/`_`), per JOSE. The two
alphabets are deliberate and the fixtures exercise both. `Digest.String()` is the
canonical encoder, giving the round-trip `ParseDigest(s).String() == s`.

## Fixture corpus ([`fixtures/`](fixtures))

Language-neutral JSON, loaded generically by this package's conformance test and
by the Go and TypeScript consumers listed below:

| File | What |
| --- | --- |
| `digests.json` | `valid` + `invalid` digest strings. Each invalid case carries `reason` (why) and `expectCode` (the pinned diagnostic code). |
| `key-states.json` | The closed `states`, the `terminal` set, and `transitions` cases each tagged `legal: true|false` with a `reason`. |
| `keyrings.json` | Projection `cases`: a `private` keyring and its expected `public` JWKS. Projecting `private` must equal `public` exactly and leak no private field. |
| `jwt-vectors.json` | Real, self-verified ES256 **and** RS256 sign/verify vectors (`valid`) plus structurally-broken ones (`invalid`). This module checks structure; the framework security packages verify the signatures against the same vectors. |
| `secret-vectors.json` | Cross-language secret-digest vectors: a known secret, its exact salt and iteration count, and the resulting PHC digest string. The Go and TypeScript hashing implementations must reproduce and verify the same bytes. |
| `equivalence/keyring.golden.json` | The byte-exact canonical serialization of the sample public projection — the cross-language byte-parity contract. |

Fixture format conventions: a `valid`/`invalid` split (or `legal` boolean); every
negative case tagged with a human `reason`; digest/JWKS negatives additionally
pin the exact diagnostic (`expectCode`).

## Tests

- `conformance_test.go` drives the whole corpus: digest parse+round-trip, the
  transition table, the private→public projection (equality **and**
  private-field exclusion), and the structural JWT-vector checks.
- `determinism_test.go` pins byte-stable canonical serialization (100× stable +
  the committed golden), digest round-trip stability, and the sorted, stable
  `AllowedTransitions` output.
- `drift_test.go` keeps the JSON schemas in [`schemas/`](schemas)
  (`keyring.json`, `jwks.json`, `digest.json`) in lockstep with the Go types:
  field parity, required fields, the pinned `protocolVersion`, closed-enum
  parity, and — critically — that the public JWK schema declares **none** of the
  private-material fields.

## Non-goals

- **No cryptography.** No signing, verifying, hashing, or key derivation. Those
  live in the consumers; this is the vocabulary they share.
- **No key-generation or rotation policy.** The transition table says which moves
  are *legal*, not *when* to make them. Iteration-count bounds are grammar bounds
  (a well-formed string), not a per-deployment minimum-work policy.
- **No storage or transport.** The keyring document and digest string are shapes;
  where they live is a consumer's concern.

## Producers and consumers

| Shape | Produced by | Consumed by |
| --- | --- | --- |
| `PrivateKeyring` / `PrivateJWK` | a keyring owner: `go.putnami.dev/keyringstore` persists one row per key with the state as the authoritative column and the full private JWK as an opaque blob | `go.putnami.dev/security`, which loads the document, signs under the single active key, and rotates copy-on-write |
| `JWKS` (public projection) | `PublicJWKS`, driven by the framework keyring | verifiers: `go.putnami.dev/security`'s JWKS parsing, and any external client fetching the published JWKS |
| `Digest` | `go.putnami.dev/security`'s secret hashing and its TypeScript twin in `@putnami/application` | the same two verify paths, plus anything reading a stored credential column |
| `KeyState` + transition table | rotation code in `go.putnami.dev/keyringstore` (the rotation scheduler) | the same, plus `PublicJWKS`'s publishability filter |

`go.putnami.dev/security`'s `rotation_proof_test.go` exercises rotation end to
end against the contract. The TypeScript side has no keyring package:
`@putnami/application` re-implements the state machine and the PHC grammar and
stays pinned to this module by the shared vectors above.

## Versioning and compatibility

Two independent version knobs, deliberately separate so one can move without
churning the other:

- `ProtocolVersion` — stamped into a `PrivateKeyring` document and pinned by the
  JSON schemas. Bumped on any backwards-incompatible change to the document
  shapes, the enums, or the transition table; adding an optional field inside an
  existing shape does not bump it.
- `DigestVersion` — the `v=` segment of the digest grammar. A stored digest
  therefore states its own version, and a grammar change never invalidates a
  keyring document.

Strict parsing is part of the compatibility promise in both directions: unknown
fields are rejected, which is exactly what keeps private key material out of a
public JWKS, and the closed enums mean an unknown state or algorithm is a
diagnostic rather than a silently trusted value.

## Durable decisions

- [`doc/adr/0001-closed-key-state-machine-and-fail-closed-publication.md`](doc/adr/0001-closed-key-state-machine-and-fail-closed-publication.md)
  — the four states, the legal-transition table, and why publication is derived
  and fail-closed.
- [`doc/adr/0002-phc-style-versioned-digest-grammar.md`](doc/adr/0002-phc-style-versioned-digest-grammar.md)
  — why stored credentials use a self-describing PHC-style string, and why two
  base64 alphabets coexist in one module.

## Product feature linkage

No user-facing product feature owns this module, and none should be minted for
it. It is a security vocabulary consumed by framework code; what a developer
experiences is token signing, verification, rotation, and credential hashing in
`go.putnami.dev/security` and `@putnami/application`. Per the spec contract in
[`protocols/features`](../features/README.md) a spec details an already-authored
feature and never mints one, so the durable design intent lives in the ADRs
above. A product feature that later owns authentication links to them rather
than restating them.

## Support

- **Status:** `preview`, recorded as
  `{"id": "go.putnami.dev/protocol/keyring", "kind": "protocol", "status": "preview"}`
  in the workspace-root [`putnami.support.json`](../../putnami.support.json)
  catalog (contract: [`protocols/support`](../support/README.md)).
- **Owner:** the Putnami maintainers, as repository owners of `protocols/`.
  Support entries carry no owner field, so the single owner is stated here.
- **Evidence:** `go.putnami.dev/security` and `go.putnami.dev/keyringstore`
  import this vocabulary in shipped code (signing, JWKS publication, secret
  digests, and the rotation scheduler's transition checks),
  `go.putnami.dev/security`'s `rotation_proof_test.go` proves rotation end to
  end, and the digest and JWT bytes are pinned across languages by
  `fixtures/secret-vectors.json` and `fixtures/jwt-vectors.json`, which the Go
  and TypeScript security tests both execute.
- **Why not `stable`:** there is no TypeScript keyring package, so the
  TypeScript twin re-implements the state machine and the grammar and keeps them
  in sync by hand against the shared fixtures. Until that mirror becomes an
  import, a vocabulary change must be applied twice, which is not a compatibility
  promise this module can make on its own.
