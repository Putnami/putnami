# ADR 0001 — Rotate durable signing keys atomically with an overlap state

- **Status**: accepted
- **Scope**: `go.putnami.dev/keyringstore` (`go/framework/keyringstore`)

## Context

Replacing a signing key at once invalidates tokens issued just before.
Committing predecessor retirement and successor insertion separately can leave
no active key after a conflict or crash. The signer needs private JWK material
that verifier output, diagnostics, and metrics must never disclose.

## Decision

The store keeps the private keyring document in a deployment-protected
database column. Lifecycle state and retirement instant are authoritative
columns, so a restart keeps the overlap window. `Rotate` runs the predecessor
compare-and-set, successor insert, and retirement update in one
`database.WithTx` transaction; any non-applied outcome rolls back, including a
successor conflict after the predecessor update.

One key is active. Its predecessor becomes retiring and stays in the public
JWKS only for the overlap window. Revoked and expired keys are neither loaded
nor published. Public projections hold only public JWK parameters. Errors and
metrics use identifiers and fixed outcomes, never material.

The scheduler owns timing only: a caller policy decides whether and how to
rotate, runs never overlap, and stop joins the loop.

## Rejected alternatives

- **Delete the predecessor on install.** In-flight tokens fail at once.
- **Commit retirement before inserting the successor.** A conflict leaves no
  active key.
- **Store only public JWKs.** The signer cannot rebuild its key after restart.
- **Let the scheduler generate keys or pick policy.** Policy would be hidden
  from the application boundary.

## Consequences

- Deployments must encrypt the material column at rest.
- Only the `applied` outcome installs the successor; a nil error alone is not
  success. Callers retry only retryable outcomes.
- Removing a retiring key before its deadline is a revocation that can
  invalidate outstanding tokens.
