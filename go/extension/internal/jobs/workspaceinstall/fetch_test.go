package workspaceinstall

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/go/extension/tools"
	"go.putnami.dev/protocol/features/spectest"
	registry "go.putnami.dev/protocol/registry"
)

// testBearer is the read credential a fetch test hands the job. It is long
// and distinctive, so finding it anywhere it must not be is unambiguous.
const testBearer = "pkt_fetch-test-bearer-7f3a9c1e5b"

func testCredential(hosts ...string) *registry.Credential {
	return &registry.Credential{Bearer: testBearer, ExpiresAt: "2099-01-01T00:00:00Z", Hosts: hosts}
}

// recordedJob is newInstallJob with a go command that records every
// invocation (jobtest.Fakes.RecordingGo), and the offline signal the engine
// sets in every job of a hosted run, workspace-fetch included.
func recordedJob(t *testing.T, workspace, proxyURL, cacheRoot string, fakeTools bool) *installJob {
	t.Helper()
	goBinary := jobtest.RequireGo(t)
	fakes := jobtest.NewFakes(t)
	if fakeTools {
		for _, tool := range devTools {
			spec, _ := tools.Lookup(tool)
			fakes.Tool(t, tool, spec.Version)
		}
	}
	fakes.RecordingGo(t, goBinary)
	path := fakes.Dir + string(filepath.ListSeparator) + filepath.Dir(goBinary)
	env := jobtest.Env(t, proxyURL, cacheRoot,
		append(fakes.Environ(), "PATH="+path, "PUTNAMI_OFFLINE_DEPENDENCIES=1")...)
	rec := &jobtest.Recorder{}
	j, stdout, stderr := jobtest.NewJob(t, rec, env, workspace)
	return &installJob{job: j, rec: rec, stdout: stdout, stderr: stderr, fakes: fakes}
}

// fetch runs workspace-fetch with credential and checks what every run owes,
// as installJob.run does.
func (i *installJob) fetch(t *testing.T, credential *registry.Credential) string {
	t.Helper()
	status := fetch(i.job, false, "", i.members, credential)
	if i.stdout.Len() > 0 {
		t.Errorf("a command wrote to the job event stream:\n%s", i.stdout)
	}
	if invocations := i.fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("workspace-fetch started shell programs: %v", invocations)
	}
	return status
}

// downloads reports whether a go invocation resolves or downloads modules.
func downloads(record jobtest.GoRecord) bool {
	args := " " + strings.Join(record.Args, " ") + " "
	return strings.Contains(args, " list -m ") || strings.Contains(args, " mod download ") ||
		strings.Contains(args, " install ")
}

// requireCredentialOnlyInDownloads checks the custody of the credential over
// every go invocation of a fetch: each command that downloads got NETRC, a
// file only this user can read in a directory only this user can enter,
// holding wantNetrc; no other command got NETRC; no environment holds the
// bearer; and no NETRC file outlived the run. It returns the downloading
// invocations.
func requireCredentialOnlyInDownloads(t *testing.T, i *installJob, wantNetrc string) []jobtest.GoRecord {
	t.Helper()
	var downloading []jobtest.GoRecord
	for _, record := range i.fakes.GoRecords(t) {
		for _, entry := range record.Env {
			if strings.Contains(entry, testBearer) {
				t.Errorf("go %v ran with the bearer in its environment: %q", record.Args, entry)
			}
		}
		netrc, set := record.Value("NETRC")
		if !downloads(record) {
			if set {
				t.Errorf("go %v got NETRC=%q; only a command that downloads may", record.Args, netrc)
			}
			continue
		}
		downloading = append(downloading, record)
		if goAuth, _ := record.Value("GOAUTH"); goAuth != "netrc" {
			t.Errorf("go %v downloaded with GOAUTH=%q, want netrc", record.Args, goAuth)
		}
		if record.Netrc == nil {
			t.Errorf("go %v downloaded without the credential NETRC", record.Args)
			continue
		}
		if record.Netrc.Content != wantNetrc || record.Netrc.Err != "" {
			t.Errorf("go %v NETRC content = %q (%s), want %q", record.Args, record.Netrc.Content, record.Netrc.Err, wantNetrc)
		}
		if runtime.GOOS != "windows" && (record.Netrc.Mode != 0o600 || record.Netrc.DirMode != 0o700) {
			t.Errorf("go %v NETRC mode %v in a directory of mode %v, want 0600 in 0700",
				record.Args, record.Netrc.Mode, record.Netrc.DirMode)
		}
		for _, gone := range []string{record.Netrc.Path, filepath.Dir(record.Netrc.Path)} {
			if _, err := os.Stat(gone); !os.IsNotExist(err) {
				t.Errorf("%s outlived the fetch: %v", gone, err)
			}
		}
	}
	for _, entry := range i.job.Env.Environ() {
		if strings.HasPrefix(entry, "NETRC=") || strings.Contains(entry, testBearer) {
			t.Errorf("the job environment kept %q after the fetch", entry)
		}
	}
	return downloading
}

