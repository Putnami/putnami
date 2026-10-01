package dbtestenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
)

// The PROVIDED-SERVER conformance suite: the same questions docker_test.go asks
// of the container provider — resource identity, reuse across invocations,
// bounded readiness, teardown, credential confinement — asked of a server the
// pipeline manages, plus the artifact fixture that pins what a test task
// actually receives in this mode.
//
// Every case runs against a fake dial seam, so none of them needs a Postgres, a
// docker daemon or a network.

// --- fake dial seam --------------------------------------------------------

// providedURL is the fixture server: a distinctive password that shares no
// substring with the host, port, database, user or any digest derived from
// them, so a leak into any of those surfaces is unambiguous.
const (
	providedURL      = "postgres://ci:provided-only-secret@10.0.0.5:6543/pgtest?sslmode=disable"
	providedPassword = "provided-only-secret"
)

// fakeDial records every reachability probe and replies from a scripted
// responder, so bounded waits and "was the server consulted at all" are
// assertable without a listener.
type fakeDial struct {
	calls       []string
	hadDeadline []bool
	respond     func(attempt int) error
}

func (f *fakeDial) dial(ctx context.Context, host string, port int) error {
	f.calls = append(f.calls, net.JoinHostPort(host, strconv.Itoa(port)))
	_, bounded := ctx.Deadline()
	f.hadDeadline = append(f.hadDeadline, bounded)
	if f.respond != nil {
		return f.respond(len(f.calls))
	}
	return nil
}

// testProvidedServer wires a ProvidedServer to a fake dial and a short
// reachability budget, so an unreachable case costs milliseconds.
func testProvidedServer(rawURL string, fd *fakeDial) *ProvidedServer {
	return &ProvidedServer{url: rawURL, dial: fd.dial, reachableTimeout: 50 * time.Millisecond}
}

// --- selection -------------------------------------------------------------

// Naming a server selects it, over docker and without parsing it: a malformed
// URL must fail loudly out of Provision, never fall back to a container the
// pipeline did not ask for.
func TestSelectProviderPrefersTheNamedServer(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "a-server-the-pipeline-named-is-used")
	cases := []struct {
		name     string
		env      string
		provided bool
	}{
		{"a named server", providedURL, true},
		{"a named server that will not parse", "://nope", true},
		{"no server named", "", false},
		{"blank is not naming a server", "   ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvProvidedServer, tc.env)
			prov := SelectProvider()
			_, isProvided := prov.(*ProvidedServer)
			if isProvided != tc.provided {
				t.Fatalf("selected %T, provided-server = %v, want %v", prov, isProvided, tc.provided)
			}
			if !tc.provided {
				if _, isDocker := prov.(*Provisioner); !isDocker {
					t.Errorf("selected %T, want the docker Provisioner as the local default", prov)
				}
			}
		})
	}
}

// --- resource identity -----------------------------------------------------

// The provided server owns no resource, so it names none. A lease over it must
// carry no container name and no label to filter by — there is nothing this run
// could reclaim, and a name would invite a later sweep to try.
func TestProvidedServerOwnsNoResource(t *testing.T) {
	s := testProvidedServer(providedURL, &fakeDial{})
	if s.LabelKey() != "" || s.ContainerName("deadbeef") != "" {
		t.Errorf("provided server names a resource: label %q, container %q", s.LabelKey(), s.ContainerName("deadbeef"))
	}
	if s.ownsServer() {
		t.Error("provided server claims to own the server it was handed")
	}
	if s.id() != providedProviderID || s.id() == providerID {
		t.Errorf("provider id = %q, want the distinct %q", s.id(), providedProviderID)
	}
	if err := s.TeardownAll(); err != nil {
		t.Errorf("TeardownAll on a server this run does not own: %v", err)
	}
}

// available is an environment read: a named server is available, a blank one is
// not, and neither answer costs a dial.
func TestProvidedServerAvailabilityIsAnEnvironmentRead(t *testing.T) {
	fd := &fakeDial{}
	if !testProvidedServer(providedURL, fd).available() {
		t.Error("a named server reports unavailable")
	}
	if testProvidedServer("  ", fd).available() {
		t.Error("a blank value reports available")
	}
	if (*ProvidedServer)(nil).available() {
		t.Error("a nil provider reports available")
	}
	if len(fd.calls) != 0 {
		t.Errorf("availability dialed the server: %v", fd.calls)
	}
}

