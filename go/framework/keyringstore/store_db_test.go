package keyringstore

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stderrors "errors"
	"os"
	"sort"
	"testing"
	"time"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
	"go.putnami.dev/protocol/keyring"
	"go.putnami.dev/protocol/transaction"
	"go.putnami.dev/security"
)

// schemaDDL is the signing_keys shape the store persists to. It is recreated
// fresh per top-level test so cases never observe each other's rows.
const schemaDDL = `
DROP TABLE IF EXISTS signing_keys;
CREATE TABLE signing_keys (
  kid        TEXT PRIMARY KEY,
  keyring_id TEXT NOT NULL,
  state      TEXT NOT NULL,
  alg        TEXT NOT NULL,
  kty        TEXT NOT NULL,
  material   TEXT NOT NULL,
  retired_at TIMESTAMPTZ,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX signing_keys_keyring_state ON signing_keys (keyring_id, state);
`

// provisionPool provisions an isolated Postgres via the shared test provider and
// creates the signing_keys schema. It is gated behind DATABASE_TEST_BINDINGS
// EXACTLY like go/samples/unit-of-work-proof/uow_test.go: with no binding it
// SKIPS, so the local unit gate stays green with no Postgres. CI injects the
// binding and runs the assertions for real.
func provisionPool(t *testing.T) *database.Pool {
	t.Helper()
	if os.Getenv(testprovider.EnvTestBinding) == "" {
		t.Skipf("%s not set; skipping keyringstore DB integration (set it to a postgres binding to run)", testprovider.EnvTestBinding)
	}

	ctx := context.Background()
	result, err := testprovider.Provision(ctx, testprovider.Options{})
	if err != nil {
		if stderrors.Is(err, testprovider.ErrSkip) {
			t.Skipf("test provider mode=skip: %v", err)
		}
		t.Fatalf("provision: %v", err)
	}
	t.Cleanup(func() {
		if err := result.Cleanup(); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	names := make([]string, 0, len(result.Binding.Databases))
	for n := range result.Binding.Databases {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("provisioned binding has no datasources")
	}

	cfg, err := database.PoolConfigFromBinding(result.Binding, names[0])
	if err != nil {
		t.Fatalf("PoolConfigFromBinding(%q): %v", names[0], err)
	}
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("connect datasource %q: %v", names[0], err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, schemaDDL); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	return pool
}

func newStore(t *testing.T, pool *database.Pool) *DBKeyringStore {
	t.Helper()
	store, err := New(pool, Config{})
	if err != nil {
		t.Fatalf("New store: %v", err)
	}
	return store
}

// TestLoadEmptyReturnsErrNoSigningKey proves the fail-closed contract: an empty
// keyring surfaces security.ErrNoSigningKey, uniform with every other backend.
func TestLoadEmptyReturnsErrNoSigningKey(t *testing.T) {
	pool := provisionPool(t)
	store := newStore(t, pool)

	_, err := store.Load(context.Background())
	if !stderrors.Is(err, security.ErrNoSigningKey) {
		t.Fatalf("Load(empty) = %v, want ErrNoSigningKey", err)
	}
}

// TestSaveLoadRoundTrip proves Save→Load persists the owner document with its
// private material and that Load returns only the PUBLISHABLE keys (a revoked
// key is stored but never loaded into a live provider).
func TestSaveLoadRoundTrip(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "durable-owner-document", "the-owner-document-round-trips-and-excludes-revoked-keys")
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	kr := &keyring.PrivateKeyring{
		ProtocolVersion: keyring.ProtocolVersion,
		Keys: []keyring.PrivateJWK{
			{Kty: "EC", Kid: "ec-0", Alg: "ES256", State: keyring.KeyStateRetiring, Crv: "P-256", X: "x0", Y: "y0", D: "d0"},
			{Kty: "EC", Kid: "ec-1", Alg: "ES256", State: keyring.KeyStateActive, Crv: "P-256", X: "x1", Y: "y1", D: "d1-secret"},
			{Kty: "EC", Kid: "ec-x", Alg: "ES256", State: keyring.KeyStateRevoked, Crv: "P-256", X: "x2", Y: "y2", D: "d2"},
		},
	}
	if err := store.Save(ctx, kr); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Only active + retiring are loaded, ordered by kid.
	if len(got.Keys) != 2 {
		t.Fatalf("loaded %d keys, want 2 (revoked excluded): %+v", len(got.Keys), got.Keys)
	}
	if got.Keys[0].Kid != "ec-0" || got.Keys[0].State != keyring.KeyStateRetiring {
		t.Fatalf("key[0] = %+v, want ec-0 retiring", got.Keys[0])
	}
	if got.Keys[1].Kid != "ec-1" || got.Keys[1].State != keyring.KeyStateActive {
		t.Fatalf("key[1] = %+v, want ec-1 active", got.Keys[1])
	}
	// Private material must survive the round-trip (this row IS the owner document).
	if got.Keys[1].D != "d1-secret" {
		t.Fatalf("private scalar d not persisted: got %q", got.Keys[1].D)
	}
	// All three rows exist in the table; only the projection filters.
	assertCount(t, ctx, pool, 3, "keyring_id = 'default'")
	assertCount(t, ctx, pool, 1, "state = 'revoked'")
}

// TestRotateCommitsAtomically proves the happy path: predecessor retired,
// successor installed, retired_at stamped — all committed together, and Load
// then returns both keys.
func TestRotateCommitsAtomically(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "atomic-rotation", "rotation-commits-in-one-transaction")
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	seed := &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k1", Alg: "ES256", State: keyring.KeyStateActive, D: "k1-secret"},
	}}
	if err := store.Save(ctx, seed); err != nil {
		t.Fatalf("Save seed: %v", err)
	}

	successor := keyring.PrivateJWK{Kty: "EC", Kid: "k2", Alg: "ES256", State: keyring.KeyStateActive, D: "k2-secret"}
	outcome, err := store.Rotate(ctx, "k1", successor)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if outcome != transaction.OutcomeApplied {
		t.Fatalf("Rotate outcome = %q, want applied", outcome)
	}

	assertCount(t, ctx, pool, 1, "kid = 'k1' AND state = 'retiring' AND retired_at IS NOT NULL")
	assertCount(t, ctx, pool, 1, "kid = 'k2' AND state = 'active' AND keyring_id = 'default'")

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after rotate: %v", err)
	}
	if len(got.Keys) != 2 {
		t.Fatalf("loaded %d keys after rotate, want 2", len(got.Keys))
	}
}

