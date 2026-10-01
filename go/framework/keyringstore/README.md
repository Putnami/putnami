# go.putnami.dev/keyringstore

The **durable, database-backed** implementation of the
`go.putnami.dev/security` `KeyringStore` interface, built on the
`go.putnami.dev/database` unit-of-work primitives.

It ships as its **own module** so that `go/framework/security` keeps **zero
database imports**. `security` defines the persistence seam — a store traffics
in the serializable `*keyring.PrivateKeyring` owner document, never Go crypto
types — and this module, which depends on **both** `security` and `database`,
provides the Postgres-backed adapter. The dependency direction is one-way
(`keyringstore → {security, database}`); nothing in `security` imports
`database`.

## Table shape

A keyring is a set of signing keys scoped by a logical `keyring_id` (a
tenant / keyring identity). Each key is one row:

```sql
CREATE TABLE signing_keys (
  kid        TEXT        PRIMARY KEY,             -- JWK key id (unique)
  keyring_id TEXT        NOT NULL,                -- keyring / tenant scope
  state      TEXT        NOT NULL,                -- keyring.KeyState (authoritative)
  alg        TEXT        NOT NULL,                -- JWS alg, e.g. ES256/RS256
  kty        TEXT        NOT NULL,                -- JWK key type: EC / RSA / oct
  material   TEXT        NOT NULL,                -- full keyring.PrivateJWK JSON (incl. private fields)
  retired_at TIMESTAMPTZ,                         -- when the key entered "retiring"; NULL otherwise
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX signing_keys_keyring_state ON signing_keys (keyring_id, state);
```

### How private material is stored

The `material` column carries the **full** `keyring.PrivateJWK` JSON, including
the private fields (`d`, `p`, `q`, `dp`, `dq`, `qi`, `k`). This is the **owner
document**: it holds private key material by design, because the sign side needs
it to reconstruct the Go crypto key on load. `TEXT` (or `JSONB`) both work;
`TEXT` stores the bytes verbatim.

> **Encryption-at-rest of the `material` column is a deployment concern and is
> deliberately out of scope for this module.** Use a KMS-wrapped column,
> transparent disk encryption, or a `pgcrypto`/column-encryption layer as your
> deployment requires. This module never logs, returns in an error, or otherwise
> emits key material.

The **`state` column is authoritative** for lifecycle state: `Load` and `Rotate`
read and transition it, and `Load` stamps it over the (possibly stale) `state`
embedded in the `material` JSON, so a rotation that moved a key
`active → retiring` is reflected even though the `material` document was written
earlier.

## API

- `New(pool, Config{KeyringID, Table})` — build a store over a `*database.Pool`.
- `Load(ctx)` — SELECT the **publishable** (`active`/`retiring`) rows for the
  configured keyring and assemble a `*keyring.PrivateKeyring`. Returns
  `security.ErrNoSigningKey` when the keyring is empty, so the fail-closed policy
  applies uniformly across every backend. Revoked/expired keys are excluded.
- `Save(ctx, kr)` — upsert every key of the owner document as a row, within one
  transaction (multi-key save is all-or-nothing). It does **not** overwrite
  `retired_at` on conflict (that column is derived state owned by `Rotate`).
- `Rotate(ctx, predecessorKid, successor)` — atomic rotation (see below).
- `RotationScheduler` — DB-agnostic scheduled trigger (see below).

## Atomic rotation

`Rotate` wraps `database.WithTx` around `database.Repository.Rotate`: a
`CompareAndSet` transitions the predecessor `active → retiring` and, **only when
that applies**, the successor row is inserted — both writes commit or roll back
together.

`Rotate` reports a typed `transaction.Outcome`:

| Outcome | Meaning | Keyring after |
| --- | --- | --- |
| `applied` | predecessor retired, successor installed, `retired_at` stamped | changed (committed) |
| `not-found` | no key with `predecessorKid` | **unchanged** |
| `already-consumed-conflict` | predecessor was not active, **or** successor kid already exists | **unchanged** |
| `retryable-serialization-failure` | serialization/deadlock abort | **unchanged** |

Every non-`applied` outcome returns a `nil` error — it is a *business* outcome,
not a failure — and **leaves the keyring unchanged**. This includes the subtle
case where the predecessor revoke applied but the successor insert then
conflicted (a duplicate successor kid): the store forces the transaction to roll
back so that a **half-rotation (retired predecessor, no successor) can never
commit**. A genuine I/O error rolls the unit back and is returned as an error.

The `active → retiring` transition is validated against the closed key-state
machine (`keyring.CanTransition`), and the successor must be an active key.

## Scheduled rotation

`RotationScheduler` periodically invokes a **caller-supplied policy** until it is
stopped or its context is cancelled. It carries **no database dependency** — it
is a timer around a `RotationFunc`, testable with a fake `Rotator`.

```go
sched, _ := keyringstore.NewRotationScheduler(24*time.Hour, func(ctx context.Context) error {
    // decide whether to rotate (e.g. inspect the active key's age), then:
    _, err := store.Rotate(ctx, activeKid, newKey)
    return err
})
stop := sched.Start(ctx)
defer stop() // idempotent; blocks until the loop goroutine has exited
```

Cancellation contract:

- It **never rotates on its own** — a rotation happens only when the policy
  decides to. Construction fails closed on a nil policy or a non-positive
  interval.
- The loop stops on **either** context cancellation **or** the returned `stop`.
- `stop` is **idempotent** and **blocks until the loop goroutine has fully
  exited**, so after `stop` returns the policy is guaranteed not to fire again.
- A tick that arrives while a policy invocation is in flight is dropped (no
  overlapping rotations).

## Tests

- **DB integration** (`store_db_test.go`) is gated behind `DATABASE_TEST_BINDINGS`
  exactly like `go/samples/unit-of-work-proof/uow_test.go`: with no binding it
  **skips**. It covers Save→Load round-trip, atomic rotation, rollback on a
  successor-insert conflict, non-applied outcomes, and the empty-keyring
  `ErrNoSigningKey`.
- **DB-free** (`serialize_test.go`, `scheduler_test.go`) run in the normal gate
  with no binding: keyring↔row (de)serialization round-trip, the `state`-column
  authority rule, the `transaction.Outcome` mapping, and the scheduler
  firing/stopping/cancellation contract against a fake `Rotator`.

## Support and contract

The SDD owner is `go`. `go.putnami.dev/keyringstore` is public, documented,
maintained, and classified **stable** in the repository
[support catalog](../../../putnami.support.json). Before v1.0, minor releases
may still require documented migrations; see the repository
[release policy](../../../RELEASE.md).

The durable owner and rotation contract is
[`go/signing-key-rotation`](specs/signing-key-rotation.json). The atomic overlap
decision is recorded in [ADR 0001](doc/adr/0001-atomic-overlap-key-rotation.md)
and protected by [`store_db_test.go`](store_db_test.go),
[`serialize_test.go`](serialize_test.go), and [`scheduler_test.go`](scheduler_test.go).