// The digest is the server's NON-SECRET identity. It must be stable, must move
// with every coordinate that would make two environments different, and must
// NOT move with the password or a query parameter — either could carry a
// credential, and a digest is copied into leases and run reports.
func TestProvidedDigestIsIdentityOnly(t *testing.T) {
	digestOfURL := func(t *testing.T, raw string) string {
		t.Helper()
		_, cfg, err := parseProvidedURL(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		return cfg.digest()
	}

	base := digestOfURL(t, providedURL)
	if base != digestOfURL(t, providedURL) {
		t.Fatal("digest is not stable for an identical URL")
	}
	if strings.ContainsAny(base, "ghijklmnopqrstuvwxyz-_:/@.") {
		t.Errorf("digest %q is not pure hex", base)
	}
	for _, tc := range []struct{ name, url string }{
		{"host", "postgres://ci:provided-only-secret@10.0.0.6:6543/pgtest?sslmode=disable"},
		{"port", "postgres://ci:provided-only-secret@10.0.0.5:6544/pgtest?sslmode=disable"},
		{"database", "postgres://ci:provided-only-secret@10.0.0.5:6543/other?sslmode=disable"},
		{"user", "postgres://other:provided-only-secret@10.0.0.5:6543/pgtest?sslmode=disable"},
	} {
		if digestOfURL(t, tc.url) == base {
			t.Errorf("digest did not change when the %s changed", tc.name)
		}
	}
	for _, tc := range []struct{ name, url string }{
		{"password", "postgres://ci:another-secret@10.0.0.5:6543/pgtest?sslmode=disable"},
		{"query parameter", "postgres://ci:provided-only-secret@10.0.0.5:6543/pgtest?sslmode=require"},
	} {
		if digestOfURL(t, tc.url) != base {
			t.Errorf("the %s moved the digest; it is derived from the non-secret identity only", tc.name)
		}
	}
}

// --- URL resolution --------------------------------------------------------

func TestProvidedURLResolvesToAStructuredConnection(t *testing.T) {
	conn, cfg, err := parseProvidedURL(providedURL)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if conn.Host != "10.0.0.5" || conn.Port != 6543 {
		t.Errorf("endpoint = %s:%d, want 10.0.0.5:6543", conn.Host, conn.Port)
	}
	if conn.User != "ci" || conn.Password != providedPassword || conn.Database != "pgtest" {
		t.Errorf("credentials not carried through: %+v", conn)
	}
	if conn.Params["sslmode"] != "disable" {
		t.Errorf("params = %v, want the URL query's driver options", conn.Params)
	}
	if conn.DSN != "" || conn.Instance != "" {
		t.Errorf("connection declares more than one transport: %+v", conn)
	}
	if cfg.Host != conn.Host || cfg.Port != conn.Port || cfg.User != conn.User || cfg.Database != conn.Database {
		t.Errorf("identity %+v does not describe the connection %+v", cfg, conn)
	}
}

// A URL that omits what Postgres has a default for still resolves, and an empty
// query contributes no params member.
func TestProvidedURLAppliesPostgresDefaults(t *testing.T) {
	conn, _, err := parseProvidedURL("postgresql://ci@db.internal")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if conn.Port != defaultProvidedPort {
		t.Errorf("port = %d, want the postgres default %d", conn.Port, defaultProvidedPort)
	}
	if conn.Database != defaultProvidedDatabase {
		t.Errorf("database = %q, want the maintenance database %q", conn.Database, defaultProvidedDatabase)
	}
	if conn.Params != nil {
		t.Errorf("params = %v, want none for a query-less URL", conn.Params)
	}
	if empty, _, err := parseProvidedURL("postgres://ci@db.internal/x?sslmode="); err != nil || empty.Params != nil {
		t.Errorf("an empty query value produced params %v (err %v)", empty.Params, err)
	}
}

// A URL this package cannot use fails with a CAUSE, and the cause never
// contains the credential — net/url echoes its whole input, password included,
// in the error it returns.
func TestProvidedURLErrorsAreCredentialFree(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"empty", "  "},
		{"unparseable", "postgres://ci:" + providedPassword + "@[10.0.0.5:6543/pgtest"},
		{"wrong scheme", "mysql://ci:" + providedPassword + "@10.0.0.5:3306/pgtest"},
		{"no host", "postgres:///pgtest"},
		{"port out of range", "postgres://ci:" + providedPassword + "@10.0.0.5:99999/pgtest"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := parseProvidedURL(tc.url)
			if err == nil {
				t.Fatalf("parse %q succeeded; a URL this package cannot use must fail loudly", tc.url)
			}
			if strings.Contains(err.Error(), providedPassword) {
				t.Errorf("the failure leaked the credential: %v", err)
			}
			if !strings.Contains(err.Error(), EnvProvidedServer) {
				t.Errorf("the failure does not name %s, so a reader cannot fix it: %v", EnvProvidedServer, err)
			}
		})
	}
}