// TestRotateRollsBackOnSuccessorConflict is the atomicity proof: when the
// successor insert fails (a duplicate successor kid), the WHOLE unit rolls back
// — the predecessor stays ACTIVE and never enters retiring, and no half-rotation
// commits. The outcome is a business conflict, not an error.
func TestRotateRollsBackOnSuccessorConflict(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "atomic-rotation", "a-successor-conflict-rolls-back-all-writes")
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	seed := &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k1", Alg: "ES256", State: keyring.KeyStateActive},
	}}
	if err := store.Save(ctx, seed); err != nil {
		t.Fatalf("Save seed: %v", err)
	}
	// Pre-insert the kid the rotation will try to install, so the successor
	// INSERT inside the unit hits a primary-key violation.
	mustExec(t, ctx, pool,
		"INSERT INTO signing_keys (kid, keyring_id, state, alg, kty, material) VALUES ('k2', 'other', 'active', 'ES256', 'EC', '{}')")

	outcome, err := store.Rotate(ctx, "k1", keyring.PrivateJWK{Kty: "EC", Kid: "k2", Alg: "ES256", State: keyring.KeyStateActive})
	if err != nil {
		t.Fatalf("Rotate returned error, want a conflict outcome: %v", err)
	}
	if SuccessorInstalled(outcome) {
		t.Fatalf("Rotate outcome = %q reported a successor installed; want a non-applied conflict", outcome)
	}

	// The predecessor revoke must have been rolled back with the failed insert.
	assertCount(t, ctx, pool, 1, "kid = 'k1' AND state = 'active' AND retired_at IS NULL")
	// The pre-existing k2 row is untouched (still 'other'); no half-rotation.
	assertCount(t, ctx, pool, 1, "kid = 'k2' AND keyring_id = 'other'")
	assertCount(t, ctx, pool, 0, "kid = 'k2' AND keyring_id = 'default'")
}

