package testprovider

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	pdb "go.putnami.dev/protocol/database"
	"go.putnami.dev/protocol/features/spectest"
)

// recordingTB is a testing.TB whose Skip and Fatal end the goroutine, as the
// real ones do, and record how the suite ended.
type recordingTB struct {
	testing.TB
	ctx      context.Context
	outcome  string
	message  string
	cleanups []func()
}

func (r *recordingTB) Helper()                  {}
func (r *recordingTB) Context() context.Context { return r.ctx }
func (r *recordingTB) Cleanup(f func())         { r.cleanups = append(r.cleanups, f) }
func (r *recordingTB) Errorf(format string, args ...any) {
	r.outcome, r.message = "error", fmt.Sprintf(format, args...)
}
func (r *recordingTB) Fatalf(format string, args ...any) {
	r.outcome, r.message = "fail", fmt.Sprintf(format, args...)
	runtime.Goexit()
}
func (r *recordingTB) Skipf(format string, args ...any) {
	r.outcome, r.message = "skip", fmt.Sprintf(format, args...)
	runtime.Goexit()
}

// runForTest runs ForTest on a recording TB and reports how the suite ended.
func runForTest(t *testing.T, opts Options) *recordingTB {
	t.Helper()
	r := &recordingTB{TB: t, ctx: t.Context()}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if ForTest(r, opts) != nil {
			r.outcome = "provisioned"
		}
	}()
	<-done
	return r
}

// unreachableBinding points one datasource at a closed loopback port, with a
// password that must never surface.
func unreachableBinding(mode pdb.TestMode) *pdb.TestBinding {
	return &pdb.TestBinding{
		ProtocolVersion: pdb.ProtocolVersion,
		Mode:            mode,
		Databases: map[string]pdb.Database{
			"default": {Engine: pdb.EnginePostgres, Schema: "public", Connection: &pdb.Connection{
				Host: "127.0.0.1", Port: 1, User: "svc", Password: "s3cret-pw", Database: "svc_test",
			}},
		},
	}
}

// A required suite whose database does not answer fails, and the failure names
// the binding it tried without its password: the require mode, or CI with any
// mode.
func TestForTestFailsARequiredSuiteAndNamesTheBinding(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "test-database-availability", "an-unreachable-required-database-fails-and-names-the-binding")
	for _, tc := range []struct {
		name    string
		ci      string
		binding *pdb.TestBinding
		names   string
	}{
		{"require mode", "", unreachableBinding(pdb.TestModeRequire), "default=svc@127.0.0.1:1/svc_test"},
		{"no mode stated", "", unreachableBinding(""), "default=svc@127.0.0.1:1/svc_test"},
		{"auto on CI", "true", unreachableBinding(pdb.TestModeAuto), "default=svc@127.0.0.1:1/svc_test"},
		{"no binding on CI", "true", nil, "no DATABASE_TEST_BINDINGS is set"},
		{"an empty require binding", "", &pdb.TestBinding{ProtocolVersion: 1, Mode: pdb.TestModeRequire}, "names no database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCI, tc.ci)
			t.Setenv(EnvTestBinding, "")
			r := runForTest(t, Options{Binding: tc.binding})
			if r.outcome != "fail" {
				t.Fatalf("outcome = %s (%s), want fail", r.outcome, r.message)
			}
			if !strings.Contains(r.message, tc.names) {
				t.Errorf("failure %q does not name the binding %q", r.message, tc.names)
			}
			if strings.Contains(r.message, "s3cret-pw") {
				t.Errorf("failure carries the password: %q", r.message)
			}
		})
	}
}

// Outside require and CI, a suite whose database does not answer is skipped,
// with the marker putnami test counts.
func TestForTestSkipsAnUnreachableDatabaseOutsideRequire(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "test-database-availability", "an-unreachable-database-is-skipped-outside-require")
	t.Setenv(envCI, "")
	t.Setenv(EnvTestBinding, "")
	for _, tc := range []struct {
		name    string
		binding *pdb.TestBinding
	}{
		{"auto mode", unreachableBinding(pdb.TestModeAuto)},
		{"no binding", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := runForTest(t, Options{Binding: tc.binding})
			if r.outcome != "skip" || !strings.HasPrefix(r.message, SkipMarker) {
				t.Fatalf("outcome = %s (%s), want a skip starting with %q", r.outcome, r.message, SkipMarker)
			}
			if strings.Contains(r.message, "s3cret-pw") {
				t.Errorf("skip carries the password: %q", r.message)
			}
		})
	}
}

