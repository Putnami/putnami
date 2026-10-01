package cliagg

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	stderrors "errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
)

// TestReadSQLNeverProjectsAnIdentifier is the static half of the "no device id
// ever crosses the boundary" invariant: the read side must only ever aggregate
// the device column, never select it.
func TestReadSQLNeverProjectsAnIdentifier(t *testing.T) {
	statements := map[string]string{
		"deviceCountSQL":      deviceCountSQL,
		"dailyDeviceCountSQL": dailyDeviceCountSQL,
		"counterSQL":          counterSQL,
	}
	for name, sql := range statements {
		lower := strings.ToLower(sql)
		projection := lower
		if idx := strings.Index(lower, "from"); idx >= 0 {
			projection = lower[:idx]
		}
		if strings.Contains(projection, "device_id") && !strings.Contains(projection, "count(distinct ") {
			t.Errorf("%s projects device_id outside an aggregate: %s", name, sql)
		}
		if strings.Contains(lower, "select *") {
			t.Errorf("%s selects *, which would leak whatever the projection grows: %s", name, sql)
		}
	}

	// The dimension filter is generated from the write-side vocabulary, so the
	// read side can never ask for a dimension the projection does not persist.
	for i := range reportDimensions {
		if !strings.Contains(counterSQL, "$"+strconv.Itoa(i+3)) {
			t.Errorf("counterSQL is missing the placeholder for dimension %q", reportDimensions[i])
		}
	}
	// Every filter value is bound, never interpolated: the predicate carries no
	// quoted literal and no format verb.
	for name, sql := range statements {
		predicate := sql
		if idx := strings.Index(sql, "WHERE"); idx >= 0 {
			predicate = sql[idx:]
		}
		if strings.Contains(predicate, "'") || strings.Contains(predicate, "%s") {
			t.Errorf("%s interpolates a literal into its predicate instead of binding it: %s", name, sql)
		}
	}
}

func TestPoolSourceWithoutPoolIsUnavailable(t *testing.T) {
	src := PoolSource{Now: func() time.Time { return fixedNow }}
	if _, err := src.Report(context.Background(), window(t, Window7d)); !stderrors.Is(err, ErrNoDatasource) {
		t.Fatalf("error = %v, want ErrNoDatasource", err)
	}
	src.Pool = func() *database.Pool { return nil }
	if _, err := src.Report(context.Background(), window(t, Window7d)); !stderrors.Is(err, ErrNoDatasource) {
		t.Fatalf("error = %v, want ErrNoDatasource for an unresolved pool", err)
	}
}

func TestCounterReadLimitDetectsOverflowInsteadOfTruncating(t *testing.T) {
	spectest.Proves(t, "telemetry-putnami-dev/cli-usage-receiver", "exhausted-dimension-is-reported-unavailable", "the-read-side-detects-overflow-instead-of-truncating")
	if !strings.Contains(counterSQL, "LIMIT "+strconv.Itoa(maxCounterRows+1)) {
		t.Fatalf("counter query must request limit+1 so overflow is observable: %s", counterSQL)
	}
	if !strings.Contains(overflowSQL, "cli_aggregate_overflow") {
		t.Fatalf("read contract does not consult durable overflow markers: %s", overflowSQL)
	}
	if !strings.Contains(overflowSQL, "SELECT DISTINCT dimension") {
		t.Fatalf("overflow read is not dimension-scoped: %s", overflowSQL)
	}
	for _, fragment := range []string{
		"contributor_totals AS", "GROUP BY dimension, key",
		"LEFT JOIN contributor_totals",
	} {
		if !strings.Contains(counterSQL, fragment) {
			t.Errorf("counter query is missing set-based contributor aggregation %q", fragment)
		}
	}
	if strings.Contains(counterSQL, "dc.dimension = c.dimension") {
		t.Fatal("counter query regressed to a per-cohort correlated contributor scan")
	}
}

