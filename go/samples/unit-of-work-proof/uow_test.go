package uowproof_test

import (
	"context"
	stderrors "errors"
	"os"
	"sort"
	"sync"
	"testing"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
	"go.putnami.dev/protocol/transaction"

	uowproof "go.putnami.dev/examples/unit-of-work-proof"
)

// The schema the proof runs against: a once-only device-code table and the
// signing-key / binding pair that the rotation spans. Recreated fresh per top
// level test so cases never observe each other's rows.
const schemaDDL = `
DROP TABLE IF EXISTS device_codes;
DROP TABLE IF EXISTS key_bindings;
DROP TABLE IF EXISTS signing_keys;
CREATE TABLE device_codes (
  code        TEXT PRIMARY KEY,
  consumed    BOOLEAN NOT NULL DEFAULT false,
  consumed_by TEXT
);
CREATE TABLE signing_keys (
  id     TEXT PRIMARY KEY,
  tenant TEXT NOT NULL,
  state  TEXT NOT NULL
);
CREATE TABLE key_bindings (
  key_id TEXT PRIMARY KEY,
  tenant TEXT NOT NULL
);
`

// provisionPool provisions an isolated Postgres via the shared test provider and
// creates the proof schema. It is gated behind DATABASE_TEST_BINDINGS exactly
// like go/framework/database/conformance_test.go: with no binding it SKIPS, so
// the local unit gate stays green with no Postgres. CI injects the binding and
// runs the assertions for real.
func provisionPool(t *testing.T) *database.Pool {
	t.Helper()
	if os.Getenv(testprovider.EnvTestBinding) == "" {
		t.Skipf("%s not set; skipping unit-of-work proof (set it to a postgres binding to run)", testprovider.EnvTestBinding)
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

// TestConsumeOnceRedeemsExactlyOnce proves the sequential once-only taxonomy: a
// registered code is applied once, already-consumed-conflict thereafter, and an
// unknown code is not-found — and a losing redemption never overwrites the
// winner's claim.
func TestConsumeOnceRedeemsExactlyOnce(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	svc := uowproof.NewDeviceCodeService(pool)

	mustExec(t, ctx, pool, "INSERT INTO device_codes (code) VALUES ($1)", "dev-1")

	if got := mustRedeem(t, ctx, svc, "dev-1", "alice"); got != transaction.OutcomeApplied {
		t.Fatalf("first redeem = %q, want applied", got)
	}
	if got := mustRedeem(t, ctx, svc, "dev-1", "bob"); got != transaction.OutcomeAlreadyConsumedConflict {
		t.Fatalf("second redeem = %q, want already-consumed-conflict", got)
	}
	if got := mustRedeem(t, ctx, svc, "never-issued", "bob"); got != transaction.OutcomeNotFound {
		t.Fatalf("unknown redeem = %q, want not-found", got)
	}

	// The losing redemption must not have clobbered the winner's claim.
	assertCount(t, ctx, pool, 1, "device_codes", "code = $1 AND consumed = true AND consumed_by = $2", "dev-1", "alice")
}

// TestConsumeOnceUnderConcurrency proves the core safety invariant: with N
// redemptions racing for one code, exactly one observes applied and the rest
// observe already-consumed-conflict. This is the property row locking + the
// still-unclaimed guard exist to guarantee.
func TestConsumeOnceUnderConcurrency(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	svc := uowproof.NewDeviceCodeService(pool)

	mustExec(t, ctx, pool, "INSERT INTO device_codes (code) VALUES ($1)", "dev-race")

	const racers = 16
	outcomes := make([]transaction.Outcome, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outcomes[i], errs[i] = svc.Redeem(ctx, "dev-race", "user")
		}(i)
	}
	wg.Wait()

	applied, conflict := 0, 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		switch outcomes[i] {
		case transaction.OutcomeApplied:
			applied++
		case transaction.OutcomeAlreadyConsumedConflict:
			conflict++
		default:
			t.Fatalf("racer %d unexpected outcome %q", i, outcomes[i])
		}
	}
	if applied != 1 || conflict != racers-1 {
		t.Fatalf("consume-once race = %d applied / %d conflict, want 1 / %d", applied, conflict, racers-1)
	}
}

