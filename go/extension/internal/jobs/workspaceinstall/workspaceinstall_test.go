package workspaceinstall

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/go/extension/internal/workspacejob"
	"go.putnami.dev/go/extension/internal/workspacejob/jobtest"
	"go.putnami.dev/go/extension/tools"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

func TestMain(m *testing.M) {
	jobtest.ServeFake()
	os.Exit(m.Run())
}

// installJob is one workspace-install run under test.
type installJob struct {
	job    *workspacejob.Job
	rec    *jobtest.Recorder
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	fakes  *jobtest.Fakes
	// members are the project paths of the workspace membership the job
	// context carries.
	members []string
}

// newInstallJob returns a job for workspace against the fixture proxy at
// proxyURL, with an isolated cache root.
//
// PATH holds the traps first, then the fake pinned tools, then the directory
// of the real go command and nothing else: a host-installed linter cannot
// answer for the pin, and a shell, curl, tar, unzip or jq the job starts runs
// a trap that records it.
func newInstallJob(t *testing.T, workspace, proxyURL, cacheRoot string, fakeTools bool, extra ...string) *installJob {
	t.Helper()
	goBinary := jobtest.RequireGo(t)
	fakes := jobtest.NewFakes(t)
	if fakeTools {
		for _, tool := range devTools {
			spec, _ := tools.Lookup(tool)
			fakes.Tool(t, tool, spec.Version)
		}
	}
	path := fakes.Dir + string(filepath.ListSeparator) + filepath.Dir(goBinary)
	env := jobtest.Env(t, proxyURL, cacheRoot, append(append(fakes.Environ(), "PATH="+path), extra...)...)
	rec := &jobtest.Recorder{}
	j, stdout, stderr := jobtest.NewJob(t, rec, env, workspace)
	return &installJob{job: j, rec: rec, stdout: stdout, stderr: stderr, fakes: fakes}
}

// run runs the job and checks what every run owes: nothing but events on the
// job's standard output, which is the JSONL stream, and no shell program
// started. The job gets no registry origin from its context.
func (i *installJob) run(t *testing.T, force bool) string {
	t.Helper()
	status := run(i.job, force, "", i.members)
	if i.stdout.Len() > 0 {
		t.Errorf("a command wrote to the job event stream:\n%s", i.stdout)
	}
	if invocations := i.fakes.Invocations(t); len(invocations) != 0 {
		t.Errorf("workspace-install started shell programs: %v", invocations)
	}
	return status
}

