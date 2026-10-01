package database

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
)

// The tests below pin the database boundary's log contract
// (protocols/logging/conformance): the nested "database" / "migration" groups
// with camelCase keys, the closed outcome vocabulary, the stable messages, the
// severity policy (a failed query is a WARNING that propagates, a failed
// migration is an ERROR), and the structured error field. They drive the real
// Pool / Migrator code and read the records back through a memory sink, so a
// call site that stops going through database_logging.go fails them.

// groupOf returns the named group of a record the way the JSON sink renders it:
// an attr-supplied group replaces the same-named field-bag key, so the attr wins
// when both are present.
func groupOf(t *testing.T, entry *logger.LogEntry, key string) map[string]any {
	t.Helper()
	if entry == nil {
		t.Fatalf("no record to read the %q group from", key)
	}
	for _, attr := range entry.Attrs {
		if attr.Key != key {
			continue
		}
		group, ok := attr.Value.Any().(map[string]any)
		if !ok {
			t.Fatalf("%q attr is not a map: %T", key, attr.Value.Any())
		}
		return group
	}
	if group, ok := entry.Context[key].(map[string]any); ok {
		return group
	}
	t.Fatalf("no %q group on record %q", key, entry.Message)
	return nil
}

// databaseGroupOf returns a record's "database" group.
func databaseGroupOf(t *testing.T, entry *logger.LogEntry) map[string]any {
	t.Helper()
	return groupOf(t, entry, "database")
}

// migrationGroupOf returns a record's "migration" group.
func migrationGroupOf(t *testing.T, entry *logger.LogEntry) map[string]any {
	t.Helper()
	return groupOf(t, entry, "migration")
}

// wantField asserts a group carries key with the expected value.
func wantField(t *testing.T, group map[string]any, key string, want any) {
	t.Helper()
	got, ok := group[key]
	if !ok {
		t.Errorf("missing %q in group %v", key, group)
		return
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("group[%q] = %v, want %v", key, got, want)
	}
}

// wantHasField asserts a group carries key with any value (durations vary).
func wantHasField(t *testing.T, group map[string]any, key string) {
	t.Helper()
	if _, ok := group[key]; !ok {
		t.Errorf("missing %q in group %v", key, group)
	}
}

// errorInfoAttrOf returns the structured error carried as an attr — the Warn
// path, which cannot use the ErrorCtx error parameter.
func errorInfoAttrOf(t *testing.T, entry *logger.LogEntry) *logger.ErrorInfo {
	t.Helper()
	for _, attr := range entry.Attrs {
		if attr.Key != "error" {
			continue
		}
		info, ok := attr.Value.Any().(*logger.ErrorInfo)
		if !ok {
			t.Fatalf("error attr is not *logger.ErrorInfo: %T", attr.Value.Any())
		}
		return info
	}
	t.Fatalf("no structured error attr on record %q", entry.Message)
	return nil
}

// hasErrorAttr reports whether the record carries an "error" attr at all.
func hasErrorAttr(entry *logger.LogEntry) bool {
	_, ok := attrByKey(entry, "error")
	return ok
}

// recordsFor returns every entry emitted by loggerName with the given message.
func recordsFor(sink *logger.MemorySink, loggerName, message string) []logger.LogEntry {
	var out []logger.LogEntry
	for _, entry := range sink.Entries {
		if entry.Logger == loggerName && entry.Message == message {
			out = append(out, entry)
		}
	}
	return out
}

// oneRecordFor returns the single entry emitted by loggerName with message,
// failing when the count is not exactly one.
func oneRecordFor(t *testing.T, sink *logger.MemorySink, loggerName, message string) logger.LogEntry {
	t.Helper()
	found := recordsFor(sink, loggerName, message)
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 %q record from %q, got %d (all:%s)",
			message, loggerName, len(found), allRecordsOf(sink))
	}
	return found[0]
}

// allRecordsOf renders the captured records for failure messages.
func allRecordsOf(sink *logger.MemorySink) string {
	var b strings.Builder
	for _, entry := range sink.Entries {
		b.WriteString("\n  [" + entry.Level.String() + "] " + entry.Logger + " " + entry.Message)
	}
	return b.String()
}

// TestQueryRecord_PinnedNameAndVocabulary proves the query records carry the
// pinned "database" logger name and the contract's operation vocabulary — where
// the exported QueryOp label (query_row, the observer/metric label) renders as
// the camelCase queryRow on the record — plus a datasource on a pool that
// declares none.
func TestQueryRecord_PinnedNameAndVocabulary(t *testing.T) {
	sink := logger.NewMemorySink()
	// A root logger with NO name, so the derived name is exactly the pinned one.
	pool := &Pool{log: databaseLoggerFrom(logger.New("", logger.LevelDebug, sink)), cfg: PoolConfig{}}
	ctx := ctxWithQuerier(pool, &mockQuerier{})

	_ = pool.QueryRow(ctx, "SELECT 1").Scan()

	entry := oneRecordFor(t, sink, "database", "query executed")
	group := databaseGroupOf(t, &entry)
	wantField(t, group, "operation", "queryRow")
	// The exported label is unchanged: only the log field is camelCase.
	if QueryOpQueryRow != "query_row" {
		t.Errorf("QueryOpQueryRow = %q, want the unchanged observer label query_row", QueryOpQueryRow)
	}
	// A pool with no declared datasource still labels the record, matching the
	// TypeScript adapter's `datasource ?? 'default'`.
	wantField(t, group, "datasource", "default")
	wantField(t, group, "outcome", outcomeSuccess)
}

