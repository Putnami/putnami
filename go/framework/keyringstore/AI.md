# go.putnami.dev/keyringstore

Durable Postgres-backed implementation of the `go.putnami.dev/security`
`KeyringStore` seam. This module owns persistence and rotation transactions;
the security module remains independent of database drivers.

## Contract invariants

- The stored private keyring is the durable owner document, while load exposes
  only active and retiring keys and treats the row state as authoritative.
- Rotation retires the predecessor and inserts the successor in one transaction;
  conflict, not-found, and retryable outcomes leave the owner unchanged.
- The retiring overlap preserves verification during rollout, and schedulers
  stop synchronously without overlapping policy calls.
- Private JWK material is never included in errors or logs. Encryption at rest
  remains an explicit deployment responsibility.

Support is **stable** in `../../../putnami.support.json`; the normative feature
contract is `specs/signing-key-rotation.json`, with the decision in
`doc/adr/0001-atomic-overlap-key-rotation.md`. See `README.md` for schema and
operational details.
