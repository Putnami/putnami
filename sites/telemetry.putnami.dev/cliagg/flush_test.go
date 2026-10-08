package cliagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stderrors "errors"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
	telemetry "go.putnami.dev/protocol/telemetry"
)

func TestFlushRejectsOversizedSnapshotBeforeIO(t *testing.T) {
	snap := Snapshot{Counters: make([]Counter, maxAccumulatorCounters+1)}
	if err := Flush(context.Background(), nil, snap); !stderrors.Is(err, ErrSnapshotOverflow) {
		t.Fatalf("error = %v, want ErrSnapshotOverflow", err)
	}
}

func TestRetentionStatementBoundsEveryDurableProjection(t *testing.T) {
	for _, table := range []string{
		"cli_daily_contributor", "cli_daily_counter", "cli_device_day",
		"cli_aggregate_overflow", "cli_cardinality_admission", "cli_applied_flush",
	} {
		if !strings.Contains(retentionSQL, table) {
			t.Errorf("retention statement does not clean %s", table)
		}
	}
	if !strings.Contains(retentionSQL, "(now() AT TIME ZONE 'UTC')::date - 34") ||
		strings.Contains(retentionSQL, "CURRENT_DATE + 1") ||
		!strings.Contains(retentionSQL, "interval '35 days'") {
		t.Fatalf("retention statement lacks the fixed 35-day/future bounds: %s", retentionSQL)
	}
}

func TestMigrationAdmissionIsIndexedConstantCost(t *testing.T) {
	sql, err := migrationsFS.ReadFile("migrations/20260729170000_harden_cli_retention.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sql)
	start := strings.Index(text, "CREATE OR REPLACE FUNCTION cli_bound_device_day")
	if start < 0 {
		t.Fatal("cap trigger functions are missing")
	}
	runtimeTriggers := text[start:]
	if strings.Contains(runtimeTriggers, "count(*)") {
		t.Fatal("runtime cap trigger scans row counts instead of using the admission ledger")
	}
	for _, required := range []string{
		"cli_cardinality_admission.used + 1",
		"pg_advisory_xact_lock",
		"dimension IN ('*', NEW.dimension)",
		"'counter:' || NEW.dimension",
		"'contributor:' || NEW.dimension",
	} {
		if !strings.Contains(runtimeTriggers, required) && !strings.Contains(text, required) {
			t.Errorf("migration is missing bounded admission invariant %q", required)
		}
	}
}

func TestMigrationSchedulesIndependentExactRetention(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "expiry-runs-without-the-service", "retention-expiry-is-scheduled-in-the-database")
	sql, err := migrationsFS.ReadFile("migrations/20260729170000_harden_cli_retention.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sql)
	for _, required := range []string{
		"pg_cron must be admin-installed",
		"EXECUTE on schedule_in_database",
		"pg_read_all_settings",
		"rolname = session_user AND rolcanlogin",
		"cron.schedule_in_database",
		"cron.job_run_details",
		"'0 0 * * *'",
		"(now() AT TIME ZONE 'UTC')::date - 34",
		"day > (now() AT TIME ZONE 'UTC')::date",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("retention migration is missing %q", required)
		}
	}
	if strings.Contains(text, "CURRENT_DATE + 1") || strings.Contains(text, "CURRENT_DATE - 35") {
		t.Fatal("retention migration retains an extra future or past calendar day")
	}
	if strings.Contains(text, "current_setting('cron.database_name'") {
		t.Fatal("migration role must not require pg_read_all_settings")
	}
	if !strings.Contains(text, "SET LOCAL ROLE NONE") {
		t.Fatal("cron job must be scheduled while acting as the authenticated login role")
	}
}

