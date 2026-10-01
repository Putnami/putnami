package dbtestenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	pdb "go.putnami.dev/protocol/database"
	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/infra"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/ownerperm"
)

// The POLICY and ARTIFACT half of the suite. Every case here pins a behavior
// the CLI planner deleted by an earlier migration used to own, so a
// reviewer can check the migration by reading the two suites side by side.

const fixturePassword = "provisioner-only-secret"

// fixture builds a job context for one project plus an optional dependency,
// with a private artifact root standing in for the orchestrator's invocation
// scratch.
type fixture struct {
	ctx  *pctx.Context
	root string
	// artifacts is the private invocation artifact root.
	artifacts string
}

func newFixture(t *testing.T, params map[string]any) *fixture {
	t.Helper()
	root := t.TempDir()
	artifacts := filepath.Join(root, ".putnami", "invocations", "inv-fixture", "artifacts")
	if err := os.MkdirAll(filepath.Join(artifacts, "database"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := &pctx.Context{
		WorkspaceRoot: root,
		Params:        encodeParams(t, params),
		Invocation:    &pctx.Invocation{ID: "inv-fixture", ArtifactRoot: artifacts},
	}
	ctx.Project = pctx.Project{
		Name:     "svc",
		Path:     "svc",
		FullPath: filepath.Join(root, "svc"),
		DependencyClosure: []pctx.ProjectRef{
			{Name: "svc", Path: "svc", FullPath: filepath.Join(root, "svc")},
		},
	}
	return &fixture{ctx: ctx, root: root, artifacts: artifacts}
}

func encodeParams(t *testing.T, params map[string]any) pctx.Params {
	t.Helper()
	out := make(pctx.Params, len(params))
	for k, v := range params {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out[k] = raw
	}
	return out
}

// declareDatabases writes a committed per-project requirements manifest for the
// named workspace-relative project.
func (f *fixture) declareDatabases(t *testing.T, projectPath, body string) {
	t.Helper()
	dir := filepath.Join(f.root, projectPath, "infra")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requirements.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// addDependency extends the closure with a second project, as the orchestrator
// does for a project whose dependency declares the database.
func (f *fixture) addDependency(t *testing.T, name, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, path), 0o755); err != nil {
		t.Fatal(err)
	}
	f.ctx.Project.DependencyClosure = append(f.ctx.Project.DependencyClosure,
		pctx.ProjectRef{Name: name, Path: path, FullPath: filepath.Join(f.root, path)})
}

// stubProvisioner is a Provisioner whose docker seam is a recorder: it never
// runs a container and reports whether it was consulted, so the zero-cost and
// fail-closed paths are checkable.
func stubProvisioner(consulted *bool) *Provisioner {
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		if consulted != nil {
			*consulted = true
		}
		switch verb(args) {
		case "run":
			return "id", nil
		case "port":
			return "127.0.0.1:5599", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)
	p.password = fixturePassword
	return p
}

const postgresRequirements = `{"protocolVersion":2,"databases":` +
	`[{"name":"primary","engine":"postgres","schemas":["app"]}]}`

// --- policy precedence ----------------------------------------------------

// The CLI planner's precedence was: --infra flag > workspace test.infra config >
// CI default require > auto. Core now merges the first two into ONE param value
// before the task runs, so this resolves that value and adds the environment
// default.
func TestResolveModePrecedence(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "the-mode-resolves-by-a-fixed-precedence")
	t.Setenv(EnvCI, "")

	explicit := newFixture(t, map[string]any{ParamInfra: "skip"})
	if got := ResolveMode(explicit.ctx, false); got != pdb.TestModeSkip {
		t.Errorf("explicit param: got %q, want skip", got)
	}
	// An unrecognized value falls through to the environment default rather
	// than failing the run.
	bogus := newFixture(t, map[string]any{ParamInfra: "bogus"})
	if got := ResolveMode(bogus.ctx, false); got != pdb.TestModeAuto {
		t.Errorf("invalid value fallthrough: got %q, want auto", got)
	}
	if got := ResolveMode(bogus.ctx, true); got != pdb.TestModeRequire {
		t.Errorf("CI default: got %q, want require", got)
	}
	empty := newFixture(t, nil)
	if got := ResolveMode(empty.ctx, false); got != pdb.TestModeAuto {
		t.Errorf("local default: got %q, want auto", got)
	}
	if got := ResolveMode(empty.ctx, true); got != pdb.TestModeRequire {
		t.Errorf("CI default: got %q, want require", got)
	}
	if got := ResolveMode(nil, true); got != pdb.TestModeRequire {
		t.Errorf("nil context on CI: got %q, want require", got)
	}
}