// buildListWorkspace lays out a workspace whose build list exercises both gaps
// the warm-up has to close:
//
//   - INDIRECT: app requires lib, and only lib requires dep. Downloading the
//     direct requirements alone would leave dep missing, which is the gap that
//     made an offline task graph reach for the network.
//   - IN THE GRAPH, NOT IN THE BUILD: lib also requires extra, which nothing
//     imports. `go list -m all` names it, so the warm-up must fetch it, and
//     both `go list -m all` and an argument-less `go mod download all` append
//     its hash to go.work.sum, which is the mutation the phase must undo.
//
// app declares `publish: ["go"]`, so it is also a standalone member: its
// GOWORK=off graph must be warmed too, because publish, deploy and extension
// jobs build it that way.
func buildListWorkspace(t *testing.T) (workspace, proxy string) {
	t.Helper()
	proxy = jobtest.WriteModuleProxy(t,
		jobtest.ProxyModule{
			Path: "example.com/dep", Version: "v1.0.0",
			GoMod:  "module example.com/dep\n\ngo 1.22\n",
			Source: "package dep\n\nfunc Name() string { return \"dep\" }\n",
		},
		jobtest.ProxyModule{
			Path: "example.com/extra", Version: "v1.0.0",
			GoMod:  "module example.com/extra\n\ngo 1.22\n",
			Source: "package extra\n\nfunc Name() string { return \"extra\" }\n",
		},
		jobtest.ProxyModule{
			Path: "example.com/lib", Version: "v1.0.0",
			GoMod: "module example.com/lib\n\ngo 1.22\n\nrequire (\n\texample.com/dep v1.0.0\n" +
				"\texample.com/extra v1.0.0\n)\n",
			Source: "package lib\n\nimport \"example.com/dep\"\n\nfunc Name() string { return dep.Name() }\n",
		},
	)
	workspace = jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["app"]}`+"\n")
	jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./app\n)\n")
	jobtest.WriteFile(t, workspace, "app/putnami.json", `{"name":"app","publish":["go"]}`+"\n")
	jobtest.WriteFile(t, workspace, "app/go.mod", "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n")
	jobtest.WriteFile(t, workspace, "app/main.go",
		"package main\n\nimport \"example.com/lib\"\n\nfunc main() { _ = lib.Name() }\n")
	return workspace, proxy
}

// goCommand is a go command the test itself runs against the fixture proxy.
func goCommand(t *testing.T, proxy, cacheRoot string, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(jobtest.RequireGo(t), args...)
	cmd.Env = jobtest.Env(t, jobtest.FileURL(proxy), cacheRoot,
		"GOMODCACHE="+filepath.Join(cacheRoot, "mod"), "GOCACHE="+filepath.Join(cacheRoot, "build"))
	return cmd
}

// seedChecksums brings the workspace to the state a real checkout is in: the
// checksum surfaces hold exactly what BUILDING the workspace needed, which is
// what a contributor commits. Seeding with `go mod download all` instead would
// write the maximal set and make the non-mutation assertion vacuous.
func seedChecksums(t *testing.T, workspace, proxy string) {
	t.Helper()
	seedCache := jobtest.CacheRoot(t)
	for _, step := range []struct{ dir, pattern, gowork, goflags string }{
		// Workspace mode writes go.work.sum on its own. Single-module mode is
		// read-only by default and would refuse instead of seeding go.sum, so
		// the standalone step asks for -mod=mod, which workspace mode rejects.
		{workspace, "./app/...", filepath.Join(workspace, "go.work"), ""},
		{filepath.Join(workspace, "app"), "./...", "off", "-mod=mod"},
	} {
		cmd := goCommand(t, proxy, seedCache, "-C", step.dir, "build", "-o", filepath.Join(t.TempDir(), "out"), step.pattern)
		cmd.Env = append(cmd.Env, "GOWORK="+step.gowork, "GOFLAGS="+step.goflags)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("seed checksums in %s: %v\n%s", step.dir, err, out)
		}
	}
}

// TestWorkspaceInstall_DownloadsTheCompleteBuildListWithoutRewritingGoWorkSum
// is the acceptance test for the install-owns-the-warm-up contract: after
// `putnami install`, a task graph running with GOPROXY=off never needs the
// network.
//
// It pins the properties that make the warm-up usable:
//
//   - COMPLETENESS: the indirect module is in the cache, not just the direct one.
//   - NON-MUTATION: go.work.sum and the standalone member's go.sum are
//     byte-identical afterwards. This is why the coordinates are passed to
//     `go mod download` as explicit arguments: the argument-less `all` form
//     rewrites the workspace checksum file.
//   - OFFLINE USABILITY: a real build succeeds with GOPROXY=off against the
//     cache the job filled.
//   - NO SHELL: the whole run, go phase to tools phase, starts no shell.
func TestWorkspaceInstall_DownloadsTheCompleteBuildListWithoutRewritingGoWorkSum(t *testing.T) {
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	seedChecksums(t, workspace, proxy)

	goWorkSum := filepath.Join(workspace, "go.work.sum")
	appSum := filepath.Join(workspace, "app", "go.sum")
	before := map[string]string{
		goWorkSum: jobtest.ReadFile(t, goWorkSum),
		appSum:    jobtest.ReadFile(t, appSum),
	}

	// A fresh cache root, so the run has to download rather than find the
	// modules the seeding step already fetched.
	cacheRoot := jobtest.CacheRoot(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	// The count sums each build list, as the script's did: lib, dep and extra
	// for the workspace, and the same three again for app's GOWORK=off graph.
	if got, ok := i.rec.MetricValue("modules-downloaded"); !ok || got != 6 {
		t.Errorf("modules-downloaded = %v (%v), want 3 workspace and 3 standalone coordinates:\n%s",
			got, ok, i.rec.Transcript())
	}
	if !i.rec.Contains("Downloaded 6 module coordinate(s) in ") {
		t.Errorf("no module download log line:\n%s", i.rec.Transcript())
	}
	for _, phase := range []string{"go", "sync", "modules", "tools"} {
		if got := i.rec.PhaseStatus(phase); got != "success" {
			t.Errorf("phase %s = %q, want success:\n%s", phase, got, i.rec.Transcript())
		}
	}
	for _, tool := range devTools {
		if !i.rec.Contains(tool + " ready at " + filepath.Join(i.fakes.Dir, pkgmeta.ExecutableName(runtime.GOOS, tool))) {
			t.Errorf("%s was not resolved from PATH:\n%s", tool, i.rec.Transcript())
		}
	}

	for path, want := range before {
		if got := jobtest.ReadFile(t, path); got != want {
			t.Errorf("%s was rewritten by the warm-up:\n--- before ---\n%s\n--- after ---\n%s", path, want, got)
		}
	}
	for _, module := range []string{"example.com/lib@v1.0.0", "example.com/dep@v1.0.0", "example.com/extra@v1.0.0"} {
		if _, err := os.Stat(filepath.Join(cacheRoot, "mod", filepath.FromSlash(module))); err != nil {
			t.Errorf("module %s is missing from the warmed cache: %v", module, err)
		}
	}

	build := goCommand(t, proxy, cacheRoot, "-C", filepath.Join(workspace, "app"), "build", "-o", filepath.Join(t.TempDir(), "app"), "./...")
	build.Env = append(build.Env, "GOPROXY=off", "GOWORK="+filepath.Join(workspace, "go.work"))
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("GOPROXY=off build after the warm-up: %v\n%s", err, out)
	}
	for path, want := range before {
		if got := jobtest.ReadFile(t, path); got != want {
			t.Errorf("%s changed during the offline build; the warm-up was incomplete", path)
		}
	}
}

// A standalone member the workspace declares only through includes is warmed
// like one the legacy "projects" member names: its GOWORK=off build list is
// downloaded too, and its go.sum, which that graph could rewrite, is put back
// byte for byte.
func TestWorkspaceInstall_WarmsAStandaloneMemberDeclaredThroughIncludes(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "included-members-are-go-projects", "included-standalone-members-are-warmed")
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"includes":["app"]}`+"\n")
	seedChecksums(t, workspace, proxy)
	appSum := filepath.Join(workspace, "app", "go.sum")
	before := jobtest.ReadFile(t, appSum)

	cacheRoot := jobtest.CacheRoot(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	i.members = []string{"app"}
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	// lib, dep and extra for the workspace, and the same three again for app's
	// GOWORK=off graph.
	if got, ok := i.rec.MetricValue("modules-downloaded"); !ok || got != 6 {
		t.Errorf("modules-downloaded = %v (%v), want 3 workspace and 3 standalone coordinates:\n%s",
			got, ok, i.rec.Transcript())
	}
	if got := jobtest.ReadFile(t, appSum); got != before {
		t.Errorf("app/go.sum was rewritten by the warm-up:\n--- before ---\n%s\n--- after ---\n%s", before, got)
	}
}

