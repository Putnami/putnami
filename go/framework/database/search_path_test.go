package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	perrors "go.putnami.dev/errors"
	"go.putnami.dev/protocol/features/spectest"
)

// recordingExecer stands in for the *pgx.Conn PrepareConn receives: it records
// every statement (with its arguments) and can fail on demand.
type recordingExecer struct {
	statements []string
	args       [][]any
	err        error
}

func (r *recordingExecer) Exec(_ context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	r.statements = append(r.statements, sql)
	r.args = append(r.args, arguments)
	if r.err != nil {
		return pgconn.CommandTag{}, r.err
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

// TestPrepareConnSearchPath_UnchangedIssuesNoStatement proves a connection
// whose marker already equals the datasource's request is handed out without a
// round trip — the common case on a warm shared pool.
func TestPrepareConnSearchPath_UnchangedIssuesNoStatement(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "an-unchanged-search-path-issues-no-statement")
	conn := &recordingExecer{}
	marker := map[string]any{searchPathMarkerKey: "iam"}

	keep, err := prepareConnSearchPath(withSearchPath(context.Background(), "iam"), conn, marker)
	if err != nil || !keep {
		t.Fatalf("(keep, err) = (%v, %v), want (true, nil)", keep, err)
	}
	if len(conn.statements) != 0 {
		t.Errorf("statements = %q, want none for an unchanged search_path", conn.statements)
	}
	if marker[searchPathMarkerKey] != "iam" {
		t.Errorf("marker = %v, want iam untouched", marker[searchPathMarkerKey])
	}

	// A fresh connection (no marker) acquired by a server-default datasource
	// (empty request) is equally a no-op: nothing to set, nothing to reset.
	fresh := &recordingExecer{}
	keep, err = prepareConnSearchPath(withSearchPath(context.Background(), ""), fresh, map[string]any{})
	if err != nil || !keep || len(fresh.statements) != 0 {
		t.Errorf("fresh connection, empty request: (keep, err, statements) = (%v, %v, %q), want (true, nil, none)", keep, err, fresh.statements)
	}
}

// TestPrepareConnSearchPath_ChangedSetsOnceAndMarks proves a differing request
// issues exactly one parameterized set_config (session-level, the value bound
// as a parameter so a comma list needs no quoting) and records the new value
// on the connection.
func TestPrepareConnSearchPath_ChangedSetsOnceAndMarks(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "a-changed-search-path-is-set-before-hand-out")
	for _, tc := range []struct {
		name   string
		marker string
		want   string
	}{
		{name: "fresh connection", marker: "", want: "iam"},
		{name: "connection last used by another datasource", marker: "billing", want: "iam"},
		{name: "comma list", marker: "iam", want: "tenant_42, public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := &recordingExecer{}
			marker := map[string]any{}
			if tc.marker != "" {
				marker[searchPathMarkerKey] = tc.marker
			}
			keep, err := prepareConnSearchPath(withSearchPath(context.Background(), tc.want), conn, marker)
			if err != nil || !keep {
				t.Fatalf("(keep, err) = (%v, %v), want (true, nil)", keep, err)
			}
			if len(conn.statements) != 1 || conn.statements[0] != "SELECT set_config('search_path', $1, false)" {
				t.Fatalf("statements = %q, want exactly one session-level set_config", conn.statements)
			}
			if len(conn.args[0]) != 1 || conn.args[0][0] != tc.want {
				t.Errorf("set_config args = %v, want [%q]", conn.args[0], tc.want)
			}
			if marker[searchPathMarkerKey] != tc.want {
				t.Errorf("marker = %v, want %q recorded after the set", marker[searchPathMarkerKey], tc.want)
			}
		})
	}
}

// TestPrepareConnSearchPath_EmptyRequestResets proves a server-default
// datasource acquiring a connection another datasource left on its schema
// resets search_path and clears the marker, so it never inherits that schema.
func TestPrepareConnSearchPath_EmptyRequestResets(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "an-empty-request-resets-a-set-search-path")
	conn := &recordingExecer{}
	marker := map[string]any{searchPathMarkerKey: "billing"}

	keep, err := prepareConnSearchPath(withSearchPath(context.Background(), ""), conn, marker)
	if err != nil || !keep {
		t.Fatalf("(keep, err) = (%v, %v), want (true, nil)", keep, err)
	}
	if len(conn.statements) != 1 || conn.statements[0] != "RESET search_path" {
		t.Fatalf("statements = %q, want exactly RESET search_path", conn.statements)
	}
	if _, present := marker[searchPathMarkerKey]; present {
		t.Errorf("marker = %v, want cleared after RESET", marker[searchPathMarkerKey])
	}
}

