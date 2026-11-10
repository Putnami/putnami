package dart

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	job "go.putnami.dev/protocol/job"
	registry "go.putnami.dev/protocol/registry"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// credentialFD is the descriptor the tests hand workspace-fetch its
// credential on, as the engine does.
const credentialFD = 9

// fetchBearer is the bearer the tests hand workspace-fetch. No process the job
// starts may see it.
const fetchBearer = "pkt_dart-fetch-bearer-5e2b7c1d"

// inheritedDescriptor is what a stub records when it inherited descriptor
// credentialFD.
var inheritedDescriptor = "inherited descriptor " + strconv.Itoa(credentialFD)

// startedRecord is the shell line a stub runs first: it appends its arguments,
// its environment and whether it inherited descriptor credentialFD to the
// started record of log.
func startedRecord(log string) string {
	return `{ echo "$tool $*"; env; [ -e /dev/fd/` + strconv.Itoa(credentialFD) + ` ] && echo "` + inheritedDescriptor + `"; } >> "` + log + `.started"
`
}

// started returns what every stub process recorded since the last call, and
// forgets it.
func started(t *testing.T, log string) string {
	t.Helper()
	return ran(t, log+".started")
}

// handCredential makes readJobCredential answer read.
func handCredential(t *testing.T, read func() (*registry.Credential, error)) {
	t.Helper()
	orig := readJobCredential
	t.Cleanup(func() { readJobCredential = orig })
	readJobCredential = read
}

// event is the part of a job protocol event the tests read.
type event struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// workspaceContext is the context of a workspace job over the packages at
// paths.
func workspaceContext(root string, params job.Params, paths ...string) *pctx.Context {
	var refs []job.ProjectRef
	for _, p := range paths {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}, Config: []byte(`{}`)})
	}
	return &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}
}

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
	quiet(t, func() {
		status, _, err = jobFunc(workspaceContext(root, params, paths...), jsonl.New(), nil)
	})
	return status, err
}

