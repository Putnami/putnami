package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.putnami.dev/protocol/features/spectest"

	cache "go.putnami.dev/protocol/cache"
	protocolcli "go.putnami.dev/protocol/cli"
	workspacepb "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/abort"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	internalgit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/profiler"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// TestMain isolates the machine-global putnami stores for the whole binary:
// execute() resolves ~/.putnami/store (and triggers opportunistic GC over the
// real store roots) unless PUTNAMI_STORE_DIR overrides it. Setting the override
// once, before any test runs, keeps every test — parallel ones included — out
// of the real home directory without per-test t.Setenv (which forbids
// t.Parallel).
func TestMain(m *testing.M) {
	// As the managed compiler of the session-reporting fixture, this binary
	// answers the toolchain's version probe; otherwise it runs as the provider.
	if strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe") == reporterCompiler && len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("1.2.3")
		os.Exit(0)
	}
	// The publication job of the bound publication fixture is a child of this
	// binary that inherits the release-set provider's variable, so its role is
	// read first (bound_publication_provider_test.go).
	if os.Getenv(boundPublicationPackEnv) != "" {
		os.Exit(packBoundPublicationMember())
	}
	// The release-set coordinator resolves its provider by running
	// os.Executable() with the reserved argv, so a child of THIS binary is the
	// provider a fixture session talks to (release_set_session_test.go).
	if os.Getenv(releaseSetFixtureProviderEnv) != "" {
		os.Exit(serveReleaseSetFixtureProvider(os.Args[1:]))
	}
	// A CI runner exports its own remote cache (PUTNAMI_CACHE_URL and
	// PUTNAMI_CACHE_MODE) to every job. A test configures the remote cache
	// through the workspace's .putnami/cache.json only, so the runner's cache
	// must not configure one for every fixture workspace.
	os.Unsetenv("PUTNAMI_CACHE_URL")
	os.Unsetenv("PUTNAMI_CACHE_MODE")
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-engine-test-")
	if err == nil {
		os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	_ = dir.Remove()
	fixtureproc.Remove()
	os.Exit(code)
}

