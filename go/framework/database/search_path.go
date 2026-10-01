package database

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"go.putnami.dev/errors"
)

// search_path is a per-acquire session setting the framework owns, not a
// startup parameter of the physical pool. Several datasources — several owned
// schemas of one physical database — share one *pgxpool.Pool (see Pools and
// doc/adr/0003), so a connection handed to datasource A may have last served
// datasource B. Every acquire therefore carries the datasource's search_path in
// its context, and the pool's PrepareConn hook sets it on the connection before
// the connection is handed out, skipping the round trip when the connection
// already carries the requested value.
//
// The marker that makes the skip safe lives on the connection itself
// (pgconn's CustomData), never in the pool: it is the last value THIS package
// set on THIS connection. pgxpool only hands out idle connections (a connection
// released mid-transaction is destroyed), so the session-level set_config below
// survives later transaction-local overrides — the migration runner's
// set_config(…, true) reverts at commit — and the marker stays truthful.
//
// User code must not issue a session-level SET search_path, RESET search_path,
// or DISCARD ALL on a framework connection: the marker would no longer describe
// the session and a later acquire would skip a set it needed.

// acquireSearchPathKey is the context key an acquire carries its search_path
// request under. The value is always an acquireSearchPath — a "present,
// possibly empty" request — so PrepareConn can tell "acquired through a
// datasource whose search_path is the server default" from "acquired outside
// any datasource", which fails closed.
type acquireSearchPathKey struct{}

// acquireSearchPath is one datasource's search_path request. An empty path is a
// valid request meaning "the server default" (a pool declared without
// SearchPath).
type acquireSearchPath struct{ path string }

// withSearchPath returns a child context that requests searchPath on the
// connection the acquire hands out. Every acquire path inside the package —
// the datasource-bound PGXPool view, Pool.Acquire, the stdlib connector, and
// the post-create ping — marks its context through here.
func withSearchPath(ctx context.Context, searchPath string) context.Context {
	return context.WithValue(ctx, acquireSearchPathKey{}, acquireSearchPath{path: strings.TrimSpace(searchPath)})
}

// searchPathRequestFrom returns the search_path ctx requests and whether ctx
// carries a request at all.
func searchPathRequestFrom(ctx context.Context) (string, bool) {
	req, ok := ctx.Value(acquireSearchPathKey{}).(acquireSearchPath)
	if !ok {
		return "", false
	}
	return req.path, true
}

// searchPathMarkerKey is the pgconn CustomData key under which the last
// search_path this package set on a connection is remembered. It is
// package-qualified so no other CustomData user can collide with it.
const searchPathMarkerKey = "go.putnami.dev/database.search_path"

// sessionExecer is the minimal statement surface prepareConnSearchPath needs —
// what *pgx.Conn offers — so the decision logic is unit-tested without a
// connection.
type sessionExecer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// prepareConnSearchPath is the PrepareConn decision: it reads the search_path
// request from ctx, compares it with the marker on the connection, and returns
// pgxpool's (keep, err) verdict:
//
//   - marker equals the request → no statement, the connection is handed out;
//   - the request is non-empty and differs → one parameterized set_config (no
//     quoting; a comma-separated list works), then the marker is updated;
//   - the request is empty ("server default") over a non-empty marker → RESET
//     search_path, the marker is cleared;
//   - the set or reset fails → (false, error): pgxpool destroys the connection
//     and the instigating query fails, so a connection is never handed out
//     under an unverified search_path;
//   - ctx carries no request → (true, error): the connection goes back to the
//     pool untouched and the query fails.
//
// The set is session-level (set_config's is_local=false). An unqualified
// prepared query re-resolves on a reused connection; only a same-named relation
// whose result shape differs between two schemas fails once with "cached plan
// must not change result type".
func prepareConnSearchPath(ctx context.Context, conn sessionExecer, marker map[string]any) (bool, error) {
	want, ok := searchPathRequestFrom(ctx)
	if !ok {
		return true, errors.Newf(CodeConnection, "connection acquired without a datasource; acquire through *database.Pool")
	}
	var have string
	if set, ok := marker[searchPathMarkerKey].(string); ok {
		have = set
	}
	if have == want {
		return true, nil
	}
	if want == "" {
		if _, err := conn.Exec(ctx, "RESET search_path"); err != nil {
			return false, errors.Wrapf(err, CodeConnection, "reset search_path")
		}
		delete(marker, searchPathMarkerKey)
		return true, nil
	}
	if _, err := conn.Exec(ctx, "SELECT set_config('search_path', $1, false)", want); err != nil {
		return false, errors.Wrapf(err, CodeConnection, "set search_path")
	}
	marker[searchPathMarkerKey] = want
	return true, nil
}

// prepareSearchPath is the pgxpool.Config.PrepareConn hook every physical pool
// this package opens carries (buildPoolConfig sets it). It binds
// prepareConnSearchPath to the real connection and its CustomData marker.
func prepareSearchPath(ctx context.Context, conn *pgx.Conn) (bool, error) {
	return prepareConnSearchPath(ctx, conn, conn.PgConn().CustomData())
}