// TestRotateNonAppliedWhenPredecessorNotActive proves that when the predecessor
// cannot be transitioned the unit installs NOTHING and reports the business
// outcome rather than an error.
func TestRotateNonAppliedWhenPredecessorNotActive(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "atomic-rotation", "a-non-applied-outcome-rolls-back-all-writes")
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	// Absent predecessor → not-found; no successor installed.
	outcome, err := store.Rotate(ctx, "missing", keyring.PrivateJWK{Kty: "EC", Kid: "s1", Alg: "ES256"})
	if err != nil {
		t.Fatalf("Rotate(missing): %v", err)
	}
	if outcome != transaction.OutcomeNotFound {
		t.Fatalf("Rotate(missing) = %q, want not-found", outcome)
	}

	// Already-retiring predecessor → conflict; no successor installed.
	mustExec(t, ctx, pool,
		"INSERT INTO signing_keys (kid, keyring_id, state, alg, kty, material) VALUES ('k5', 'default', 'retiring', 'ES256', 'EC', '{}')")
	outcome, err = store.Rotate(ctx, "k5", keyring.PrivateJWK{Kty: "EC", Kid: "s2", Alg: "ES256"})
	if err != nil {
		t.Fatalf("Rotate(retiring): %v", err)
	}
	if outcome != transaction.OutcomeAlreadyConsumedConflict {
		t.Fatalf("Rotate(retiring) = %q, want already-consumed-conflict", outcome)
	}

	// Neither non-applied rotation installed a successor.
	assertCount(t, ctx, pool, 0, "kid IN ('s1', 's2')")
}

// TestRotateRetiredAtRoundTripsThroughLoad proves the retirement instant Rotate
// stamps in the retired_at COLUMN is surfaced back through Load as
// PrivateJWK.RetiredAt, so the in-memory provider measures the overlap window
// from the real retirement time and it survives a process restart — rather than
// resetting to "now" on every reload and republishing a long-retired key forever.
func TestRotateRetiredAtRoundTripsThroughLoad(t *testing.T) {
	spectest.Proves(t, "go/signing-key-rotation", "durable-owner-document", "the-retirement-instant-survives-a-restart")
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	seed := &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k1", Alg: "ES256", State: keyring.KeyStateActive, D: "k1-secret"},
	}}
	if err := store.Save(ctx, seed); err != nil {
		t.Fatalf("Save seed: %v", err)
	}
	before := time.Now().Add(-time.Second)
	if _, err := store.Rotate(ctx, "k1", keyring.PrivateJWK{Kty: "EC", Kid: "k2", Alg: "ES256", State: keyring.KeyStateActive, D: "k2-secret"}); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	after := time.Now().Add(time.Second)

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var retiring *keyring.PrivateJWK
	for i := range got.Keys {
		if got.Keys[i].Kid == "k1" {
			retiring = &got.Keys[i]
		}
	}
	if retiring == nil {
		t.Fatalf("retiring k1 not returned by Load: %+v", got.Keys)
	}
	if retiring.State != keyring.KeyStateRetiring {
		t.Fatalf("k1 state = %q, want retiring", retiring.State)
	}
	if retiring.RetiredAt == "" {
		t.Fatal("retiring k1 has empty RetiredAt: retirement instant lost across the seam")
	}
	ts, err := time.Parse(time.RFC3339Nano, retiring.RetiredAt)
	if err != nil {
		t.Fatalf("RetiredAt %q is not RFC 3339: %v", retiring.RetiredAt, err)
	}
	if ts.Before(before) || ts.After(after) {
		t.Fatalf("RetiredAt %v outside the rotation window [%v, %v]", ts, before, after)
	}

	// A non-retiring (active) key carries no instant.
	for i := range got.Keys {
		if got.Keys[i].Kid == "k2" && got.Keys[i].RetiredAt != "" {
			t.Fatalf("active k2 unexpectedly carries RetiredAt %q", got.Keys[i].RetiredAt)
		}
	}
}