// taskCommand places program outside the workspace and returns its path as a
// JSON string, for the "command" of a task in a manifest the test writes.
func taskCommand(t *testing.T, program fixtureproc.Program) string {
	t.Helper()
	encoded, err := json.Marshal(fixtureproc.Write(t, filepath.Join(t.TempDir(), "task"), program))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// executeFixture builds a minimal in-memory workspace + one project + one
// extension job pointing at a real executable, so (*Engine).execute can be
// driven end-to-end without going through workspace.Load or extension
// discovery.
type executeFixture struct {
	wsRoot   string
	engine   *Engine
	ws       *workspace.Workspace
	cfg      *workspacepb.Config
	req      *Request
	project  *workspace.Project
	ext      *extension.ExtensionDescription
	planned  []*jobs.ScheduledJob
	procPath string // resolved absolute path to the binary the job will run
}

func newExecuteFixture(t *testing.T, opts ...func(*executeFixture)) *executeFixture {
	t.Helper()

	wsRoot := t.TempDir()
	projDir := filepath.Join(wsRoot, "proj")
	if err := os.MkdirAll(projDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// The stand-in exits 0 with no output and ignores its arguments, so the
	// per-job --putnamiContext flag the runner appends is fine. It is this test
	// binary, so it runs on every platform, where /bin/true does not exist.
	procPath := fixtureproc.Write(t, filepath.Join(t.TempDir(), "true"), fixtureproc.Program{})

	proj := &workspace.Project{
		ID:   "/proj",
		Name: "proj",
		Path: "proj",
	}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{proj})
	ws.Name = "test-ws"
	ext := &extension.ExtensionDescription{
		Name: "@putnami/test",
		Path: "/ext/test",
	}

	job := &jobs.ScheduledJob{
		Project:   proj,
		Extension: ext,
		JobDef: &extension.JobDefinition{
			Name:          "build",
			ExtensionName: "@putnami/test",
			Command:       procPath,
		},
	}

	cfg := &workspacepb.Config{}
	f := &executeFixture{
		wsRoot:   wsRoot,
		engine:   New(),
		ws:       ws,
		cfg:      cfg,
		req:      &Request{WorkspaceRoot: wsRoot, Config: cfg, Commands: []string{"build"}},
		project:  proj,
		ext:      ext,
		planned:  []*jobs.ScheduledJob{job},
		procPath: procPath,
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

func TestSelectProjects_ImpactedNonStrictFallbackSelectsAll(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	// A repository whose baseline git cannot resolve: the fallback belongs to
	// that case, and a root with no repository is refused instead.
	initCLISelectionGitRepo(t, wsRoot)
	projects := []*workspace.Project{
		{ID: "/app", Name: "app", Path: "app"},
		{ID: "/lib", Name: "lib", Path: "lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.Impacted = true
	req.Global.Baseline = "no-such-ref"
	req.Global.Projects = "[impacted]"

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != 2 {
		t.Fatalf("selected projects = %d, want 2", len(selected))
	}
	if req.Global.Projects != "*" {
		t.Errorf("fallback Projects = %q, want *", req.Global.Projects)
	}
}

// The lenient fallback is the ONLY branch that turns `--impacted` into a
// whole-workspace run, and it used to print nothing at all — which is how a run
// that verified every project for a two-file diff gets diagnosed months later
// as "the selector treats a root file as everything changed". The message must
// name the cause, the consequence, and the flag that converts the consequence
// into a failure, or the fallback stays indistinguishable from a wide change.
func TestImpactedFallbackExplanationNamesCauseConsequenceAndFlag(t *testing.T) {
	t.Parallel()
	got := fmt.Sprintf(impactedFallbackExplanation, errors.New("resolve git baseline: no origin/HEAD"))

	for _, want := range []string{
		"--impacted",
		"resolve git baseline: no origin/HEAD",
		"EVERY project",
		"--impacted-strict",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("explanation %q does not mention %q", got, want)
		}
	}
}

// A changed workspace-root path selects nothing, and the run says so — on the
// engine's human notice stream, beside the outcome it explains. Without the
// line, "No impacted projects found. Nothing to do." over a non-empty diff is
// indistinguishable from a clean tree.
func TestSelectProjects_ImpactedExplainsUnownedRootFiles(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)

	projects := []*workspace.Project{{ID: "/packages/lib", Name: "lib", Path: "packages/lib"}}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	if err := os.WriteFile(filepath.Join(wsRoot, "bun.lock"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var notices strings.Builder
	req := &Request{Config: &workspacepb.Config{}, Stdout: &notices}
	req.Global.Impacted = true
	req.Global.Baseline = "main"
	req.Global.Projects = "[impacted]"

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess || len(selected) != 0 {
		t.Fatalf("selection = %v (code %d), want none: a root path belongs to no project", selected, code)
	}
	if !strings.Contains(notices.String(), "bun.lock") {
		t.Errorf("notices = %q, want the unattributed root path named", notices.String())
	}

	// A structured or quiet run has no human notice stream: a stray line there
	// is output corruption, not an explanation.
	var quiet strings.Builder
	quietReq := &Request{Config: &workspacepb.Config{}, Stdout: &quiet}
	quietReq.Global.Impacted = true
	quietReq.Global.Baseline = "main"
	quietReq.Global.Projects = "[impacted]"
	quietReq.Global.Output = "jsonl"
	if _, code := selectProjects(quietReq, ws); code != ExitSuccess {
		t.Fatalf("structured selection exit code = %d, want %d", code, ExitSuccess)
	}
	if quiet.Len() != 0 {
		t.Errorf("structured run wrote %q to its notice stream, want no explanation line", quiet.String())
	}
}

// A selection that is too LARGE explains itself on request: --verbose (or
// --debug) prints which file seeded which project and which edge pulled each
// of the others in, on the human notice stream and nowhere else. A plain run
// keeps its output; a quiet or structured run has no notice stream to write
// to.
func TestSelectProjects_ImpactedTracePrintsOnlyUnderVerbose(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	libFile := filepath.Join(wsRoot, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(mutate func(g *GlobalFlags)) string {
		var notices strings.Builder
		req := &Request{Config: &workspacepb.Config{}, Stdout: &notices}
		req.Global.Impacted = true
		req.Global.Baseline = "main"
		req.Global.Projects = "[impacted]"
		mutate(&req.Global)
		selected, code := selectProjects(req, ws)
		if code != ExitSuccess || len(selected) != 2 {
			t.Fatalf("selection = %v (code %d), want lib and app", selected, code)
		}
		return notices.String()
	}

	verbose := run(func(g *GlobalFlags) { g.Verbose = true })
	for _, want := range []string{
		"--impacted: 1 changed file(s) reached 1 project(s) directly; propagation added 1 (dependency 1)",
		"packages/lib/lib.go → /packages/lib  [path-owner packages/lib]",
		"/packages/app ← /packages/lib  [dependency]",
	} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose notices = %q, want %q", verbose, want)
		}
	}
	if debug := run(func(g *GlobalFlags) { g.Debug = true }); !strings.Contains(debug, "--impacted: 1 changed file(s) reached") {
		t.Errorf("debug notices = %q, want the trace", debug)
	}
	if plain := run(func(*GlobalFlags) {}); strings.Contains(plain, "changed file(s) reached") {
		t.Errorf("plain notices = %q, want no trace without --verbose", plain)
	}
	if quiet := run(func(g *GlobalFlags) { g.Verbose, g.Quiet = true, true }); quiet != "" {
		t.Errorf("quiet run wrote %q to its notice stream", quiet)
	}
	for _, mode := range []string{"json", "jsonl", "cloud-logging"} {
		if structured := run(func(g *GlobalFlags) { g.Verbose, g.Output = true, mode }); structured != "" {
			t.Errorf("--output=%s wrote %q to its notice stream", mode, structured)
		}
	}
	// `text` IS the human channel, and a workspace may declare it in config —
	// where it lands in the same field as the flag. Treating any non-empty
	// --output as a machine channel silenced every selection notice in exactly
	// those workspaces.
	if text := run(func(g *GlobalFlags) { g.Verbose, g.Output = true, "text" }); !strings.Contains(text, "changed file(s) reached") {
		t.Errorf("--output=text notices = %q, want the trace", text)
	}
}

func TestSelectProjects_ImpactedStrictFailsOnUnresolvedBaseline(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	projects := []*workspace.Project{{ID: "/app", Name: "app", Path: "app"}}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Config: &workspacepb.Config{}}
	req.Global.Impacted = true
	req.Global.ImpactedStrict = true
	req.Global.Projects = "[impacted]"

	selected, code := selectProjects(req, ws)
	if code != ExitError {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitError)
	}
	if selected != nil {
		t.Fatalf("selected = %v, want nil", selected)
	}
}

func TestSelectProjects_BareFeatureBranchUsesTrunkImpacted(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/workspace-discovery-selection", "safe-baseline", "a-feature-branch-compares-with-the-trunk")
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	runCLISelectionGit(t, wsRoot, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, wsRoot, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, wsRoot, "checkout", "-b", "feature")

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/docs", Name: "docs", Path: "packages/docs"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	libFile := filepath.Join(wsRoot, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	selectedIDs := make(map[string]bool, len(selected))
	for _, p := range selected {
		selectedIDs[p.ID] = true
	}
	if !selectedIDs["/packages/lib"] || !selectedIDs["/packages/app"] {
		t.Fatalf("selected IDs = %v, want lib and dependent app", selectedIDs)
	}
	if selectedIDs["/packages/docs"] {
		t.Fatalf("selected IDs = %v, did not want docs", selectedIDs)
	}
	if !req.Global.Impacted {
		t.Fatal("bare feature selection should route through impacted mode")
	}
	if req.Global.Baseline != "origin/main" {
		t.Fatalf("baseline = %q, want origin/main", req.Global.Baseline)
	}
}

func TestSelectProjects_BareMainIgnoresLastSHAForDifferentCommand(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	head, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if err := workspace_state.NewSessionStore(wsRoot).RecordSuccessfulBuild("main", []string{"lint"}, nil, head); err != nil {
		t.Fatalf("RecordSuccessfulBuild lint: %v", err)
	}

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All {
		t.Fatal("bare build on main without a build marker should select all projects")
	}
	if req.Global.Impacted {
		t.Fatal("lint marker must not make bare build select impacted")
	}
}

func TestSelectProjects_BareMainIgnoresLastSHAForDifferentParams(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	head, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if err := workspace_state.NewSessionStore(wsRoot).RecordSuccessfulBuild("main", []string{"build"}, map[string]any{"target": "linux/amd64"}, head); err != nil {
		t.Fatalf("RecordSuccessfulBuild build linux: %v", err)
	}

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app"},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}, CommandParams: map[string]any{"target": "darwin/arm64"}}

	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All {
		t.Fatal("bare build on main with different params should select all projects")
	}
	if req.Global.Impacted {
		t.Fatal("marker for different params must not make bare build select impacted")
	}
}

func TestSelectProjects_BareMainPrefersLocalMarkerBeforeRemote(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	base, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	commitCLISelectionFile(t, wsRoot, "packages/lib/lib.go", "package lib\n", "add lib")
	if err := workspace_state.NewSessionStore(wsRoot).RecordSuccessfulBuild("main", []string{"build"}, nil, base); err != nil {
		t.Fatalf("RecordSuccessfulBuild: %v", err)
	}

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		t.Fatal("remote marker should not be queried when local marker exists")
		return nil
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, selectionFixtureProjects())
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	ids := selectedProjectIDs(selected)
	if !ids["/packages/lib"] || !ids["/packages/app"] || ids["/packages/docs"] {
		t.Fatalf("selected IDs = %v, want lib and app only", ids)
	}
	if req.Global.Baseline != base {
		t.Fatalf("baseline = %q, want local marker %q", req.Global.Baseline, base)
	}
	if state.lookupCount() != 0 {
		t.Fatalf("remote lookups = %d, want 0", state.lookupCount())
	}
}

func TestSelectProjects_BareMainRemoteMarkerRequiresProvider(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	base, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	commitCLISelectionFile(t, wsRoot, "packages/lib/lib.go", "package lib\n", "add lib")

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		return &cache.RunMarker{SHA: base}
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, selectionFixtureProjects())
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	ids := selectedProjectIDs(selected)
	if !ids["/packages/lib"] || !ids["/packages/app"] || !ids["/packages/docs"] {
		t.Fatalf("selected IDs = %v, want all projects without provider", ids)
	}
	if req.Global.Impacted || req.Global.Baseline == base {
		t.Fatalf("selection = impacted:%v baseline:%q, want local all-project fallback", req.Global.Impacted, req.Global.Baseline)
	}
	if state.lookupCount() != 0 {
		t.Fatalf("remote lookups = %d, want 0 without provider", state.lookupCount())
	}
}

