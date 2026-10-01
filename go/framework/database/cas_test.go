package database

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/protocol/transaction"
)

// existsRow returns a mock QueryRow func that scans a single bool = value, used
// to stand in for the "SELECT EXISTS(...)" disambiguation check.
func existsRow(value bool) func(context.Context, string, ...any) pgx.Row {
	return func(_ context.Context, _ string, _ ...any) pgx.Row {
		return &mockRow{scanFn: func(dest ...any) error {
			*dest[0].(*bool) = value
			return nil
		}}
	}
}

func execTag(tag string) func(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
		return pgconn.NewCommandTag(tag), nil
	}
}

// --- Classifier ---

func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		want    transaction.Outcome
		wantOK  bool
		retryab bool
	}{
		{"serialization_failure", &pgconn.PgError{Code: "40001"}, transaction.OutcomeRetryableSerializationFailure, true, true},
		{"deadlock_detected", &pgconn.PgError{Code: "40P01"}, transaction.OutcomeRetryableSerializationFailure, true, true},
		{"unique_violation", &pgconn.PgError{Code: "23505"}, transaction.OutcomeAlreadyConsumedConflict, true, false},
		{"unrelated_pg_error", &pgconn.PgError{Code: "42P01"}, "", false, false},
		{"plain_error", errors.New("connection refused"), "", false, false},
		{"nil", nil, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ClassifyOutcome(tt.err)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("ClassifyOutcome(%v) = (%q, %v), want (%q, %v)", tt.err, got, ok, tt.want, tt.wantOK)
			}
			if ok && got.Retryable() != tt.retryab {
				t.Errorf("outcome %q Retryable() = %v, want %v", got, got.Retryable(), tt.retryab)
			}
		})
	}
}

// TestClassifyOutcome_WrappedError proves the classifier walks the error chain,
// so a pg error wrapped by errors.Wrapf is still recognized.
func TestClassifyOutcome_WrappedError(t *testing.T) {
	wrapped := wrapQueryError(context.Background(), &pgconn.PgError{Code: "40001"}, "update")
	got, ok := ClassifyOutcome(wrapped)
	if !ok || got != transaction.OutcomeRetryableSerializationFailure {
		t.Fatalf("ClassifyOutcome(wrapped) = (%q, %v), want (retryable, true)", got, ok)
	}
}

func TestIsSerializationFailureAndDeadlock(t *testing.T) {
	if !IsSerializationFailure(&pgconn.PgError{Code: "40001"}) {
		t.Error("40001 should be a serialization failure")
	}
	if IsSerializationFailure(&pgconn.PgError{Code: "40P01"}) {
		t.Error("40P01 is a deadlock, not a serialization failure")
	}
	if !IsDeadlock(&pgconn.PgError{Code: "40P01"}) {
		t.Error("40P01 should be a deadlock")
	}
	if IsDeadlock(&pgconn.PgError{Code: "23505"}) {
		t.Error("23505 is not a deadlock")
	}
	if IsSerializationFailure(errors.New("boom")) || IsDeadlock(nil) {
		t.Error("non-pg errors must not classify as serialization/deadlock")
	}
}

// --- CompareAndSet ---