// The script rebuilt go.work from `go work edit -json` split on whitespace, so
// a replace without an old version lost its fields and was dropped. The port
// keeps every replace, with or without a version on either side, and the
// warm-up downloads the replacement a versioned replace names.
func TestWorkspaceInstall_SyncKeepsEveryGoWorkReplace(t *testing.T) {
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	jobtest.WriteModuleProxyAt(t, proxy, jobtest.SimpleProxyModules("v1.0.1", "example.com/extra")...)
	jobtest.WriteFile(t, workspace, "local/dep/go.mod", "module example.com/dep\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "local/dep/dep.go", "package dep\n\nfunc Name() string { return \"local\" }\n")
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["app","tool","app"]}`+"\n")
	jobtest.WriteFile(t, workspace, "tool/go.mod", "module example.com/tool\n\ngo 1.21\n")
	jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse ./app\n\n"+
		"replace example.com/dep => ./local/dep\n\n"+
		"replace example.com/extra v1.0.0 => example.com/extra v1.0.1\n")

	cacheRoot := jobtest.CacheRoot(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	work := jobtest.ReadFile(t, filepath.Join(workspace, "go.work"))
	for _, want := range []string{
		"go 1.22\n",
		"\t./app\n\t./tool\n",
		"replace example.com/dep => ./local/dep",
		"replace example.com/extra v1.0.0 => example.com/extra v1.0.1",
	} {
		if !strings.Contains(work, want) {
			t.Errorf("go.work lost %q:\n%s", want, work)
		}
	}
	if strings.Count(work, "./app") != 1 {
		t.Errorf("a project declared twice is used twice:\n%s", work)
	}
	for _, want := range []string{
		"Synced go.work with 2 module(s)",
		"Aligned go.mod go directive in 1 module(s) to Go 1.22",
	} {
		if !i.rec.Contains(want) {
			t.Errorf("missing log %q:\n%s", want, i.rec.Transcript())
		}
	}
	if got := workspacejob.GoDirective(filepath.Join(workspace, "tool", "go.mod")); got != "1.22" {
		t.Errorf("tool/go.mod go directive = %q, want 1.22", got)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "mod", "example.com", "extra@v1.0.1")); err != nil {
		t.Errorf("the replacement example.com/extra@v1.0.1 was not warmed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheRoot, "mod", "cache", "download", "example.com", "dep")); err == nil {
		t.Errorf("the directory-replaced example.com/dep was downloaded")
	}
}

func TestWorkspaceInstall_CreatesGoWorkForTheDeclaredProjects(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["solo"]}`)
	jobtest.WriteFile(t, workspace, "solo/go.mod", "module example.com/solo\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "solo/main.go", "package main\n\nfunc main() {}\n")

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	goVersion := i.job.GoVersion()
	if !i.rec.Contains("Created go.work with 1 module(s) (go " + goVersion + ")") {
		t.Errorf("no creation log:\n%s", i.rec.Transcript())
	}
	if got, want := jobtest.ReadFile(t, filepath.Join(workspace, "go.work")), "go "+goVersion+"\n\nuse (\n\t./solo\n)\n"; got != want {
		t.Errorf("go.work = %q, want %q", got, want)
	}
}

// TestWorkspaceInstall_WarmsThePinnedToolSourcesForOfflinePackaging is the
// other half of the install-owns-the-warm-up contract. `putnami install`
// restores the pinned tool BINARIES, which leaves their SOURCES out of the
// module cache, and `package~archives` compiles those sources inside a
// GOPROXY=off task graph. So the install fetches them, and the packaging
// command that follows resolves the pin, the deprecation query on
// <module>@latest and the whole build list out of the cache.
func TestWorkspaceInstall_WarmsThePinnedToolSourcesForOfflinePackaging(t *testing.T) {
	t.Parallel()
	for _, declaration := range []string{`{"extensions":["/ext"]}`, `{"projects":["ext"]}`} {
		t.Run(declaration, func(t *testing.T) {
			t.Parallel()
			const toolInstall = "example.com/tool@v1.0.0"
			proxy := jobtest.WriteModuleProxy(t, jobtest.ProxyModule{
				Path: "example.com/tool", Version: "v1.0.0",
				GoMod:  "module example.com/tool\n\ngo 1.22\n",
				Source: "package main\n\nfunc main() {}\n",
			})
			jobtest.WriteVersionList(t, proxy, "example.com/tool", "v1.0.0")

			workspace := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, workspace, "putnami.workspace.json", declaration+"\n")
			jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./ext\n)\n")
			jobtest.WriteFile(t, workspace, "ext/putnami.json", `{"name":"ext"}`+"\n")
			jobtest.WriteFile(t, workspace, "ext/go.mod", "module example.com/ext\n\ngo 1.22\n")
			jobtest.WriteFile(t, workspace, "ext/main.go", "package main\n\nfunc main() {}\n")
			jobtest.WriteFile(t, workspace, "ext/tools/versions.json",
				`{"schemaVersion":1,"goVersion":"1.22","tools":{"tool":{"install":"`+toolInstall+`","version":"v1.0.0"}}}`+"\n")

			cacheRoot := jobtest.CacheRoot(t)
			i := newInstallJob(t, workspace, jobtest.FileURL(proxy), cacheRoot, true)
			if status := i.run(t, false); status != statusOK {
				t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
			}
			if got, ok := i.rec.MetricValue("pinned-tool-modules-warmed"); !ok || got != 1 {
				t.Fatalf("pinned-tool-modules-warmed = %v, %v:\n%s", got, ok, i.rec.Transcript())
			}
			if !i.rec.Contains("Warmed the sources of 1 pinned tool module(s) for offline packaging") {
				t.Errorf("no warm-up log:\n%s", i.rec.Transcript())
			}

			// OFFLINE PACKAGING: the archive task's command, with the network gone
			// and the module cache serving as the proxy.
			gopath := t.TempDir()
			install := goCommand(t, proxy, cacheRoot, "install", "-trimpath", toolInstall)
			install.Env = append(install.Env,
				"GOPROXY="+jobtest.FileURL(filepath.Join(cacheRoot, "mod", "cache", "download"))+",off",
				"GOPATH="+gopath, "GOFLAGS=-mod=mod", "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
			if out, err := install.CombinedOutput(); err != nil {
				t.Fatalf("offline cross build after the warm-up: %v\n%s", err, out)
			}
			if _, err := os.Stat(filepath.Join(gopath, "bin", "linux_arm64", "tool")); err != nil {
				t.Fatalf("the offline cross build produced no binary: %v", err)
			}
		})
	}
}

