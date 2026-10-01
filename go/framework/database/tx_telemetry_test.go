package database

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/logger"
	"go.putnami.dev/protocol/features/spectest"
)

// observingDatasource is the fixed logical datasource name every observing pool
// in this file declares, so tests can assert the label without a live database.
const observingDatasource = "orders"

// observingPool builds a bare Pool (nil inner *pgxpool.Pool) wired to a memory
// log sink and a capturing TxObserver, plus a beginTx seam that hands out tx, so
// the transaction-boundary telemetry WithTx / UnitOfWork emit is assertable
// without a live database.
func observingPool(tx pgx.Tx) (*Pool, *logger.MemorySink, *[]TxObservation) {
	sink := logger.NewMemorySink()
	observed := &[]TxObservation{}
	pool := &Pool{
		log: logger.New("database", logger.LevelDebug, sink),
		cfg: PoolConfig{
			Database: observingDatasource,
			TxObserver: func(_ context.Context, obs TxObservation) {
				*observed = append(*observed, obs)
			},
		},
		beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil },
	}
	return pool, sink, observed
}

// metricNames returns the canonical metric names an observation emits.
func metricNames(obs TxObservation) []string {
	names := make([]string, 0, 4)
	for _, m := range obs.Metrics() {
		names = append(names, m.Name)
	}
	return names
}

func hasName(names []string, want string) bool {
	return slices.Contains(names, want)
}

// TestWithTx_Telemetry_Commit pins the committed-boundary metrics + span: a
// successful WithTx emits exactly one observation with outcome=committed, the
// datasource label, zero retries, no rollback cause, the sql.tx.duration /
// sql.tx.retries / sql.tx.outcome.committed metric names, and a "transaction
// committed" log entry.
func TestWithTx_Telemetry_Commit(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "transaction-finalization", "commits-after-a-nil-callback-result")
	pool, sink, observed := observingPool(&mockTx{})

	if err := WithTx(context.Background(), pool, func(_ context.Context) error { return nil }); err != nil {
		t.Fatalf("WithTx: %v", err)
	}

	if len(*observed) != 1 {
		t.Fatalf("expected 1 tx observation, got %d", len(*observed))
	}
	obs := (*observed)[0]
	if obs.Outcome != TxOutcomeCommitted {
		t.Errorf("outcome = %q, want committed", obs.Outcome)
	}
	if obs.Datasource != "orders" {
		t.Errorf("datasource = %q, want orders", obs.Datasource)
	}
	if obs.Retries != 0 {
		t.Errorf("retries = %d, want 0", obs.Retries)
	}
	if obs.RollbackCause != "" {
		t.Errorf("rollback cause = %q, want empty on commit", obs.RollbackCause)
	}

	names := metricNames(obs)
	for _, want := range []string{MetricTxDuration, MetricTxRetries, "sql.tx.outcome.committed"} {
		if !hasName(names, want) {
			t.Errorf("missing metric %q in %v", want, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, metricTxRollbackPrefix) {
			t.Errorf("commit must not emit a rollback metric, got %q", n)
		}
	}

	if last := sink.Last(); last == nil || last.Message != "transaction committed" {
		t.Fatalf("expected 'transaction committed' log entry, got %+v", last)
	}
	// The record reports the contract's outcome vocabulary (committed → success)
	// while the observation keeps the exported committed/rolled-back label.
	group := databaseGroupOf(t, sink.Last())
	wantField(t, group, "outcome", outcomeSuccess)
	wantField(t, group, "datasource", observingDatasource)
	wantField(t, group, "retries", 0)
	wantHasField(t, group, "durationMs")
	wantHasField(t, group, "durationUs")
}