// TestReportRoundTrip is the live-DB integration test for the read contract. It
// stays GATE-GREEN by skipping whenever no test binding is injected: the normal
// gate case is DATABASE_TEST_BINDINGS unset, and testprovider.Provision defaults
// to mode=require (a loud error, not ErrSkip) when the env is blank, so the env
// is checked FIRST — same shape as TestFlushRoundTrip.
//
// With a binding injected it provisions an isolated database, applies this
// feature's migrations, flushes a fixture that straddles the window boundary,
// and asserts the report counts only what is inside the requested window.
func TestReportRoundTrip(t *testing.T) {
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

	// Fixture. Inside the 7d window (2026-07-22 .. 2026-07-29): six devices on
	// the 27th, five on the 28th with two of them repeating, so the distinct
	// count differs from the sum of the daily counts. Far outside it: a much
	// larger day that must never be counted.
	const (
		inWindowA  = "2026-07-27"
		inWindowB  = "2026-07-28"
		outOfRange = "2026-07-01"
	)
	snap := Snapshot{
		Counters: []Counter{
			{Day: inWindowA, Dimension: DimOutcome, Key: "success", Count: 40},
			{Day: inWindowA, Dimension: DimOutcome, Key: "error:api", Count: 8},
			{Day: inWindowB, Dimension: DimOutcome, Key: "success", Count: 12},
			{Day: inWindowA, Dimension: DimCommand, Key: "build", Count: 30},
			{Day: inWindowB, Dimension: DimCommand, Key: "build", Count: 10},
			{Day: outOfRange, Dimension: DimOutcome, Key: "success", Count: 1000},
			{Day: outOfRange, Dimension: DimCommand, Key: "build", Count: 1000},
		},
	}
	for i := 1; i <= 6; i++ {
		id := "d" + strconv.Itoa(i)
		snap.DeviceDays = append(snap.DeviceDays, DeviceDay{Day: inWindowA, DeviceID: id})
		for _, cohort := range []struct{ dim, key string }{
			{DimOutcome, "success"}, {DimOutcome, "error:api"}, {DimCommand, "build"},
		} {
			snap.Contributors = append(snap.Contributors, Contributor{
				Day: inWindowA, Dimension: cohort.dim, Key: cohort.key, DeviceID: id,
			})
		}
	}
	for _, id := range []string{"d1", "d2", "d7", "d8", "d9"} {
		snap.DeviceDays = append(snap.DeviceDays, DeviceDay{Day: inWindowB, DeviceID: id})
		for _, cohort := range []struct{ dim, key string }{
			{DimOutcome, "success"}, {DimCommand, "build"},
		} {
			snap.Contributors = append(snap.Contributors, Contributor{
				Day: inWindowB, Dimension: cohort.dim, Key: cohort.key, DeviceID: id,
			})
		}
	}
	for i := 20; i < 40; i++ {
		snap.DeviceDays = append(snap.DeviceDays, DeviceDay{Day: outOfRange, DeviceID: "d" + strconv.Itoa(i)})
	}
	if err := Flush(ctx, pool, snap); err != nil {
		t.Fatalf("flush fixture: %v", err)
	}

	src := PoolSource{
		Pool: func() *database.Pool { return pool },
		Now:  func() time.Time { return fixedNow },
	}

	t.Run("7d window counts only what is inside it", func(t *testing.T) {
		report, err := src.Report(ctx, window(t, Window7d))
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		// d1..d9 distinct inside the window; the 20 devices on 2026-07-01 are out.
		if got := mustValue(t, report.Summary.Devices); got != 9 {
			t.Errorf("summary.devices = %d, want 9 (distinct inside the window only)", got)
		}
		if got := mustValue(t, report.Summary.Sessions); got != 60 {
			t.Errorf("summary.sessions = %d, want 60 (the 1000 outside the window must not count)", got)
		}
		if got := mustValue(t, report.Summary.Commands); got != 40 {
			t.Errorf("summary.commands = %d, want 40", got)
		}
		outcome := breakdownOf(t, report, DimOutcome)
		if len(outcome.Buckets) != 2 {
			t.Fatalf("outcome buckets = %+v, want success and error:api", outcome.Buckets)
		}
		if outcome.Buckets[0].Key != "success" || outcome.Buckets[0].Count != 52 {
			t.Errorf("outcome[0] = %+v, want success=52 (40+12)", outcome.Buckets[0])
		}
		if outcome.Buckets[1].Key != "error:api" || outcome.Buckets[1].Count != 8 {
			t.Errorf("outcome[1] = %+v, want error:api=8", outcome.Buckets[1])
		}

		byDay := map[string]DailyPoint{}
		for _, p := range report.Daily {
			byDay[p.Day] = p
		}
		if got := mustValue(t, byDay[inWindowA].Devices); got != 6 {
			t.Errorf("daily %s devices = %d, want 6", inWindowA, got)
		}
		if got := mustValue(t, byDay[inWindowB].Devices); got != 5 {
			t.Errorf("daily %s devices = %d, want 5", inWindowB, got)
		}
		if got := mustValue(t, byDay[inWindowA].Sessions); got != 48 {
			t.Errorf("daily %s sessions = %d, want 48", inWindowA, got)
		}
		if got := mustValue(t, byDay[inWindowB].Sessions); got != 12 {
			t.Errorf("daily %s sessions = %d, want 12", inWindowB, got)
		}
		if !byDay[inWindowB].Partial || byDay[inWindowA].Partial {
			t.Errorf("only the current UTC day is partial: %+v", report.Daily)
		}
		if report.Freshness.LatestDay != inWindowB {
			t.Errorf("freshness.latestDay = %q, want %q", report.Freshness.LatestDay, inWindowB)
		}
		assertNoSmallCounts(t, report)
	})

	t.Run("1d window counts only the current UTC day", func(t *testing.T) {
		report, err := src.Report(ctx, window(t, Window1d))
		if err != nil {
			t.Fatalf("report: %v", err)
		}
		if got := mustValue(t, report.Summary.Devices); got != 5 {
			t.Errorf("summary.devices = %d, want 5 (only %s)", got, inWindowB)
		}
		if got := mustValue(t, report.Summary.Sessions); got != 12 {
			t.Errorf("summary.sessions = %d, want 12", got)
		}
		if len(report.Daily) != 1 || report.Daily[0].Day != inWindowB {
			t.Fatalf("daily = %+v, want a single %s bucket", report.Daily, inWindowB)
		}
		assertNoSmallCounts(t, report)
	})
}