// TestRotateCommitsAllOrNothing proves the happy path of the atomic two
// repository rotation: predecessor revoked, successor installed, binding written
// — all present after commit.
func TestRotateCommitsAllOrNothing(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	svc := uowproof.NewKeyRotationService(pool)

	mustExec(t, ctx, pool, "INSERT INTO signing_keys (id, tenant, state) VALUES ($1, $2, 'active')", "k1", "acme")

	got, err := svc.Rotate(ctx, "k1", "k2", "acme")
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if got != transaction.OutcomeApplied {
		t.Fatalf("rotate = %q, want applied", got)
	}
	assertCount(t, ctx, pool, 1, "signing_keys", "id = $1 AND state = 'revoked'", "k1")
	assertCount(t, ctx, pool, 1, "signing_keys", "id = $1 AND state = 'active'", "k2")
	assertCount(t, ctx, pool, 1, "key_bindings", "key_id = $1 AND tenant = $2", "k2", "acme")
}

// TestRotateRollsBackOnSecondWriteFailure is the atomicity proof: when the second
// repository write fails (here a duplicate binding key id), the whole unit rolls
// back — the predecessor stays active and the successor never appears. No
// transient unbound state.
func TestRotateRollsBackOnSecondWriteFailure(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	svc := uowproof.NewKeyRotationService(pool)

	mustExec(t, ctx, pool, "INSERT INTO signing_keys (id, tenant, state) VALUES ($1, $2, 'active')", "k3", "globex")
	// Pre-bind the id the rotation will try to install for its successor, so the
	// binding insert inside the unit hits a primary-key violation.
	mustExec(t, ctx, pool, "INSERT INTO key_bindings (key_id, tenant) VALUES ($1, $2)", "k4", "someone-else")

	if _, err := svc.Rotate(ctx, "k3", "k4", "globex"); err == nil {
		t.Fatal("rotate succeeded; want an error from the conflicting binding insert")
	}

	// The revoke and the successor insert must both have been rolled back.
	assertCount(t, ctx, pool, 1, "signing_keys", "id = $1 AND state = 'active'", "k3")
	assertCount(t, ctx, pool, 0, "signing_keys", "id = $1", "k4")
	// The pre-existing binding is untouched; no binding was added for globex.
	assertCount(t, ctx, pool, 1, "key_bindings", "key_id = $1 AND tenant = $2", "k4", "someone-else")
}

// TestRotateNonApplied proves that when the predecessor cannot be revoked the
// unit installs nothing and reports the business outcome rather than an error.
func TestRotateNonApplied(t *testing.T) {
	pool := provisionPool(t)
	ctx := context.Background()
	svc := uowproof.NewKeyRotationService(pool)

	// Absent predecessor → not-found.
	got, err := svc.Rotate(ctx, "missing", "s1", "t")
	if err != nil {
		t.Fatalf("rotate(missing): %v", err)
	}
	if got != transaction.OutcomeNotFound {
		t.Fatalf("rotate(missing) = %q, want not-found", got)
	}

	// Already-revoked predecessor → already-consumed-conflict.
	mustExec(t, ctx, pool, "INSERT INTO signing_keys (id, tenant, state) VALUES ($1, $2, 'revoked')", "k5", "t")
	got, err = svc.Rotate(ctx, "k5", "s2", "t")
	if err != nil {
		t.Fatalf("rotate(revoked): %v", err)
	}
	if got != transaction.OutcomeAlreadyConsumedConflict {
		t.Fatalf("rotate(revoked) = %q, want already-consumed-conflict", got)
	}

	// Neither non-applied rotation installed a successor or a binding.
	assertCount(t, ctx, pool, 0, "signing_keys", "id IN ($1, $2)", "s1", "s2")
	assertCount(t, ctx, pool, 0, "key_bindings", "key_id IN ($1, $2)", "s1", "s2")
}

// --- helpers ---

func mustRedeem(t *testing.T, ctx context.Context, svc *uowproof.DeviceCodeService, code, user string) transaction.Outcome {
	t.Helper()
	got, err := svc.Redeem(ctx, code, user)
	if err != nil {
		t.Fatalf("redeem(%q): %v", code, err)
	}
	return got
}

func mustExec(t *testing.T, ctx context.Context, pool *database.Pool, query string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func assertCount(t *testing.T, ctx context.Context, pool *database.Pool, want int, table, where string, args ...any) {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+table+" WHERE "+where, args...).Scan(&n); err != nil {
		t.Fatalf("count %s WHERE %s: %v", table, where, err)
	}
	if n != want {
		t.Errorf("count %s WHERE %s = %d, want %d", table, where, n, want)
	}
}
