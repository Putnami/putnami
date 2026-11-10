package rust

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
)

// runWorkspaceJob runs a workspace job of the runtime over the workspace
// projects refs, and returns what it emitted.
func runWorkspaceJob(t *testing.T, root, name string, params job.Params, refs ...job.ProjectRef) (string, string, error) {
	t.Helper()
	s := stack(t)
	jobs := OfflineCargo(Jobs(s))
	defer keepEnv(cargoNetOfflineEnv)()
	emitted, err := os.CreateTemp(t.TempDir(), "emitted")
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = emitted
	status, _, runErr := jobs[name](&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}, jsonl.New(), nil)
	os.Stdout = stdout
	_ = emitted.Close()
	text, err := os.ReadFile(emitted.Name())
	if err != nil {
		t.Fatal(err)
	}
	return status, string(text), runErr
}

// crateRefs are the workspace projects of the fixture: the crates of the root
// Cargo workspace and gen, a crate of its own with no Cargo.lock.
func crateRefs(root string) []job.ProjectRef {
	var refs []job.ProjectRef
	for _, path := range []string{"crates/api", "crates/pricing", "tools/gen"} {
		refs = append(refs, crateRef(root, path, `{}`))
	}
	return refs
}

func crateRef(root, path, config string) job.ProjectRef {
	return job.ProjectRef{Name: path, Path: path, FullPath: filepath.Join(root, path), Extensions: []string{ExtensionName}, Config: []byte(config)}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

// stubCredential makes workspace-fetch read the credential through read, and
// restores the SDK's reader when the test ends.
func stubCredential(t *testing.T, read func() (*registry.Credential, error)) {
	t.Helper()
	restore := readJobCredential
	readJobCredential = read
	t.Cleanup(func() { readJobCredential = restore })
}

func TestWorkspaceFetchDownloadsTheLockedCratesAndRunsNoneOfTheirCode(t *testing.T) {
	spectest.Proves(t, feature, "fetches-the-locked-crates", "workspace-fetch-runs-cargo-fetch-locked-at-each-stack-root")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	// A hosted run's engine runs workspace-fetch with the network.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	var startedBefore []string
	stubCredential(t, func() (*registry.Credential, error) {
		startedBefore = readLines(t, log)
		return &registry.Credential{Bearer: "secret", Hosts: []string{"put.putnami.dev"}}, nil
	})
	lockBefore, err := os.ReadFile(filepath.Join(root, "Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}

	status, emitted, err := runWorkspaceJob(t, root, WorkspaceFetchJobName, nil, crateRefs(root)...)
	if status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v\n%s", status, err, emitted)
	}
	if startedBefore != nil {
		t.Errorf("a process started before the credential was read: %q", startedBefore)
	}
	if lines := readLines(t, log); len(lines) != 1 || lines[0] != root+": cargo fetch --locked" {
		t.Fatalf("one cargo fetch --locked at the stack root that holds a Cargo.lock, got %q", lines)
	}
	env := readLines(t, log+".env")[0]
	rustc := filepath.Join(filepath.Dir(log), "rustc")
	if !strings.Contains(env, " RUSTC_WRAPPER= RUSTC_WORKSPACE_WRAPPER= RUSTC="+rustc) || strings.Contains(env, "secret") {
		t.Errorf("the fetch runs the selected rustc and no wrapper, and gets no credential: %q", env)
	}
	gen := filepath.Join(root, "tools/gen")
	if !strings.Contains(emitted, CodeLockfileMissing) || !strings.Contains(emitted, gen) {
		t.Errorf("the root with no Cargo.lock is named in a warning:\n%s", emitted)
	}
	if _, err := os.Stat(filepath.Join(gen, "Cargo.lock")); !os.IsNotExist(err) {
		t.Errorf("the fetch wrote a Cargo.lock: %v", err)
	}
	if lockAfter, _ := os.ReadFile(filepath.Join(root, "Cargo.lock")); string(lockAfter) != string(lockBefore) {
		t.Errorf("the fetch changed Cargo.lock: %q", lockAfter)
	}
}

func TestAnUnreadableCredentialFailsTheFetchBeforeCargoRuns(t *testing.T) {
	spectest.Proves(t, feature, "fetches-the-locked-crates", "workspace-fetch-runs-cargo-fetch-locked-at-each-stack-root")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	stubCredential(t, func() (*registry.Credential, error) { return nil, errors.New("descriptor 9 is not open") })
	status, emitted, err := runWorkspaceJob(t, root, WorkspaceFetchJobName, nil, crateRefs(root)...)
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeCargoFetchFailed) || !strings.Contains(emitted, "descriptor 9") {
		t.Fatalf("fetch: %s %v\n%s", status, err, emitted)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("cargo ran: %q", lines)
	}
}

func TestAFailedFetchNamesTheStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "fetches-the-locked-crates", "workspace-fetch-runs-cargo-fetch-locked-at-each-stack-root")
	root := fixture(t)
	stubTools(t, root, "1.82.0", junitPass)
	stubCredential(t, func() (*registry.Credential, error) { return nil, nil })
	t.Setenv("STUB_FETCH_FAILS", "1")
	status, emitted, err := runWorkspaceJob(t, root, WorkspaceFetchJobName, nil, crateRefs(root)...)
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeCargoFetchFailed) || !strings.Contains(emitted, "cargo fetch --locked in "+root) {
		t.Fatalf("fetch: %s %v\n%s", status, err, emitted)
	}
}