func TestCronLoginRepairMigrationIsReversible(t *testing.T) {
	up, err := migrationsFS.ReadFile("migrations/20260729190000_fix_cli_retention_cron_user.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	down, err := migrationsFS.ReadFile("migrations/20260729190000_fix_cli_retention_cron_user.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"up": string(up), "down": string(down)} {
		for _, required := range []string{
			"cron.unschedule",
			"'cliagg-retention-' || current_database()",
		} {
			if !strings.Contains(text, required) {
				t.Errorf("%s repair migration is missing %q", name, required)
			}
		}
	}
	if !strings.Contains(string(up), "session_user") ||
		!strings.Contains(string(up), "rolcanlogin") ||
		!strings.Contains(string(up), "cron.schedule_in_database") {
		t.Fatal("up repair does not select and validate the login role")
	}
	if !strings.Contains(string(up), "SET LOCAL ROLE NONE") ||
		!strings.Contains(string(down), "SET LOCAL ROLE NONE") {
		t.Fatal("repair migrations do not manage the login-owned job under the login role")
	}
	if strings.Contains(string(down), "cron.schedule_in_database") {
		t.Fatal("down repair must not restore the non-executable owner job")
	}
}

func TestRetentionMigrationRollbackRestoresPriorContract(t *testing.T) {
	sql, err := migrationsFS.ReadFile("migrations/20260729170000_harden_cli_retention.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	text := string(sql)
	for _, required := range []string{
		"cron.unschedule",
		"DROP FUNCTION IF EXISTS cli_expire_aggregates",
		"CURRENT_DATE - 35",
		"CURRENT_DATE + 1",
		"VALUES (NEW.day, 'counter', 1)",
		"VALUES (NEW.day, 'contributor', 1)",
		"DROP COLUMN dimension",
		"ADD PRIMARY KEY (day)",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("rollback is missing prior-contract fragment %q", required)
		}
	}
	update := strings.Index(text, "UPDATE cli_aggregate_overflow SET dimension = '*'")
	dedupe := strings.Index(text, "DELETE FROM cli_aggregate_overflow a")
	if update < 0 || dedupe < 0 || update > dedupe {
		t.Fatal("rollback must collapse dimensions before deduplicating days")
	}
}