// runWorkspaceJobEvents runs a workspace job over the packages at paths and
// returns the events it wrote.
func runWorkspaceJobEvents(t *testing.T, root string, jobFunc cli.JobFunc, paths ...string) (status string, events []event, err error) {
	t.Helper()
	out, openErr := os.Create(filepath.Join(t.TempDir(), "events.jsonl"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer out.Close()
	stdout := os.Stdout
	os.Stdout = out
	status, _, err = jobFunc(workspaceContext(root, nil, paths...), jsonl.New(), nil)
	os.Stdout = stdout
	if _, seekErr := out.Seek(0, 0); seekErr != nil {
		t.Fatal(seekErr)
	}
	for scanner := bufio.NewScanner(out); scanner.Scan(); {
		var e event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	return status, events, err
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
	stackstest.WriteFile(t, root, ".dart_tool/package_config.json", `{"by": "an editor"}`)
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil, paths...); status != "OK" || err != nil || ran(t, log) != root+": flutter pub get --enforce-lockfile --offline\n" {
		t.Errorf("a resolution another tool wrote is not current: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "packages/ui/pubspec.yaml", "name: ui\nresolution: workspace\n")
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

	stackstest.WriteFile(t, root, "tools/gen/bin/server.dart", "void main() {}\n")
	if status, _, err := runProjectJob(t, root, "tools/gen", ServeJob(stack(t)), job.Params{"port": json.RawMessage(`8081`), "args": json.RawMessage(`"--verbose"`)}); status != "OK" || err != nil {
		t.Fatalf("serve: %s %v", status, err)
	}
	if got := ran(t, log); got != gen+": dart run bin/server.dart --verbose\n" {
		t.Errorf("serve ran %q", got)
	}
	if env, _ := os.ReadFile(log + ".env"); !strings.Contains(string(env), "PORT=8081") {
		t.Errorf("the server gets its port: %s", env)
	}
	if status, _, err := runProjectJob(t, root, "apps/mobile", ServeJob(stack(t)), nil); status != "FAILED" || err == nil || err.Error() != CodeNotRunnable {
		t.Errorf("a Flutter app with no web/ is not served: %s %v", status, err)
	}
	stackstest.WriteFile(t, root, "apps/mobile/web/index.html", "<html></html>\n")
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

// workspace-fetch reads the credential the engine hands it before it starts
// any process, the SDK check included, and then fetches each stack root.
func TestWorkspaceFetchReadsTheCredentialBeforeAnyProcess(t *testing.T) {
	spectest.Proves(t, feature, "fetch-reads-the-credential-first", "workspace-fetch-reads-the-credential-before-any-process")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	reads := 0
	handCredential(t, func() (*registry.Credential, error) {
		reads++
		if got := started(t, log); got != "" {
			t.Errorf("a process started before the credential read:\n%s", got)
		}
		return &registry.Credential{Bearer: fetchBearer, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"registry.example.test"}}, nil
	})
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil, "apps/mobile", "tools/gen"); status != "OK" || err != nil {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	if reads != 1 {
		t.Errorf("workspace-fetch read the credential %d times, want once", reads)
	}
	gen := filepath.Join(root, "tools/gen")
	if got, want := ran(t, log), root+": flutter pub get --enforce-lockfile\n"+gen+": dart pub get --enforce-lockfile\n"; got != want {
		t.Errorf("workspace-fetch ran\n%s\nwant\n%s", got, want)
	}
	if got := started(t, log); !strings.Contains(got, "dart --version") {
		t.Errorf("the SDK check did not run after the read:\n%s", got)
	}
}

// A credential descriptor workspace-fetch cannot read fails the job with
// dart.pub_get_failed before any process starts, the SDK check included.
func TestAnUnreadableCredentialFailsTheFetchBeforePubRuns(t *testing.T) {
	spectest.Proves(t, feature, "fetch-reads-the-credential-first", "an-unreadable-credential-fails-the-fetch-before-any-process")
	root := fixture(t)
	log := stubTools(t, "3.29.2", "3.7.2", junitPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	handCredential(t, func() (*registry.Credential, error) {
		return nil, errors.New(extensionproto.JobCredentialFDEnv + " names descriptor 9, which is not open")
	})
	status, events, err := runWorkspaceJobEvents(t, root, WorkspaceFetchJob(stack(t)), "apps/mobile", "tools/gen")
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodePubGetFailed+": ") {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	diagnosed := false
	for _, e := range events {
		if e.Type == "diagnostic" && e.Code == CodePubGetFailed && strings.HasPrefix(e.Message, "cannot read the registry credential the engine handed workspace-fetch: ") {
			diagnosed = true
		}
	}
	if !diagnosed {
		t.Errorf("no %s diagnostic names the unreadable credential: %+v", CodePubGetFailed, events)
	}
	if got := started(t, log); got != "" {
		t.Errorf("a process started after the credential read failed:\n%s", got)
	}
}

func TestASwitchBehindOneShimMakesTheRootStale(t *testing.T) {
	spectest.Proves(t, feature, "workspace-commands", "workspace-install-and-deps-upgrade-run-pub-at-each-stack-root")
	root := fixture(t)
	bin := t.TempDir()
	log := filepath.Join(bin, "tools.log")
	version := filepath.Join(bin, "version")
	stackstest.WriteFile(t, bin, "version", "3.7.2")
	// A shim, as asdf or mise install: one file whose SDK follows a setting.
	stackstest.WriteFile(t, bin, "dart", "#!/bin/sh\nread v < "+version+"\nif [ \"$1\" = --version ]; then echo \"Dart SDK version: $v (stable)\"; exit 0; fi\necho \"$PWD: dart $v $*\" >> "+log+"\nif [ \"$1 $2\" = \"pub get\" ]; then mkdir -p .dart_tool && echo '{}' > .dart_tool/package_config.json; fi\n")
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
	stackstest.WriteFile(t, bin, "version", "3.8.1")
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