func TestCompareAndSet(t *testing.T) {
	tests := []struct {
		name     string
		affected string // command tag the UPDATE returns
		exists   bool   // existence check result (only consulted when affected=0)
		want     transaction.Outcome
	}{
		{"matching expected -> applied", "UPDATE 1", false, transaction.OutcomeApplied},
		{"mismatched -> already-consumed-conflict", "UPDATE 0", true, transaction.OutcomeAlreadyConsumedConflict},
		{"absent key -> not-found", "UPDATE 0", false, transaction.OutcomeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := &mockQuerier{
				execFn:     execTag(tt.affected),
				queryRowFn: existsRow(tt.exists),
			}
			repo, ctx := newTestRepo(q)
			got, err := repo.CompareAndSet(ctx, "id", 7, "status", "pending", "done")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("CompareAndSet = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCompareAndSet_InvalidColumnPanics(t *testing.T) {
	repo := NewRepository[testEntity](nil, "users", scanTestEntity)
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for invalid column name (SQL injection guard)")
		}
	}()
	_, _ = repo.CompareAndSet(context.Background(), "id", 1, "status; DROP TABLE users", "a", "b")
}

// --- ConsumeOnce ---

func TestConsumeOnce(t *testing.T) {
	tests := []struct {
		name     string
		affected string
		exists   bool
		want     transaction.Outcome
	}{
		{"fresh -> applied", "UPDATE 1", false, transaction.OutcomeApplied},
		{"already-consumed -> conflict", "UPDATE 0", true, transaction.OutcomeAlreadyConsumedConflict},
		{"missing -> not-found", "UPDATE 0", false, transaction.OutcomeNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenSQL string
			q := &mockQuerier{
				execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
					seenSQL = sql
					return pgconn.NewCommandTag(tt.affected), nil
				},
				queryRowFn: existsRow(tt.exists),
			}
			repo, ctx := newTestRepo(q)
			got, err := repo.ConsumeOnce(ctx, "code", "abc", "consumed = false", "consumed = true")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ConsumeOnce = %q, want %q", got, tt.want)
			}
			// The claim guard and the key match are both in the UPDATE predicate,
			// so the transition is the atomic guard against a double consume.
			if !strings.Contains(seenSQL, "consumed = false") || !strings.Contains(seenSQL, `"code" = $1`) {
				t.Errorf("ConsumeOnce SQL missing guard or key match: %q", seenSQL)
			}
		})
	}
}

// TestConsumeOnce_ParamNumbering pins the positional-parameter numbering: set and
// guard args come first ($1..$N), then the key is appended as the final param.
func TestConsumeOnce_ParamNumbering(t *testing.T) {
	var seenSQL string
	var seenArgs []any
	q := &mockQuerier{
		execFn: func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			seenSQL, seenArgs = sql, args
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	repo, ctx := newTestRepo(q)
	got, err := repo.ConsumeOnce(ctx, "id", 99, "status = $2", "status = $1, consumed_at = now()", "done", "pending")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != transaction.OutcomeApplied {
		t.Fatalf("got %q, want applied", got)
	}
	if !strings.Contains(seenSQL, `"id" = $3`) {
		t.Errorf("key should be bound as $3 (after 2 caller args): %q", seenSQL)
	}
	if len(seenArgs) != 3 || seenArgs[0] != "done" || seenArgs[1] != "pending" || seenArgs[2] != 99 {
		t.Errorf("args = %v, want [done pending 99]", seenArgs)
	}
}

// --- Serialization failure surfaced from a helper ---

func TestCompareAndSet_SerializationFailure(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, &pgconn.PgError{Code: "40001"}
		},
	}
	repo, ctx := newTestRepo(q)
	got, err := repo.CompareAndSet(ctx, "id", 1, "status", "a", "b")
	if err != nil {
		t.Fatalf("serialization failure must be a typed outcome, not a raw error: %v", err)
	}
	if got != transaction.OutcomeRetryableSerializationFailure || !got.Retryable() {
		t.Errorf("got %q (retryable=%v), want retryable-serialization-failure", got, got.Retryable())
	}
}

// TestCompareAndSet_GenuineErrorPassthrough proves a genuine I/O error is returned
// raw (not masked as a business outcome).
func TestCompareAndSet_GenuineErrorPassthrough(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, errors.New("connection refused")
		},
	}
	repo, ctx := newTestRepo(q)
	got, err := repo.CompareAndSet(ctx, "id", 1, "status", "a", "b")
	if err == nil {
		t.Fatal("expected a raw error for a genuine I/O failure")
	}
	if got != "" {
		t.Errorf("expected empty outcome on error, got %q", got)
	}
}

// --- UpdateWhere ---

func TestUpdateWhere(t *testing.T) {
	q := &mockQuerier{execFn: execTag("UPDATE 3")}
	repo, ctx := newTestRepo(q)
	n, err := repo.UpdateWhere(ctx, "status = $1", "org = $2", "archived", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 3 {
		t.Errorf("UpdateWhere affected = %d, want 3", n)
	}
}

func TestUpdateWhere_Error(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, errors.New("boom")
		},
	}
	repo, ctx := newTestRepo(q)
	if _, err := repo.UpdateWhere(ctx, "a = $1", "b = $2", 1, 2); err == nil {
		t.Fatal("expected error")
	}
}

// --- Rotate ---