// A binding in the skip mode skips without the marker: the run asked for no
// database, so the summary of suites whose database did not answer leaves it
// out.
func TestForTestSkipsTheSkipModeWithoutTheMarker(t *testing.T) {
	t.Setenv(envCI, "")
	r := runForTest(t, Options{Binding: &pdb.TestBinding{ProtocolVersion: 1, Mode: pdb.TestModeSkip}})
	if r.outcome != "skip" || strings.Contains(r.message, SkipMarker) {
		t.Errorf("outcome = %s (%s), want a skip without %q", r.outcome, r.message, SkipMarker)
	}
}

// A server that answers with an error is there: a connection refused for its
// password or its database fails even outside require.
func TestUnavailableTellsAnAnsweringServerFromNoServer(t *testing.T) {
	tb := unreachableBinding(pdb.TestModeAuto)
	dial := &pgconn.ConnectError{Config: &pgconn.Config{}}
	if !unavailable(tb, fmt.Errorf("provision: %w", dial)) {
		t.Error("a connection no server answered is not unavailable")
	}
	answered := fmt.Errorf("provision: %w", &pgconn.ConnectError{Config: &pgconn.Config{}})
	answered = fmt.Errorf("%w: %w", answered, &pgconn.PgError{Code: "28P01"})
	if unavailable(tb, answered) {
		t.Error("a server that refused the password reads as not answering")
	}
	if unavailable(tb, fmt.Errorf("migrate")) {
		t.Error("a failure that is no connection failure reads as not answering")
	}
}

// A failure that is not a connection failure fails the suite even outside
// require: a binding that lacks the datasource the suite asks for, or an
// invalid binding in the environment.
func TestForTestFailsAProvisioningFailureThatIsNotAConnectionFailure(t *testing.T) {
	spectest.Proves(t, "go/persistence-transactions", "test-database-availability", "a-provisioning-failure-that-is-not-a-connection-failure-fails")
	t.Setenv(envCI, "")

	t.Setenv(EnvTestBinding, "")
	r := runForTest(t, Options{Binding: unreachableBinding(pdb.TestModeAuto), Datasources: []string{"billing"}})
	if r.outcome != "fail" || !strings.Contains(r.message, "billing") {
		t.Errorf("missing datasource: outcome = %s (%s), want a failure naming it", r.outcome, r.message)
	}

	t.Setenv(EnvTestBinding, `{"protocolVersion":1,"mode":"bogus","databases":{}}`)
	r = runForTest(t, Options{})
	if r.outcome != "fail" {
		t.Errorf("invalid binding: outcome = %s (%s), want fail", r.outcome, r.message)
	}
}

// DescribeBinding renders coordinates only: a DSN's password, a structured
// password and an unparseable DSN's text never appear.
func TestDescribeBindingNeverRendersAPassword(t *testing.T) {
	tb := &pdb.TestBinding{ProtocolVersion: 1, Databases: map[string]pdb.Database{
		"a": {Connection: &pdb.Connection{DSN: "postgres://svc:s3cret-pw@db.local:6432/app?sslmode=disable"}},
		"b": {Connection: &pdb.Connection{Instance: "proj:region:inst", User: "svc", Password: "s3cret-pw", Database: "app"}},
		"c": {Connection: &pdb.Connection{DSN: "host=x password=s3cret-pw"}},
		"d": {},
	}}
	got := DescribeBinding(tb, nil)
	want := "a=svc@db.local:6432/app, b=svc@proj:region:inst/app, c=(a DSN that is not a URL), d=(no connection)"
	if got != want {
		t.Errorf("DescribeBinding = %q, want %q", got, want)
	}
	if got := DescribeBinding(tb, []string{"z"}); got != "z=(not in the binding)" {
		t.Errorf("DescribeBinding(z) = %q", got)
	}
}