// TestWorkspaceFetch_GivesTheCredentialOnlyToTheDownloads is the acceptance
// test of workspace-fetch: it downloads the complete build list with the
// network allowed, although the offline signal is set, and only the go
// commands that download see the credential, through an ephemeral NETRC that
// is gone when the job ends. The NETRC names each host as go matches it: a
// port is kept, and the default https port is also written without it.
func TestWorkspaceFetch_GivesTheCredentialOnlyToTheDownloads(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"workspace-fetch-gives-the-credential-only-to-the-downloads")
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	seedChecksums(t, workspace, proxy)
	cacheRoot := jobtest.CacheRoot(t)
	i := recordedJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)

	credential := testCredential("mirror.example.test:8443", "other.example.test:443", "registry.example.test")
	if status := i.fetch(t, credential); status != statusOK {
		t.Fatalf("workspace-fetch = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	wantNetrc := "machine mirror.example.test:8443 login _token password " + testBearer + "\n" +
		"machine other.example.test:443 login _token password " + testBearer + "\n" +
		"machine other.example.test login _token password " + testBearer + "\n" +
		"machine registry.example.test login _token password " + testBearer + "\n"
	downloading := requireCredentialOnlyInDownloads(t, i, wantNetrc)
	if len(downloading) < 4 {
		t.Fatalf("%d downloading go commands, want the list and the download of two graphs:\n%s",
			len(downloading), i.rec.Transcript())
	}
	for _, record := range downloading {
		if proxyURL, _ := record.Value("GOPROXY"); proxyURL != jobtest.FileURL(proxy) {
			t.Errorf("go %v GOPROXY = %q, want the fixture proxy: workspace-fetch downloads", record.Args, proxyURL)
		}
		if goflags, _ := record.Value("GOFLAGS"); strings.Contains(goflags, "-mod=readonly") {
			t.Errorf("go %v GOFLAGS = %q: the offline policy reached workspace-fetch", record.Args, goflags)
		}
	}
	if got, ok := i.rec.MetricValue("modules-downloaded"); !ok || got != 6 {
		t.Errorf("modules-downloaded = %v (%v), want 6:\n%s", got, ok, i.rec.Transcript())
	}
	for _, module := range []string{"example.com/lib@v1.0.0", "example.com/dep@v1.0.0", "example.com/extra@v1.0.0"} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "mod", filepath.FromSlash(module))); err != nil {
			t.Errorf("module %s is missing from the fetched cache: %v", module, err)
		}
	}
}

// A download that fails still removes the credential file, and fails the job.
func TestWorkspaceFetch_RemovesTheCredentialWhenTheDownloadFails(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"workspace-fetch-removes-the-credential-on-failure")
	t.Parallel()
	workspace, _ := buildListWorkspace(t)
	i := recordedJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.fetch(t, testCredential("registry.example.test")); status != statusFailed {
		t.Fatalf("workspace-fetch = %s, want FAILED:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("Failed to resolve the workspace build list") {
		t.Errorf("no resolution diagnostic:\n%s", i.rec.Transcript())
	}
	downloading := requireCredentialOnlyInDownloads(t, i,
		"machine registry.example.test login _token password "+testBearer+"\n")
	if len(downloading) == 0 {
		t.Fatalf("the failed fetch ran no downloading go command:\n%s", i.rec.Transcript())
	}
}