func TestSelectProjects_BareMainRemoteMarkerMissingFallsBackToAll(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		return nil
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	projects := selectionFixtureProjects()
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All || req.Global.Impacted {
		t.Fatalf("selection = all:%v impacted:%v, want all", req.Global.All, req.Global.Impacted)
	}
}

func TestSelectProjects_BareMainStaleRemoteMarkerFallsBackToAll(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		return &cache.RunMarker{SHA: "ffffffffffffffffffffffffffffffffffffffff"}
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	projects := selectionFixtureProjects()
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All || req.Global.Impacted {
		t.Fatalf("selection = all:%v impacted:%v, want all", req.Global.All, req.Global.Impacted)
	}
}

func TestSelectProjects_BareMainFutureRemoteMarkerFallsBackToAll(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	base, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA base: %v", err)
	}
	commitCLISelectionFile(t, wsRoot, "packages/lib/lib.go", "package lib\n", "future marker")
	future, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA future: %v", err)
	}
	runCLISelectionGit(t, wsRoot, "reset", "--hard", base)

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		return &cache.RunMarker{SHA: future}
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	projects := selectionFixtureProjects()
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All || req.Global.Impacted {
		t.Fatalf("selection = all:%v impacted:%v, want all", req.Global.All, req.Global.Impacted)
	}
}

