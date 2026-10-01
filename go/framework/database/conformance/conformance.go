// Package conformance provides the exported, cross-language transaction /
// concurrency conformance runner for the Putnami database adapter. A downstream
// project opts into the full corpus with a single committed line:
//
//	func TestConformance(t *testing.T) { conformance.Run(t) }
//
// Run provisions a real Postgres via the shared testprovider, loads the single
// canonical corpus embedded in go.putnami.dev/protocol/transaction (no
// repo-relative path), and executes every case in listed order against the
// tx/CAS primitives — honoring concurrency groups with real goroutines — while
// asserting the emitted Outcome and row post-conditions. It is gated behind
// DATABASE_TEST_BINDINGS exactly like the rest of the database integration
// suite: with no binding it SKIPS, so a unit gate stays green with no Postgres.
//
// This package deliberately imports "testing" in non-test source: it exists to
// be called by a downstream project's own test binary.
package conformance

import (
	"context"
	stderrors "errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"go.putnami.dev/database"
	"go.putnami.dev/database/testprovider"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/transaction"
)

// Sentinel boundary errors. The runner rolls a transaction boundary back by
// returning one of these from the callback; a boundary that returns one is an
// EXPECTED rollback (a conflict outcome, or an injected fault), not a runner
// failure. Any other error is a real failure.
var (
	errInjectedFault      = stderrors.New("conformance: injected callback fault")
	errRollbackNonApplied = stderrors.New("conformance: rollback because outcome was not applied")
)

func isExpectedRollback(err error) bool {
	return stderrors.Is(err, errInjectedFault) || stderrors.Is(err, errRollbackNonApplied)
}

// Run is the Go half of the cross-language transaction / concurrency conformance
// corpus. It provisions a real Postgres via the shared testprovider
// and, for every case in the manifest, renders the setup DDL, runs the operations
// (honoring concurrency groups with real goroutines) against the tx/CAS
// primitives, and asserts the emitted Outcome and row post-conditions.
//
// It is gated behind DATABASE_TEST_BINDINGS exactly like
// testprovider/integration_test.go: with no binding it SKIPS, so the local unit
// gate stays green with no Postgres. CI injects the binding and runs it for real.
func Run(t *testing.T) {
	t.Helper()
	if os.Getenv(testprovider.EnvTestBinding) == "" {
		t.Skipf("%s not set; skipping cross-language transaction conformance (set it to a postgres binding to run)", testprovider.EnvTestBinding)
	}

	manifest, diags := transaction.ParseAndValidateManifest(transaction.ConformanceManifestJSON())
	if diag.HasErrors(diags) {
		t.Fatalf("conformance manifest is invalid: %v", diags)
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
	dsName := names[0]

	cfg, err := database.PoolConfigFromBinding(result.Binding, dsName)
	if err != nil {
		t.Fatalf("PoolConfigFromBinding(%q): %v", dsName, err)
	}
	pool, err := database.NewPool(ctx, cfg)
	if err != nil {
		t.Fatalf("connect datasource %q: %v", dsName, err)
	}
	defer pool.Close()

	for _, c := range manifest.Cases {
		t.Run(c.ID, func(t *testing.T) {
			runConformanceCase(ctx, t, pool, c)
		})
	}
}

func runConformanceCase(ctx context.Context, t *testing.T, pool *database.Pool, c transaction.Case) {
	t.Helper()

	repos := map[string]*database.Repository[struct{}]{}
	for _, tbl := range c.Setup.Tables {
		if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+quoteIdent(tbl.Name)); err != nil {
			t.Fatalf("drop table %q: %v", tbl.Name, err)
		}
		if _, err := pool.Exec(ctx, createTableSQL(tbl)); err != nil {
			t.Fatalf("create table %q: %v", tbl.Name, err)
		}
		for _, row := range tbl.Seed {
			q, args := insertSQL(tbl.Name, row)
			if _, err := pool.Exec(ctx, q, args...); err != nil {
				t.Fatalf("seed table %q: %v", tbl.Name, err)
			}
		}
		repos[tbl.Name] = database.NewRepository(pool, tbl.Name, scanNothing)
	}

	opByID := map[string]transaction.Operation{}
	concurrent := map[string]bool{}
	for _, op := range c.Operations {
		opByID[op.ID] = op
	}
	for _, g := range c.Concurrency {
		for _, id := range g.Operations {
			concurrent[id] = true
		}
	}

	// Sequential operations (those carrying an expected Outcome) run first, in
	// listed order; concurrency groups run afterwards in parallel.
	for _, op := range c.Operations {
		if concurrent[op.ID] {
			continue
		}
		outcome, err := runOperation(ctx, repos[op.Table], op)
		if err != nil {
			t.Fatalf("operation %s: %v", op.ID, err)
		}
		if outcome != op.Expect {
			t.Errorf("operation %s = %q, want %q", op.ID, outcome, op.Expect)
		}
	}

	for _, g := range c.Concurrency {
		runConcurrencyGroup(ctx, t, repos, opByID, g)
	}

	for _, a := range c.Asserts {
		var n int
		q := "SELECT COUNT(*) FROM " + quoteIdent(a.Table) + " WHERE " + a.Where
		if err := pool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatalf("assert query %q: %v", q, err)
		}
		if n != a.Count {
			t.Errorf("assert %s WHERE %s = %d, want %d", a.Table, a.Where, n, a.Count)
		}
	}
}

