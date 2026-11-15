package testprovider

import (
	stderrors "errors"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	pdb "go.putnami.dev/protocol/database"
)

// SkipMarker starts the message of every suite ForTest skips because its test
// database did not answer. `putnami test` counts the suites whose skip carries
// it and names them in one summary line, so a skip never passes unseen. The Go
// extension's test job holds the same text; keep the two equal.
const SkipMarker = "[putnami: no test database]"

// envCI is the CI marker: on CI a suite whose database does not answer always
// fails.
const envCI = "CI"

// ForTest provisions the suite's test databases as Provision does, registers
// their teardown with t.Cleanup, and returns them.
//
// When the database does not answer — no binding names one, or no server
// accepts the connection — the suite fails when the binding's mode is require
// or CI is set, naming the binding it tried without its password, and is
// skipped otherwise, with a message that starts with SkipMarker. A binding in
// the skip mode skips without the marker: the run asked for no database. Any
// other failure fails the suite: an invalid binding, a server that answers but
// refuses the role or lacks the database, or a migration that does not apply.
//
// `putnami test` sets the binding's mode: require under --require-test-db and
// on CI, auto on a developer run.
func ForTest(t testing.TB, opts Options) *Result {
	t.Helper()
	if opts.Binding == nil {
		tb, err := ResolveTestBinding(os.Getenv(EnvTestBinding))
		if err != nil {
			t.Fatalf("test database: %v", err)
		}
		opts.Binding = tb
	}
	result, err := Provision(t.Context(), opts)
	if err == nil {
		t.Cleanup(func() {
			if err := result.Cleanup(); err != nil {
				t.Errorf("test database teardown: %v", err)
			}
		})
		return result
	}
	if stderrors.Is(err, ErrSkip) {
		t.Skipf("test database: %v", err)
	}
	if !unavailable(opts.Binding, err) {
		t.Fatalf("test database: %v", err)
	}
	what := DescribeBinding(opts.Binding, opts.Datasources)
	if required(opts.Binding) {
		t.Fatalf("test database %s did not answer: %v", what, err)
	}
	t.Skipf("%s %s did not answer, so this suite did not run; --require-test-db or CI fails it instead: %v", SkipMarker, what, err)
	return nil
}

// unavailable reports whether err means the database is not there to answer:
// no binding names one, or a connection to it failed before any server
// answered. A server that answered with an error, a wrong password or a
// missing database, is there: that is a failure to report.
func unavailable(tb *pdb.TestBinding, err error) bool {
	if tb == nil || len(tb.Databases) == 0 {
		return true
	}
	var connectErr *pgconn.ConnectError
	if !stderrors.As(err, &connectErr) {
		return false
	}
	var serverErr *pgconn.PgError
	return !stderrors.As(err, &serverErr)
}

// required reports whether a suite whose database does not answer must fail:
// the binding's mode is require, or the run is on CI. A binding that states no
// mode is required, as Provision reads it; no binding at all is required on
// CI only.
func required(tb *pdb.TestBinding) bool {
	if os.Getenv(envCI) != "" {
		return true
	}
	return tb != nil && effectiveMode(tb) == pdb.TestModeRequire
}

// DescribeBinding names what a binding points the given datasources at — every
// datasource when names is empty — as `datasource=user@host:port/database`,
// never with a password. A binding that names no database reads as such.
func DescribeBinding(tb *pdb.TestBinding, names []string) string {
	if tb == nil {
		return "(no " + EnvTestBinding + " is set)"
	}
	if len(tb.Databases) == 0 {
		return "(" + EnvTestBinding + " names no database)"
	}
	keys := names
	if len(keys) == 0 {
		for name := range tb.Databases {
			keys = append(keys, name)
		}
		slices.Sort(keys)
	}
	parts := make([]string, 0, len(keys))
	for _, name := range keys {
		db, ok := tb.Databases[name]
		if !ok {
			parts = append(parts, name+"=(not in the binding)")
			continue
		}
		parts = append(parts, name+"="+describeConnection(db.Connection))
	}
	return strings.Join(parts, ", ")
}

// describeConnection renders a connection's coordinates without its password.
// A DSN is parsed for its coordinates only; one that does not parse reads as
// an unparseable DSN, never as its text.
func describeConnection(c *pdb.Connection) string {
	if c == nil {
		return "(no connection)"
	}
	user, host, database := c.User, c.Host, c.Database
	port := c.Port
	switch {
	case c.DSN != "":
		u, err := url.Parse(c.DSN)
		if err != nil || u.Host == "" {
			return "(a DSN that is not a URL)"
		}
		user = u.User.Username()
		host = u.Hostname()
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
		database = strings.TrimPrefix(u.Path, "/")
	case c.Instance != "":
		host = c.Instance
	}
	address := host
	if port != 0 {
		address = net.JoinHostPort(host, strconv.Itoa(port))
	}
	out := address + "/" + database
	if user != "" {
		out = user + "@" + out
	}
	return out
}