func TestOfflineJobsRunCargoOfflineWithTheHomeTheFetchFilled(t *testing.T) {
	spectest.Proves(t, feature, "offline-tasks-stay-offline", "offline-jobs-run-cargo-offline-with-the-home-the-fetch-filled")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	home := t.TempDir()
	t.Setenv("CARGO_HOME", home)
	defer keepEnv(cargoNetOfflineEnv)()
	_ = os.Unsetenv(cargoNetOfflineEnv)
	stubCredential(t, func() (*registry.Credential, error) { return nil, nil })

	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, emitted, err := runWorkspaceJob(t, root, WorkspaceFetchJobName, nil, crateRefs(root)...); status != "OK" || err != nil {
		t.Fatalf("fetch: %s %v\n%s", status, err, emitted)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	for _, task := range []string{stacks.TaskLint, stacks.TaskTest, stacks.TaskBuild, RunJobName} {
		if status, _, _, emitted, err := runJobOutput(t, root, "crates/api", task); status != "OK" || err != nil {
			t.Fatalf("%s: %s %v\n%s", task, status, err, emitted)
		}
	}
	env := readLines(t, log+".env")
	if len(env) < 5 {
		t.Fatalf("cargo ran %d times: %q", len(env), env)
	}
	if !strings.Contains(env[0], "cargo fetch CARGO_NET_OFFLINE=unset CARGO_HOME="+home+" ") {
		t.Errorf("the fetch has the network and the environment's cargo home: %q", env[0])
	}
	for _, line := range env[1:] {
		if !strings.Contains(line, " CARGO_NET_OFFLINE=true CARGO_HOME="+home+" ") {
			t.Errorf("an offline step reaches the network or another cargo home: %q", line)
		}
	}
	if value, set := os.LookupEnv(cargoNetOfflineEnv); set {
		t.Errorf("the job left %s=%s in the test process", cargoNetOfflineEnv, value)
	}

	// cargo publish uploads: it keeps the network.
	seen := map[string]string{}
	record := func(name string) cli.JobFunc {
		return func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error) {
			seen[name] = os.Getenv(cargoNetOfflineEnv)
			return "OK", nil, nil
		}
	}
	jobs := OfflineCargo(map[string]cli.JobFunc{"publish-crates": record("publish-crates")})
	_, _, _ = jobs["publish-crates"](&pctx.Context{}, jsonl.New(), nil)
	jobs = OfflineCargo(map[string]cli.JobFunc{"package-crate": record("package-crate")})
	_, _, _ = jobs["package-crate"](&pctx.Context{}, jsonl.New(), nil)
	if seen["publish-crates"] != "" || seen["package-crate"] != "true" {
		t.Errorf("offline, publish keeps the network and package does not: %v", seen)
	}
}

func TestAnOnlineJobLeavesCargosNetworkAsItIs(t *testing.T) {
	spectest.Proves(t, feature, "offline-tasks-stay-offline", "offline-jobs-run-cargo-offline-with-the-home-the-fetch-filled")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, _, _, emitted, err := runJobOutput(t, root, "crates/api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v\n%s", status, err, emitted)
	}
	if env := readLines(t, log+".env"); len(env) != 1 || !strings.Contains(env[0], "CARGO_NET_OFFLINE=unset") {
		t.Errorf("env = %q", env)
	}
}

