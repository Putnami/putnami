package test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/dbtestenv"
	"go.putnami.dev/sdk/extension/hostenv"

	"go.putnami.dev/protocol/features/spectest"
)

func goworkValues(env []string) []string {
	var vals []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, "GOWORK="); ok {
			vals = append(vals, v)
		}
	}
	return vals
}

func physicalTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// buildTestEnv must not let a leaked GOWORK=off reach `go test`. GOWORK=off
// disables workspace resolution, so each framework `require ... v0.0.0`
// placeholder escapes to the private GOPROXY as a doomed 404. The env must strip
// the leaked entry and re-point GOWORK at the governing go.work.
func TestBuildTestEnv_StripsLeakedGoWorkOff(t *testing.T) {
	root := t.TempDir()
	gowork := filepath.Join(root, "go.work")
	if err := os.WriteFile(gowork, []byte("go 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(root, "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")

	env, err := buildTestEnv(nil, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	wantGoWork := physicalTestPath(t, gowork)
	if vals := goworkValues(env); len(vals) != 1 || vals[0] != wantGoWork {
		t.Fatalf("GOWORK = %v, want exactly [%q]", vals, wantGoWork)
	}
	if slices.Contains(env, "GOWORK=off") {
		t.Errorf("leaked GOWORK=off survived into the test env: %v", env)
	}
	// The existing FORCE_COLOR wiring must be preserved by the new base env.
	if !slices.Contains(env, "FORCE_COLOR=1") {
		t.Errorf("FORCE_COLOR=1 not preserved: %v", env)
	}
}

// With no governing go.work the module is genuinely standalone: a leaked
// GOWORK=off is dropped (not re-asserted) and no GOWORK is added.
func TestBuildTestEnv_StandaloneDropsGoWorkOff(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "standalone")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOWORK", "off")

	env, err := buildTestEnv(nil, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	if vals := goworkValues(env); len(vals) != 0 {
		t.Errorf("expected no GOWORK entry for a standalone module, got %v", vals)
	}
	if slices.Contains(env, "GOWORK=off") {
		t.Errorf("leaked GOWORK=off survived into the test env: %v", env)
	}
}

// envNames returns the variable names present in a KEY=VALUE env slice.
func envNames(env []string) []string {
	names := make([]string, 0, len(env))
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return names
}

// A `go test` subprocess must not inherit the HARNESS HOST's platform identity.
// On a CI worker that is itself a Cloud Run service the host exports K_SERVICE;
// application code reads it as the production signal and refuses test-only
// behavior, failing deterministically there and nowhere else.
func TestBuildTestEnv_ScrubsHostPlatformIdentity(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-host-identity-scrub", "the-host-platform-identity-is-scrubbed-from-the-test-subprocess")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv("K_REVISION", "ci-worker-00042-abc")
	t.Setenv("K_CONFIGURATION", "ci-worker")
	t.Setenv("GAE_ENV", "standard")
	t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "fn")

	env, err := buildTestEnv(nil, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	names := envNames(env)
	for _, name := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION", "GAE_ENV", "AWS_LAMBDA_FUNCTION_NAME"} {
		if slices.Contains(names, name) {
			t.Errorf("%s leaked into the `go test` environment", name)
		}
	}
}

// On a hosted run the engine tells the job that its dependencies are already
// downloaded. The `go test` process, and the tests it runs, keep the go settings
// that forbid a download and see neither variable that describes the job: a
// repository's tests are not jobs of the run.
func TestBuildTestEnv_HostedRunKeepsDownloadsOffAndScrubsTheJobVariables(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-host-identity-scrub",
		"a-test-process-runs-offline-and-sees-no-job-variable")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	t.Setenv("PUTNAMI_JOB_CREDENTIAL_FD", "7")
	t.Setenv("GOPROXY", "https://proxy.golang.org,direct")
	t.Setenv("GONOPROXY", "corp.example/*")
	t.Setenv("GOFLAGS", "-trimpath")

	env, err := buildTestEnv(nil, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	names := envNames(env)
	for _, name := range hostenv.JobVars() {
		if slices.Contains(names, name) {
			t.Errorf("%s leaked into the `go test` environment", name)
		}
	}
	for _, want := range []string{"GOPROXY=off", "GONOPROXY=none", "GOFLAGS=-trimpath -mod=readonly", "GOTOOLCHAIN=local"} {
		if !slices.Contains(env, want) {
			t.Errorf("the `go test` environment lacks %s: a test process could download a module", want)
		}
	}
}

// The scrub must cost nothing the test env already guaranteed: the harness
// wiring, the toolchain env, credentials, and the capability-bearing GCP
// project selector all survive.
func TestBuildTestEnv_ScrubPreservesEverythingElse(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-host-identity-scrub", "every-other-inherited-variable-is-preserved")
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.work"), []byte("go 1.24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(root, "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("K_SERVICE", "ci-worker")
	t.Setenv("CGO_ENABLED", "0")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/secrets/adc.json")
	t.Setenv(envDatabaseTestBindings, `{"protocolVersion":1,"databases":{}}`)

	env, err := buildTestEnv(nil, proj, true, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	for _, want := range []string{
		"FORCE_COLOR=1",
		"APP_ENV=test",
		// --race must still override an ambient CGO_ENABLED=0.
		"CGO_ENABLED=1",
		// Capability, not identity: a client SDK needs it to address an API.
		"GOOGLE_CLOUD_PROJECT=my-project",
		// Credentials are explicitly out of scope for the scrub.
		"GOOGLE_APPLICATION_CREDENTIALS=/secrets/adc.json",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("%s missing from the scrubbed test env", want)
		}
	}
	if vals := goworkValues(env); len(vals) != 1 || vals[0] != physicalTestPath(t, filepath.Join(root, "go.work")) {
		t.Errorf("GOWORK = %v, want the governing go.work", vals)
	}
	if !slices.Contains(envNames(env), "PATH") {
		t.Error("inherited PATH did not survive the scrub")
	}
}

// A host with no platform identity — every developer machine — must see exactly
// the environment it saw before the scrub existed.
func TestBuildTestEnv_ScrubIsNoopWithoutPlatformIdentity(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "test-host-identity-scrub", "the-scrub-is-a-noop-when-there-is-no-platform-identity")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(hostenv.PlatformIdentityVars(), hostenv.JobVars()...) {
		if _, ok := os.LookupEnv(name); ok {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}

	env, err := buildTestEnv(nil, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	base := toolchain.WorkspaceBuildEnv(os.Environ(), proj, "")
	baseNames := envNames(base)
	for _, name := range baseNames {
		if !slices.Contains(envNames(env), name) {
			t.Errorf("%s was dropped from an environment carrying no platform identity", name)
		}
	}
}

func TestBuildTestEnv_RaceEnablesCGO(t *testing.T) {
	proj := filepath.Join(t.TempDir(), "race")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CGO_ENABLED", "0")

	env, err := buildTestEnv(nil, proj, true, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	var cgoValues []string
	for _, entry := range env {
		if strings.HasPrefix(entry, "CGO_ENABLED=") {
			cgoValues = append(cgoValues, entry)
		}
	}
	if !slices.Equal(cgoValues, []string{"CGO_ENABLED=1"}) {
		t.Fatalf("CGO_ENABLED entries = %v, want exactly [CGO_ENABLED=1]", cgoValues)
	}
}

// --- invocation-scoped binding precedence ---------

// invocationFixture writes a provisioned bindings artifact into a private
// artifact root and returns the job context that locates it, exactly as the
// orchestrator delivers it to a finalizes relation's consumer.
func invocationFixture(t *testing.T, binding string) *pctx.Context {
	t.Helper()
	artifacts := filepath.Join(t.TempDir(), "artifacts")
	path := filepath.Join(artifacts, filepath.FromSlash(dbtestenv.BindingsArtifact))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(binding), 0o600); err != nil {
		t.Fatal(err)
	}
	return &pctx.Context{Invocation: &pctx.Invocation{ID: "inv-test", ArtifactRoot: artifacts}}
}

const provisionedBinding = `{"protocolVersion":1,"mode":"auto","databases":` +
	`{"primary":{"engine":"postgres","schema":"app","connection":` +
	`{"host":"127.0.0.1","port":55001,"user":"putnami","password":"local-dev-secret","database":"putnami_test"}}}}`

// The provisioned artifact reaches `go test` as DATABASE_TEST_BINDINGS. It is
// the only delivery path for a credential now: the CLI used to inject the same
// value through a private subprocess-env seam it then had to hide from its own
// cache key.
func TestBuildTestEnv_UsesTheProvisionedInvocationArtifact(t *testing.T) {
	t.Setenv(dbtestenv.EnvBindings, "")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	env, err := buildTestEnv(invocationFixture(t, provisionedBinding), proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	if !slices.Contains(env, envDatabaseTestBindings+"="+provisionedBinding) {
		t.Errorf("the provisioned binding did not reach the test env: %v",
			bindingValues(env))
	}
}

// An externally supplied binding still wins over the provisioned artifact. It
// is today's CI contract and the value this task declares as a cache input, so
// letting a local container shadow it would make a CI run key on one binding
// and execute against another.
func TestBuildTestEnv_ExternalBindingBeatsTheArtifact(t *testing.T) {
	t.Setenv(dbtestenv.EnvBindings, "external-binding")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	env, err := buildTestEnv(invocationFixture(t, provisionedBinding), proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	for _, value := range bindingValues(env) {
		if value == provisionedBinding {
			t.Errorf("the provisioned artifact shadowed the external binding: %v", bindingValues(env))
		}
	}
}

// A run with no provisioned artifact must reach `go test` exactly as it did
// before the slice: no binding entry at all when the project declares no conf
// datasource.
func TestBuildTestEnv_NoArtifactAddsNoBinding(t *testing.T) {
	t.Setenv(dbtestenv.EnvBindings, "")
	proj := filepath.Join(t.TempDir(), "svc")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}

	env, err := buildTestEnv(&pctx.Context{}, proj, false, "")
	if err != nil {
		t.Fatalf("buildTestEnv: %v", err)
	}
	if vals := bindingValues(env); len(vals) != 0 {
		t.Errorf("binding entries = %v, want none", vals)
	}
}

// bindingValues returns every NON-EMPTY DATABASE_TEST_BINDINGS assignment in
// env, in order. The empty assignment an unset-but-present variable leaves in
// os.Environ() is not a binding, and os/exec keeps the last duplicate anyway.
func bindingValues(env []string) []string {
	var vals []string
	for _, e := range env {
		if v, ok := strings.CutPrefix(e, envDatabaseTestBindings+"="); ok && v != "" {
			vals = append(vals, v)
		}
	}
	return vals
}

func TestWithGoTempDirRoutesGoWorkDirectoriesIntoScratch(t *testing.T) {
	parent := t.TempDir()
	env, err := withGoTempDir([]string{"PATH=/bin", "GOTMPDIR="}, parent)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(parent, "gotmp")
	if !slices.Equal(env, []string{"PATH=/bin", "GOTMPDIR=" + want}) {
		t.Fatalf("GOTMPDIR must point into the scratch, env: %v", env)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the go work directory parent must exist: %v", err)
	}

	// The last entry is the one exec uses, so it is the one replaced.
	env, err = withGoTempDir([]string{"GOTMPDIR=/a", "GOTMPDIR="}, parent)
	if err != nil || !slices.Equal(env, []string{"GOTMPDIR=/a", "GOTMPDIR=" + want}) {
		t.Fatalf("the effective empty GOTMPDIR must be replaced, env: %v, err: %v", env, err)
	}

	explicit := []string{"GOTMPDIR=/chosen"}
	env, err = withGoTempDir(explicit, parent)
	if err != nil || len(env) != 1 || env[0] != "GOTMPDIR=/chosen" {
		t.Fatalf("an explicit GOTMPDIR must be kept, env: %v, err: %v", env, err)
	}
}