// A pin that leads with "-" would reach go as a flag, so the job fails on it
// instead of running a command it did not intend.
func TestWorkspaceInstall_RefusesAMalformedToolPin(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"extensions":["/ext"]}`)
	manifest := jobtest.WriteFile(t, workspace, "ext/tools/versions.json",
		`{"tools":{"bad":{"install":"-toolexec=evil@v1.0.0"}}}`)

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusFailed {
		t.Fatalf("workspace-install = %s, want FAILED:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("Refusing to warm the malformed tool pin '-toolexec=evil@v1.0.0' from " + manifest) {
		t.Errorf("no refusal diagnostic:\n%s", i.rec.Transcript())
	}
	if got := i.rec.PhaseStatus("tools"); got != "failed" {
		t.Errorf("tools phase = %q, want failed", got)
	}
}

// Fetching a linter's whole dependency tree costs a few hundred megabytes,
// and a workspace that never packages the Go extension must not pay it.
func TestWorkspaceInstall_WarmsNoToolSourcesInAConsumerWorkspace(t *testing.T) {
	t.Parallel()
	workspace, proxy := buildListWorkspace(t)
	seedChecksums(t, workspace, proxy)
	i := newInstallJob(t, workspace, jobtest.FileURL(proxy), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	if _, ok := i.rec.MetricValue("pinned-tool-modules-warmed"); ok {
		t.Errorf("a workspace that ships no tool pins warmed tool sources anyway:\n%s", i.rec.Transcript())
	}
}

// A graph with no external module, and a standalone member whose own graph
// resolves to nothing, log instead of downloading and still report a count.
func TestWorkspaceInstall_EmptyBuildListReportsZero(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["solo"]}`+"\n")
	jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./solo\n)\n")
	jobtest.WriteFile(t, workspace, "solo/putnami.json", `{"name":"solo","publish":["go"]}`+"\n")
	jobtest.WriteFile(t, workspace, "solo/go.mod", "module example.com/solo\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "solo/main.go", "package main\n\nfunc main() {}\n")

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	for _, want := range []string{
		"The workspace build list resolves no downloadable module",
		"The solo build list resolves no downloadable module",
	} {
		if !i.rec.Contains(want) {
			t.Errorf("missing log %q:\n%s", want, i.rec.Transcript())
		}
	}
	if got, ok := i.rec.MetricValue("modules-downloaded"); !ok || got != 0 {
		t.Errorf("modules-downloaded = %v (%v), want the literal 0", got, ok)
	}
	if _, exists := jobtest.ReadOptional(t, filepath.Join(workspace, "go.work.sum")); exists {
		t.Errorf("the warm-up created go.work.sum")
	}
}

func TestWorkspaceInstall_NoGoWorkAndNoRootGoModDownloadsNothing(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("No go.work or root go.mod; nothing to pre-download") {
		t.Errorf("no skip log:\n%s", i.rec.Transcript())
	}
}

