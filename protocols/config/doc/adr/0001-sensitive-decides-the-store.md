# ADR 0001 — `sensitive` decides the store, and the schema decides what may be written

- **Status**: accepted
- **Scope**: `go.putnami.dev/protocol/config` (`protocols/config`)

## Context

A database host and its password are authored, read, and consumed as one typed
tree, but only the host may sit in plaintext. Something must decide which store
each value belongs to.

Both the Go and TypeScript extractors already derive the schema from code at
build time, so a field-level attribute costs nothing and is reviewed with the
code that reads it. The consumer must not care where a value came from:
`cfg.Database.Password` works the same from either store.

## Decision

1. **`FieldSchema.Sensitive` is the single routing authority.** A sensitive
   field belongs in the envelope-encrypted secrets store; every other field in
   the plaintext config store. Both resolve into one typed tree at runtime.
2. **Writes are checked against the registered schema** (`policy.go`):
   - a sensitive field, anywhere in nested values, written through the config
     store is an error;
   - an unknown field, a wrong value type, or an invalid map key in a config
     write is an error;
   - a non-sensitive field (or a block with no sensitive field) written through
     the secrets store is a warning;
   - a path absent from the registered schema is an error, with no
     grandfathering.
3. **The helpers are diagnostic-only.** They return `diag.Diagnostic` slices
   and reject nothing; the caller decides what is fatal, so a control plane can
   observe before it enforces.
4. **The protocol carries no secret material.** `SealedEnvelope` is
   `{keyUri, wrappedDek, nonce, ciphertext}`; this package performs no
   cryptography.
5. **Sensitivity is identical in every environment.**

## Rejected alternatives

- **A separate secrets API** (`secrets.Get("db.password")`). Splits one config
  block across two access paths.
- **Inline references** (`"${secret:db-password}"`). Every reader needs an
  interpolator, and a typo yields a plausible literal.
- **The writer decides at write time.** Least reviewable moment, and the same
  field can be routed two ways.
- **Errors instead of diagnostics.** A strict rejection in a shared library
  takes effect on upgrade with no staged rollout.
- **Environment-scoped sensitivity.** The wrong answer is a leak.
- **Enforce `constraints` (min/max/pattern/enum).** Speculative: it would
  invent a validation vocabulary both languages must match forever.

## Consequences

- The policy helpers have no caller in this repository, so a sensitive value
  written through the config store is caught by nothing here. This gap is one
  reason the module is `preview`.
- Marking an existing field `sensitive` is a storage migration this protocol
  does not model.
- The two extractors must agree on how sensitivity is declared; only the shared
  fixtures enforce it.
- `constraints` is declared and advisory; no reader honours it.