// --- fail closed: docker is never touched outside auto --------------------

// CI and every non-auto mode must provision nothing AND must not reach the
// docker seam at all. This is the invariant that keeps a CI run from silently
// depending on a local container.
func TestUpFailsClosedOutsideAuto(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "no-infrastructure-is-created-outside-auto-mode")
	cases := []struct {
		name   string
		ci     string
		params map[string]any
	}{
		{"ci run", "true", nil},
		{"ci run with explicit auto", "true", map[string]any{ParamInfra: "auto"}},
		{"require mode", "", map[string]any{ParamInfra: "require"}},
		{"skip mode", "", map[string]any{ParamInfra: "skip"}},
		{"ci teardown", "true", map[string]any{ParamInfraDown: true}},
		{"skip mode teardown", "", map[string]any{ParamInfra: "skip", ParamInfraDown: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvCI, tc.ci)
			t.Setenv(EnvBindings, "")
			f := newFixture(t, tc.params)
			f.declareDatabases(t, "svc", postgresRequirements)

			consulted := false
			result := Up(f.ctx, stubProvisioner(&consulted))

			if consulted {
				t.Error("the docker seam was consulted outside auto mode — fail-closed broken")
			}
			if result.Outcome != OutcomeNotProvisioned {
				t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeNotProvisioned)
			}
			if _, ok := BindingFrom(f.ctx); ok {
				t.Error("a binding artifact was written despite provisioning being forbidden")
			}
		})
	}
}

// Under `require` the run must say WHY the tests are about to fail, without
// failing setup itself.
func TestUpRequireEmitsAnAdvisory(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, map[string]any{ParamInfra: "require"})
	f.declareDatabases(t, "svc", postgresRequirements)

	result := Up(f.ctx, stubProvisioner(nil))

	if diag.HasErrors(result.Diagnostics) {
		t.Errorf("require advisory produced an error diagnostic: %+v", result.Diagnostics)
	}
	found := false
	for _, d := range result.Diagnostics {
		if d.Code == DiagnosticNoProvider {
			found = true
		}
	}
	if !found {
		t.Errorf("no %s advisory: %+v", DiagnosticNoProvider, result.Diagnostics)
	}
}

// --- external binding always wins -----------------------------------------

// An externally supplied DATABASE_TEST_BINDINGS short-circuits everything: no
// docker call, no artifact, so the inherited value reaches the test task
// through the ambient environment and folds into its cache key via the test
// task's own `{"from": "env"}` input.
func TestUpExternalBindingWins(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "an-externally-supplied-binding-wins-over-any-provider")
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, `{"protocolVersion":1,"databases":{}}`)
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	consulted := false
	result := Up(f.ctx, stubProvisioner(&consulted))

	if result.Outcome != OutcomeExternal {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeExternal)
	}
	if consulted {
		t.Error("the docker seam was consulted despite an external binding — external must always win")
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("wrote a binding artifact that would shadow the external value")
	}
}

// --- zero cost for a unit-only project ------------------------------------

func TestUpZeroCostWithoutDatabases(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy", "a-closure-declaring-no-database-is-a-no-op")
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)

	consulted := false
	result := Up(f.ctx, stubProvisioner(&consulted))

	if result.Outcome != OutcomeNoDatabases {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeNoDatabases)
	}
	if consulted {
		t.Error("the docker seam was consulted for a project declaring no database")
	}
	if _, ok := ReadLease(f.artifacts); ok {
		t.Error("wrote a lease with nothing provisioned")
	}
}

// --- closure discovery ----------------------------------------------------