// TestFlushRoundTrip is the live-DB integration test. It stays GATE-GREEN by
// skipping whenever no test binding is injected: the normal gate case is
// DATABASE_TEST_BINDINGS unset, and testprovider.Provision defaults to
// mode=require (a loud error, not ErrSkip) when the env is blank, so the env
// is checked FIRST. With a binding injected it provisions an isolated database,
// applies this feature's migrations, flushes a fixture accumulator, and reads
// back to assert daily uniques, monthly uniques, counter values, and the
// additive/idempotent upsert semantics.
func TestFlushRoundTrip(t *testing.T) {
	if strings.TrimSpace(os.Getenv(testprovider.EnvTestBinding)) == "" {
		t.Skipf("%s unset; skipping live DB integration test", testprovider.EnvTestBinding)
	}

	ctx := context.Background()
	res, err := testprovider.Provision(ctx, testprovider.Options{})
	if err != nil {
		if stderrors.Is(err, testprovider.ErrSkip) {
			t.Skip("test binding mode=skip; skipping live DB integration test")
		}
		t.Fatalf("provision test database: %v", err)
	}
	defer func() {
		if cerr := res.Cleanup(); cerr != nil {
			t.Errorf("cleanup: %v", cerr)
		}
	}()

	cfg, err := database.PoolConfigFromBinding(res.Binding, Datasource)
	if err != nil {
		t.Fatalf("pool config from binding: %v", err)
	}
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	defer pool.Close()

	applyTestMigrations(t, ctx, pool)

	// Fixture: two devices on day A (one success, one failure), and day A device
	// d1 again on day B, so unique-device counts differ from session counts.
	a := nanosAt(dayA)
	b := nanosAt(dayB)
	acc := testAccumulator()
	acc.Add([]telemetry.ResourceLogs{cliResource(
		sessionStart(a, "d1", "build,test", true),
		sessionEnd(a, "d1", "1.2.3", "darwin", "arm64", true, true, ""),
		sessionStart(a, "d2", "deploy", false),
		sessionEnd(a, "d2", "1.2.3", "linux", "amd64", false, false, "api"),
		sessionStart(b, "d1", "build", true),
		sessionEnd(b, "d1", "1.3.0", "darwin", "arm64", true, true, ""),
	)})
	snap := acc.Drain()

	if err := Flush(ctx, pool, snap); err != nil {
		t.Fatalf("flush: %v", err)
	}
	overflowSnap := Snapshot{
		ID:        strings.Repeat("a", 32),
		Overflows: []Overflow{{Day: "2026-07-25", Dimension: DimCLIVersion}},
	}
	if err := Flush(ctx, pool, overflowSnap); err != nil {
		t.Fatalf("persist overflow marker: %v", err)
	}
	var overflowed bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM cli_aggregate_overflow WHERE day = $1::date AND dimension = $2)`,
		"2026-07-25", DimCLIVersion,
	).Scan(&overflowed); err != nil {
		t.Fatalf("read overflow marker: %v", err)
	}
	if !overflowed {
		t.Fatal("fail-closed overflow marker was not persisted")
	}

	// Daily uniques: 2 on day A (d1, d2), 1 on day B (d1).
	if got := dailyUniques(t, ctx, pool, "2026-07-25"); got != 2 {
		t.Errorf("daily uniques 2026-07-25 = %d, want 2", got)
	}
	if got := dailyUniques(t, ctx, pool, "2026-07-26"); got != 1 {
		t.Errorf("daily uniques 2026-07-26 = %d, want 1", got)
	}
	// Monthly uniques: distinct devices across July = d1, d2 = 2.
	if got := monthlyUniques(t, ctx, pool, "2026-07-01", "2026-08-01"); got != 2 {
		t.Errorf("monthly uniques July = %d, want 2", got)
	}

	// Counter spot-checks.
	if got := counterValue(t, ctx, pool, DimCommand, "test"); got != 1 {
		t.Errorf("command test 2026-07-25 = %d, want 1", got)
	}
	if got := counterValue(t, ctx, pool, DimOutcome, "error:api"); got != 1 {
		t.Errorf("outcome error:api 2026-07-25 = %d, want 1", got)
	}
	if got := counterValue(t, ctx, pool, DimCLIVersion, "1.2.3"); got != 2 {
		t.Errorf("cli_version 1.2.3 2026-07-25 = %d, want 2", got)
	}

	// Re-flushing the SAME generated snapshot is wholly idempotent.
	if err := Flush(ctx, pool, snap); err != nil {
		t.Fatalf("re-flush: %v", err)
	}
	if got := dailyUniques(t, ctx, pool, "2026-07-25"); got != 2 {
		t.Errorf("after re-flush daily uniques 2026-07-25 = %d, want 2 (idempotent)", got)
	}
	if got := counterValue(t, ctx, pool, DimCLIVersion, "1.2.3"); got != 2 {
		t.Errorf("after replay cli_version 1.2.3 = %d, want 2 (idempotent)", got)
	}
}

func applyTestMigrations(t *testing.T, ctx context.Context, pool *database.Pool) {
	t.Helper()
	if _, err := database.ApplyToPool(ctx, pool, Source()); err != nil {
		if strings.Contains(err.Error(), "pg_cron must be admin-installed") {
			t.Skip("test database lacks required pg_cron extension; production migration deliberately fails until cloudsql.enable_pg_cron=on")
		}
		t.Fatalf("apply migrations: %v", err)
	}
}

func dailyUniques(t *testing.T, ctx context.Context, pool *database.Pool, day string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM cli_device_day WHERE day = $1::date`, day,
	).Scan(&n); err != nil {
		t.Fatalf("daily uniques query: %v", err)
	}
	return n
}

func monthlyUniques(t *testing.T, ctx context.Context, pool *database.Pool, from, to string) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx,
		`SELECT count(DISTINCT device_id) FROM cli_device_day WHERE day >= $1::date AND day < $2::date`, from, to,
	).Scan(&n); err != nil {
		t.Fatalf("monthly uniques query: %v", err)
	}
	return n
}

func counterValue(t *testing.T, ctx context.Context, pool *database.Pool, dim, key string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(ctx,
		`SELECT count FROM cli_daily_counter WHERE day = $1::date AND dimension = $2 AND key = $3`,
		"2026-07-25", dim, key,
	).Scan(&n)
	if err != nil {
		if database.IsNoRows(err) {
			return 0
		}
		t.Fatalf("counter query: %v", err)
	}
	return n
}