// When the WORKSPACE build list cannot be resolved (an empty proxy here, the
// network in CI) the job fails there and then, with a diagnostic, and puts
// back every checksum file `go list` may have touched.
func TestWorkspaceInstall_UnresolvableWorkspaceGraphFails(t *testing.T) {
	t.Parallel()
	workspace, _ := buildListWorkspace(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusFailed {
		t.Fatalf("workspace-install = %s, want FAILED:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("Failed to resolve the workspace build list") {
		t.Errorf("no resolution diagnostic:\n%s", i.rec.Transcript())
	}
	if i.rec.Contains("Skipping the workspace build list") {
		t.Errorf("the workspace build list was skipped instead of failing:\n%s", i.rec.Transcript())
	}
	if got := i.rec.PhaseStatus("modules"); got != "failed" {
		t.Errorf("modules phase = %q, want failed", got)
	}
	if _, exists := jobtest.ReadOptional(t, filepath.Join(workspace, "go.work.sum")); exists {
		t.Errorf("a failed warm-up left go.work.sum behind")
	}
}

// A go.mod that requires a module go.work uses, at a version no proxy serves,
// fails the build list, and the diagnostic names that cause: the go command's
// own error names only the version and the proxy's answer.
func TestWorkspaceInstall_RequireOfAWorkspaceModuleNamesTheCause(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["api","lib"]}`+"\n")
	jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./api\n\t./lib\n)\n")
	jobtest.WriteFile(t, workspace, "api/go.mod", "module example.com/api\n\ngo 1.22\n\nrequire example.com/lib v0.0.0\n")
	jobtest.WriteFile(t, workspace, "api/main.go", "package main\n\nfunc main() {}\n")
	jobtest.WriteFile(t, workspace, "lib/go.mod", "module example.com/lib\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "lib/lib.go", "package lib\n")
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusFailed {
		t.Fatalf("workspace-install = %s, want FAILED:\n%s", status, i.rec.Transcript())
	}
	for _, want := range []string{
		"Failed to resolve the workspace build list",
		"example.com/lib is a module go.work uses",
		"points at ./lib",
	} {
		if !i.rec.Contains(want) {
			t.Errorf("missing %q:\n%s", want, i.rec.Transcript())
		}
	}
}

// namesModuleVersion matches a version of the module, never of a longer path
// that ends with it.
func TestNamesModuleVersion(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"go: example.com/lib@v0.0.0: 404", true},
		{"example.com/lib@v0.0.0: 404", true},
		{"go: foo.example.com/lib@v0.0.0: 404", false},
		{"go: example.com/lib/v2@v2.0.0: 404", false},
		{"go: foo.example.com/lib@v1 and example.com/lib@v0.0.0", true},
		{"go: example.com/lib: no version", false},
	} {
		if got := namesModuleVersion(tc.text, "example.com/lib"); got != tc.want {
			t.Errorf("namesModuleVersion(%q) = %t, want %t", tc.text, got, tc.want)
		}
	}
}

// A standalone member that resolves only through a go.work replace is
// skipped with a warning: the workspace graph, which did resolve, is the one
// tasks use.
func TestWorkspaceInstall_UnresolvableStandaloneGraphIsAWarning(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"projects":["app"]}`)
	jobtest.WriteFile(t, workspace, "go.work",
		"go 1.22\n\nuse (\n\t./app\n)\n\nreplace example.com/lib v1.0.0 => ./local/lib\n")
	jobtest.WriteFile(t, workspace, "app/putnami.json", `{"publish":["go"]}`)
	jobtest.WriteFile(t, workspace, "app/go.mod", "module example.com/app\n\ngo 1.22\n\nrequire example.com/lib v1.0.0\n")
	jobtest.WriteFile(t, workspace, "app/main.go", "package main\n\nimport _ \"example.com/lib\"\n\nfunc main() {}\n")
	jobtest.WriteFile(t, workspace, "local/lib/go.mod", "module example.com/lib\n\ngo 1.22\n")
	jobtest.WriteFile(t, workspace, "local/lib/lib.go", "package lib\n")

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("Skipping the app build list: ") {
		t.Errorf("no standalone skip warning:\n%s", i.rec.Transcript())
	}
}

func TestWorkspaceInstall_ForceRemovesTheMachineToolCopy(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	if !i.job.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary:\n%s", i.rec.Transcript())
	}
	w := &install{Job: i.job}
	stale := make([]string, 0, len(devTools))
	for _, tool := range devTools {
		spec, _ := tools.Lookup(tool)
		path := w.toolHomeBinary(tool, spec.Version)
		if path == "" {
			t.Fatal("no tool home resolves")
		}
		jobtest.WriteFile(t, filepath.Dir(path), filepath.Base(path), "stale")
		stale = append(stale, path)
	}

	if status := i.run(t, true); status != statusOK {
		t.Fatalf("workspace-install --force = %s:\n%s", status, i.rec.Transcript())
	}
	if !i.rec.Contains("Force-reinstalling dev tools...") {
		t.Errorf("no force log:\n%s", i.rec.Transcript())
	}
	for _, path := range stale {
		if _, exists := jobtest.ReadOptional(t, path); exists {
			t.Errorf("--force kept the machine copy %s", path)
		}
	}
}

// A tool that cannot be installed is a warning: lint skips its checks, and
// `putnami install` still succeeds.
func TestWorkspaceInstall_ToolFailureIsAWarning(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), false)
	i.fakes.Tool(t, "golangci-lint", "v0.0.1")
	i.job.Env.Set("JOBTEST_VERSION_GOLANGCI_LINT", "v0.0.1")
	spec, _ := tools.Lookup("staticcheck")
	i.fakes.Tool(t, "staticcheck", spec.Version)
	i.job.Env.Set("JOBTEST_VERSION_STATICCHECK", spec.Version)

	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s", status, i.rec.Transcript())
	}
	lint, _ := tools.Lookup("golangci-lint")
	for _, want := range []string{
		"Ignoring golangci-lint at " + filepath.Join(i.fakes.Dir, pkgmeta.ExecutableName(runtime.GOOS, "golangci-lint")) + ": expected " + lint.Version,
		"Installing golangci-lint " + lint.Version + "...",
		"Failed to install pinned golangci-lint " + lint.Version,
		"golangci-lint installation failed (non-fatal)",
		"staticcheck ready at ",
		"Some tools could not be installed; lint may skip those checks",
	} {
		if !i.rec.Contains(want) {
			t.Errorf("missing %q:\n%s", want, i.rec.Transcript())
		}
	}
	if got := i.rec.PhaseStatus("tools"); got != "success" {
		t.Errorf("tools phase = %q, want success", got)
	}
}

// resolvedTools returns a job whose go command and Go environment are set up,
// for the tool resolution cases.
func resolvedTools(t *testing.T, workspace string, extra ...string) (*install, *installJob) {
	t.Helper()
	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), false, extra...)
	if !i.job.ResolveGoBinary() {
		t.Fatalf("ResolveGoBinary:\n%s", i.rec.Transcript())
	}
	i.job.SetupGoEnv("")
	return &install{Job: i.job, goWork: filepath.Join(workspace, "go.work")}, i
}