// The datasource set is the whole dependency CLOSURE's, merged and deduplicated
// — the same set the infra-aggregation task walks, so provisioning and the
// deployability manifest can never disagree about what a workload needs.
func TestClosureDatabasesMergesTheClosure(t *testing.T) {
	f := newFixture(t, nil)
	f.addDependency(t, "lib", "lib")
	f.declareDatabases(t, "svc", postgresRequirements)
	f.declareDatabases(t, "lib",
		`{"protocolVersion":2,"databases":[`+
			`{"name":"primary","engine":"postgres","schemas":["app"]},`+
			`{"name":"analytics","engine":"postgres","schemas":["metrics"]}]}`)

	databases, diags := ClosureDatabases(f.ctx)
	if diag.HasErrors(diags) {
		t.Fatalf("closure walk produced errors: %+v", diags)
	}
	names := make([]string, 0, len(databases))
	for _, db := range databases {
		names = append(names, db.Name)
	}
	// Deduplicated by (name, engine) and sorted by name for determinism.
	if got := strings.Join(names, ","); got != "analytics,primary" {
		t.Errorf("closure datasources = %q, want analytics,primary", got)
	}
}

// A malformed manifest is a diagnostic attributed to its project, never a
// crash and never a contribution.
func TestClosureDatabasesReportsMalformedManifest(t *testing.T) {
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", `{"protocolVersion":99}`)

	databases, diags := ClosureDatabases(f.ctx)
	if len(databases) != 0 {
		t.Errorf("a malformed manifest contributed %d datasources", len(databases))
	}
	if !diag.HasErrors(diags) {
		t.Fatal("a malformed manifest produced no diagnostic")
	}
	if !strings.Contains(diags[0].Message, "project svc") {
		t.Errorf("diagnostic is not attributed to its project: %q", diags[0].Message)
	}
}

// --- provisioned: artifacts, binding shape, credential confinement --------

func TestUpProvisionsAndWritesBothArtifacts(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	prov := stubProvisioner(nil)
	result := Up(f.ctx, prov)

	if result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q (%+v), want %q", result.Outcome, result.Diagnostics, OutcomeProvisioned)
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
	if !ok {
		t.Fatalf("binding is missing the declared datasource: %+v", tb.Databases)
	}
	if db.Engine != pdb.EnginePostgres || db.Schema != "app" {
		t.Errorf("datasource engine/schema = %q/%q, want postgres/app", db.Engine, db.Schema)
	}
	if db.Connection == nil || db.Connection.Port != 5599 {
		t.Errorf("datasource connection = %+v, want the provisioned endpoint", db.Connection)
	}

	lease, ok := ReadLease(f.artifacts)
	if !ok {
		t.Fatal("no lease was written beside the binding")
	}
	if !lease.Retain {
		t.Error("lease says the server must be torn down; the reuse contract retains it")
	}
	if lease.Digest != prov.cfg.digest() || lease.Container != prov.ContainerName(lease.Digest) {
		t.Errorf("lease does not identify the provisioned container: %+v", lease)
	}
}

// The credential reaches EXACTLY one file. Every other surface this task writes
// — the lease, the result data, every diagnostic — must be free of it, and the
// bindings artifact itself must be owner-only.
func TestUpConfinesTheCredentialToTheSensitiveArtifact(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "credential-confinement", "the-credential-is-confined-to-the-sensitive-artifact")
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	result := Up(f.ctx, stubProvisioner(nil))
	if result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}

	// The result data a task publishes to events, session records and the cache.
	data, err := json.Marshal(result.Data())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), fixturePassword) {
		t.Errorf("result data leaked the credential: %s", data)
	}
	for _, d := range result.Diagnostics {
		if strings.Contains(d.String(), fixturePassword) {
			t.Errorf("diagnostic leaked the credential: %s", d.String())
		}
	}

	// The lease is copied onto labels and filters; it may never carry one.
	leaseBytes, err := os.ReadFile(filepath.Join(f.artifacts, filepath.FromSlash(LeaseArtifact)))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(leaseBytes), fixturePassword) {
		t.Errorf("lease leaked the credential: %s", leaseBytes)
	}

	// The one file that DOES hold it is owner-only.
	assertOwnerOnlyArtifact(t, filepath.Join(f.artifacts, filepath.FromSlash(BindingsArtifact)))
	binding, _ := BindingFrom(f.ctx)
	if !strings.Contains(binding, fixturePassword) {
		t.Error("the bindings artifact does not carry the credential; the fixture proves nothing")
	}
}

// The producer PRE-CREATES the sensitive artifact at 0600; writing must not
// replace that inode with one created at the process umask.
func TestUpWritesInPlaceIntoTheReservedArtifact(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	path := filepath.Join(f.artifacts, filepath.FromSlash(BindingsArtifact))
	// Reserve it exactly as the orchestrator's arm() does.
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if result := Up(f.ctx, stubProvisioner(nil)); result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the reserved 0600 artifact was replaced rather than written in place")
	}
}

