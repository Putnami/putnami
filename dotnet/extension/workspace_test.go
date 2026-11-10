package dotnet

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
	"go.putnami.dev/protocol/job"
	registry "go.putnami.dev/protocol/registry"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// credentialFD is the descriptor the tests hand workspace-fetch its
// credential on, as the engine does.
const credentialFD = 9

// fetchBearer is the bearer the tests hand workspace-fetch. No process the job
// starts may see it.
const fetchBearer = "pkt_dotnet-fetch-bearer-8a4f0e6b"

// inheritedDescriptor is what the stub records when it inherited descriptor
// credentialFD.
var inheritedDescriptor = "inherited descriptor " + strconv.Itoa(credentialFD)

// startedRecord is the shell line the stub runs first: it appends its
// arguments, its environment and whether it inherited descriptor credentialFD
// to the started record of log.
func startedRecord(log string) string {
	return `{ echo "$tool $*"; env; [ -e /dev/fd/` + strconv.Itoa(credentialFD) + ` ] && echo "` + inheritedDescriptor + `"; } >> "` + log + `.started"
`
}

// started returns what every stub process recorded since the last call, and
// forgets it.
func started(t *testing.T, log string) string {
	t.Helper()
	data, _ := os.ReadFile(log + ".started")
	if err := os.Remove(log + ".started"); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

// handCredential makes readJobCredential answer read.
func handCredential(t *testing.T, read func() (*registry.Credential, error)) {
	t.Helper()
	orig := readJobCredential
	t.Cleanup(func() { readJobCredential = orig })
	readJobCredential = read
}

// workspaceContext is the context of a workspace job over the fixture's
// projects.
func workspaceContext(root string, params job.Params) *pctx.Context {
	var refs []job.ProjectRef
	for _, p := range []string{"src/Api", "src/Pricing", "tests/Pricing.Tests"} {
		refs = append(refs, job.ProjectRef{Name: p, Path: p, FullPath: filepath.Join(root, p), Extensions: []string{ExtensionName}, Config: []byte(`{}`)})
	}
	return &pctx.Context{WorkspaceRoot: root, WorkspaceProjects: refs, Params: params}
}

// runWorkspaceJob runs a workspace job over the fixture's projects.
func runWorkspaceJob(t *testing.T, root string, jobFunc cli.JobFunc, params job.Params) (string, error) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	stdout := os.Stdout
	os.Stdout = devnull
	defer func() { os.Stdout = stdout }()
	status, _, err := jobFunc(workspaceContext(root, params), jsonl.New(), nil)
	return status, err
}

// runWorkspaceJobEvents runs a workspace job over the fixture's projects and
// returns the events it wrote.
func runWorkspaceJobEvents(t *testing.T, root string, jobFunc cli.JobFunc) (status string, events []runtimeproto.Event, err error) {
	t.Helper()
	out, openErr := os.Create(filepath.Join(t.TempDir(), "events.jsonl"))
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer out.Close()
	stdout := os.Stdout
	os.Stdout = out
	status, _, err = jobFunc(workspaceContext(root, nil), jsonl.New(), nil)
	os.Stdout = stdout
	if _, seekErr := out.Seek(0, 0); seekErr != nil {
		t.Fatal(seekErr)
	}
	for scanner := bufio.NewScanner(out); scanner.Scan(); {
		var e runtimeproto.Event
		if json.Unmarshal(scanner.Bytes(), &e) == nil {
			events = append(events, e)
		}
	}
	return status, events, err
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

// workspace-fetch reads the credential the engine hands it before it starts
// any process, the SDK check included, and then restores every project.
func TestWorkspaceFetchReadsTheCredentialBeforeAnyProcess(t *testing.T) {
	spectest.Proves(t, feature, "fetch-reads-the-credential-first", "workspace-fetch-reads-the-credential-before-any-process")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	reads := 0
	handCredential(t, func() (*registry.Credential, error) {
		reads++
		if got := started(t, log); got != "" {
			t.Errorf("a process started before the credential read:\n%s", got)
		}
		return &registry.Credential{Bearer: fetchBearer, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"registry.example.test"}}, nil
	})
	if status, err := runWorkspaceJob(t, root, WorkspaceFetchJob(stack(t)), nil); status != "OK" || err != nil {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	if reads != 1 {
		t.Errorf("workspace-fetch read the credential %d times, want once", reads)
	}
	if got := readLines(t, log); len(got) != 3 || !strings.Contains(got[0], ": dotnet restore ") {
		t.Errorf("ran %q", got)
	}
	if got := started(t, log); !strings.Contains(got, "dotnet --version") {
		t.Errorf("the SDK check did not run after the read:\n%s", got)
	}
}

// A credential descriptor workspace-fetch cannot read fails the job with
// dotnet.restore_failed before any process starts, the SDK check included.
func TestAnUnreadableCredentialFailsTheFetchBeforeDotnetRuns(t *testing.T) {
	spectest.Proves(t, feature, "fetch-reads-the-credential-first", "an-unreadable-credential-fails-the-fetch-before-any-process")
	root := fixture(t)
	log := stubDotnet(t, "8.0.404", trxPass)
	handCredential(t, func() (*registry.Credential, error) {
		return nil, errors.New(extensionproto.JobCredentialFDEnv + " names descriptor 9, which is not open")
	})
	status, events, err := runWorkspaceJobEvents(t, root, WorkspaceFetchJob(stack(t)))
	if status != "FAILED" || err == nil || !strings.HasPrefix(err.Error(), CodeRestoreFailed+": ") {
		t.Fatalf("workspace-fetch: %s %v", status, err)
	}
	diagnosed := false
	for _, e := range events {
		if e.Type == runtimeproto.EventDiagnostic && e.Code == CodeRestoreFailed && strings.HasPrefix(e.Message, "cannot read the registry credential the engine handed workspace-fetch: ") {
			diagnosed = true
		}
	}
	if !diagnosed {
		t.Errorf("no %s diagnostic names the unreadable credential: %+v", CodeRestoreFailed, events)
	}
	if got := started(t, log); got != "" {
		t.Errorf("a process started after the credential read failed:\n%s", got)
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