// The workspace locations lint still reads are candidates too, in the order
// toolchain.LegacyManagedToolPaths lists them, and one whose version is not
// the pin is skipped for the next.
func TestResolveToolPrefersAMatchingLegacyCopy(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	w, i := resolvedTools(t, workspace)
	legacy := toolchain.LegacyManagedToolPaths("staticcheck", workspace)
	if len(legacy) != 2 {
		t.Fatalf("legacy candidates = %v", legacy)
	}
	i.fakes.ToolAt(t, legacy[0], "0.6.1")
	i.fakes.ToolAt(t, legacy[1], "0.7.0")

	got, ok := w.resolveTool("staticcheck")
	if !ok || got != legacy[1] {
		t.Fatalf("resolveTool(staticcheck) = %q, %v; want %q:\n%s", got, ok, legacy[1], i.rec.Transcript())
	}
	if !i.rec.Contains("Ignoring managed staticcheck at " + legacy[0] + ": expected v0.7.0") {
		t.Errorf("the stale legacy copy was not reported:\n%s", i.rec.Transcript())
	}
	if _, ok := w.resolveTool("gofmt"); ok || !i.rec.Contains("Unknown tool: gofmt") {
		t.Errorf("an unknown tool resolved:\n%s", i.rec.Transcript())
	}
}

// An installed extension ships the pinned tools it was packaged with. The job
// copies the one whose embedded build information names the pin and a Go that
// serves the local one into the machine tool home, instead of compiling it.
func TestResolveToolRestoresTheExtensionArtifact(t *testing.T) {
	t.Parallel()
	spec, _ := tools.Lookup("staticcheck")
	proxy := jobtest.WriteModuleProxy(t, jobtest.ProxyModule{
		Path: "example.com/fakecheck", Version: spec.Version,
		GoMod:  "module example.com/fakecheck\n\ngo 1.22\n",
		Source: "package main\n\nfunc main() {}\n",
	})
	jobtest.WriteVersionList(t, proxy, "example.com/fakecheck", spec.Version)
	gobin := t.TempDir()
	build := goCommand(t, proxy, jobtest.CacheRoot(t), "install", "example.com/fakecheck@"+spec.Version)
	build.Env = append(build.Env, "GOBIN="+gobin, "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build the artifact: %v\n%s", err, out)
	}
	extensionRoot := t.TempDir()
	artifact := filepath.Join(extensionRoot, "compiled", "tools", pkgmeta.ExecutableName(runtime.GOOS, "staticcheck"))
	data, err := os.ReadFile(filepath.Join(gobin, pkgmeta.ExecutableName(runtime.GOOS, "fakecheck")))
	if err != nil {
		t.Fatal(err)
	}
	jobtest.WriteFile(t, filepath.Dir(artifact), filepath.Base(artifact), string(data))

	workspace := jobtest.RealTempDir(t)
	w, i := resolvedTools(t, workspace, "PUTNAMI_EXTENSION_ROOT="+extensionRoot)
	dest := w.toolHomeBinary("staticcheck", spec.Version)
	got, ok := w.resolveTool("staticcheck")
	if !ok || got != dest {
		t.Fatalf("resolveTool(staticcheck) = %q, %v; want the tool home %q:\n%s", got, ok, dest, i.rec.Transcript())
	}
	if !i.rec.Contains("Restored staticcheck " + spec.Version + " from the extension artifact") {
		t.Errorf("no restore log:\n%s", i.rec.Transcript())
	}
	if copied := jobtest.ReadFile(t, dest); copied != string(data) {
		t.Error("the tool home does not hold the artifact bytes")
	}
	if !workspacejob.IsExecutable(dest) {
		t.Error("the restored tool is not executable")
	}
	if staged, _ := filepath.Glob(dest + ".tmp.*"); len(staged) != 0 {
		t.Errorf("staged copies were left behind: %v", staged)
	}

	// Built for another pin, the artifact is not used.
	if w.artifactMatches(artifact, "v9.9.9") {
		t.Error("an artifact built at another version matched the pin")
	}
	if w.artifactMatches(filepath.Join(extensionRoot, "missing"), spec.Version) {
		t.Error("a missing artifact matched")
	}
	assertArtifactServesUpToItsMinor(t, w, i, artifact, spec.Version)
}

// install writes the tool home and lint reads it: a path is a contract
// between two runtimes, so the job's answer must be byte-identical to the one
// toolchain.ToolHome gives the lint job for the same environment.
func TestToolHomeMatchesTheLintResolver(t *testing.T) {
	putnamiHome := t.TempDir()
	t.Setenv("PUTNAMI_HOME", putnamiHome)
	workspace := jobtest.RealTempDir(t)
	w, _ := resolvedTools(t, workspace, "PUTNAMI_HOME="+putnamiHome)
	for _, tool := range devTools {
		spec, _ := tools.Lookup(tool)
		fromJob := w.toolHomeBinary(tool, spec.Version)
		fromLint := toolchain.ToolHome(tool)
		if fromJob == "" || fromJob != fromLint {
			t.Fatalf("workspace-install tool home = %q, lint ToolHome = %q", fromJob, fromLint)
		}
		for _, segment := range []string{putnamiHome, tool, spec.Version, runtime.GOOS + "-" + runtime.GOARCH} {
			if !strings.Contains(fromJob, segment) {
				t.Errorf("tool home %q does not key on %q", fromJob, segment)
			}
		}
	}
	if got := w.toolHomeBinary("staticcheck", ""); got != "" {
		t.Errorf("a tool without a version has a home: %q", got)
	}
}

func TestToolVersionMatchesAsksEachToolItsOwnWay(t *testing.T) {
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	w, i := resolvedTools(t, workspace)
	lint := i.fakes.ToolAt(t, filepath.Join(t.TempDir(), pkgmeta.ExecutableName(runtime.GOOS, "golangci-lint")), "2.10.1", "version", "--short")
	if !w.toolVersionMatches("golangci-lint", lint, "v2.10.1") {
		t.Fatal("golangci-lint v2 was not asked `version --short`")
	}
	if w.toolVersionMatches("staticcheck", lint, "v2.10.1") {
		t.Fatal("a tool that answers only `version --short` matched on `-version`")
	}
	check := i.fakes.ToolAt(t, filepath.Join(t.TempDir(), pkgmeta.ExecutableName(runtime.GOOS, "staticcheck")), "v0.7.0", "-version")
	if !w.toolVersionMatches("staticcheck", check, "v0.7.0") {
		t.Fatal("staticcheck was not asked -version")
	}
	if w.toolVersionMatches("staticcheck", check, "v0.7.1") {
		t.Fatal("another version matched")
	}
}