// A reservation that grants more than its owner, such as a file left at 0644
// or one that inherited its directory's access list on Windows, is
// restricted to the owner before the credential lands in it, still in place.
func TestUpRestrictsALooserReservationBeforeWritingTheCredential(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.declareDatabases(t, "svc", postgresRequirements)

	path := filepath.Join(f.artifacts, filepath.FromSlash(BindingsArtifact))
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if private, err := ownerperm.OwnerOnly(path); err != nil || private {
		t.Fatalf("precondition: the reservation is owner-only (%v, %v); the test proves nothing", private, err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if result := Up(f.ctx, stubProvisioner(nil)); result.Outcome != OutcomeProvisioned {
		t.Fatalf("outcome = %q, want provisioned", result.Outcome)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("the reservation was replaced rather than written in place")
	}
	assertOwnerOnlyArtifact(t, path)
	if binding, _ := BindingFrom(f.ctx); !strings.Contains(binding, fixturePassword) {
		t.Error("the bindings artifact does not carry the credential; the fixture proves nothing")
	}
}

// assertOwnerOnlyArtifact fails t unless nobody but the owner may access path.
// On Unix the mode must also be exactly 0600. Windows reports 0666 for every
// writable file, so there the access list is the whole check.
func assertOwnerOnlyArtifact(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("bindings artifact mode = %04o, want 0600", perm)
		}
	}
	private, err := ownerperm.OwnerOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if !private {
		t.Errorf("bindings artifact %s grants access beyond its owner", path)
	}
}

// An empty reservation is "nothing was provisioned", never "an empty binding".
func TestBindingFromTreatsAnEmptyReservationAsAbsent(t *testing.T) {
	f := newFixture(t, nil)
	path := filepath.Join(f.artifacts, filepath.FromSlash(BindingsArtifact))
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("an empty reservation was reported as a binding")
	}
	if _, ok := BindingFrom(nil); ok {
		t.Error("a nil context reported a binding")
	}
}

// --- teardown -------------------------------------------------------------

// `--infra-down` reaps every workspace-labeled container and provisions
// nothing, exactly as the CLI planner's teardown path did.
func TestUpInfraDownTearsDownAndProvisionsNothing(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, map[string]any{ParamInfraDown: true})
	f.declareDatabases(t, "svc", postgresRequirements)

	var removed []string
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			return "a\nb", nil
		case "rm":
			removed = append(removed, args[2:]...)
			return "", nil
		}
		return "", nil
	}}
	prov := testProvisioner(fd, okProbe)

	result := Up(f.ctx, prov)

	if result.Outcome != OutcomeTornDown {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeTornDown)
	}
	if strings.Join(removed, ",") != "a,b" {
		t.Errorf("teardown removed %v, want every workspace-labeled container", removed)
	}
	if findCall(fd.calls, "run") != nil {
		t.Error("--infra-down provisioned a container")
	}
	if _, ok := BindingFrom(f.ctx); ok {
		t.Error("--infra-down wrote a binding artifact")
	}
	lease, ok := ReadLease(f.artifacts)
	if !ok || lease.Retain {
		t.Errorf("teardown lease = %+v, want a non-retaining record", lease)
	}
}

// --- finalizer ------------------------------------------------------------

// The ordinary run RETAINS its server: tearing it down after every `putnami
// test` would trade a container start (image pull included) for nothing, and
// the reuse digest is what makes the next run's server interchangeable.
func TestDownRetainsAReusableServer(t *testing.T) {
	f := newFixture(t, nil)
	if err := writeLease(f.artifacts, Lease{
		Version: LeaseVersion, InvocationID: "inv-fixture", Provider: providerID,
		Label: labelKey, Digest: "deadbeefdeadbeef", Container: "putnami-test-pg-x", Retain: true,
	}); err != nil {
		t.Fatal(err)
	}

	fd := &fakeDocker{}
	result := Down(f.ctx, testProvisioner(fd, okProbe))

	if result.Outcome != OutcomeRetained {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeRetained)
	}
	if len(fd.calls) != 0 {
		t.Errorf("the finalizer touched docker for a retained server: %v", fd.calls)
	}
}