func TestSelectProjects_BareMainRemoteMarkerIsCommandSpecific(t *testing.T) {
	t.Parallel()
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	base, err := internalgit.HeadSHA(wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, func(req *cache.RunMarkerRequest) *cache.RunMarker {
		if len(req.Commands) == 1 && req.Commands[0] == "lint" {
			return &cache.RunMarker{SHA: base}
		}
		return nil
	})
	defer srv.Close()
	enableRunMarkerServer(t, wsRoot, srv)

	projects := selectionFixtureProjects()
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}}
	selected, code := selectProjects(req, ws)
	if code != ExitSuccess {
		t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
	}
	if len(selected) != len(projects) {
		t.Fatalf("selected projects = %d, want %d", len(selected), len(projects))
	}
	if !req.Global.All || req.Global.Impacted {
		t.Fatalf("remote lint marker must not affect build selection: all:%v impacted:%v", req.Global.All, req.Global.Impacted)
	}
}

func TestSelectProjects_BareSelectionSuppressesNoteForStructuredOutput(t *testing.T) {
	wsRoot := t.TempDir()
	initCLISelectionGitRepo(t, wsRoot)
	runCLISelectionGit(t, wsRoot, "update-ref", "refs/remotes/origin/main", "HEAD")
	runCLISelectionGit(t, wsRoot, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	runCLISelectionGit(t, wsRoot, "checkout", "-b", "feature")

	libFile := filepath.Join(wsRoot, "packages", "lib", "lib.go")
	if err := os.MkdirAll(filepath.Dir(libFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(libFile, []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	projects := []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
	}
	ws := workspace.NewWorkspace(wsRoot, &workspacepb.Config{}, projects)
	req := &Request{Commands: []string{"build"}, Config: &workspacepb.Config{}, Global: GlobalFlags{Output: "jsonl"}}

	out := captureStdout(t, func() {
		selected, code := selectProjects(req, ws)
		if code != ExitSuccess {
			t.Fatalf("SelectProjects exit code = %d, want %d", code, ExitSuccess)
		}
		if len(selected) != 2 {
			t.Fatalf("selected projects = %d, want 2", len(selected))
		}
	})
	if out != "" {
		t.Fatalf("structured output selection wrote stdout %q, want empty", out)
	}
}

// TestExecute_DryRunReturnsEarly verifies that --dry-run short-circuits
// before any scheduler/session/telemetry wiring runs.
func TestExecute_DryRunReturnsEarly(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.DryRun = true

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Errorf("dry-run exit code = %d, want %d", code, ExitSuccess)
	}

	// Dry-run must not create any session under .putnami/sessions.
	if _, err := os.Stat(filepath.Join(f.wsRoot, ".putnami", "sessions")); err == nil {
		t.Error("dry-run should not create session directory")
	}
}

// TestExecute_SuccessfulRun drives the full non-watch path: scheduler runs,
// session is created and finalized, telemetry is invoked, and ExitSuccess
// is returned when all jobs succeed.
func TestExecute_SuccessfulRun(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Errorf("exit code = %d, want %d (ExitSuccess)", code, ExitSuccess)
	}

	// A session directory must have been created and finalized.
	sessDir := filepath.Join(f.wsRoot, ".putnami", "sessions")
	entries, err := os.ReadDir(sessDir)
	if err != nil {
		t.Fatalf("session dir not created: %v", err)
	}
	if len(entries) == 0 {
		t.Error("expected at least one session directory")
	}
}

// TestExecute_ResultFinalizerFailureIncludedInJSONLStream pins that a
// failure injected by a result finalizer — work with no scheduled task behind it
// — still reaches the bounded stream as failure-priority task detail and the
// terminal exact failure count.
func TestExecute_ResultFinalizerFailureIncludedInJSONLStream(t *testing.T) {
	f := newExecuteFixture(t)
	f.req.Global.Output = "jsonl"
	finalize := func(results map[string]*jobs.JobResult) {
		results[unpublishedArchiveFailureKey] = &jobs.JobResult{
			Status: "failed",
			Error:  &jobs.JobError{Message: "archive publisher missing"},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var code int
	out := captureStdout(t, func() {
		code = f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil, finalize).ExitCode
	})
	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}

	sessionEnd := lastStreamRecord(t, out)
	if sessionEnd.ProtocolVersion != protocolcli.ResultProtocolVersion {
		t.Fatalf("session:end protocolVersion = %d, want %d\noutput:\n%s",
			sessionEnd.ProtocolVersion, protocolcli.ResultProtocolVersion, out)
	}
	if sessionEnd.Record != protocolcli.RecordSessionEnd {
		t.Fatalf("last record = %q, want %q\noutput:\n%s", sessionEnd.Record, protocolcli.RecordSessionEnd, out)
	}
	run := sessionEnd.Run
	if run == nil {
		t.Fatalf("session:end carries no run summary\noutput:\n%s", out)
	}
	if run.Outcome != protocolcli.RunOutcomeFailure || run.Counts.Failed == 0 {
		t.Fatalf("session:end outcome=%q failed=%d, want a failed session\noutput:\n%s",
			run.Outcome, run.Counts.Failed, out)
	}
	if sessionEnd.MachineOutput == nil {
		t.Fatal("session:end did not opt into the bounded machine-output profile")
	}
	var archiveFailure *protocolcli.TaskRecord
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Record == protocolcli.RecordTaskEnd && record.Identity != nil && record.Identity.Key == unpublishedArchiveFailureKey {
			archiveFailure = record.Task
		}
	}
	if archiveFailure == nil || archiveFailure.Error == nil || !strings.Contains(archiveFailure.Error.Message, "archive publisher missing") {
		t.Fatalf("archive failure task = %+v, want complete finalizer detail", archiveFailure)
	}
}