// TestQueryRecord_FailureIsWarnWithStructuredError pins the severity policy: a
// failed query is a WARNING carrying the structured error (the pool propagates
// the error; the boundary owns ERROR), never a stringified error field.
func TestQueryRecord_FailureIsWarnWithStructuredError(t *testing.T) {
	pool, sink := debugPoolWithSink(PoolConfig{Database: "orders"})
	wantErr := fmt.Errorf("relation \"missing\" does not exist")
	q := &mockQuerier{
		execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
			return pgconn.CommandTag{}, wantErr
		},
	}
	ctx := ctxWithQuerier(pool, q)

	_, _ = pool.Exec(ctx, "SELECT * FROM missing")

	entry := oneRecordFor(t, sink, "database", "query failed")
	if entry.Level != logger.LevelWarn {
		t.Errorf("level = %v, want Warn (the pool reports and propagates; the boundary owns ERROR)", entry.Level)
	}
	group := databaseGroupOf(t, &entry)
	wantField(t, group, "operation", "exec")
	wantField(t, group, "datasource", "orders")
	wantField(t, group, "outcome", outcomeFailure)
	wantHasField(t, group, "durationMs")
	wantHasField(t, group, "durationUs")
	if info := errorInfoAttrOf(t, &entry); info.Message != wantErr.Error() {
		t.Errorf("structured error message = %q, want %q", info.Message, wantErr.Error())
	}
	// The failure text must not be interpolated into the message.
	if strings.Contains(entry.Message, "missing") {
		t.Errorf("message %q interpolates the error text", entry.Message)
	}
}

// TestTransactionRecord_GroupIsClosedAndCauseAlwaysPresent pins the transaction
// record's exact field set (the contract allows no extra key there) and that a
// rollback ALWAYS carries a rollbackCause — matching the TypeScript adapter's
// `cause || 'unknown'` — so a dashboard never sees the field missing.
func TestTransactionRecord_GroupIsClosedAndCauseAlwaysPresent(t *testing.T) {
	sink := logger.NewMemorySink()
	log := databaseLoggerFrom(logger.New("", logger.LevelDebug, sink))

	logTransaction(context.Background(), log, TxObservation{
		Datasource: "orders", Outcome: TxOutcomeCommitted, Duration: 2 * time.Millisecond,
	})
	// A rollback with NO classified cause (no call site does this today; the
	// record must still carry the field).
	logTransaction(context.Background(), log, TxObservation{
		Datasource: "orders", Outcome: TxOutcomeRolledBack, Duration: time.Millisecond,
	})

	commit := oneRecordFor(t, sink, "database", "transaction committed")
	commitGroup := databaseGroupOf(t, &commit)
	if len(commitGroup) != 5 {
		t.Errorf("commit group = %v, want exactly datasource/outcome/durationMs/durationUs/retries", commitGroup)
	}
	wantField(t, commitGroup, "outcome", outcomeSuccess)
	if _, ok := commitGroup["rollbackCause"]; ok {
		t.Error("commit record carries a rollbackCause")
	}

	rollback := oneRecordFor(t, sink, "database", "transaction rolled back")
	rollbackGroup := databaseGroupOf(t, &rollback)
	wantField(t, rollbackGroup, "outcome", outcomeFailure)
	wantField(t, rollbackGroup, "rollbackCause", "unknown")
	if len(rollbackGroup) != 6 {
		t.Errorf("rollback group = %v, want exactly the 5 commit fields plus rollbackCause", rollbackGroup)
	}
}

// TestMigrationRecord_AppliedTerminalRecord proves each applied migration emits
// exactly one INFO terminal record under the pinned database.migration logger,
// with the migration group the contract pins.
func TestMigrationRecord_AppliedTerminalRecord(t *testing.T) {
	db, _ := newFakeMigratorDB(t)
	sink := logger.NewMemorySink()
	m := NewMigrator(db, MigrationConfig{
		Datasource: "primary",
		Definitions: []Definition{
			{Name: "iam/001", SQL: "CREATE TABLE a (id int)"},
			{Name: "iam/002", SQL: "CREATE TABLE b (id int)"},
		},
	})
	m.log = migrationLoggerFrom(logger.New("", logger.LevelDebug, sink))

	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	applied := recordsFor(sink, "database.migration", "migration applied")
	if len(applied) != 2 {
		t.Fatalf("expected one terminal record per migration, got %d (all:%s)", len(applied), allRecordsOf(sink))
	}
	for i, want := range []string{"iam/001", "iam/002"} {
		entry := applied[i]
		if entry.Level != logger.LevelInfo {
			t.Errorf("record %d level = %v, want Info", i, entry.Level)
		}
		group := migrationGroupOf(t, &entry)
		wantField(t, group, "name", want)
		wantField(t, group, "datasource", "primary")
		wantField(t, group, "outcome", outcomeSuccess)
		wantHasField(t, group, "durationMs")
	}
	// A success emits no failure record.
	if got := recordsFor(sink, "database.migration", "migration failed"); len(got) != 0 {
		t.Errorf("successful run emitted %d failure records", len(got))
	}
}