func TestVersionReported(t *testing.T) {
	for _, tc := range []struct {
		output, version string
		want            bool
	}{
		{"2.10.1", "v2.10.1", true},
		{"golangci-lint has version 2.10.1 built with go1.26", "v2.10.1", true},
		{"staticcheck 2025.1 (0.7.0)", "v0.7.0", true},
		{"2.10.12", "v2.10.1", false},
		{"12.10.1", "2.10.1", false},
		{"2x10x1", "2.10.1", false},
		{"first line\n2.10.1\n", "2.10.1", true},
		{"anything", "", false},
		{"anything", "v", false},
	} {
		if got := VersionReported(tc.output, tc.version); got != tc.want {
			t.Errorf("VersionReported(%q, %q) = %v, want %v", tc.output, tc.version, got, tc.want)
		}
	}
}

func TestToolManifestOwnersFollowTheScriptProgram(t *testing.T) {
	for _, tc := range []struct {
		config string
		want   []string
	}{
		{`{"extensions":["/go/extension","@putnami/go"],"projects":["a","b"]}`, []string{"go/extension", "a", "b"}},
		{`{"extensions":{"x":"/ext"},"projects":{"y":"p"}}`, []string{"ext", "p"}},
		{`{"extensions":null,"projects":["a",3,null]}`, []string{"a"}},
		{`{"extensions":"/ext","projects":["a"]}`, nil},
		{`{"extensions":["/ext"],"projects":"a"}`, []string{"ext"}},
		{`{"extensions":false,"projects":false}`, nil},
		{`not json`, nil},
	} {
		if got := toolManifestOwners([]byte(tc.config)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("toolManifestOwners(%s) = %#v, want %#v", tc.config, got, tc.want)
		}
	}
}

func TestPinnedInstallSpecsFollowTheScriptProgram(t *testing.T) {
	for _, tc := range []struct {
		manifest string
		want     []string
	}{
		{`{"tools":{"a":{"install":"x@v1"},"b":{"install":"y@v2"}}}`, []string{"x@v1", "y@v2"}},
		{`{"tools":[{"install":"x@v1"},null,{"install":null},{"install":false},{},{"install":"y@v2"}]}`, []string{"x@v1", "y@v2"}},
		// jq stops at the first entry it cannot index, keeping what it printed.
		{`{"tools":[{"install":"x@v1"},"text",{"install":"y@v2"}]}`, []string{"x@v1"}},
		{`{"tools":[{"install":3}]}`, []string{"3"}},
		{`{"tools":[{"install":{"a":1}}]}`, []string{"{", `  "a": 1`, "}"}},
		{`{"tools":"text"}`, nil},
		{`{"tools":null}`, nil},
		{`{}`, nil},
		{`{`, nil},
	} {
		manifest := jobtest.WriteFile(t, t.TempDir(), "versions.json", tc.manifest)
		if got := pinnedInstallSpecs(manifest); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("pinnedInstallSpecs(%s) = %#v, want %#v", tc.manifest, got, tc.want)
		}
	}
	if got := pinnedInstallSpecs(filepath.Join(t.TempDir(), "missing.json")); got != nil {
		t.Errorf("pinnedInstallSpecs(missing) = %v", got)
	}
}

func TestParseForce(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"--force"}, true},
		{[]string{"--force", "--no-force"}, false},
		{[]string{"--no-force", "--force"}, true},
		{[]string{"--forced"}, false},
	} {
		if got := parseForce(tc.args); got != tc.want {
			t.Errorf("parseForce(%v) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

func TestMajorMinorAndSortedUnique(t *testing.T) {
	for in, want := range map[string]string{"1.26.1": "1.26", "1.26": "1.26", "1": "1", "": ""} {
		if got := majorMinor(in); got != want {
			t.Errorf("majorMinor(%q) = %q, want %q", in, got, want)
		}
	}
	if got := sortedUnique([]string{"b", "a", "b", "c", "a"}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Errorf("sortedUnique = %v", got)
	}
}

// goLessJob is a workspace-install run on a host with no go command: PATH
// holds only the traps.
func goLessJob(t *testing.T, workspace string) *installJob {
	t.Helper()
	fakes := jobtest.NewFakes(t)
	env := jobtest.Env(t, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), append(fakes.Environ(), "PATH="+fakes.Dir)...)
	rec := &jobtest.Recorder{}
	j, stdout, stderr := jobtest.NewJob(t, rec, env, workspace)
	return &installJob{job: j, rec: rec, stdout: stdout, stderr: stderr, fakes: fakes}
}

// `putnami init --extension go` installs the workspace's dependencies before
// any Go project exists. On a host without Go, nothing pins a Go release to
// install yet, and nothing needs one: the job succeeds without Go, and the
// first Go project pins the release and installs it. A workspace that
// declares Go still fails without a verified Go to install.
func TestWorkspaceInstall_WithoutGoOrAGoDeclarationSkipsGo(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "install-without-go", "workspace-install-skips-go-when-nothing-declares-it")
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"name":"fresh","includes":[]}`+"\n")
	jobtest.WriteFile(t, workspace, "putnami.lock.json", `{"lockVersion":3,"extensions":{}}`+"\n")
	jobtest.WriteFile(t, workspace, "web/package.json", "{}\n")

	i := goLessJob(t, workspace)
	i.members = []string{"web"}
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s, want OK:\n%s", status, i.rec.Transcript())
	}
	if got := i.rec.PhaseStatus("go"); got != "skipped" {
		t.Errorf("go phase = %q, want skipped:\n%s", got, i.rec.Transcript())
	}
	if !i.rec.Contains("The workspace declares no Go module or Go toolchain yet; its first Go project installs Go") {
		t.Errorf("no skip log:\n%s", i.rec.Transcript())
	}
	for _, event := range i.rec.Events() {
		if event.Kind == "diagnostic" || (event.Kind == "phase-start" && event.Name != "go") {
			t.Errorf("a Go-less workspace got %s:\n%s", event, i.rec.Transcript())
		}
	}

	for name, declare := range map[string]func(ws string){
		"go.work":     func(ws string) { jobtest.WriteFile(t, ws, "go.work", "go 1.22\n") },
		"root go.mod": func(ws string) { jobtest.WriteFile(t, ws, "go.mod", "module example.com/root\n\ngo 1.22\n") },
		"Go member":   func(ws string) { jobtest.WriteFile(t, ws, "web/go.mod", "module example.com/web\n\ngo 1.22\n") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			workspace := jobtest.RealTempDir(t)
			jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"includes":["web"]}`+"\n")
			jobtest.WriteFile(t, workspace, "web/package.json", "{}\n")
			declare(workspace)
			i := goLessJob(t, workspace)
			i.members = []string{"web"}
			if status := i.run(t, false); status != statusFailed {
				t.Fatalf("workspace-install = %s, want FAILED:\n%s", status, i.rec.Transcript())
			}
			if !i.rec.Contains("Cannot install a verified Go toolchain") {
				t.Errorf("no install diagnostic:\n%s", i.rec.Transcript())
			}
			if got := i.rec.PhaseStatus("go"); got != "failed" {
				t.Errorf("go phase = %q, want failed", got)
			}
		})
	}
}

