package dotnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// runWorkspaceJob runs a workspace job over the fixture's projects.
func runWorkspaceJob(t *testing.T, root string, jobFunc cli.JobFunc, params job.Params) (string, error) {
	t.Helper()
	var refs []job.ProjectRef
	for _, p := range []string{"src/Api", "src/Pricing", "tests/Pricing.Tests"} {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}, Config: []byte(`{}`)})
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, err := jobFunc(&pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}, jsonl.New(), nil)
	return status, err
}

func TestWorkspaceInstallRestoresEveryProject(t *testing.T) {
	spectest.Proves(t, feature, "restores-the-workspace", "workspace-install-restores-every-project-file")
	root, err := filepath.EvalSymlinks(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "src/Pricing/"+packagesLock, `{"version": 1}`)
	log := stubDotnet(t, "8.0.404", trxPass)
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-install: %s %v", status, err)
	}
	want := []string{
		root + ": dotnet restore " + filepath.Join(root, "src/Api/Api.csproj"),
		root + ": dotnet restore " + filepath.Join(root, "src/Pricing/Pricing.csproj") + " --locked-mode",
		root + ": dotnet restore " + filepath.Join(root, "tests/Pricing.Tests/Pricing.Tests.csproj"),
	}
	if got := readLines(t, log); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("ran\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	_ = os.Remove(log)
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), job.Params{"force": []byte("true")}); status != "OK" || err != nil {
		t.Fatalf("workspace-install --force: %s %v", status, err)
	}
	for _, line := range readLines(t, log) {
		if !strings.HasSuffix(line, " --force") {
			t.Errorf("the force flag restores again: %s", line)
		}
	}
}

func TestWorkspaceFetchRestoresEveryProject(t *testing.T) {
	spectest.Proves(t, feature, "restores-the-workspace", "workspace-fetch-downloads-the-packages-of-every-project")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	if got := readLines(t, log); len(got) != 3 || !strings.Contains(got[0], ": dotnet restore ") {
		t.Errorf("ran %q", got)
	}
}

func TestAWorkspaceJobWithNoSDKOfflineWarnsAndPasses(t *testing.T) {
	spectest.Proves(t, feature, "restores-the-workspace", "an-offline-workspace-install-with-no-sdk-warns")
	root := fixture(t)
	isolateSDKs(t)
	t.Setenv("PATH", t.TempDir())
	if status, err := runWorkspaceJob(t, root, WorkspaceInstallJob(stack(t)), nil); status != "OK" || err != nil {
		t.Errorf("a machine with no SDK offline: %s %v", status, err)
	}
}

func TestDepsUpgradeUpdatesThePackagesOfEveryProject(t *testing.T) {
	spectest.Proves(t, feature, "upgrades-the-packages", "deps-upgrade-runs-dotnet-package-update-on-every-project")
	root := fixture(t)
	writeFile(t, root, "global.json", `{"sdk": {"version": "10.0.100", "rollForward": "latestFeature"}}`)
	log := stubDotnet(t, "10.0.100", trxPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), nil); status != "OK" || err != nil {
		t.Fatalf("deps-upgrade: %s %v", status, err)
	}
	got := readLines(t, log)
	if len(got) != 3 || !strings.HasSuffix(got[1], ": dotnet package update --project "+filepath.Join(root, "src/Pricing/Pricing.csproj")) {
		t.Errorf("ran %q", got)
	}
}

func TestDepsUpgradeNeedsTheDotnet10SDKAndTheNetwork(t *testing.T) {
	spectest.Proves(t, feature, "upgrades-the-packages", "deps-upgrade-refuses-an-sdk-before-10-and-an-offline-run")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), nil); status != "FAILED" || err == nil || !strings.Contains(err.Error(), CodePackageUpdateFailed) {
		t.Errorf("an SDK before 10: %s %v", status, err)
	}
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	if status, err := runWorkspaceJob(t, root, DepsUpgradeJob(stack(t)), nil); status != "FAILED" || err == nil {
		t.Errorf("an offline run: %s %v", status, err)
	}
	if got := readLines(t, log); len(got) != 0 {
		t.Errorf("ran %q", got)
	}
}