// TestWithTx_Telemetry_Rollback pins the rolled-back-boundary metrics + span: a
// WithTx whose callback fails with a PostgreSQL serialization_failure emits
// outcome=rolled-back with the SQLSTATE as the classified cause, the
// sql.tx.outcome.rolled-back + sql.tx.rollback.40001 metric names, and a
// "transaction rolled back" Warn entry.
func TestWithTx_Telemetry_Rollback(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "secret-safe-telemetry", "rollback-telemetry-uses-fixed-outcome-vocabulary")
	pool, sink, observed := observingPool(&mockTx{})

	pgErr := &pgconn.PgError{Code: sqlStateSerializationFailure, Message: "could not serialize access due to concurrent update"}
	err := WithTx(context.Background(), pool, func(_ context.Context) error { return pgErr })
	if err != pgErr {
		t.Fatalf("WithTx should surface the callback error, got %v", err)
	}

	if len(*observed) != 1 {
		t.Fatalf("expected 1 tx observation, got %d", len(*observed))
	}
	obs := (*observed)[0]
	if obs.Outcome != TxOutcomeRolledBack {
		t.Errorf("outcome = %q, want rolled-back", obs.Outcome)
	}
	if obs.RollbackCause != sqlStateSerializationFailure {
		t.Errorf("rollback cause = %q, want %q", obs.RollbackCause, sqlStateSerializationFailure)
	}

	names := metricNames(obs)
	for _, want := range []string{"sql.tx.outcome.rolled-back", metricTxRollbackPrefix + sqlStateSerializationFailure} {
		if !hasName(names, want) {
			t.Errorf("missing metric %q in %v", want, names)
		}
	}

	if last := sink.Last(); last == nil || last.Message != "transaction rolled back" {
		t.Fatalf("expected 'transaction rolled back' log entry, got %+v", last)
	}
	group := databaseGroupOf(t, sink.Last())
	wantField(t, group, "outcome", outcomeFailure)
	wantField(t, group, "rollbackCause", sqlStateSerializationFailure)
	// A rollback record carries the classified cause ONLY — never a structured
	// or stringified error, which could embed a bound value.
	if hasErrorAttr(sink.Last()) {
		t.Error("rollback record carries an error attr, want the classified cause only")
	}
}

// TestWithTx_Telemetry_SecretSafety is the load-bearing secret-safety test: a
// transaction whose rollback error embeds a secret-looking bound value must
// classify the cause to a code and leak that secret into NO emitted metric name,
// observation field, or log line. It runs both a driver error (classified to its
// SQLSTATE) and a plain error (classified to "unknown"), proving the raw
// err.Error() message — which DOES carry the secret — is never used.
func TestWithTx_Telemetry_SecretSafety(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "secret-safe-telemetry", "rollback-telemetry-carries-no-secret-payload")
	const secret = "tok_SUPERSECRET_hunter2_pw"

	cases := []struct {
		name      string
		err       error
		wantCause string
	}{
		{
			name: "driver error classified to SQLSTATE",
			// A unique_violation whose message and detail embed the offending
			// bound value, exactly as pgx surfaces it (PgError.Error() renders the
			// Message, so the secret is genuinely reachable through err.Error()).
			err: &pgconn.PgError{
				Code:    sqlStateUniqueViolation,
				Message: fmt.Sprintf("duplicate key value violates unique constraint: Key (token)=(%s) already exists", secret),
				Detail:  fmt.Sprintf("Key (token)=(%s) already exists.", secret),
			},
			wantCause: sqlStateUniqueViolation,
		},
		{
			name:      "plain error classified to unknown",
			err:       fmt.Errorf("insert failed for token=%s", secret),
			wantCause: "unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Sanity: the raw error genuinely carries the secret, so the test is
			// real — the protection is the classification, not a benign error.
			if !strings.Contains(tc.err.Error(), secret) {
				t.Fatalf("test setup: raw error must carry the secret")
			}

			pool, sink, observed := observingPool(&mockTx{})
			_ = WithTx(context.Background(), pool, func(_ context.Context) error { return tc.err })

			if len(*observed) != 1 {
				t.Fatalf("expected 1 tx observation, got %d", len(*observed))
			}
			obs := (*observed)[0]
			if obs.RollbackCause != tc.wantCause {
				t.Errorf("rollback cause = %q, want %q", obs.RollbackCause, tc.wantCause)
			}

			// The secret must not appear anywhere in the emitted telemetry.
			assertNoSecret(t, secret, obs, sink)
		})
	}
}