// TestSavePrunesDroppedKeys proves Save REPLACES the owner document: a key no
// longer in the saved document is pruned, so re-seeding a keyring with a new
// active key cannot leave the previous active row behind (which would make Load
// return two active keys and fail provider construction). Saving an empty
// document clears the keyring entirely.
func TestSavePrunesDroppedKeys(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	first := &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k1", Alg: "ES256", State: keyring.KeyStateActive, D: "k1"},
		{Kty: "EC", Kid: "k2", Alg: "ES256", State: keyring.KeyStateRetiring, D: "k2"},
	}}
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save first: %v", err)
	}
	assertCount(t, ctx, pool, 2, "keyring_id = 'default'")

	// Re-seed with a different active key, dropping k1 and k2. Without pruning the
	// old active k1 would linger, giving two active keys on the next Load.
	second := &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k3", Alg: "ES256", State: keyring.KeyStateActive, D: "k3"},
	}}
	if err := store.Save(ctx, second); err != nil {
		t.Fatalf("Save second: %v", err)
	}
	assertCount(t, ctx, pool, 1, "keyring_id = 'default'")
	assertCount(t, ctx, pool, 0, "kid IN ('k1', 'k2')")
	assertCount(t, ctx, pool, 1, "kid = 'k3' AND state = 'active'")

	got, err := store.Load(ctx)
	if err != nil {
		t.Fatalf("Load after re-seed: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].Kid != "k3" {
		t.Fatalf("Load = %+v, want only k3", got.Keys)
	}

	// Saving an empty document clears the keyring (full replace).
	if err := store.Save(ctx, &keyring.PrivateKeyring{}); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	assertCount(t, ctx, pool, 0, "keyring_id = 'default'")
	if _, err := store.Load(ctx); !stderrors.Is(err, security.ErrNoSigningKey) {
		t.Fatalf("Load after clear = %v, want ErrNoSigningKey", err)
	}
}

// TestSaveDoesNotPruneOtherKeyrings proves pruning is scoped to the store's own
// keyring_id: re-seeding one keyring never touches another keyring's rows.
func TestSaveDoesNotPruneOtherKeyrings(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	store := newStore(t, pool)

	// A row owned by a different keyring must survive this store's saves.
	mustExec(t, ctx, pool,
		"INSERT INTO signing_keys (kid, keyring_id, state, alg, kty, material) VALUES ('other-k', 'other', 'active', 'ES256', 'EC', '{}')")

	if err := store.Save(ctx, &keyring.PrivateKeyring{Keys: []keyring.PrivateJWK{
		{Kty: "EC", Kid: "k1", Alg: "ES256", State: keyring.KeyStateActive, D: "k1"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	// Replace again, clearing 'default'; 'other' keyring is untouched throughout.
	if err := store.Save(ctx, &keyring.PrivateKeyring{}); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	assertCount(t, ctx, pool, 1, "keyring_id = 'other' AND kid = 'other-k'")
}

// --- helpers ---

func mustExec(t *testing.T, ctx context.Context, pool *database.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func assertCount(t *testing.T, ctx context.Context, pool *database.Pool, want int, where string) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM signing_keys WHERE "+where).Scan(&n); err != nil {
		t.Fatalf("count WHERE %s: %v", where, err)
	}
	if n != want {
		t.Errorf("count WHERE %s = %d, want %d", where, n, want)
	}
}
