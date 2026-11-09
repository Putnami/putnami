package dart

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// quiet runs fn with the job protocol's stdout sent nowhere.
func quiet(t *testing.T, fn func()) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	fn()
}

// runWorkspaceJob runs a workspace job over the packages at paths.
func runWorkspaceJob(t *testing.T, root string, jobFunc cli.JobFunc, params job.Params, paths ...string) (status string, err error) {
	t.Helper()
	var refs []job.ProjectRef
	for _, p := range paths {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}, Config: []byte(`{}`)})
	}
	quiet(t, func() {
		status, _, err = jobFunc(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}, jsonl.New(), nil)
	})
	return status, err
}

// runProjectJob runs a job of the package at rel, with its probe metadata.
func runProjectJob(t *testing.T, root, rel string, jobFunc cli.JobFunc, params job.Params) (status string, data map[string]any, err error) {
	t.Helper()
	var meta json.RawMessage
	for _, p := range probe(t, root).Projects {
		if p.Path == rel {
			meta = p.Metadata
		}
	}
	ctx := &pctx.Context{WorkspaceRoot: root, Params: params, Project: pctx.Project{
		Name: rel, Path: rel, FullPath: filepath.Join(root, rel),
		Metadata: map[string]json.RawMessage{ExtensionName: meta},
	}}
	quiet(t, func() { status, data, err = jobFunc(ctx, jsonl.New(), nil) })
	return status, data, err
}

func TestWorkspaceInstallAndDepsUpgradeRunPubAtEachStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "workspace-commands", "workspace-install-and-deps-upgrade-run-pub-at-each-stack-root")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	paths := []string{"apps/mobile", "packages/pricing", "tools/gen"}
	status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, paths...)
	if status != "OK" || err != nil {
		t.Fatalf("workspace-install: %s %v", status, err)
	}
	gen := filepath.Join(root, "tools/gen")
	if got, want := ran(t, log), root+": flutter pub get --enforce-lockfile --offline\n"+gen+": dart pub get --enforce-lockfile --offline\n"; got != want {
		t.Errorf("one pub get per stack root, flutter for the Flutter workspace:\n%s\nwant\n%s", got, want)
	}

	// A root the selected SDK resolved last is current, unless --force; one
	// another SDK or an editor resolved since is not.
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, paths...); status != "OK" || err != nil || ran(t, log) != "" {
		t.Errorf("a current root is skipped: %s %v", status, err)
	}
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), job.Params{"force": json.RawMessage(`true`)}, paths...); status != "OK" || err != nil || !strings.Contains(ran(t, log), root+": flutter pub get") {
		t.Errorf("--force resolves a current root: %s %v", status, err)
	}
	writeFile(t, root, ".dart_tool/package_config.json", `{"by": "an editor"}`)
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, paths...); status != "OK" || err != nil || ran(t, log) != root+": flutter pub get --enforce-lockfile --offline\n" {
		t.Errorf("a resolution another tool wrote is not current: %s %v", status, err)
	}
	writeFile(t, root, "packages/ui/pubspec.yaml", "name: ui\nresolution: workspace\n")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, paths...); status != "OK" || err != nil || ran(t, log) != root+": flutter pub get --enforce-lockfile --offline\n" {
		t.Errorf("a member's pubspec change makes the root stale: %s %v", status, err)
	}

	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), nil, paths...); status != "OK" || err != nil {
		t.Fatalf("deps-upgrade: %s %v", status, err)
	}
	if got, want := ran(t, log), root+": flutter pub upgrade\n"+gen+": dart pub upgrade\n"; got != want {
		t.Errorf("one pub upgrade per stack root:\n%s\nwant\n%s", got, want)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), nil, paths...); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodePubUpgradeFailed) {
		t.Errorf("an offline upgrade fails: %s %v", status, err)
	}
	t.Setenv("STUB_EXIT", "1")
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), job.Params{"force": json.RawMessage(`true`)}, "tools/gen"); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodePubGetFailed) {
		t.Errorf("a failed pub get fails the job: %s %v", status, err)
	}
}