// TestPrepareConnSearchPath_MissingRequestFailsClosed proves an acquire that
// carries no datasource request — one that bypassed every *Pool surface —
// fails with a db.connection error, issues no statement, and returns the
// connection to the pool untouched (keep=true).
func TestPrepareConnSearchPath_MissingRequestFailsClosed(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "a-missing-request-fails-closed")
	conn := &recordingExecer{}
	marker := map[string]any{searchPathMarkerKey: "iam"}

	keep, err := prepareConnSearchPath(context.Background(), conn, marker)
	if err == nil {
		t.Fatal("expected an error for an acquire without a datasource request")
	}
	if !keep {
		t.Error("keep = false, want true: the connection is fine, only the acquire is not")
	}
	if !perrors.Is(err, CodeConnection) || !strings.Contains(err.Error(), "acquire through *database.Pool") {
		t.Errorf("error = %v, want a %s error pointing at *database.Pool", err, CodeConnection)
	}
	if len(conn.statements) != 0 {
		t.Errorf("statements = %q, want none", conn.statements)
	}
	if marker[searchPathMarkerKey] != "iam" {
		t.Errorf("marker = %v, want untouched", marker[searchPathMarkerKey])
	}
}

// TestPrepareConnSearchPath_FailedSetDestroysTheConnection proves a failing
// set or reset returns (false, error): pgxpool destroys the connection and the
// query fails, and the marker is not updated to a value the session does not
// have.
func TestPrepareConnSearchPath_FailedSetDestroysTheConnection(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "acquire-time-search-path", "a-failed-set-destroys-the-connection")
	boom := errors.New("connection reset by peer")

	conn := &recordingExecer{err: boom}
	marker := map[string]any{searchPathMarkerKey: "billing"}
	keep, err := prepareConnSearchPath(withSearchPath(context.Background(), "iam"), conn, marker)
	if keep || !errors.Is(err, boom) || !perrors.Is(err, CodeConnection) {
		t.Errorf("failed set: (keep, err) = (%v, %v), want (false, a %s error wrapping the cause)", keep, err, CodeConnection)
	}
	if marker[searchPathMarkerKey] != "billing" {
		t.Errorf("marker after a failed set = %v, want the previous value", marker[searchPathMarkerKey])
	}

	conn = &recordingExecer{err: boom}
	marker = map[string]any{searchPathMarkerKey: "billing"}
	keep, err = prepareConnSearchPath(withSearchPath(context.Background(), ""), conn, marker)
	if keep || !errors.Is(err, boom) {
		t.Errorf("failed reset: (keep, err) = (%v, %v), want (false, the cause)", keep, err)
	}
	if marker[searchPathMarkerKey] != "billing" {
		t.Errorf("marker after a failed reset = %v, want the previous value", marker[searchPathMarkerKey])
	}
}

// TestWithSearchPath_RequestIsPresentEvenWhenEmpty pins the context contract
// PrepareConn relies on: a marked context always carries a request, an empty
// one included (trimmed), and an unmarked context carries none.
func TestWithSearchPath_RequestIsPresentEvenWhenEmpty(t *testing.T) {
	if _, ok := searchPathRequestFrom(context.Background()); ok {
		t.Error("an unmarked context must carry no request")
	}
	if got, ok := searchPathRequestFrom(withSearchPath(context.Background(), "  ")); !ok || got != "" {
		t.Errorf("empty request = (%q, %v), want present and empty", got, ok)
	}
	if got, ok := searchPathRequestFrom(withSearchPath(context.Background(), " iam ")); !ok || got != "iam" {
		t.Errorf("request = (%q, %v), want (iam, true)", got, ok)
	}
	// The innermost datasource wins when contexts nest.
	nested := withSearchPath(withSearchPath(context.Background(), "core"), "billing")
	if got, _ := searchPathRequestFrom(nested); got != "billing" {
		t.Errorf("nested request = %q, want billing", got)
	}
}

// recordingConnector stands in for pgx's pool connector and records the
// context database/sql handed it.
type recordingConnector struct {
	ctx context.Context
}

func (c *recordingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	c.ctx = ctx
	return nil, errors.New("no connection in this test")
}

func (c *recordingConnector) Driver() driver.Driver { return nil }

// TestDatasourceConnector_MarksEveryConnect proves the stdlib connector the
// migration runner and ApplyToPool go through marks each Connect context with
// the pool's search_path before delegating, whatever context database/sql
// passes (an operation's, or its own background opener's).
func TestDatasourceConnector_MarksEveryConnect(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "datasource-bound-acquire", "the-stdlib-connector-marks-every-connect")
	inner := &recordingConnector{}
	pool := &Pool{cfg: PoolConfig{SearchPath: "iam"}}
	c := datasourceConnector{inner: inner, pool: pool}

	if _, err := c.Connect(context.Background()); err == nil {
		t.Fatal("the recording connector always fails; the test only reads the context it saw")
	}
	if got, ok := searchPathRequestFrom(inner.ctx); !ok || got != "iam" {
		t.Errorf("delegated Connect context request = (%q, %v), want (iam, true)", got, ok)
	}
	if c.Driver() != nil {
		t.Error("Driver must delegate to the inner connector")
	}
}