// A non-retaining lease is the `--infra-down` path: the sweep is idempotent and
// catches whatever a concurrent run started after Up's own teardown.
func TestDownSweepsANonRetainingLease(t *testing.T) {
	f := newFixture(t, nil)
	if err := writeLease(f.artifacts, Lease{
		Version: LeaseVersion, InvocationID: "inv-fixture", Provider: providerID,
		Label: labelKey, Retain: false,
	}); err != nil {
		t.Fatal(err)
	}

	var removed []string
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			return "late", nil
		case "rm":
			removed = append(removed, args[2:]...)
			return "", nil
		}
		return "", nil
	}}

	result := Down(f.ctx, testProvisioner(fd, okProbe))

	if result.Outcome != OutcomeTornDown {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeTornDown)
	}
	if strings.Join(removed, ",") != "late" {
		t.Errorf("sweep removed %v, want the late container", removed)
	}
}

// No lease means the producer provisioned nothing; the finalizer must not
// invent work.
func TestDownWithoutALeaseDoesNothing(t *testing.T) {
	f := newFixture(t, nil)
	fd := &fakeDocker{}
	if result := Down(f.ctx, testProvisioner(fd, okProbe)); result.Outcome != OutcomeNoDatabases {
		t.Errorf("outcome = %q, want %q", result.Outcome, OutcomeNoDatabases)
	}
	if len(fd.calls) != 0 {
		t.Errorf("the finalizer touched docker with no lease: %v", fd.calls)
	}
}