// The CLI resolves the workspace's projects from the configuration's
// includes, and the job context carries them. go.work gains every Go project
// among them that it does not use yet, so `putnami install` repairs a go.work
// that lost a member; a use directive, the go directive and the go.mod files
// are left as they are.
func TestWorkspaceInstall_AddsTheIncludedGoProjectsToGoWork(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "included-members-join-go-work", "included-go-members-are-added-to-go-work")
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"includes":["services"]}`+"\n")
	jobtest.WriteFile(t, workspace, "services/putnami.json", `{"includes":["api","lib","web"]}`+"\n")
	goWork := jobtest.WriteFile(t, workspace, "go.work", "go 1.22\n\nuse (\n\t./services/api\n\t./tools/local\n)\n")
	jobtest.WriteFile(t, workspace, "services/api/go.mod", "module example.com/api\n\ngo 1.22\n")
	libMod := jobtest.WriteFile(t, workspace, "services/lib/go.mod", "module example.com/lib\n\ngo 1.21\n")
	jobtest.WriteFile(t, workspace, "services/web/package.json", "{}\n")
	jobtest.WriteFile(t, workspace, "tools/local/go.mod", "module example.com/local\n\ngo 1.22\n")

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	i.members = []string{"services/api", "services/lib", "services/web", "services/lib"}
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s\n%s", status, i.rec.Transcript(), i.stderr)
	}
	if !i.rec.Contains("Added 1 Go project(s) to go.work: ./services/lib") {
		t.Errorf("no sync log:\n%s", i.rec.Transcript())
	}
	uses, err := toolchain.ParseGoWorkUses(goWork)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(uses)
	if want := []string{"services/api", "services/lib", "tools/local"}; !reflect.DeepEqual(uses, want) {
		t.Fatalf("go.work uses %v, want %v:\n%s", uses, want, jobtest.ReadFile(t, goWork))
	}
	if got := workspacejob.GoDirective(goWork); got != "1.22" {
		t.Errorf("go.work go directive = %q, want 1.22", got)
	}
	if got := jobtest.ReadFile(t, libMod); got != "module example.com/lib\n\ngo 1.21\n" {
		t.Errorf("services/lib/go.mod was rewritten:\n%s", got)
	}

	// A go.work that uses every Go project is left byte for byte.
	before := jobtest.ReadFile(t, goWork)
	again := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	again.members = i.members
	if status := again.run(t, false); status != statusOK {
		t.Fatalf("second workspace-install = %s:\n%s", status, again.rec.Transcript())
	}
	if again.rec.Contains("to go.work") {
		t.Errorf("a synced go.work was edited again:\n%s", again.rec.Transcript())
	}
	if got := jobtest.ReadFile(t, goWork); got != before {
		t.Errorf("go.work changed:\n%s\nwant:\n%s", got, before)
	}
}

// Without a go.work the job creates none for the included projects: its go
// directive would be the running go's release, and the Go pin derives from it.
func TestWorkspaceInstall_CreatesNoGoWorkForIncludedProjects(t *testing.T) {
	spectest.Proves(t, "go/go-project-toolchain", "included-members-join-go-work", "included-members-create-no-go-work")
	t.Parallel()
	workspace := jobtest.RealTempDir(t)
	jobtest.WriteFile(t, workspace, "putnami.workspace.json", `{"includes":["api"]}`+"\n")
	jobtest.WriteFile(t, workspace, "api/go.mod", "module example.com/api\n\ngo 1.22\n")

	i := newInstallJob(t, workspace, jobtest.FileURL(t.TempDir()), jobtest.CacheRoot(t), true)
	i.members = []string{"api"}
	if status := i.run(t, false); status != statusOK {
		t.Fatalf("workspace-install = %s:\n%s", status, i.rec.Transcript())
	}
	if _, exists := jobtest.ReadOptional(t, filepath.Join(workspace, "go.work")); exists {
		t.Fatal("workspace-install created go.work for included projects")
	}
}