// TestPool_AcquirePathsAreDatasourceBound proves the *Pool surfaces a closed
// or never-opened pool exposes fail without a panic and that the view is nil
// (so `pool.PGXPool() != nil` keeps meaning "open"), and that the logical
// pool's acquire context carries its own search_path.
func TestPool_AcquirePathsAreDatasourceBound(t *testing.T) {
	spectest.Proves(t, "go/shared-datasource-pooling", "datasource-bound-acquire", "every-pool-surface-marks-the-acquire-with-its-datasource")
	pool := &Pool{cfg: PoolConfig{SearchPath: "billing"}}
	if pool.PGXPool() != nil {
		t.Error("a pool that is not open must expose a nil view")
	}
	if pool.Querier(context.Background()) != Querier((*PGXPool)(nil)) {
		t.Error("Querier on a pool that is not open must return the typed nil view")
	}
	if _, err := pool.Acquire(context.Background()); err == nil || !perrors.Is(err, CodeConnection) {
		t.Errorf("Acquire on a pool that is not open = %v, want a %s error", err, CodeConnection)
	}
	if err := pool.Ping(context.Background()); err == nil || !perrors.Is(err, CodeConnection) {
		t.Errorf("Ping on a pool that is not open = %v, want a %s error", err, CodeConnection)
	}
	if pool.Stats() != nil {
		t.Error("Stats on a pool that is not open must be nil")
	}
	if got, ok := searchPathRequestFrom(pool.acquireContext(context.Background())); !ok || got != "billing" {
		t.Errorf("acquire context request = (%q, %v), want (billing, true)", got, ok)
	}
	if newStdlibDB(pool) != nil || newStdlibDB(nil) != nil {
		t.Error("newStdlibDB must be nil for a pool that is not open")
	}

	// A pool over a physical entry with no live pgxpool (the test seam) is
	// still "not open" to the view.
	seam := newLogicalPool(newPhysicalPool(nil, PoolConfig{}.withDefaults(), "seam"), PoolConfig{SearchPath: "core"})
	if seam.PGXPool() != nil {
		t.Error("a physical entry without a live pgxpool must expose a nil view")
	}
	seam.Close() // releases the handle; a nil pgxpool is tolerated
	if !seam.physical.isClosed() {
		t.Error("closing the only handle must close the physical entry")
	}
}

// TestPoolTuning_DiffNamesEveryDifferingField proves the agreement check
// compares exactly the fields the physical pool is created with and renders
// each difference for the declaration error.
func TestPoolTuning_DiffNamesEveryDifferingField(t *testing.T) {
	base := PoolConfig{IdentityResolver: fakeIdentity, TokenFetcher: fakeToken}.withDefaults()
	same := base
	same.SearchPath = "other"   // logical: excluded
	same.ConnectTimeout = 0     // logical: excluded
	same.SlowQueryThreshold = 5 // logical: excluded
	same.Database = "x"         // declarative: excluded
	same.QueryObserver = func(context.Context, QueryOp, time.Duration, error) {}
	if diffs := tuningOf(base).diff(tuningOf(same)); len(diffs) != 0 {
		t.Errorf("logical fields must not count as tuning differences, got %v", diffs)
	}

	other := base
	other.MaxConns = 4
	other.MinConns = 1
	other.MaxConnLifetime = 2 * base.MaxConnLifetime
	other.MaxConnIdleTime = 2 * base.MaxConnIdleTime
	other.HealthCheckPeriod = 2 * base.HealthCheckPeriod
	other.StatementTimeout = -1
	other.IdentityResolver = otherIdentity
	other.TokenFetcher = nil
	diffs := tuningOf(base).diff(tuningOf(other))
	want := []string{"MaxConns 10 vs 4", "MinConns 2 vs 1", "MaxConnLifetime 1h0m0s vs 2h0m0s", "MaxConnIdleTime 30m0s vs 1h0m0s", "HealthCheckPeriod 1m0s vs 2m0s", "StatementTimeout 30s vs -1ns", "IdentityResolver differs", "TokenFetcher differs"}
	if strings.Join(diffs, "|") != strings.Join(want, "|") {
		t.Errorf("diff = %v, want %v", diffs, want)
	}
	if tuningMismatchError("a", "b", diffs).Error() == "" || !perrors.Is(tuningMismatchError("a", "b", diffs), CodeDatasource) {
		t.Error("the mismatch is a db.datasource declaration error")
	}
}
