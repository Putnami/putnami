// Package uowproof is an executable proof for the Putnami Go unit-of-work and
// consume-once primitives. It exercises, against a real Postgres,
// the two safety properties those primitives exist to guarantee:
//
//   - Consume-once. A device code is redeemable exactly once. Even under racing
//     redemptions, at most one caller observes Outcome "applied"; every other one
//     observes "already-consumed-conflict", and an unknown code is "not-found".
//     Backed by Repository.ConsumeOnce — a single conditional UPDATE whose
//     still-unclaimed predicate plus Postgres row locking serialize concurrent
//     claims, so no transaction is required for the once-only guarantee.
//
//   - Atomic rotation. Retiring a predecessor signing key and installing its
//     successor — together with the successor's tenant binding — is ONE
//     all-or-nothing unit of work spanning two repositories. Either the whole
//     rotation commits (predecessor revoked, successor installed, binding
//     written) or it rolls back leaving the predecessor active and no
//     successor/binding, so there is never a transient window where a successor
//     key exists unbound, nor a binding pointing at a key that was never
//     installed. Backed by Repository.Rotate inside database.WithTx.
//
// The proof lives in the test (uow_test.go), gated behind DATABASE_TEST_BINDINGS
// exactly like the framework's cross-language conformance corpus: with no
// Postgres binding it SKIPS, so the local build gate stays green; CI injects the
// binding and runs the assertions for real. This file holds the reusable
// service code the test drives (and that a reader can copy), so the proof is
// real, compiled code rather than a doc snippet.
package uowproof

import (
	"context"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/database"
	"go.putnami.dev/protocol/transaction"
)

// scanNone is the scan function for the tables this proof only writes to and
// asserts on with COUNT(*); the repositories never materialize a row into a
// struct, so an empty scan is sufficient.
func scanNone(pgx.Row) (struct{}, error) { return struct{}{}, nil }

// DeviceCodeService issues single-use device codes. A code registered in the
// device_codes table may be redeemed exactly once.
type DeviceCodeService struct {
	codes *database.Repository[struct{}]
}

// NewDeviceCodeService binds the service to a pool.
func NewDeviceCodeService(pool *database.Pool) *DeviceCodeService {
	return &DeviceCodeService{codes: database.NewRepository[struct{}](pool, "device_codes", scanNone)}
}

// Redeem claims the device code for userID exactly once, reporting the typed
// Outcome:
//
//   - applied on the first redemption (the code is marked consumed by userID),
//   - already-consumed-conflict on every later redemption of the same code,
//   - not-found when the code was never issued.
//
// The claim runs as a single ConsumeOnce UPDATE guarded by "consumed = false",
// so two racing redemptions serialize on the row and only the first matches the
// still-unclaimed guard — the once-only guarantee holds even without an enclosing
// transaction.
func (s *DeviceCodeService) Redeem(ctx context.Context, code, userID string) (transaction.Outcome, error) {
	return s.codes.ConsumeOnce(ctx, "code", code,
		"consumed = false",
		"consumed = true, consumed_by = $1", userID)
}

// KeyRotationService atomically rotates a tenant's signing key: it retires the
// active predecessor and installs a successor, then binds the successor to the
// tenant, as one unit of work spanning two repositories.
type KeyRotationService struct {
	pool     *database.Pool
	keys     *database.Repository[struct{}]
	bindings *database.Repository[struct{}]
}

// NewKeyRotationService binds the service to a pool. Both repositories share the
// one pool, so both writes join the same WithTx transaction.
func NewKeyRotationService(pool *database.Pool) *KeyRotationService {
	return &KeyRotationService{
		pool:     pool,
		keys:     database.NewRepository[struct{}](pool, "signing_keys", scanNone),
		bindings: database.NewRepository[struct{}](pool, "key_bindings", scanNone),
	}
}

// Rotate retires the active predecessor signing key and installs successor for
// tenant as ONE all-or-nothing unit of work spanning two repositories:
//
//   - signing_keys, via Rotate: compare-and-set the predecessor's state from
//     "active" to "revoked", and only when that applies insert the successor row;
//   - key_bindings: bind the successor key id to the tenant.
//
// It reports:
//
//   - applied when the predecessor was revoked, the successor installed, and the
//     binding written — all committed together;
//   - already-consumed-conflict / not-found when the predecessor could not be
//     revoked (already rotated, or absent) — the unit installs nothing;
//
// and returns a non-nil error when a write inside the unit fails (e.g. the
// binding insert hits a unique violation). In every non-applied and every
// failing case the whole unit rolls back, so a caller never observes a successor
// key without its binding, nor a predecessor revoked without a successor. That
// "no transient unbound state" property is exactly what wrapping Rotate in
// WithTx buys: Rotate performs two writes and is atomic only inside a
// transaction.
func (s *KeyRotationService) Rotate(ctx context.Context, predecessorID, successorID, tenant string) (transaction.Outcome, error) {
	// Default carried out only on the applied path; overwritten with the real
	// business outcome when the predecessor could not be revoked.
	outcome := transaction.OutcomeApplied
	err := database.WithTx(ctx, s.pool, func(ctx context.Context) error {
		o, err := s.keys.Rotate(ctx, database.RotateSpec{
			KeyColumn:        "id",
			PredecessorKey:   predecessorID,
			StateColumn:      "state",
			Expected:         "active",
			Revoked:          "revoked",
			SuccessorColumns: []string{"id", "tenant", "state"},
			SuccessorValues:  []any{successorID, tenant, "active"},
		})
		if err != nil {
			return err
		}
		if o != transaction.OutcomeApplied {
			// The predecessor was not revoked, so Rotate installed no successor.
			// Carry the conflict/not-found outcome out; the empty unit commits (it
			// wrote nothing), which is indistinguishable from a rollback here.
			outcome = o
			return nil
		}
		// Second repository, SAME transaction: bind the freshly installed
		// successor to the tenant. A failure here (e.g. a duplicate key id) returns
		// an error, so WithTx rolls the signing-key rotation back with it — the
		// predecessor stays active and the successor disappears. All-or-nothing.
		if _, err := s.bindings.Query(ctx,
			"INSERT INTO key_bindings (key_id, tenant) VALUES ($1, $2)", successorID, tenant); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return outcome, nil
}