// lastStreamRecord decodes the final --output=jsonl line as a v2 session-stream
// record.
func lastStreamRecord(t *testing.T, out string) protocolcli.SessionStreamRecord {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("expected JSONL output")
	}
	var record protocolcli.SessionStreamRecord
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &record); err != nil {
		t.Fatalf("parse the last stream record: %v\noutput:\n%s", err, out)
	}
	return record
}

func TestExecute_SuccessfulRunRecordsLastBuildSHA(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.All = true
	initCLISelectionGitRepo(t, f.wsRoot)
	wantSHA, err := internalgit.HeadSHA(f.wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}

	gotSHA, err := workspace_state.NewSessionStore(f.wsRoot).LastBuildSHA("main", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA: %v", err)
	}
	if gotSHA != wantSHA {
		t.Errorf("LastBuildSHA = %q, want %q", gotSHA, wantSHA)
	}
}

func TestExecute_SuccessfulRunWithoutProviderDoesNotPublishRemoteRunMarker(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.All = true
	initCLISelectionGitRepo(t, f.wsRoot)
	wantSHA, err := internalgit.HeadSHA(f.wsRoot)
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, nil)
	defer srv.Close()
	enableRunMarkerServer(t, f.wsRoot, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Fatalf("exit code = %d, want %d", code, ExitSuccess)
	}

	published := state.publishedRequests()
	if len(published) != 0 {
		t.Fatalf("published markers = %d, want 0 without provider (HEAD %s)", len(published), wantSHA)
	}
}