// A tool the fetch builds from source is downloaded by `go install` with the
// credential; the built tool, which the job then runs, is not.
func TestWorkspaceFetch_GivesTheCredentialToTheToolBuild(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	i := recordedJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), false)
	for _, tool := range devTools {
		i.fakes.Tool(t, tool, "v0.0.1")
		i.job.Env.Set("JOBTEST_VERSION_"+strings.ToUpper(strings.ReplaceAll(tool, "-", "_")), "v0.0.1")
	}
	if status := i.fetch(t, testCredential("registry.example.test")); status != statusOK {
		t.Fatalf("workspace-fetch = %s:\n%s", status, i.rec.Transcript())
	}
	var installs int
	for _, record := range requireCredentialOnlyInDownloads(t, i,
		"machine registry.example.test login _token password "+testBearer+"\n") {
		if len(record.Args) > 0 && record.Args[0] == "install" {
			installs++
		}
	}
	if installs != len(devTools) {
		t.Errorf("%d go install commands had the credential, want %d:\n%s", installs, len(devTools), i.rec.Transcript())
	}
}

// Without a credential the fetch runs the same commands with the machine's
// own credentials: NETRC is left as the job inherited it.
func TestWorkspaceFetch_WithoutACredentialLeavesNetrcAlone(t *testing.T) {
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	seedChecksums(t, workspace, proxy)
	i := recordedJob(t, workspace, jobtest.FileURL(proxy), jobtest.CacheRoot(t), true)
	if status := i.fetch(t, nil); status != statusOK {
		t.Fatalf("workspace-fetch = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	var downloading int
	for _, record := range i.fakes.GoRecords(t) {
		if netrc, set := record.Value("NETRC"); set {
			t.Errorf("go %v got NETRC=%q without a credential", record.Args, netrc)
		}
		if downloads(record) {
			downloading++
		}
	}
	if downloading == 0 {
		t.Fatalf("the fetch ran no downloading go command:\n%s", i.rec.Transcript())
	}
}

// fetchAndOfflineWorkspace is buildListWorkspace with a project that owns
// tool pins, whose module the fixture proxy serves too.
func fetchAndOfflineWorkspace(t *testing.T) (workspace, proxy string) {
	t.Helper()
	workspace, proxy = buildListWorkspace(t)
	jobtest.WriteModuleProxyAt(t, proxy, jobtest.ProxyModule{
		Path: "example.com/tool", Version: "v1.0.0",
		GoMod:  "module example.com/tool\n\ngo 1.22\n",
		Source: "package main\n\nfunc main() {}\n",
	})
	jobtest.WriteVersionList(t, proxy, "example.com/tool", "v1.0.0")
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["app","ext"]}`+"\n")
	jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./app\n\t./ext\n)\n")
	jobtest.WriteFile(t, workspace, "ext/putnami.json", `{"name":"ext"}`+"\n")
	jobtest.WriteFile(t, workspace, "ext/go.mod", "module example.com/ext\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "ext/main.go", "package main\n\nfunc main() {}\n")
	jobtest.WriteFile(t, workspace, "ext/tools/versions.json",
		`{"schemaVersion":1,"goVersion":"1.22","tools":{"tool":{"install":"example.com/tool@v1.0.0","version":"v1.0.0"}}}`+"\n")
	seedChecksums(t, workspace, proxy)
	return workspace, proxy
}

// A hosted run writes no file the repository commits: workspace-fetch and the
// offline workspace-install keep a go.work that misses a member as committed,
// where an install without the offline signal adds the member.
func TestWorkspaceInstall_AHostedRunKeepsGoWorkAsCommitted(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"a-hosted-run-keeps-go-work-as-committed")
	t.Parallel()
	workspace, proxy := fetchAndOfflineWorkspace(t)
	stale := "go 1.22\n\nuse ./app\n"
	jobtest.WriteFile(t, workspace, "go.work", stale)
	cacheRoot := jobtest.CacheRoot(t)

	fetchJob := recordedJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	fetchJob.members = []string{"app", "ext"}
	fetchJob.fetch(t, testCredential("registry.example.test"))
	offline := recordedJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	offline.members = []string{"app", "ext"}
	offline.run(t, false)
	for name, job := range map[string]*installJob{"workspace-fetch": fetchJob, "offline workspace-install": offline} {
		if got := job.rec.PhaseStatus("sync"); got != "skipped" {
			t.Errorf("%s sync phase = %q, want skipped:\n%s", name, got, job.rec.Transcript())
		}
	}
	if got, err := os.ReadFile(filepath.Join(workspace, "go.work")); err != nil || string(got) != stale {
		t.Fatalf("the hosted run rewrote go.work (%v):\n%s", err, got)
	}

	online := newInstallJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	online.members = []string{"app", "ext"}
	online.run(t, false)
	if got, _ := os.ReadFile(filepath.Join(workspace, "go.work")); !strings.Contains(string(got), "./ext") {
		t.Errorf("an install without the offline signal kept go.work without ./ext:\n%s", got)
	}
}

// TestWorkspaceInstall_OfflineDownloadsNothing is the hosted run end to end:
// workspace-fetch fills the module cache, then workspace-install runs with the
// offline signal against the same cache and downloads nothing. Every go
// command it runs, from the selection of Go on, has GOPROXY=off,
// GONOPROXY=none and -mod=readonly; it warms neither the build list nor the
// pinned tool sources, which the fetch did, and a --force does not delete the
// tools the fetch installed. The fixture proxy stays reachable throughout, so
// a download would have succeeded.
func TestWorkspaceInstall_OfflineDownloadsNothing(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"offline-workspace-install-downloads-nothing")
	t.Parallel()
	workspace, proxy := fetchAndOfflineWorkspace(t)
	cacheRoot := jobtest.CacheRoot(t)
	committed := map[string][]byte{}
	for _, name := range []string{"go.work", "ext/go.mod"} {
		data, err := os.ReadFile(filepath.Join(workspace, name))
		if err != nil {
			t.Fatal(err)
		}
		committed[name] = data
	}
	defer func() {
		for name, want := range committed {
			if got, err := os.ReadFile(filepath.Join(workspace, name)); err != nil || !bytes.Equal(got, want) {
				t.Errorf("the hosted fetch and install rewrote %s (%v):\n%s", name, err, got)
			}
		}
	}()

	fetchJob := recordedJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	if status := fetchJob.fetch(t, testCredential("registry.example.test")); status != statusOK {
		t.Fatalf("workspace-fetch = %s:\n%s\n%s", status, fetchJob.rec.Transcript(), fetchJob.stderr)
	}
	if got, ok := fetchJob.rec.MetricValue("pinned-tool-modules-warmed"); !ok || got != 1 {
		t.Fatalf("workspace-fetch pinned-tool-modules-warmed = %v, %v:\n%s", got, ok, fetchJob.rec.Transcript())
	}

	i := recordedJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	i.members = []string{"app", "ext"}
	if status := i.run(t, true); status != statusOK {
		t.Fatalf("offline workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	if got := i.rec.PhaseStatus("modules"); got != "skipped" {
		t.Errorf("modules phase = %q, want skipped:\n%s", got, i.rec.Transcript())
	}
	if !i.rec.Contains("Module downloads are off on this run; workspace-fetch filled the module cache") {
		t.Errorf("no log names workspace-fetch:\n%s", i.rec.Transcript())
	}
	if i.rec.Contains("Force-reinstalling dev tools") {
		t.Errorf("--force removed the tools workspace-fetch installed:\n%s", i.rec.Transcript())
	}
	if !i.rec.Contains("Module downloads are off on this run; workspace-fetch warmed the pinned tool sources") {
		t.Errorf("no log names workspace-fetch for the tool sources:\n%s", i.rec.Transcript())
	}
	if got, ok := i.rec.MetricValue("pinned-tool-modules-warmed"); ok {
		t.Errorf("the offline install warmed %v pinned tool module(s):\n%s", got, i.rec.Transcript())
	}
	records := i.fakes.GoRecords(t)
	if len(records) == 0 {
		t.Fatal("the offline install ran no go command")
	}
	for _, record := range records {
		if downloads(record) {
			t.Errorf("the offline install ran go %v", record.Args)
		}
		for key, want := range map[string]string{"GOPROXY": "off", "GONOPROXY": "none"} {
			if got, _ := record.Value(key); got != want {
				t.Errorf("go %v ran with %s=%q, want %q", record.Args, key, got, want)
			}
		}
		if goflags, _ := record.Value("GOFLAGS"); !strings.Contains(goflags, "-mod=") {
			t.Errorf("go %v ran with GOFLAGS=%q, want -mod=readonly or the command's own -mod", record.Args, goflags)
		}
		if _, set := record.Value("NETRC"); set {
			t.Errorf("go %v ran offline with NETRC set", record.Args)
		}
	}

	// What the fetch downloaded builds the workspace with the network gone.
	build := goCommand(t, proxy, cacheRoot, "-C", filepath.Join(workspace, "app"), "build", "-o", filepath.Join(t.TempDir(), "app"), "./...")
	build.Env = append(build.Env, "GOPROXY=off", "GOFLAGS=-mod=readonly", "GOWORK="+filepath.Join(workspace, "go.work"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("GOPROXY=off build after workspace-fetch: %v\n%s", err, out)
	}
}

// Offline, a pinned tool that is not installed is not built: building it
// downloads its modules, which workspace-fetch does.
func TestWorkspaceInstall_OfflineBuildsNoTool(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	i := recordedJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), false)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("offline workspace-install = %s:\n%s", status, i.rec.Transcript())
	}
	for _, tool := range devTools {
		spec, _ := tools.Lookup(tool)
		if !i.rec.Contains(tool + " " + spec.Version + " is not installed, and module downloads are off on this run: workspace-fetch installs it") {
			t.Errorf("no log names workspace-fetch for %s:\n%s", tool, i.rec.Transcript())
		}
	}
	for _, record := range i.fakes.GoRecords(t) {
		if downloads(record) {
			t.Errorf("the offline install ran go %v", record.Args)
		}
	}
}

// Offline, a workspace no installed Go qualifies for fails, naming where a
// hosted run finds Go, and never asks a go command for a toolchain it would have
// to download.
func TestWorkspaceInstall_OfflineWithoutAQualifyingGoFails(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "go.work", "go 1.999.0\n")
	i := recordedJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusFailed {
		t.Fatalf("offline workspace-install = %s, want FAILED:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("No installed go command qualifies for Go 1.999.0, and module downloads are off on this run: " +
		"a hosted run installs no Go, so put the pinned Go on the runner's PATH, in GOROOT, or in the Putnami home") {
		t.Errorf("no diagnostic names where a hosted run finds Go:\n%s", i.rec.Transcript())
	}
	if got := i.rec.PhaseStatus("go"); got != "failed" {
		t.Errorf("go phase = %q, want failed", got)
	}
	for _, record := range i.fakes.GoRecords(t) {
		if got, _ := record.Value("GOPROXY"); got != "off" {
			t.Errorf("go %v ran with GOPROXY=%q while selecting Go offline", record.Args, got)
		}
	}
}

func TestCredentialNetrc(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "hosted-run-dependencies",
		"the-fetch-netrc-names-each-host-as-go-matches-it")
	got, err := credentialNetrc(testCredential("a.example.test", "b.example.test:443", "b.example.test", "c.example.test:8443"))
	if err != nil {
		t.Fatal(err)
	}
	want := "machine a.example.test login _token password " + testBearer + "\n" +
		"machine b.example.test:443 login _token password " + testBearer + "\n" +
		"machine b.example.test login _token password " + testBearer + "\n" +
		"machine c.example.test:8443 login _token password " + testBearer + "\n"
	if got != want {
		t.Errorf("credentialNetrc =\n%s\nwant\n%s", got, want)
	}

	for name, credential := range map[string]*registry.Credential{
		"nil":                     nil,
		"no host":                 testCredential(),
		"host with white space":   testCredential("a.example.test\nmachine evil.example.test"),
		"host with a bad port":    testCredential("a.example.test:0"),
		"bearer with white space": {Bearer: "pkt_a password evil", ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"a.example.test"}},
		"empty bearer":            {ExpiresAt: "2099-01-01T00:00:00Z", Hosts: []string{"a.example.test"}},
	} {
		if content, err := credentialNetrc(credential); err == nil {
			t.Errorf("%s: credentialNetrc = %q, want a refusal", name, content)
		}
	}
}

func TestWriteCredentialNetrcIsPrivateAndRemovable(t *testing.T) {
	path, remove, err := writeCredentialNetrc(testCredential("a.example.test"))
	if err != nil {
		t.Fatal(err)
	}
	if got := jobtest.ReadFile(t, path); got != "machine a.example.test login _token password "+testBearer+"\n" {
		t.Errorf("NETRC content = %q", got)
	}
	if runtime.GOOS != "windows" {
		for target, want := range map[string]os.FileMode{path: 0o600, filepath.Dir(path): 0o700} {
			if info, err := os.Stat(target); err != nil || info.Mode().Perm() != want {
				t.Errorf("%s mode = %v (%v), want %v", target, info.Mode().Perm(), err, want)
			}
		}
	}
	remove()
	remove()
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Errorf("the NETRC directory outlived remove: %v", err)
	}
	if _, _, err := writeCredentialNetrc(testCredential("bad host")); err == nil {
		t.Error("writeCredentialNetrc wrote a malformed host")
	}
}