// TestMigrationRecord_FailedTerminalRecord proves a failing migration emits one
// ERROR terminal record with the structured cause AND still returns the error,
// so the run aborts. It also pins that the failure is reported once, not once
// per bookkeeping step.
func TestMigrationRecord_FailedTerminalRecord(t *testing.T) {
	db, store := newFakeMigratorDB(t)
	const body = "CREATE TABLE broken ("
	store.failBody(body, fmt.Errorf("syntax error at end of input"))

	sink := logger.NewMemorySink()
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: body}},
	})
	m.log = migrationLoggerFrom(logger.New("", logger.LevelDebug, sink))

	if _, err := m.Up(context.Background()); err == nil {
		t.Fatal("Up must still return the apply error after logging it")
	}

	entry := oneRecordFor(t, sink, "database.migration", "migration failed")
	if entry.Level != logger.LevelError {
		t.Errorf("level = %v, want Error", entry.Level)
	}
	group := migrationGroupOf(t, &entry)
	wantField(t, group, "name", "iam/001")
	wantField(t, group, "datasource", "primary")
	wantField(t, group, "outcome", outcomeFailure)
	wantHasField(t, group, "durationMs")
	if entry.Error == nil || !strings.Contains(entry.Error.Message, "syntax error") {
		t.Errorf("structured error = %+v, want the apply cause", entry.Error)
	}
	if strings.Contains(entry.Message, "syntax") || strings.Contains(entry.Message, "iam/001") {
		t.Errorf("message %q interpolates the error or the identifier", entry.Message)
	}
	// No success record for a failed migration.
	if got := recordsFor(sink, "database.migration", "migration applied"); len(got) != 0 {
		t.Errorf("failed run emitted %d applied records", len(got))
	}
}

// TestMigrationRecord_BookkeepingRecordsUseTheGroup proves the migrator's
// debug/bookkeeping records moved their identifiers into the camelCase migration
// group under the same pinned logger name (no flat, snake_case leftovers).
func TestMigrationRecord_BookkeepingRecordsUseTheGroup(t *testing.T) {
	db, _ := newFakeMigratorDB(t)
	sink := logger.NewMemorySink()
	m := NewMigrator(db, MigrationConfig{
		Datasource:  "primary",
		Definitions: []Definition{{Name: "iam/001", SQL: "CREATE TABLE a (id int)"}},
	})
	m.log = migrationLoggerFrom(logger.New("", logger.LevelDebug, sink))

	if _, err := m.Up(context.Background()); err != nil {
		t.Fatalf("Up: %v", err)
	}

	executing := oneRecordFor(t, sink, "database.migration", "executing migration")
	group := migrationGroupOf(t, &executing)
	wantField(t, group, "name", "iam/001")
	wantField(t, group, "datasource", "primary")
	wantHasField(t, group, "hash")

	scan := oneRecordFor(t, sink, "database.migration", "migration scan completed")
	scanGroup := migrationGroupOf(t, &scan)
	wantField(t, scanGroup, "count", 1)
	wantField(t, scanGroup, "datasource", "primary")
}

// TestSQLRunner_SummaryRecordsUseTheGroup proves the runner's aggregate summary
// records carry the migration group under the pinned logger name.
func TestSQLRunner_SummaryRecordsUseTheGroup(t *testing.T) {
	db, _ := newFakeMigratorDB(t)
	sink := logger.NewMemorySink()
	reg := migration.NewRegistry()
	if err := reg.AddSource(NewSQLSource("iam", Datasource{Name: "primary"}, fstest.MapFS{
		"001_init.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE a (id int)")},
	})); err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	runner := newSQLRunner(SQLRunnerOptions{
		Registry:  reg,
		OpenDB:    func() (*stdsql.DB, error) { return db, nil },
		AutoApply: true,
	})
	runner.log = migrationLoggerFrom(logger.New("", logger.LevelDebug, sink))

	if _, err := runner.Apply(context.Background(), migration.ApplyOpts{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	entry := oneRecordFor(t, sink, "database.migration", "migrations applied")
	group := migrationGroupOf(t, &entry)
	wantField(t, group, "count", 1)
	wantField(t, group, "datasources", 1)
}