func TestExecute_FailedRunDoesNotPublishRemoteRunMarker(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.All = true
	initCLISelectionGitRepo(t, f.wsRoot)
	f.planned[0].JobDef.Command = "/nonexistent-binary-xyzzy"

	state := &runMarkerServerState{}
	srv := newRunMarkerTestServer(t, state, nil)
	defer srv.Close()
	enableRunMarkerServer(t, f.wsRoot, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}
	if got := len(state.publishedRequests()); got != 0 {
		t.Fatalf("published markers = %d, want 0", got)
	}
}

// TestExecute_FailureReturnsJobFailedCode ensures the result→exit-code
// mapping fires when the scheduler reports any failed job.
func TestExecute_FailureReturnsJobFailedCode(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	// Override the command to one that does not exist so the job fails.
	f.planned[0].JobDef.Command = "/nonexistent-binary-xyzzy"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitError {
		t.Errorf("exit code = %d, want %d (ExitError)", code, ExitError)
	}
}

func TestExecute_FailedRunDoesNotAdvanceLastBuildSHA(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.All = true
	initCLISelectionGitRepo(t, f.wsRoot)
	store := workspace_state.NewSessionStore(f.wsRoot)
	if err := store.RecordSuccessfulBuild("main", []string{"build"}, nil, "previous"); err != nil {
		t.Fatalf("RecordSuccessfulBuild: %v", err)
	}
	f.planned[0].JobDef.Command = "/nonexistent-binary-xyzzy"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitError {
		t.Fatalf("exit code = %d, want %d", code, ExitError)
	}

	gotSHA, err := store.LastBuildSHA("main", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA: %v", err)
	}
	if gotSHA != "previous" {
		t.Errorf("LastBuildSHA = %q, want previous", gotSHA)
	}
}

func TestShouldRecordSuccessfulBuildPolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		req  *Request
		want bool
	}{
		{name: "nil", req: nil, want: false},
		{name: "all projects", req: &Request{Global: GlobalFlags{All: true}}, want: true},
		{name: "auto impacted", req: &Request{Global: GlobalFlags{AutoSelected: true, Impacted: true}}, want: true},
		{name: "explicit target", req: &Request{Global: GlobalFlags{Projects: "/tooling/cli"}}, want: false},
		{name: "all with exclude", req: &Request{Global: GlobalFlags{All: true, Exclude: "/legacy"}}, want: false},
		{name: "tag filter", req: &Request{Global: GlobalFlags{FilterTag: "frontend"}}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldRecordSuccessfulBuild(tt.req); got != tt.want {
				t.Fatalf("shouldRecordSuccessfulBuild = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestExecute_NoCacheSkipsCacheManager verifies the --no-cache branch
// (cache == nil path) executes without panic and still returns the
// correct exit code.
func TestExecute_NoCacheSkipsCacheManager(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	f.req.Global.NoCache = true

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Errorf("exit code = %d, want %d", code, ExitSuccess)
	}
}

// TestExecute_ProfilerWritesFile asserts that --profile <path> produces a
// trace file alongside the run.
func TestExecute_ProfilerWritesFile(t *testing.T) {
	t.Parallel()
	f := newExecuteFixture(t)
	profPath := filepath.Join(f.wsRoot, "prof.json")
	f.req.Global.TraceProfile = profPath

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSuccess {
		t.Errorf("exit code = %d, want %d", code, ExitSuccess)
	}
	if _, err := os.Stat(profPath); err != nil {
		t.Errorf("profile file not written: %v", err)
	}
}

func TestRecordProfilerSessionEvent_Coalesced(t *testing.T) {
	t.Parallel()
	p := profiler.New(true)
	// Built through the canonical projection, so the trace is driven by the same
	// typed value the scheduler hands the handler — not by a hand-written payload
	// that could disagree with it.
	task := jobs.TaskResultOf(nil, &jobs.JobResult{
		Status:    "success",
		Coalesced: true,
		Duration:  125 * time.Millisecond,
	})
	recordProfilerSessionEvent(p, jobs.SessionRecord{
		JobKey: "/proj:build",
		Type:   jobs.SessionRecordJobEnd,
		Data:   jobs.JobEndPayload(&task, 0),
		Task:   &task,
	})

	path := filepath.Join(t.TempDir(), "trace.json")
	if err := p.Write(path); err != nil {
		t.Fatalf("write trace: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	var events []profiler.TraceEvent
	if err := json.Unmarshal(data, &events); err != nil {
		t.Fatalf("parse trace: %v", err)
	}
	var sawOutcome, sawInstant bool
	for _, event := range events {
		if event.Name == "/proj:build" && event.Phase == "E" && event.Args["status"] == jobs.JobOutcomeCoalesced {
			sawOutcome = true
		}
		if event.Name == "cache-coalesced" && event.Category == "cache" && event.Phase == "i" {
			sawInstant = true
		}
	}
	if !sawOutcome || !sawInstant {
		t.Fatalf("trace missing coalesced outcome/end marker: %+v", events)
	}
}

type runMarkerServerState struct {
	mu        sync.Mutex
	lookups   int
	published []cache.PublishRunMarkerRequest
}

func (s *runMarkerServerState) lookupCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookups
}

func (s *runMarkerServerState) publishedRequests() []cache.PublishRunMarkerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]cache.PublishRunMarkerRequest(nil), s.published...)
}

func newRunMarkerTestServer(t *testing.T, state *runMarkerServerState, lookup func(*cache.RunMarkerRequest) *cache.RunMarker) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc(cache.RunMarkerLookupPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidateRunMarkerRequest(body)
		if req == nil {
			t.Fatalf("invalid run marker request: %v", diags)
		}
		state.mu.Lock()
		state.lookups++
		state.mu.Unlock()
		var marker *cache.RunMarker
		if lookup != nil {
			marker = lookup(req)
		}
		_ = json.NewEncoder(w).Encode(cache.RunMarkerResponse{ProtocolVersion: cache.ProtocolVersion, Marker: marker})
	})
	mux.HandleFunc(cache.RunMarkerPublishPath, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, diags := cache.ParseAndValidatePublishRunMarkerRequest(body)
		if req == nil {
			t.Fatalf("invalid publish run marker request: %v", diags)
		}
		state.mu.Lock()
		state.published = append(state.published, *req)
		state.mu.Unlock()
		_ = json.NewEncoder(w).Encode(cache.PublishRunMarkerResponse{
			ProtocolVersion: cache.ProtocolVersion,
			Published:       true,
			Marker:          &cache.RunMarker{SHA: req.SHA},
		})
	})
	mux.HandleFunc(cache.NegotiatePath, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(cache.NegotiateResponse{ProtocolVersion: cache.ProtocolVersion})
	})
	return httptest.NewServer(mux)
}