// A lease from another format version is not one this build wrote, and acting
// on it would tear down resources whose ownership cannot be established.
func TestReadLeaseRejectsAForeignVersion(t *testing.T) {
	f := newFixture(t, nil)
	path := filepath.Join(f.artifacts, filepath.FromSlash(LeaseArtifact))
	if err := os.WriteFile(path, []byte(`{"version":99,"invocationId":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadLease(f.artifacts); ok {
		t.Error("a foreign-version lease was accepted")
	}
}

// --- unarmed --------------------------------------------------------------

// Without an invocation locator there is no private tree, so both halves must
// no-op rather than write somewhere else.
func TestUnarmedRelationDoesNothing(t *testing.T) {
	t.Setenv(EnvCI, "")
	t.Setenv(EnvBindings, "")
	f := newFixture(t, nil)
	f.ctx.Invocation = nil
	f.declareDatabases(t, "svc", postgresRequirements)

	consulted := false
	if result := Up(f.ctx, stubProvisioner(&consulted)); result.Outcome != OutcomeUnarmed {
		t.Errorf("Up outcome = %q, want %q", result.Outcome, OutcomeUnarmed)
	}
	if consulted {
		t.Error("an unarmed producer consulted the docker seam")
	}
	if result := Down(f.ctx, stubProvisioner(nil)); result.Outcome != OutcomeUnarmed {
		t.Errorf("Down outcome = %q, want %q", result.Outcome, OutcomeUnarmed)
	}
}

// --- binding synthesis ----------------------------------------------------

func TestSynthesizeBindingIsPostgresOnly(t *testing.T) {
	conn := pdb.Connection{Host: "db", Port: 5432}
	_, err := SynthesizeBinding(TestPolicy{Mode: pdb.TestModeAuto},
		[]infra.Database{{Name: "d", Engine: infra.EngineMySQL, Schemas: []string{"app"}}}, conn)
	if err == nil {
		t.Error("expected an error for a non-postgres engine (postgres-only in v1)")
	}
}

func TestSynthesizeBindingDefaultsTheSchema(t *testing.T) {
	conn := pdb.Connection{Host: "db", Port: 5432}
	raw, err := SynthesizeBinding(TestPolicy{Mode: pdb.TestModeRequire},
		[]infra.Database{{Name: "primary", Engine: infra.EnginePostgres}}, conn)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("synthesized binding failed validation: %v", diags)
	}
	if tb.Databases["primary"].Schema != "public" {
		t.Errorf("schema = %q, want the postgres default public", tb.Databases["primary"].Schema)
	}
	if tb.Mode != pdb.TestModeRequire {
		t.Errorf("mode = %q, want the resolved policy to travel with the binding", tb.Mode)
	}
}

// A provider that states no isolation or reuse must produce a binding that
// states none either, so the runtime provider keeps its own defaults. This is
// what makes the docker artifact byte-identical to the one it wrote before a
// second provider existed.
func TestSynthesizeBindingOmitsAnUnstatedPolicy(t *testing.T) {
	conn := pdb.Connection{Host: "db", Port: 5432}
	raw, err := SynthesizeBinding((&Provisioner{}).bindingDefaults(),
		[]infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"app"}}}, conn)
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if strings.Contains(raw, "isolation") || strings.Contains(raw, "reuse") {
		t.Errorf("docker binding states a policy it never stated before: %s", raw)
	}
}

// --- the pool a test datasource gets ---------------------------------------

// Every binding this package emits bounds the pool, whichever provider handed
// back the connection: 10 connections held idle for 30 minutes is the framework
// default a long-lived service wants, and a workload declaring 24 datasources
// would open 240 of them to use one or two at a time.
func TestSynthesizeBindingBoundsTheTestPool(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "test-environment-policy",
		"every-emitted-binding-bounds-the-test-pool")
	for _, policy := range []TestPolicy{
		(&Provisioner{}).bindingDefaults(),
		(&ProvidedServer{}).bindingDefaults(),
	} {
		raw, err := SynthesizeBinding(policy,
			[]infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"app"}}},
			pdb.Connection{Host: "db", Port: 5432})
		if err != nil {
			t.Fatalf("synthesize: %v", err)
		}
		tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
		if diag.HasErrors(diags) {
			t.Fatalf("synthesized binding failed validation: %v", diags)
		}
		params := tb.Databases["primary"].Connection.Params
		want := map[string]string{
			"pool_max_conns":          "2",
			"pool_min_conns":          "0",
			"pool_max_conn_idle_time": "5s",
		}
		for key, value := range want {
			if params[key] != value {
				t.Errorf("params[%q] = %q, want %q (params %v)", key, params[key], value, params)
			}
		}
	}
}

// TWO, not one. A suite that holds a transaction on one connection and runs a
// query beside it needs a second; a pool of 1 turns that into a deadlock rather
// than a wait.
func TestSynthesizeBindingLeavesRoomForAQueryBesideATransaction(t *testing.T) {
	if testPoolMaxConns != "2" {
		t.Fatalf("test pool ceiling = %s, want 2 — a transaction plus one query beside it needs two "+
			"connections, and 1 deadlocks where 2 waits", testPoolMaxConns)
	}
}

// A connection that already states a pool parameter keeps its own value, per
// key: that is how PUTNAMI_TEST_PG_URL overrides a default it disagrees with
// without surrendering the two it does not.
func TestSynthesizeBindingKeepsAStatedPoolValue(t *testing.T) {
	raw, err := SynthesizeBinding(TestPolicy{Mode: pdb.TestModeRequire},
		[]infra.Database{{Name: "primary", Engine: infra.EnginePostgres, Schemas: []string{"app"}}},
		pdb.Connection{Host: "db", Port: 5432, Params: map[string]string{
			"pool_max_conns": "5",
			"sslmode":        "disable",
		}})
	if err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	tb, diags := pdb.ParseAndValidateTestBinding([]byte(raw))
	if diag.HasErrors(diags) {
		t.Fatalf("synthesized binding failed validation: %v", diags)
	}
	params := tb.Databases["primary"].Connection.Params
	if params["pool_max_conns"] != "5" {
		t.Errorf("pool_max_conns = %q, want the stated 5", params["pool_max_conns"])
	}
	if params["pool_min_conns"] != "0" || params["pool_max_conn_idle_time"] != "5s" {
		t.Errorf("an override of one key dropped the others: %v", params)
	}
	if params["sslmode"] != "disable" {
		t.Errorf("sslmode = %q, want the unrelated driver option to survive", params["sslmode"])
	}
}

// The provider hands its connection back once and it is mapped onto every
// datasource, so filling the defaults in place would make them a property of the
// provider rather than of this binding.
func TestSynthesizeBindingDoesNotMutateTheProvidedConnection(t *testing.T) {
	conn := pdb.Connection{Host: "db", Port: 5432}
	if _, err := SynthesizeBinding(TestPolicy{Mode: pdb.TestModeRequire},
		[]infra.Database{{Name: "primary", Engine: infra.EnginePostgres}}, conn); err != nil {
		t.Fatalf("synthesize: %v", err)
	}
	if conn.Params != nil {
		t.Errorf("the caller's connection grew params %v", conn.Params)
	}
}