func TestWorkspaceInstallFetchesEachStackRootTheWorkspaceIncludes(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-runs-cargo-fetch", "workspace-install-fetches-each-stack-root-the-workspace-includes")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	// The test tools come from the stub server, whatever the machine holds.
	t.Setenv("PATH", filepath.Dir(log)+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv(putnamihome.Env, t.TempDir())
	downloads := stubTestTools(t)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	writeFile(t, root, "putnami.workspace.json", `{"disable": {"tags": ["e2e"]}}`)
	writeFile(t, root, "samples/demo/Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\n")
	writeFile(t, root, "samples/demo/src/main.rs", "fn main() {}\n")
	writeFile(t, root, "other/Cargo.toml", "[package]\nname = \"other\"\nversion = \"0.1.0\"\n")
	foreign := crateRef(root, "other", `{}`)
	foreign.Extensions = []string{"@putnami/typescript"}
	refs := append(crateRefs(root), crateRef(root, "samples/demo", `{"tags": ["e2e"]}`), foreign)

	gen := filepath.Join(root, "tools/gen")
	var emitted string
	install := func(params job.Params) []string {
		t.Helper()
		_ = os.Remove(log)
		var status string
		var err error
		if status, emitted, err = runWorkspaceJob(t, root, WorkspaceInstallJobName, params, refs...); status != "OK" || err != nil {
			t.Fatalf("install: %s %v\n%s", status, err, emitted)
		}
		return readLines(t, log)
	}
	want := []string{
		root + ": cargo fetch --locked --offline --quiet",
		root + ": cargo fetch",
		gen + ": cargo fetch",
	}
	if got := install(nil); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("each included stack root fetches, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(gen, "Cargo.lock")); err != nil {
		t.Errorf("the install wrote the missing Cargo.lock: %v", err)
	}
	if downloads.Load() != 2 {
		t.Errorf("the test tools were downloaded %d times, want once each:\n%s", downloads.Load(), emitted)
	}
	want = []string{root + ": cargo fetch --locked --offline --quiet", gen + ": cargo fetch --locked --offline --quiet"}
	if got := install(nil); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("a current root is checked and not fetched again, got %q", got)
	}
	want = []string{root + ": cargo fetch", gen + ": cargo fetch"}
	if got := install(job.Params{"force": json.RawMessage(`true`)}); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the force flag fetches a current root, got %q", got)
	}
	if downloads.Load() != 2 {
		t.Errorf("the installed test tools were downloaded again: %d", downloads.Load())
	}
}

func TestAnOfflineWorkspaceInstallChecksTheCacheAndWritesNothing(t *testing.T) {
	spectest.Proves(t, feature, "workspace-install-runs-cargo-fetch", "an-offline-workspace-install-checks-cargos-home-and-writes-nothing")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	t.Setenv(putnamihome.Env, t.TempDir())
	downloads := stubTestTools(t)
	writeFile(t, root, ".stub-fetched", "")
	status, emitted, err := runWorkspaceJob(t, root, WorkspaceInstallJobName, nil, crateRefs(root)...)
	if status != "OK" || err != nil {
		t.Fatalf("install: %s %v\n%s", status, err, emitted)
	}
	if lines := readLines(t, log); len(lines) != 1 || lines[0] != root+": cargo fetch --locked --offline" {
		t.Errorf("one check of the root that holds a Cargo.lock, got %q", lines)
	}
	gen := filepath.Join(root, "tools/gen")
	if !strings.Contains(emitted, CodeLockfileMissing) || !strings.Contains(emitted, gen) {
		t.Errorf("the root with no Cargo.lock is named in a warning:\n%s", emitted)
	}
	if _, err := os.Stat(filepath.Join(gen, "Cargo.lock")); !os.IsNotExist(err) {
		t.Errorf("an offline install wrote a Cargo.lock: %v", err)
	}
	if downloads.Load() != 0 {
		t.Errorf("an offline install downloaded %d tools", downloads.Load())
	}

	// A crate cargo's home lacks fails the offline install.
	if err := os.Remove(filepath.Join(root, ".stub-fetched")); err != nil {
		t.Fatal(err)
	}
	if status, emitted, err := runWorkspaceJob(t, root, WorkspaceInstallJobName, nil, crateRefs(root)...); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeCargoFetchFailed) {
		t.Errorf("install: %s %v\n%s", status, err, emitted)
	}
}

func TestDepsUpgradeRunsCargoUpdateAtEachStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "upgrades-the-lockfile", "deps-upgrade-runs-cargo-update-at-each-stack-root")
	root := fixture(t)
	log := stubTools(t, root, "1.82.0", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, emitted, err := runWorkspaceJob(t, root, DepsUpgradeJobName, nil, crateRefs(root)...); status != "OK" || err != nil {
		t.Fatalf("upgrade: %s %v\n%s", status, err, emitted)
	}
	want := []string{root + ": cargo update", filepath.Join(root, "tools/gen") + ": cargo update"}
	if got := readLines(t, log); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("cargo update at each stack root, got %q", got)
	}

	_ = os.Remove(log)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	status, emitted, err := runWorkspaceJob(t, root, DepsUpgradeJobName, nil, crateRefs(root)...)
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeCargoUpdateFailed) || !strings.Contains(emitted, "cargo update needs the network") {
		t.Errorf("an offline upgrade fails with a clear message: %s %v\n%s", status, err, emitted)
	}
	if lines := readLines(t, log); lines != nil {
		t.Errorf("cargo ran offline: %q", lines)
	}
}