// enableRunMarkerServer configures the workspace's remote cache to point at the
// test server. It writes the per-workspace .putnami/cache.json the engine reads
// (jobs.RemoteCacheConfigPath) instead of setting PUTNAMI_CACHE_URL, which is
// the same configuration through the file half of the resolution — and, being
// workspace-scoped rather than process-global, it is compatible with t.Parallel.
func enableRunMarkerServer(t *testing.T, wsRoot string, srv *httptest.Server) {
	t.Helper()
	path := jobs.RemoteCacheConfigPath(wsRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"url":`+strconv.Quote(srv.URL)+`}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func selectionFixtureProjects() []*workspace.Project {
	return []*workspace.Project{
		{ID: "/packages/app", Name: "app", Path: "packages/app", Dependencies: []string{"lib"}},
		{ID: "/packages/lib", Name: "lib", Path: "packages/lib"},
		{ID: "/packages/docs", Name: "docs", Path: "packages/docs"},
	}
}

func selectedProjectIDs(projects []*workspace.Project) map[string]bool {
	ids := make(map[string]bool, len(projects))
	for _, p := range projects {
		ids[p.ID] = true
	}
	return ids
}

func commitCLISelectionFile(t *testing.T, dir, relPath, content, message string) {
	t.Helper()
	path := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLISelectionGit(t, dir, "add", relPath)
	runCLISelectionGit(t, dir, "commit", "-m", message)
}

func initCLISelectionGitRepo(t *testing.T, dir string) {
	t.Helper()
	runCLISelectionGit(t, dir, "init")
	runCLISelectionGit(t, dir, "config", "user.email", "test@test.com")
	runCLISelectionGit(t, dir, "config", "user.name", "Test")
	runCLISelectionGit(t, dir, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCLISelectionGit(t, dir, "add", "-A")
	runCLISelectionGit(t, dir, "commit", "-m", "initial")
	runCLISelectionGit(t, dir, "branch", "-M", "main")
}

func runCLISelectionGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// Ctrl-C used to exit 0: no job had "failed" — the remaining ones were killed
// before they could — so the run reported success and a CI gate would
// green-light a build that never finished. 130 is the documented code for a
// signal-interrupted run.
func TestExecute_AbortedRunExitsSignalReceived(t *testing.T) {
	f := newExecuteFixture(t)
	f.req.Global.All = true
	initCLISelectionGitRepo(t, f.wsRoot)

	abort.Reset()
	abort.Record(syscall.SIGINT)
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code := f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	if code != ExitSignalReceived {
		t.Fatalf("exit code = %d, want %d (ExitSignalReceived)", code, ExitSignalReceived)
	}

	// The successful-build marker is keyed off the same Success flag. An abort
	// must not leave one behind, or the next run skips work this one never did.
	gotSHA, err := workspace_state.NewSessionStore(f.wsRoot).LastBuildSHA("main", []string{"build"}, nil)
	if err != nil {
		t.Fatalf("LastBuildSHA: %v", err)
	}
	if gotSHA != "" {
		t.Errorf("LastBuildSHA = %q after an aborted run, want empty", gotSHA)
	}
}

// The JSONL stream is what CI parses, so the abort has to be visible there too
// and not just in the human summary.
func TestExecute_AbortedRunJSONLSessionEndReportsAbort(t *testing.T) {
	f := newExecuteFixture(t)
	f.req.Global.Output = "jsonl"

	abort.Reset()
	abort.Record(syscall.SIGINT)
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var code int
	out := captureStdout(t, func() {
		code = f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	})
	if code != ExitSignalReceived {
		t.Fatalf("exit code = %d, want %d", code, ExitSignalReceived)
	}

	sessionEnd := lastStreamRecord(t, out)
	if sessionEnd.Record != protocolcli.RecordSessionEnd {
		t.Fatalf("last record = %q, want %q\noutput:\n%s", sessionEnd.Record, protocolcli.RecordSessionEnd, out)
	}
	run := sessionEnd.Run
	if run == nil {
		t.Fatalf("session:end carries no run summary\noutput:\n%s", out)
	}
	// v2 reports the abort as the outcome itself rather than as a flag beside a
	// success boolean, and the record's exit code must be the 130 the process
	// returned above — the "every surface agrees" rule, checked end to end.
	if run.Outcome != protocolcli.RunOutcomeAborted || run.AbortedBy != protocolcli.AbortedByUser {
		t.Errorf("session:end outcome=%q abortedBy=%q, want %q/%q\noutput:\n%s",
			run.Outcome, run.AbortedBy, protocolcli.RunOutcomeAborted, protocolcli.AbortedByUser, out)
	}
	if run.ExitCode != ExitSignalReceived {
		t.Errorf("session:end exitCode = %d, want the process's %d\noutput:\n%s",
			run.ExitCode, ExitSignalReceived, out)
	}
}

// Both watch loops leave their select on ctx.Done and return normally, carrying
// the last iteration's exit code. Ctrl-C therefore reported 0 — or a stale
// failure from an iteration the user had already fixed — instead of 130.
func TestExecute_AbortedWatchRunExitsSignalReceived(t *testing.T) {
	f := newExecuteFixture(t)
	f.req.Global.Watch = true

	abort.Reset()
	abort.Record(syscall.SIGINT)
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan int, 1)
	go func() {
		done <- f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	}()

	select {
	case code := <-done:
		if code != ExitSignalReceived {
			t.Fatalf("watch exit code = %d, want %d (ExitSignalReceived)", code, ExitSignalReceived)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("watch mode did not return within 15s of a canceled context")
	}
}

// Without a signal, a watch session that ends for its own reasons keeps its
// own exit code — the 130 upgrade must not swallow it.
func TestExecute_WatchWithoutSignalKeepsOwnExitCode(t *testing.T) {
	f := newExecuteFixture(t)
	f.req.Global.Watch = true

	abort.Reset()
	t.Cleanup(abort.Reset)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan int, 1)
	go func() {
		done <- f.engine.execute(ctx, f.req, f.ws, []*workspace.Project{f.project}, &extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{f.ext}}, f.planned, nil, nil).ExitCode
	}()

	select {
	case code := <-done:
		if code == ExitSignalReceived {
			t.Fatalf("watch exit code = %d with no signal recorded; 130 must be signal-only", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("watch mode did not return within 15s of a canceled context")
	}
}

// TestRun_SelectionReceivesTheResolvedExtensions pins the wiring the remote
// run-marker lookup depends on. Auto-selection resolves its cache provider from
// the extensions its caller passes (LoadRemoteCache → DetectCacheProvider), so a
// lifecycle that selects projects WITHOUT them silently demotes every bare
// command to local markers only — a degradation no output reveals and no
// selection assertion catches, because the fallback result is a legitimate one.
//
// TestSelectProjects_BareMainRemoteMarkerRequiresProvider is the behavioral half
// (no extensions ⇒ no remote lookup). This is the structural half: standing up a
// live provider means a subprocess speaking the cache RPC, which internal/jobs
// already owns and this package should not fork.
func TestRun_SelectionReceivesTheResolvedExtensions(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("engine.go")
	if err != nil {
		t.Fatalf("read engine.go: %v", err)
	}
	const want = "selectProjects(req, ws, extensions...)"
	if !strings.Contains(string(data), want) {
		t.Fatalf("engine.go does not call %q: the run's selection stage must receive the "+
			"resolved extensions, or bare commands lose remote run markers", want)
	}
}