// --- reachability: bounded, no unbounded wait ------------------------------

// The probe is a retry loop with a deadline on the whole wait AND on each
// attempt, exactly like the container provider's readiness wait — a server that
// is still binding its port must not fail the first task to reach it.
func TestProvidedServerWaitsForTheEndpoint(t *testing.T) {
	fd := &fakeDial{respond: func(attempt int) error {
		if attempt == 1 {
			return errors.New("connection refused")
		}
		return nil
	}}
	s := testProvidedServer(providedURL, fd)
	s.reachableTimeout = time.Second

	conn, digest, err := s.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(fd.calls) != 2 {
		t.Fatalf("probe attempts = %d, want 2", len(fd.calls))
	}
	if fd.calls[0] != "10.0.0.5:6543" {
		t.Errorf("probed %q, want the URL's endpoint", fd.calls[0])
	}
	for i, bounded := range fd.hadDeadline {
		if !bounded {
			t.Errorf("probe %d had no context deadline", i)
		}
	}
	if conn.Port != 6543 || digest == "" {
		t.Errorf("Provision returned %+v / %q", conn, digest)
	}
}

func TestProvidedServerReachabilityIsBounded(t *testing.T) {
	fd := &fakeDial{respond: func(int) error { return context.DeadlineExceeded }}
	s := testProvidedServer(providedURL, fd)

	start := time.Now()
	_, _, err := s.Provision()
	if err == nil {
		t.Fatal("Provision succeeded against a server that never answered")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the reachability wait was not bounded: took %s", elapsed)
	}
	if !strings.Contains(err.Error(), "10.0.0.5:6543") {
		t.Errorf("the timeout does not name the endpoint: %v", err)
	}
	if strings.Contains(err.Error(), providedPassword) {
		t.Errorf("the timeout leaked the credential: %v", err)
	}
}

// A URL that cannot be resolved never reaches the network.
func TestProvidedServerDoesNotDialAnUnusableURL(t *testing.T) {
	fd := &fakeDial{}
	if _, _, err := testProvidedServer("mysql://x@h/db", fd).Provision(); err == nil {
		t.Fatal("Provision succeeded on a non-postgres URL")
	}
	if len(fd.calls) != 0 {
		t.Errorf("dialed %v despite an unusable URL", fd.calls)
	}
}

// --- Up: the CI path this mode exists for ----------------------------------

// The acceptance case: CI=true, a named server, and NO injected
// DATABASE_TEST_BINDINGS. The ordinary per-project task provisions its own
// closure — no workspace union — and writes both artifacts.
func TestUpProvisionsAgainstTheNamedServerOnCI(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	fd := &fakeDial{}
	prov := testProvidedServer(providedURL, fd)
	result := Up(f.ctx, prov)

	if result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q (%+v), want %q", result.Outcome, result.Diagnostics, OutcomeProvisioned)
	}
	if result.Mode != pdb.TestModeRequire {
		t.Errorf("mode = %q, want the CI default require", result.Mode)
	}
	if result.Datasources != 1 {
		t.Errorf("datasources = %d, want this project's closure only", result.Datasources)
	}
	if len(fd.calls) != 1 {
		t.Errorf("reachability probes = %d, want exactly one per task", len(fd.calls))
	}

	binding, ok := BindingFrom(f.ctx)
	if !ok {
		t.Fatal("no bindings artifact was written")
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(binding))
	if diag.HasErrors(diags) {
		t.Fatalf("written binding failed strict validation: %v", diags)
	}
	db, ok := tb.Databases["primary"]
	if !ok || db.Connection == nil || db.Connection.Host != "10.0.0.5" || db.Connection.Port != 6543 {
		t.Fatalf("binding does not point at the named server: %+v", tb.Databases)
	}

	lease, ok := ReadLease(f.artifacts)
	if !ok {
		t.Fatal("no lease was written beside the binding")
	}
	if !lease.Retain {
		t.Error("the lease says tear down a server this run never created")
	}
	if lease.Provider != providedProviderID || lease.Container != "" || lease.Label != "" {
		t.Errorf("lease describes a resource this run owns: %+v", lease)
	}
	if lease.Digest != result.Digest || lease.Digest == "" {
		t.Errorf("lease digest %q does not identify the reported server %q", lease.Digest, result.Digest)
	}
}