func runConcurrencyGroup(ctx context.Context, t *testing.T, repos map[string]*database.Repository[struct{}], opByID map[string]transaction.Operation, g transaction.Concurrency) {
	t.Helper()
	outcomes := make([]transaction.Outcome, len(g.Operations))
	errs := make([]error, len(g.Operations))
	var wg sync.WaitGroup
	for i, id := range g.Operations {
		op := opByID[id]
		wg.Add(1)
		go func(i int, op transaction.Operation) {
			defer wg.Done()
			outcomes[i], errs[i] = runOperation(ctx, repos[op.Table], op)
		}(i, op)
	}
	wg.Wait()

	got := map[transaction.Outcome]int{}
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrency group %s operation %s: %v", g.ID, g.Operations[i], err)
		}
		got[outcomes[i]]++
	}
	if !multisetEqual(got, g.Expect) {
		t.Errorf("concurrency group %s outcomes = %v, want %v", g.ID, got, g.Expect)
	}
}

// runOperation runs one operation op.EffectiveRepeat() times, returning the final
// outcome. Every repetition must produce the same outcome (the corpus's
// repeated-failure cases keep re-applying against a rolled-back row).
func runOperation(ctx context.Context, repo *database.Repository[struct{}], op transaction.Operation) (transaction.Outcome, error) {
	var outcome transaction.Outcome
	for i := 0; i < op.EffectiveRepeat(); i++ {
		o, err := runOnce(ctx, repo, op)
		if err != nil {
			return o, err
		}
		outcome = o
	}
	return outcome, nil
}

func runOnce(ctx context.Context, repo *database.Repository[struct{}], op transaction.Operation) (transaction.Outcome, error) {
	switch op.EffectiveBoundary() {
	case transaction.BoundaryNone:
		return callPrimitive(ctx, repo, op)
	case transaction.BoundaryTransaction:
		return runInBoundary(ctx, repo, op, false)
	case transaction.BoundaryNestedTransaction:
		return runInBoundary(ctx, repo, op, true)
	default:
		return "", fmt.Errorf("unknown boundary %q", op.Boundary)
	}
}