// TestRotate_AppliedInsideTx runs Rotate inside WithTx and asserts the revoke and
// the successor insert are BOTH issued on the one transaction begun by WithTx
// (atomic predecessor→successor), and that the successful path commits once.
func TestRotate_AppliedInsideTx(t *testing.T) {
	var stmts []string
	commits := 0
	mq := &mockQuerier{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			stmts = append(stmts, strings.TrimSpace(sql))
			if strings.HasPrefix(strings.TrimSpace(sql), "UPDATE") {
				return pgconn.NewCommandTag("UPDATE 1"), nil // revoke applied
			}
			return pgconn.NewCommandTag("INSERT 0 1"), nil
		},
	}
	tx := &mockTx{
		mockQuerier: *mq,
		commitFn:    func(_ context.Context) error { commits++; return nil },
	}
	// A single tx instance is begun by WithTx and shared by both writes.
	pool := &Pool{beginTx: func(_ context.Context) (pgx.Tx, error) { return tx, nil }}
	repo := NewRepository[testEntity](pool, "tokens", scanTestEntity)

	var outcome transaction.Outcome
	err := WithTx(context.Background(), pool, func(ctx context.Context) error {
		var e error
		outcome, e = repo.Rotate(ctx, RotateSpec{
			KeyColumn:        "id",
			PredecessorKey:   1,
			StateColumn:      "revoked",
			Expected:         false,
			Revoked:          true,
			SuccessorColumns: []string{"id", "secret"},
			SuccessorValues:  []any{2, "new-secret"},
		})
		return e
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	if outcome != transaction.OutcomeApplied {
		t.Fatalf("Rotate outcome = %q, want applied", outcome)
	}
	if len(stmts) != 2 {
		t.Fatalf("expected exactly 2 writes on the shared tx, got %d: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "UPDATE") {
		t.Errorf("first write should be the revoke UPDATE, got %q", stmts[0])
	}
	if !strings.HasPrefix(stmts[1], "INSERT") || !strings.Contains(stmts[1], `"tokens"`) {
		t.Errorf("second write should be the successor INSERT, got %q", stmts[1])
	}
	if commits != 1 {
		t.Errorf("expected the shared tx to commit exactly once, got %d", commits)
	}
}

// TestRotate_PredecessorConflictSkipsInsert proves a non-applied revoke
// short-circuits: the successor is NOT inserted when the predecessor was already
// rotated (conflict), so rotation never installs a successor for a stale
// predecessor.
func TestRotate_PredecessorConflictSkipsInsert(t *testing.T) {
	var inserts int
	q := &mockQuerier{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			if strings.HasPrefix(strings.TrimSpace(sql), "INSERT") {
				inserts++
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			}
			return pgconn.NewCommandTag("UPDATE 0"), nil // revoke matched nothing
		},
		queryRowFn: existsRow(true), // predecessor exists but already revoked
	}
	repo, ctx := newTestRepo(q)
	got, err := repo.Rotate(ctx, RotateSpec{
		KeyColumn:        "id",
		PredecessorKey:   1,
		StateColumn:      "revoked",
		Expected:         false,
		Revoked:          true,
		SuccessorColumns: []string{"id"},
		SuccessorValues:  []any{2},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != transaction.OutcomeAlreadyConsumedConflict {
		t.Errorf("Rotate = %q, want already-consumed-conflict", got)
	}
	if inserts != 0 {
		t.Errorf("successor must not be inserted when the revoke did not apply (got %d inserts)", inserts)
	}
}

// TestRotate_SuccessorUniqueViolation maps a unique_violation on the successor
// insert to a conflict outcome rather than a raw error.
func TestRotate_SuccessorUniqueViolation(t *testing.T) {
	q := &mockQuerier{
		execFn: func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
			if strings.HasPrefix(strings.TrimSpace(sql), "INSERT") {
				return pgconn.CommandTag{}, &pgconn.PgError{Code: "23505"}
			}
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}
	repo, ctx := newTestRepo(q)
	got, err := repo.Rotate(ctx, RotateSpec{
		KeyColumn:        "id",
		PredecessorKey:   1,
		StateColumn:      "revoked",
		Expected:         false,
		Revoked:          true,
		SuccessorColumns: []string{"id"},
		SuccessorValues:  []any{2},
	})
	if err != nil {
		t.Fatalf("unique violation should be a typed outcome, not a raw error: %v", err)
	}
	if got != transaction.OutcomeAlreadyConsumedConflict {
		t.Errorf("Rotate = %q, want already-consumed-conflict", got)
	}
}