// The two policy decisions this mode makes, pinned where a test task reads
// them: schema isolation (no CREATE/DROP DATABASE on a shared cluster) and no
// bundle-template reuse (a cold rebuild on a per-run server is pure overhead).
func TestUpProvidedBindingSelectsSchemaIsolationAndNoReuse(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	if result := Up(f.ctx, testProvidedServer(providedURL, &fakeDial{})); result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}
	binding, _ := BindingFrom(f.ctx)
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(binding))
	if diag.HasErrors(diags) {
		t.Fatalf("binding failed validation: %v", diags)
	}
	if tb.Isolation != pdb.IsolationSchema {
		t.Errorf("isolation = %q, want %q — a shared server may not pay for CREATE/DROP DATABASE",
			tb.Isolation, pdb.IsolationSchema)
	}
	if tb.Reuse != pdb.ReuseNone {
		t.Errorf("reuse = %q, want %q — a template rebuild on a per-run server amortizes nothing",
			tb.Reuse, pdb.ReuseNone)
	}
}

// The artifact FIXTURE: the exact bytes a provided-server test task reads, for a
// two-datasource closure. It pins the whole contract at once — protocol version,
// policy, the per-datasource schema each project declared, the bounded test pool
// every datasource gets, and the one physical server they all map onto.
func TestProvidedBindingArtifactFixture(t *testing.T) {
	conn, _, err := parseProvidedURL(providedURL)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what Up assembles: the provider's policy plus the run's resolved
	// mode, over a two-project dependency closure.
	policy := (&ProvidedServer{}).bindingDefaults()
	policy.Mode = pdb.TestModeRequire
	raw, err := SynthesizeBinding(policy, []infra.Database{
		{Name: "analytics", Engine: infra.EnginePostgres, Schemas: []string{"metrics"}},
		{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"app"}},
	}, conn)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}

	const params = `"params":{"pool_max_conn_idle_time":"5s","pool_max_conns":"2","pool_min_conns":"0",` +
		`"sslmode":"disable"}`
	const want = `{"protocolVersion":1,"mode":"require","isolation":"schema","reuse":"none","databases":{` +
		`"analytics":{"engine":"postgres","schema":"metrics","connection":` +
		`{"host":"10.0.0.5","port":6543,"database":"pgtest","user":"ci","password":"provided-only-secret",` +
		params + `}},` +
		`"primary":{"engine":"postgres","schema":"app","connection":` +
		`{"host":"10.0.0.5","port":6543,"database":"pgtest","user":"ci","password":"provided-only-secret",` +
		params + `}}}}`
	if raw != want {
		t.Errorf("provided-server binding artifact drifted.\n got: %s\nwant: %s", raw, want)
	}
	if _, diags := pdb.ParseAndValidateTestBinding([]byte(raw)); diag.HasErrors(diags) {
		t.Errorf("the fixture is not a valid test binding: %v", diags)
	}
}

// --- Up: the gates still hold ----------------------------------------------

// An externally supplied DATABASE_TEST_BINDINGS beats a named server exactly as
// it beats docker: the pipeline that hands over a complete binding has said
// everything, and this run must not shadow it.
func TestUpExternalBindingBeatsTheNamedServer(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, `{"protocolVersion":1,"databases":{}}`)
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	fd := &fakeDial{}
	result := Up(f.ctx, testProvidedServer(providedURL, fd))

	if result.Outcome != OutcomeExternal {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeExternal)
	}
	if len(fd.calls) != 0 {
		t.Errorf("the named server was consulted despite an external binding: %v", fd.calls)
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("wrote a binding artifact that would shadow the external value")
	}
}

