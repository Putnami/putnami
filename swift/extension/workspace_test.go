package swift

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// runWorkspaceJob runs a workspace job over the demo's three components.
func runWorkspaceJob(t *testing.T, root string, run cli.JobFunc, params job.Params) (string, error) {
	t.Helper()
	var refs []job.ProjectRef
	for _, path := range []string{"App", "Packages/Networking", "Packages/Pricing"} {
		refs = append(refs, job.ProjectRef{Name: path, Path: path, FullPath: filepath.Join(root, path), Extensions: []string{ExtensionName}, Config: []byte("{}")})
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, runErr := run(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}, jsonl.New(), nil)
	return status, runErr
}

func TestInstallAndUpgradeRunAtEachComponent(t *testing.T) {
	spectest.Proves(t, feature, "workspace-commands", "install-and-upgrade-run-at-each-component")
	root := fixture(t)
	s := stubTools(t)
	st := stack(t)
	app := filepath.Join(root, "App")

	install := WorkspaceInstallJob(st)
	if status, err := runWorkspaceJob(t, root, install, nil); status != "OK" || err != nil {
		t.Fatalf("install: %s %v", status, err)
	}
	if got := strings.Count(s.ran(t), ": swift package resolve --force-resolved-versions\n"); got != 2 {
		t.Errorf("install resolves both packages: %d\n%s", got, s.ran(t))
	}
	if !strings.Contains(s.ran(t), app+": xcodebuild -resolvePackageDependencies -onlyUsePackageVersionsFromResolvedFile\n") {
		t.Errorf("install resolves the Xcode project: %s", s.ran(t))
	}
	// A package whose manifests did not change since its last resolve is
	// current; --force resolves it again, and so does a changed manifest.
	_ = os.Remove(s.log)
	if status, err := runWorkspaceJob(t, root, install, nil); status != "OK" || err != nil || strings.Contains(s.ran(t), "package resolve") {
		t.Errorf("a current package is not resolved again: %s %v %s", status, err, s.ran(t))
	}
	stackstest.WriteFile(t, root, "Packages/Networking/Package.resolved", `{"pins": [], "version": 3}`)
	if status, err := runWorkspaceJob(t, root, install, nil); status != "OK" || err != nil || strings.Count(s.ran(t), "package resolve") != 1 {
		t.Errorf("a changed Package.resolved resolves its package: %s %v %s", status, err, s.ran(t))
	}
	_ = os.Remove(s.log)
	if status, err := runWorkspaceJob(t, root, install, job.Params{"force": json.RawMessage("true")}); status != "OK" || err != nil || strings.Count(s.ran(t), "package resolve") != 2 {
		t.Errorf("--force resolves every package: %s %v %s", status, err, s.ran(t))
	}

	// An Xcode project keeps its pins: on a machine with no xcodebuild, the
	// upgrade of the packages still runs.
	_ = os.Remove(s.log)
	_ = os.Remove(filepath.Join(filepath.Dir(s.log), "xcodebuild"))
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(st), nil); status != "OK" || err != nil {
		t.Fatalf("upgrade: %s %v", status, err)
	}
	if ran := s.ran(t); strings.Count(ran, ": swift package update") != 2 || strings.Contains(ran, app+": ") {
		t.Errorf("upgrade updates the packages, not the Xcode project: %s", ran)
	}
	t.Setenv("PUTNAMI_OFFLINE_DEPENDENCIES", "1")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(st), nil); status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeUpdateFailed) {
		t.Errorf("an offline upgrade fails: %s %v", status, err)
	}
	// Offline, a resolve would reach the network: install resolves nothing.
	_ = os.Remove(s.log)
	if status, err := runWorkspaceJob(t, root, install, job.Params{"force": json.RawMessage("true")}); status != "OK" || err != nil || strings.Contains(s.ran(t), "package resolve") {
		t.Errorf("an offline install resolves nothing: %s %v %s", status, err, s.ran(t))
	}
}