func TestRunForwardsTheExitCodeAndServeReportsReady(t *testing.T) {
	spectest.Proves(t, feature, "runs-and-serves", "run-forwards-the-exit-code-and-serve-reports-ready")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	gen := filepath.Join(root, "tools/gen")

	t.Setenv("STUB_EXIT", "7")
	status, data, err := runProjectJob(t, root, "tools/gen", RunJob(stack(t)), job.Params{"args": json.RawMessage(`"--name world"`)})
	if status != "FAILED" || err != nil || data[exitCodeKey] != 7 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	t.Setenv("STUB_EXIT", "")
	if status, data, err := runProjectJob(t, root, "tools/gen", RunJob(stack(t)), job.Params{"entrypoint": json.RawMessage(`"bin/gen.dart"`)}); status != "OK" || err != nil || data[exitCodeKey] != 0 {
		t.Fatalf("run: %s %v %v", status, data, err)
	}
	if got, want := ran(t, log), gen+": dart run --name world\n"+gen+": dart run bin/gen.dart\n"; got != want {
		t.Errorf("run ran\n%s\nwant\n%s", got, want)
	}
	if status, _, err := runProjectJob(t, root, "apps/mobile", RunJob(stack(t)), nil); status != "FAILED" || err == nil || err.Error() != CodeNotRunnable {
		t.Errorf("a Flutter app does not run with dart run: %s %v", status, err)
	}

	writeFile(t, root, "tools/gen/bin/server.dart", "void main() {}\n")
	if status, _, err := runProjectJob(t, root, "tools/gen", ServeJob(stack(t)), job.Params{"port": json.RawMessage(`8081`)}); status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	if got := ran(t, log); got != gen+": dart run bin/server.dart\n" {
		t.Errorf("serve ran %q", got)
	}
	if env, _ := os.ReadFile(log + ".env"); !strings.Contains(string(env), "PORT=8081") {
		t.Errorf("the server gets its port: %s", env)
	}
	if status, _, err := runProjectJob(t, root, "apps/mobile", ServeJob(stack(t)), nil); status != "FAILED" || err == nil || err.Error() != CodeNotRunnable {
		t.Errorf("a Flutter app with no web/ is not served: %s %v", status, err)
	}
	writeFile(t, root, "apps/mobile/web/index.html", "<html></html>\n")
	if status, _, err := runProjectJob(t, root, "apps/mobile", ServeJob(stack(t)), nil); status != "OK" || err != nil {
		t.Fatalf("serve mobile: %s %v", status, err)
	}
	if got, want := ran(t, log), filepath.Join(root, "apps/mobile")+": flutter run -d web-server --web-port 3000 --web-hostname 0.0.0.0\n"; got != want {
		t.Errorf("a Flutter web app serves on the web server device:\n%s\nwant\n%s", got, want)
	}
	t.Setenv("STUB_EXIT", "5")
	if status, data, err := runProjectJob(t, root, "tools/gen", ServeJob(stack(t)), nil); status != "FAILED" || err != nil || data[exitCodeKey] != 5 {
		t.Errorf("a server that exits on its own fails serve: %s %v %v", status, data, err)
	}
}

func TestWorkspaceFetchFillsThePubCacheAtEachStackRoot(t *testing.T) {
	spectest.Proves(t, feature, "workspace-commands", "workspace-install-and-deps-upgrade-run-pub-at-each-stack-root")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil, "apps/mobile", "tools/gen"); status != "OK" || err != nil {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	gen := filepath.Join(root, "tools/gen")
	if got, want := ran(t, log), root+": flutter pub get --enforce-lockfile\n"+gen+": dart pub get --enforce-lockfile\n"; got != want {
		t.Errorf("workspace-fetch ran\n%s\nwant\n%s", got, want)
	}
}

func TestASwitchBehindOneShimMakesTheRootStale(t *testing.T) {
	spectest.Proves(t, feature, "workspace-commands", "workspace-install-and-deps-upgrade-run-pub-at-each-stack-root")
	root := fixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "tools.log")
	version := filepath.Join(bin, "version")
	writeFile(t, bin, "version", "3.7.2")
	// A shim, as asdf or mise install: one file whose SDK follows a setting.
	writeFile(t, bin, "dart", "#!/bin/sh\nread v < "+version+"\nif [ \"$1\" = --version ]; then echo \"Dart SDK version: $v (stable)\"; exit 0; fi\necho \"$PWD: dart $v $*\" >> "+log+"\nif [ \"$1 $2\" = \"pub get\" ]; then mkdir -p .dart_tool && echo '{}' > .dart_tool/package_config.json; fi\n")
	if err := os.Chmod(filepath.Join(bin, "dart"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	install := func() string {
		if status, _, err := runProjectJob(t, root, "tools/gen", InstallJob(stack(t)), nil); status != "OK" || err != nil {
			t.Fatalf("install: %s %v", status, err)
		}
		return ran(t, log)
	}
	if got := install(); !strings.Contains(got, "dart 3.7.2 pub get") {
		t.Fatalf("first install: %q", got)
	}
	if got := install(); got != "" {
		t.Errorf("the same SDK finds the root current: %q", got)
	}
	writeFile(t, bin, "version", "3.8.1")
	if got := install(); !strings.Contains(got, "dart 3.8.1 pub get") {
		t.Errorf("another SDK behind the same shim resolves again: %q", got)
	}
}

func TestATaskNeverWritesTheLockfileItReads(t *testing.T) {
	spectest.Proves(t, feature, "tasks-run-the-profile", "a-task-runs-the-variant-steps-after-the-toolchain-check")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	gen := filepath.Join(root, "tools/gen")
	lock := filepath.Join(gen, pubspecLock)
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	status, _, err := runProjectJob(t, root, "tools/gen", InstallJob(stack(t)), nil)
	if status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodePubGetFailed) {
		t.Errorf("the install of a root with no pubspec.lock fails: %s %v", status, err)
	}
	if got := ran(t, log); got != gen+": dart pub get --enforce-lockfile\n" {
		t.Errorf("the install enforces the lockfile it lacks: %q", got)
	}
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil, "tools/gen"); status != "FAILED" || err == nil || ran(t, log) != gen+": dart pub get --enforce-lockfile\n" {
		t.Errorf("workspace-fetch, which a hosted run calls before its tasks, fails too: %s %v", status, err)
	}
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Errorf("neither wrote a pubspec.lock: %v", err)
	}

	// workspace-install resolves the lockfile a root lacks; the next task
	// install finds the root current.
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, "tools/gen"); status != "OK" || err != nil {
		t.Fatalf("workspace-install: %s %v", status, err)
	}
	if got := ran(t, log); got != gen+": dart pub get\n" {
		t.Errorf("workspace-install resolves a lockfile: %q", got)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Errorf("workspace-install wrote pubspec.lock: %v", err)
	}
	if status, _, err := runProjectJob(t, root, "tools/gen", InstallJob(stack(t)), nil); status != "OK" || err != nil || ran(t, log) != "" {
		t.Errorf("the root workspace-install resolved is current: %s %v", status, err)
	}
}