// `skip` declines a named server too: a run that asked for no database gets
// none, on CI included. Every other mode uses it — that is the difference from
// docker, and the reason a CI run can have a test environment at all.
func TestProvidedServerHonorsTheMode(t *testing.T) {
	cases := []struct {
		mode   pdb.TestMode
		ci     string
		usable bool
	}{
		{pdb.TestModeAuto, "", true},
		{pdb.TestModeAuto, "true", true},
		{pdb.TestModeRequire, "true", true},
		{pdb.TestModeRequire, "", true},
		{pdb.TestModeSkip, "", false},
		{pdb.TestModeSkip, "true", false},
	}
	for _, tc := range cases {
		t.Run(string(tc.mode)+"/ci="+tc.ci, func(t *testing.T) {
			t.Setenv(EnvCI, tc.ci)
			provided := testProvidedServer(providedURL, &fakeDial{})
			if got := mayProvision(tc.mode, provided); got != tc.usable {
				t.Errorf("provided server usable = %v, want %v", got, tc.usable)
			}
			// The container provider's gate is untouched: it may still only
			// start something under auto, off CI.
			docker := stubProvisioner(nil)
			wantDocker := tc.mode == pdb.TestModeAuto && tc.ci == ""
			if got := mayProvision(tc.mode, docker); got != wantDocker {
				t.Errorf("docker may provision = %v, want %v", got, wantDocker)
			}
		})
	}
	if mayProvision(pdb.TestModeRequire, testProvidedServer("", &fakeDial{})) {
		t.Error("an unnamed server passed the gate")
	}
}

// Under `skip` the server is never even dialed.
func TestUpSkipNeverTouchesTheNamedServer(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, map[string]any{ParamInfra: "skip"})
	f.declareDatabases(t, "svc", postgresRequirements)

	fd := &fakeDial{}
	result := Up(f.ctx, testProvidedServer(providedURL, fd))

	if result.Outcome != OutcomeNotProvisioned {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeNotProvisioned)
	}
	if len(fd.calls) != 0 {
		t.Errorf("skip dialed the server: %v", fd.calls)
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("skip wrote a binding artifact")
	}
}

// A closure with no database costs nothing here either: no dial, no artifact.
func TestUpProvidedIsZeroCostWithoutDatabases(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)

	fd := &fakeDial{}
	if result := Up(f.ctx, testProvidedServer(providedURL, fd)); result.Outcome != OutcomeNoDatabases {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeNoDatabases)
	}
	if len(fd.calls) != 0 {
		t.Errorf("a project declaring no database dialed the server: %v", fd.calls)
	}
}

// An unreachable server is a WARNING with a cause, never a failed setup: the
// test task reports the real problem (it fails under require), and failing here
// too would report it twice with the less useful message.
func TestUpProvidedUnreachableServerIsAdvisory(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	fd := &fakeDial{respond: func(int) error { return errors.New("connection refused") }}
	result := Up(f.ctx, testProvidedServer(providedURL, fd))

	if result.Outcome != OutcomeNotProvisioned {
		t.Fatalf("outcome = %q, want %q", result.Outcome, OutcomeNotProvisioned)
	}
	if diag.HasErrors(result.Diagnostics) {
		t.Errorf("an unreachable server produced an error diagnostic: %+v", result.Diagnostics)
	}
	found := false
	for _, d := range result.Diagnostics {
		if d.Code == DiagnosticProvisionFailed {
			found = true
		}
		if strings.Contains(d.String(), providedPassword) {
			t.Errorf("a diagnostic leaked the credential: %s", d.String())
		}
	}
	if !found {
		t.Errorf("no %s diagnostic explains the empty environment: %+v", DiagnosticProvisionFailed, result.Diagnostics)
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("wrote a binding artifact for a server that never answered")
	}
}

// --- teardown --------------------------------------------------------------

// `--infra-down` reaps what this workspace STARTED. A provided server is not
// that: nothing is removed, nothing is dialed, and no binding is written.
func TestUpProvidedInfraDownRemovesNothing(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, map[string]any{ParamInfraDown: true})
	f.declareDatabases(t, "svc", postgresRequirements)

	fd := &fakeDial{}
	result := Up(f.ctx, testProvidedServer(providedURL, fd))

	if result.Outcome != OutcomeTornDown {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeTornDown)
	}
	if len(fd.calls) != 0 {
		t.Errorf("teardown dialed the server: %v", fd.calls)
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("teardown wrote a binding artifact")
	}
	lease, ok := ReadLease(f.artifacts)
	if !ok || lease.Provider != providedProviderID {
		t.Errorf("teardown lease = %+v, want one this provider wrote", lease)
	}
}