func TestAFailedResolveNamesItsComponent(t *testing.T) {
	root := fixture(t)
	stubTools(t)
	// A swift that exits non-zero on any package command.
	bin := t.TempDir()
	stackstest.WriteFile(t, bin, "swift", "#!/bin/sh\ncase \"$1\" in --version) echo 'Apple Swift version 6.0.2';; *) exit 3;; esac\n")
	if err := os.Chmod(filepath.Join(bin, "swift"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), "Packages/Networking") {
		t.Errorf("a failed resolve names its package: %s %v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, "Packages/Networking/.build", resolvedMarker)); err == nil {
		t.Error("a failed resolve records nothing")
	}
}

func TestTheTestFilterReachesSwiftTestAndXcodebuild(t *testing.T) {
	spectest.Proves(t, feature, "test-filter", "the-test-filter-reaches-swift-test-and-xcodebuild")
	root := fixture(t)
	s := stubTools(t)
	s.demoReports(t, root, false)
	test := TestJob(stack(t))
	filter := job.Params{"test": json.RawMessage(`"ClientTests"`)}
	status, _, out, err := runJobWith(t, root, "Packages/Networking", stacks.TaskTest, test, filter)
	if status != "OK" || err != nil {
		t.Fatalf("test: %s %v", status, err)
	}
	if want := "--xunit-output " + filepath.Join(out, "test", "xunit.xml") + " --filter ClientTests\n"; !strings.Contains(s.ran(t), want) {
		t.Errorf("the filter reaches swift test: %s", s.ran(t))
	}
	// The filter reaches the components the selected ones depend on, where
	// it may select nothing: no report is no failure then.
	_ = os.Remove(filepath.Join(s.dir, "xunit-Networking.xml"))
	status, data, _, err := runJobWith(t, root, "Packages/Networking", stacks.TaskTest, test, job.Params{"t": json.RawMessage(`"Nothing"`)})
	if status != "OK" || err != nil || summary(t, data, "testSummary")["total"] != 0 {
		t.Errorf("a filter that selects nothing passes: %s %v %v", status, err, data)
	}
	stackstest.WriteFile(t, root, "App/.xcode-version", "16.1\n")
	status, _, _, err = runJobWith(t, root, "App", stacks.TaskTest, test, job.Params{"test": json.RawMessage(`"ShopTests/CheckoutTests"`)})
	if status != "OK" || err != nil || !strings.Contains(s.ran(t), "-only-testing:ShopTests/CheckoutTests CODE_SIGNING_ALLOWED=NO") {
		t.Errorf("the filter reaches xcodebuild: %s %v %s", status, err, s.ran(t))
	}
	// An override whose last step is another program gets no filter, and a
	// filter after "--" in a swiftpm-test step still passes an empty run.
	stackstest.WriteFile(t, root, "Packages/Networking/putnami.json", `{"options": {"@putnami/swift": {"tasks": {"test": {"steps": [["swift", "build", "--force-resolved-versions"]], "reports": null}}}}}`)
	_ = os.Remove(s.log)
	if status, _, _, err := runJobWith(t, root, "Packages/Networking", stacks.TaskTest, test, filter); status != "OK" || err != nil || strings.Contains(s.ran(t), "--filter") {
		t.Errorf("a build-only test step gets no filter: %s %v %s", status, err, s.ran(t))
	}
	stackstest.WriteFile(t, root, "Packages/Networking/putnami.json", `{"options": {"@putnami/swift": {"tasks": {"test": {"steps": [["{runtime}", "swiftpm-test", "--output", "{output}", "--", "--filter", "Nothing"]]}}}}}`)
	if status, data, _, err := runJobWith(t, root, "Packages/Networking", stacks.TaskTest, test, nil); status != "OK" || err != nil || summary(t, data, "testSummary")["total"] != 0 {
		t.Errorf("a filter after -- that selects nothing passes: %s %v %v", status, err, data)
	}
	_ = os.Remove(filepath.Join(root, "Packages/Networking/putnami.json"))

	// An Xcode project whose scheme has no target the filter names fails:
	// xcodebuild refuses the selection and no case failed.
	t.Setenv("STUB_FAIL", "1")
	s.write(t, "tests.json", `{"testNodes":[]}`)
	if status, _, _, err := runJobWith(t, root, "App", stacks.TaskTest, test, job.Params{"test": json.RawMessage(`"Missing"`)}); status != "FAILED" || err == nil {
		t.Errorf("an Xcode filter that names no target fails: %s %v", status, err)
	}
	t.Setenv("STUB_FAIL", "")
	s.demoReports(t, root, false)

	// Without a filter, the test task runs every test.
	if status, _, _, err := runJobWith(t, root, "Packages/Pricing", stacks.TaskTest, test, nil); status != "OK" || err != nil {
		t.Errorf("test without a filter: %s %v", status, err)
	}
}