// runInBoundary runs the primitive inside a WithTx boundary. The boundary commits
// iff the primitive returned OutcomeApplied and no fault is injected; otherwise
// it rolls back (a conflict must not half-commit; an injected fault must release
// the connection). For a nested boundary the primitive runs in an INNER WithTx
// joined to the OUTER on the same pool (join-outer, no savepoints), and the
// outer governs the shared fate — so an outer rollback undoes the inner's write.
func runInBoundary(ctx context.Context, repo *database.Repository[struct{}], op transaction.Operation, nested bool) (transaction.Outcome, error) {
	var outcome transaction.Outcome
	pool := repo.Pool()

	finalize := func() error {
		if op.Fault == transaction.FaultCallbackError {
			return errInjectedFault
		}
		if outcome != transaction.OutcomeApplied {
			return errRollbackNonApplied
		}
		return nil
	}

	body := func(txCtx context.Context) error {
		o, err := callPrimitive(txCtx, repo, op)
		if err != nil {
			return err
		}
		outcome = o
		return finalize()
	}

	var err error
	if nested {
		err = database.WithTx(ctx, pool, func(outer context.Context) error {
			if innerErr := database.WithTx(outer, pool, func(inner context.Context) error {
				o, e := callPrimitive(inner, repo, op)
				if e != nil {
					return e
				}
				outcome = o
				return nil // inner "commit" is a no-op: it joined the outer transaction
			}); innerErr != nil {
				return innerErr
			}
			return finalize() // the OUTER decides commit/rollback for the joined write
		})
	} else {
		err = database.WithTx(ctx, pool, body)
	}

	if err != nil && !isExpectedRollback(err) {
		return outcome, err
	}
	return outcome, nil
}

func callPrimitive(ctx context.Context, repo *database.Repository[struct{}], op transaction.Operation) (transaction.Outcome, error) {
	a := op.Args
	switch op.Primitive {
	case transaction.PrimitiveCompareAndSet:
		return repo.CompareAndSet(ctx, a.KeyColumn, a.Key, a.Column, a.Expected, a.Next)
	case transaction.PrimitiveConsumeOnce:
		return repo.ConsumeOnce(ctx, a.KeyColumn, a.Key, a.ClaimGuard, a.Set)
	case transaction.PrimitiveRotate:
		return repo.Rotate(ctx, database.RotateSpec{
			KeyColumn:        a.KeyColumn,
			PredecessorKey:   a.PredecessorKey,
			StateColumn:      a.StateColumn,
			Expected:         a.Expected,
			Revoked:          a.Revoked,
			SuccessorColumns: a.SuccessorColumns,
			SuccessorValues:  a.SuccessorValues,
		})
	default:
		return "", fmt.Errorf("unknown primitive %q", op.Primitive)
	}
}

func scanNothing(pgx.Row) (struct{}, error) { return struct{}{}, nil }

func createTableSQL(tbl transaction.Table) string {
	cols := make([]string, 0, len(tbl.Columns))
	for _, col := range tbl.Columns {
		part := quoteIdent(col.Name) + " " + pgColumnType(col.Type)
		if col.PrimaryKey {
			part += " PRIMARY KEY"
		}
		if col.NotNull {
			part += " NOT NULL"
		}
		if col.Default != "" {
			part += " DEFAULT " + col.Default
		}
		cols = append(cols, part)
	}
	return "CREATE TABLE " + quoteIdent(tbl.Name) + " (" + strings.Join(cols, ", ") + ")"
}

// insertSQL renders a seed INSERT with deterministically ordered columns (sorted
// by name) so the statement is stable across runs.
func insertSQL(table string, row transaction.Row) (string, []any) {
	keys := make([]string, 0, len(row))
	for k := range row {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cols := make([]string, len(keys))
	placeholders := make([]string, len(keys))
	args := make([]any, len(keys))
	for i, k := range keys {
		cols[i] = quoteIdent(k)
		placeholders[i] = "$" + strconv.Itoa(i+1)
		args[i] = row[k]
	}
	return "INSERT INTO " + quoteIdent(table) + " (" + strings.Join(cols, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")", args
}

func pgColumnType(t transaction.ColumnType) string {
	switch t {
	case transaction.ColumnBoolean:
		return "BOOLEAN"
	case transaction.ColumnInteger:
		return "INTEGER"
	default:
		return "TEXT"
	}
}

func quoteIdent(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

func multisetEqual(got, want map[transaction.Outcome]int) bool {
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