// The finalizer retains a provided server for the strongest possible reason:
// this run never created it, and other tasks are still using it.
func TestDownRetainsAProvidedServer(t *testing.T) {
	f := newFixture(t, nil)
	if err := writeLease(f.artifacts, Lease{
		Version: LeaseVersion, InvocationID: "inv-fixture", Provider: providedProviderID,
		Digest: "0123456789abcdef", Retain: true,
	}); err != nil {
		t.Fatal(err)
	}

	fd := &fakeDial{}
	result := Down(f.ctx, testProvidedServer(providedURL, fd))

	if result.Outcome != OutcomeRetained {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeRetained)
	}
	if len(fd.calls) != 0 {
		t.Errorf("the finalizer touched a server it does not own: %v", fd.calls)
	}
}

// --- security: the credential reaches exactly one file ---------------------

// Same invariant as the docker provider's, against a URL-shaped credential: the
// result data, the lease and every diagnostic must be free of it, and the one
// file that carries it is owner-only.
func TestUpProvidedConfinesTheCredential(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	result := Up(f.ctx, testProvidedServer(providedURL, &fakeDial{}))
	if result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}

	for key, value := range result.Data() {
		if text, ok := value.(string); ok && strings.Contains(text, providedPassword) {
			t.Errorf("result data %q leaked the credential: %s", key, text)
		}
	}
	leaseBytes, err := os.ReadFile(filepath.Join(f.artifacts, filepath.FromSlash(LeaseArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(leaseBytes), providedPassword) {
		t.Errorf("lease leaked the credential: %s", leaseBytes)
	}

	assertOwnerOnlyArtifact(t, filepath.Join(f.artifacts, filepath.FromSlash(BindingsArtifact)))
	binding, _ := BindingFrom(f.ctx)
	if !strings.Contains(binding, providedPassword) {
		t.Error("the bindings artifact does not carry the credential; the fixture proves nothing")
	}
}

// --- the pool a provided-server binding carries ----------------------------

// The whole point of the default, seen where a test task reads it: the binding a
// named server produces bounds every datasource at 2 connections released after
// 5 seconds idle, instead of the framework's 10 held for 30 minutes.
func TestUpProvidedBindingBoundsTheTestPool(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	if result := Up(f.ctx, testProvidedServer(providedURL, &fakeDial{})); result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}
	raw, _ := BindingFrom(f.ctx)
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("binding failed validation: %v", diags)
	}
	params := tb.Databases["primary"].Connection.Params
	if params["pool_max_conns"] != "2" || params["pool_min_conns"] != "0" {
		t.Errorf("pool size = %q/%q, want 2/0", params["pool_max_conns"], params["pool_min_conns"])
	}
	if params["pool_max_conn_idle_time"] != "5s" {
		t.Errorf("pool_max_conn_idle_time = %q, want 5s — a bounded pool that never releases "+
			"pins its connections for the whole run", params["pool_max_conn_idle_time"])
	}
	if params["sslmode"] != "disable" {
		t.Errorf("sslmode = %q, want the URL's own driver option intact", params["sslmode"])
	}
}

// PUTNAMI_TEST_PG_URL wins, per key. A pipeline whose suites need a wider pool
// says so in the URL it already exports, and keeps the two defaults it did not
// contradict.
func TestUpProvidedURLOverridesThePoolDefault(t *testing.T) {
	t.Setenv(EnvCI, "true")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	const wide = "postgres://ci:provided-only-secret@10.0.0.5:6543/pgtest" +
		"?sslmode=disable&pool_max_conns=5&pool_max_conn_idle_time=90s"
	if result := Up(f.ctx, testProvidedServer(wide, &fakeDial{})); result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}
	raw, _ := BindingFrom(f.ctx)
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("binding failed validation: %v", diags)
	}
	params := tb.Databases["primary"].Connection.Params
	if params["pool_max_conns"] != "5" {
		t.Errorf("pool_max_conns = %q, want the URL's 5", params["pool_max_conns"])
	}
	if params["pool_max_conn_idle_time"] != "90s" {
		t.Errorf("pool_max_conn_idle_time = %q, want the URL's 90s", params["pool_max_conn_idle_time"])
	}
	if params["pool_min_conns"] != "0" {
		t.Errorf("pool_min_conns = %q, want the default the URL did not contradict",
			params["pool_min_conns"])
	}
}