// assertNoSecret scans every metric name, observation field, and log line for
// the secret and fails if it appears in any of them.
func assertNoSecret(t *testing.T, secret string, obs TxObservation, sink *logger.MemorySink) {
	t.Helper()

	names := metricNames(obs)
	surfaces := make([]string, 0, 3+len(names))
	surfaces = append(surfaces, obs.Datasource, string(obs.Outcome), obs.RollbackCause)
	surfaces = append(surfaces, names...)
	for _, s := range surfaces {
		if strings.Contains(s, secret) {
			t.Errorf("secret leaked into observation/metric surface %q", s)
		}
	}

	for _, entry := range sink.Entries {
		if strings.Contains(entry.Message, secret) {
			t.Errorf("secret leaked into log message %q", entry.Message)
		}
		for _, a := range entry.Attrs {
			// Value.String() renders a group attr's whole map (keys AND values),
			// so this scan reaches inside the nested "database" group too.
			if strings.Contains(a.Value.String(), secret) {
				t.Errorf("secret leaked into log attr %s=%v", a.Key, a.Value)
			}
		}
		if entry.Error != nil && strings.Contains(entry.Error.Message, secret) {
			t.Errorf("secret leaked into the structured error of %q", entry.Message)
		}
	}
}

// TestUnitOfWork_Telemetry_CommitAndRollback covers the UnitOfWork boundary:
// Commit emits committed per enrolled datasource, and FinalizeScope(err) emits
// rolled-back with the request outcome classified as a secret-free cause.
func TestUnitOfWork_Telemetry_CommitAndRollback(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "secret-safe-telemetry", "unit-of-work-telemetry-uses-fixed-outcome-vocabulary")
	t.Run("commit emits committed", func(t *testing.T) {
		pool, _, observed := observingPool(&mockTx{})
		u := newUnitOfWork(0)
		if _, err := u.enroll(context.Background(), pool); err != nil {
			t.Fatalf("enroll: %v", err)
		}
		if err := u.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if len(*observed) != 1 || (*observed)[0].Outcome != TxOutcomeCommitted {
			t.Fatalf("expected one committed observation, got %+v", *observed)
		}
		if (*observed)[0].Datasource != "orders" {
			t.Errorf("datasource = %q, want orders", (*observed)[0].Datasource)
		}
	})

	t.Run("finalize(err) emits rolled-back with secret-free cause", func(t *testing.T) {
		const secret = "pw_do_not_log_me"
		pool, sink, observed := observingPool(&mockTx{})
		u := newUnitOfWork(0)
		if _, err := u.enroll(context.Background(), pool); err != nil {
			t.Fatalf("enroll: %v", err)
		}

		outcome := &pgconn.PgError{
			Code:   sqlStateSerializationFailure,
			Detail: fmt.Sprintf("token=%s", secret),
		}
		if err := u.FinalizeScope(context.Background(), outcome); err != nil {
			t.Fatalf("FinalizeScope: %v", err)
		}

		if len(*observed) != 1 {
			t.Fatalf("expected 1 observation, got %d", len(*observed))
		}
		obs := (*observed)[0]
		if obs.Outcome != TxOutcomeRolledBack {
			t.Errorf("outcome = %q, want rolled-back", obs.Outcome)
		}
		if obs.RollbackCause != sqlStateSerializationFailure {
			t.Errorf("cause = %q, want %q", obs.RollbackCause, sqlStateSerializationFailure)
		}
		assertNoSecret(t, secret, obs, sink)
	})

	t.Run("no enrollment emits nothing", func(t *testing.T) {
		u := newUnitOfWork(0)
		if err := u.Commit(context.Background()); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		// A unit that never touched a datasource has no transaction, so there is
		// nothing to observe — this exercises the empty-emission guard.
	})
}
