package qualify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	httproutes "go.putnami.dev/protocol/http-routes"
	qualifyproto "go.putnami.dev/protocol/qualify"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/sdk/extension/scratch"
	"go.putnami.dev/tooling/cli/internal/compose"
	"go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The local-target tests compose real workloads through the real engine: a
// temporary git repository holds a workspace whose in-tree extension serves
// every project by re-executing this test binary in fixture mode. Each member is
// then a process that binds a real ephemeral port, announces it with the runtime
// protocol's typed ready event, reports completed startup the way an
// application framework does, and answers HTTP.
const localFixtureEnv = "PUTNAMI_QUALIFY_LOCAL_FIXTURE"

// Fixture member behaviors, read from fixture.json in the member's directory.
const (
	fixtureServe      = "serve"       // bind, announce, report completed startup, serve
	fixtureNeverReady = "never-ready" // start, log, never announce
	fixtureWriteTree  = "write-tree"  // write a file into the worktree, then serve
	// fixtureListenOnly announces its listener and answers /readyz, but never
	// reports completed startup.
	fixtureListenOnly = "listen-only"
	// fixtureGatedStart announces its listener, then reports completed startup
	// once the test creates its release marker.
	fixtureGatedStart = "gated-start"
	// fixtureFailAfterListen announces its listener, then exits 3 without
	// completed startup once the test creates its release marker, as an
	// application whose start hook fails after its HTTP server bound does.
	fixtureFailAfterListen = "fail-after-listen"
)

func TestMain(m *testing.M) {
	if os.Getenv(localFixtureEnv) != "" {
		os.Exit(runLocalFixtureWorkload())
	}
	// scratch.New, not os.MkdirTemp: a killed binary cannot run the removal
	// below, and the next run reclaims what it left.
	dir, err := scratch.New("putnami-qualify-test-")
	if err == nil {
		_ = os.Setenv("PUTNAMI_STORE_DIR", filepath.Join(dir.Path(), "store"))
		_ = os.Setenv("PUTNAMI_ARTIFACT_DIR", filepath.Join(dir.Path(), "artifacts"))
	}
	code := m.Run()
	_ = dir.Remove()
	os.Exit(code)
}

// localFixtureConfig is a member's fixture.json.
type localFixtureConfig struct {
	Mode    string `json:"mode"`
	Markers string `json:"markers"`
}

// localFixtureRecord is what a member writes about itself when it starts.
type localFixtureRecord struct {
	PID     int    `json:"pid"`
	Port    string `json:"port"`
	NodeEnv string `json:"nodeEnv"`
}

// localFixtureEvent is one runtime protocol v2 line a member prints.
type localFixtureEvent struct {
	V       int                     `json:"v"`
	Type    string                  `json:"type"`
	Level   string                  `json:"level,omitempty"`
	Message string                  `json:"message,omitempty"`
	Data    *runtimeproto.ReadyData `json:"data,omitempty"`
}

func emitFixtureEvent(event localFixtureEvent) {
	event.V = 2
	data, _ := json.Marshal(event)
	fmt.Println(string(data))
}

func emitFixtureLog(level, message string) {
	emitFixtureEvent(localFixtureEvent{Type: "log", Level: level, Message: message})
}

// runLocalFixtureWorkload is a member process. Its cwd is the project directory
// (the job runner's default), which is where its fixture.json lives.
func runLocalFixtureWorkload() int {
	cwd, _ := os.Getwd()
	name := filepath.Base(cwd)
	var config localFixtureConfig
	data, err := os.ReadFile("fixture.json")
	if err == nil {
		err = json.Unmarshal(data, &config)
	}
	if err != nil {
		emitFixtureLog("error", "fixture.json: "+err.Error())
		return 2
	}
	marker := func(suffix string) string { return filepath.Join(config.Markers, name+suffix) }
	record, _ := json.Marshal(localFixtureRecord{PID: os.Getpid(), Port: os.Getenv("PORT"), NodeEnv: os.Getenv("NODE_ENV")})
	_ = os.WriteFile(marker(".json"), record, 0o600)
	emitFixtureLog("info", "fixture "+name+" starting in "+config.Mode)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	parentGone := func() bool { return os.Getppid() == 1 }
	wait := func() int {
		for {
			select {
			case <-stop:
				return 0
			case <-time.After(50 * time.Millisecond):
				if parentGone() {
					return 0
				}
			}
		}
	}
	if config.Mode == fixtureNeverReady {
		return wait()
	}
	if config.Mode == fixtureWriteTree {
		_ = os.WriteFile("written-at-boot.txt", []byte("a workload that writes into its own sources\n"), 0o600)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:"+os.Getenv("PORT"))
	if err != nil {
		emitFixtureLog("error", err.Error())
		return 1
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		appendLine(marker(".readyz"), "GET /readyz")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("/items", func(w http.ResponseWriter, _ *http.Request) {
		appendLine(marker(".requests"), "GET /items")
		_, _ = io.WriteString(w, `[]`)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		appendLine(marker(".requests"), "GET /slow")
		select {
		case <-r.Context().Done():
		case <-time.After(hangDetector):
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	emitFixtureEvent(localFixtureEvent{Type: "ready", Data: &runtimeproto.ReadyData{
		Target: runtimeproto.ReadyTargetServer,
		Endpoints: []runtimeproto.ReadyEndpoint{{
			Scheme: runtimeproto.ReadySchemeHTTP, Host: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port,
		}},
	}})
	_ = os.WriteFile(marker(".listening"), nil, 0o600)
	// released waits for the release marker the test creates; false means the
	// member was stopped first.
	released := func() bool {
		for {
			if _, err := os.Stat(marker(".release")); err == nil {
				return true
			}
			select {
			case <-stop:
				return false
			case <-time.After(20 * time.Millisecond):
				if parentGone() {
					return false
				}
			}
		}
	}
	reportStartup := func() {
		emitFixtureEvent(localFixtureEvent{Type: "ready", Data: &runtimeproto.ReadyData{Target: runtimeproto.ReadyTargetWorkload}})
	}
	switch config.Mode {
	case fixtureListenOnly:
	case fixtureGatedStart, fixtureFailAfterListen:
		if !released() {
			_ = server.Close()
			return 0
		}
		if config.Mode == fixtureFailAfterListen {
			emitFixtureLog("error", "fixture "+name+" failed after it started listening")
			_ = server.Close()
			return 3
		}
		appendLine(marker(".requests"), "started")
		reportStartup()
	default:
		reportStartup()
	}
	code := wait()
	_ = server.Close()
	return code
}

func appendLine(path, line string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer func() { _ = file.Close() }()
	_, _ = io.WriteString(file, line+"\n")
}

// localMember is one project of a local fixture workspace.
type localMember struct {
	name     string
	mode     string
	runsWith []string
	// routes, when set, are committed as the member's route inventory.
	routes []httproutes.Route
}

// localFixture is a committed git repository holding a composable workspace.
type localFixture struct {
	root    string
	markers string
	ws      *workspace.Workspace
}

func requireLocalFixtureTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not available, and a local target binds to a git worktree")
	}
}

func newLocalFixture(t *testing.T, members ...localMember) *localFixture {
	t.Helper()
	requireLocalFixtureTools(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	fixture := &localFixture{root: t.TempDir(), markers: t.TempDir()}
	write := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(fixture.root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	includes := []string{"fixture-extension"}
	for _, member := range members {
		includes = append(includes, member.name)
	}
	workspaceManifest, _ := json.Marshal(struct {
		Name     string   `json:"name"`
		Includes []string `json:"includes"`
	}{Name: "qualify-local", Includes: includes})
	write("putnami.workspace.json", workspaceManifest)
	// The engine records sessions and compose records leases under .putnami:
	// runtime state, never sources, so the worktree fingerprint must not see it.
	write(".gitignore", []byte(".putnami/\n.gen/\n"))
	write("fixture-extension/putnami.json", []byte(`{"name":"@putnami/qualify-fixture"}`))
	command, _ := json.Marshal(executable)
	write("fixture-extension/putnami.extension.json", []byte(fmt.Sprintf(`{
  "name": "@putnami/qualify-fixture",
  "version": "0.1.0",
  "cliContract": 4,
  "commands": {
    "serve": {
      "description": "Serve the qualify fixture workload.",
      "run": [{"id": "serve", "task": "serve-run"}]
    }
  },
  "tasks": {
    "serve-run": {
      "description": "Long-lived fixture workload that announces typed readiness.",
      "kind": "command",
      "command": %s,
      "env": {%q: "1"},
      "cache": false,
      "timeoutMs": -1,
      "declares": {"effects": ["network", "process"]}
    }
  }
}`, command, localFixtureEnv)))
	for _, member := range members {
		projectManifest, _ := json.Marshal(struct {
			Name       string   `json:"name"`
			Extensions []string `json:"extensions"`
			RunsWith   []string `json:"runsWith,omitempty"`
		}{Name: member.name, Extensions: []string{"@putnami/qualify-fixture"}, RunsWith: member.runsWith})
		write(member.name+"/putnami.json", projectManifest)
		config, _ := json.Marshal(localFixtureConfig{Mode: member.mode, Markers: fixture.markers})
		write(member.name+"/fixture.json", config)
		if member.routes != nil {
			inventory, diags := httproutes.Canonicalize(member.routes)
			if diag.HasErrors(diags) {
				t.Fatalf("canonicalize fixture routes: %v", diags)
			}
			body, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			write(member.name+"/schema/http-routes.json", body)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "-A"},
		{"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = fixture.root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	workspace.InvalidateLoadCache(fixture.root)
	t.Cleanup(func() { workspace.InvalidateLoadCache(fixture.root) })
	fixture.ws, err = workspace.Load(fixture.root)
	if err != nil {
		t.Fatalf("load the fixture workspace: %v", err)
	}
	t.Cleanup(fixture.killRecorded)
	return fixture
}

func (f *localFixture) project(t *testing.T, name string) *workspace.Project {
	t.Helper()
	project := f.ws.ProjectByName(name)
	if project == nil {
		t.Fatalf("the fixture workspace has no project %s", name)
	}
	return project
}

func (f *localFixture) target(t *testing.T, name string, readyTimeout time.Duration) Target {
	t.Helper()
	target, err := NewLocalTarget(LocalOptions{WorkspaceRoot: f.root, Project: f.project(t, name), ReadyTimeout: readyTimeout})
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	return target
}

// record reads what a member wrote when it started, and whether it started.
func (f *localFixture) record(t *testing.T, name string) (localFixtureRecord, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.markers, name+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return localFixtureRecord{}, false
	}
	if err != nil {
		t.Fatal(err)
	}
	var record localFixtureRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	return record, true
}

func (f *localFixture) requests(name string) string {
	data, _ := os.ReadFile(filepath.Join(f.markers, name+".requests"))
	return strings.TrimSpace(string(data))
}

// readinessPolls counts the GET /readyz requests a member answered.
func (f *localFixture) readinessPolls(name string) int {
	data, _ := os.ReadFile(filepath.Join(f.markers, name+".readyz"))
	return strings.Count(string(data), "\n")
}

// releaseOnceListening creates a member's release marker once the member
// announced its listener and pause elapsed.
func (f *localFixture) releaseOnceListening(name string, pause time.Duration) {
	go func() {
		deadline := time.Now().Add(hangDetector)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(filepath.Join(f.markers, name+".listening")); err == nil {
				time.Sleep(pause)
				_ = os.WriteFile(filepath.Join(f.markers, name+".release"), nil, 0o600)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

// assertStopped fails when a member that started is still running, or when a
// composition lease is left behind.
func (f *localFixture) assertStopped(t *testing.T, names ...string) {
	t.Helper()
	for _, name := range names {
		record, started := f.record(t, name)
		if !started {
			t.Errorf("member %s never started", name)
			continue
		}
		if proctree.ProcessAlive(record.PID) {
			t.Errorf("member %s (pid %d) is still running after teardown", name, record.PID)
		}
	}
	if entries, err := os.ReadDir(compose.Root(f.root)); err == nil && len(entries) > 0 {
		t.Errorf("teardown left %d composition lease(s) under .putnami/compose", len(entries))
	}
}

// killRecorded is the backstop: a failed assertion must not leave a fixture
// process running past the test.
func (f *localFixture) killRecorded() {
	entries, _ := os.ReadDir(f.markers)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(f.markers, entry.Name()))
		var record localFixtureRecord
		if err == nil && json.Unmarshal(data, &record) == nil && record.PID > 1 {
			_ = proctree.KillGroup(record.PID)
		}
	}
}

var compositionIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

func TestLocalTarget_BindsToTreeFingerprintAndComposes(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"a-local-verdict-names-the-worktree-it-composed-and-a-clean-teardown")
	fixture := newLocalFixture(t,
		localMember{name: "provider", mode: fixtureServe},
		localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}},
	)
	want, err := git.FingerprintTree(fixture.root)
	if err != nil {
		t.Fatal(err)
	}

	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), fixture.target(t, "app", hangDetector), fastOptions())

	if verdict.State != qualifyproto.StatePassed {
		t.Fatalf("state = %s (%s)\n%+v", verdict.State, phaseStates(verdict), verdict.Phases)
	}
	if b := verdict.Binding; b.Kind != qualifyproto.BindingTree || b.Fingerprint != want.Fingerprint || b.HeadSHA != want.HeadSHA || b.Dirty {
		t.Errorf("binding = %+v, want the tree %s at %s, clean", b, want.Fingerprint, want.HeadSHA)
	}
	if target := verdict.Target; target.Kind != qualifyproto.TargetLocal || !compositionIDPattern.MatchString(target.CompositionID) ||
		!strings.HasPrefix(target.URL, "http://127.0.0.1:") {
		t.Errorf("target = %+v, want the local composition and its proxy URL", target)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v, want clean", verdict.Cleanup)
	}
	if got := fixture.requests("app"); got != "GET /items" {
		t.Errorf("requests reaching the target = %q", got)
	}
	// The target is qualified the way it ships: production mode, no watch, an
	// ephemeral port behind the proxy — exactly like the member it runs with.
	for _, name := range []string{"provider", "app"} {
		if record, _ := fixture.record(t, name); record.NodeEnv != "production" || record.Port != "0" {
			t.Errorf("member %s ran with NODE_ENV=%q PORT=%q, want production and 0", name, record.NodeEnv, record.Port)
		}
	}
	fixture.assertStopped(t, "provider", "app")
}

func TestLocalTarget_TreeThatChangesBeforeReadinessIsDigestMismatch(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"a-worktree-that-changes-before-the-workload-is-ready-is-digest-mismatch")
	fixture := newLocalFixture(t, localMember{name: "app", mode: fixtureWriteTree})

	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), fixture.target(t, "app", hangDetector), fastOptions())

	if verdict.State != qualifyproto.StateDigestMismatch {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=passed version-binding=digest_mismatch smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	phase := verdict.Phases[2]
	if len(phase.Diagnostics) != 1 || phase.Diagnostics[0].Code != qualifyproto.PhaseCodeVersionMismatch ||
		!strings.Contains(phase.Diagnostics[0].Message, "worktree changed") {
		t.Errorf("version-binding diagnostics = %+v", phase.Diagnostics)
	}
	if verdict.Binding.Dirty {
		t.Errorf("the binding must keep the tree qualify started from, which was clean: %+v", verdict.Binding)
	}
	if got := fixture.requests("app"); got != "" {
		t.Errorf("a workload whose tree moved must not be smoked, got %q", got)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v", verdict.Cleanup)
	}
	fixture.assertStopped(t, "app")
}

func TestLocalTarget_CompositionFailureIsCompositionFailedAndStillCleansUp(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"a-composition-failure-names-the-member-and-still-tears-down")
	fixture := newLocalFixture(t,
		localMember{name: "provider", mode: fixtureNeverReady},
		localMember{name: "app", mode: fixtureServe, runsWith: []string{"provider"}},
	)

	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), fixture.target(t, "app", 2*time.Second), fastOptions())

	if verdict.State != qualifyproto.StateCompositionFailed {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=composition_failed readiness=not_run version-binding=not_run smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	resolve := verdict.Phases[0]
	if len(resolve.Diagnostics) != 1 || resolve.Diagnostics[0].Code != qualifyproto.PhaseCodeCompositionFailed {
		t.Fatalf("resolve-target diagnostics = %+v", resolve.Diagnostics)
	}
	for _, want := range []string{compose.CodeReadyTimeout, "/provider", "(phase " + compose.PhaseReadiness + ")"} {
		if !strings.Contains(resolve.Diagnostics[0].Message, want) {
			t.Errorf("the diagnostic does not name %q: %s", want, resolve.Diagnostics[0].Message)
		}
	}
	// The member's output is its own and may repeat the configuration it was
	// given: the verdict never records it.
	if strings.Contains(resolve.Diagnostics[0].Message, "fixture provider starting") {
		t.Errorf("the verdict diagnostic carries member output: %s", resolve.Diagnostics[0].Message)
	}
	if verdict.Target.CompositionID == "" {
		t.Error("a composition_failed verdict does not name the composition whose start failed")
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean || len(verdict.Cleanup.Leftovers) != 0 {
		t.Errorf("cleanup = %+v, want the failed start's clean teardown", verdict.Cleanup)
	}
	if _, started := fixture.record(t, "app"); started {
		t.Error("the target started although the member it runs with never became ready")
	}
	fixture.assertStopped(t, "provider")
}

func TestQualifyLocal_CancelMidSmokeIsCanceledAndCleaned(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"canceling-a-local-run-tears-the-composition-down")
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceManual}
	fixture := newLocalFixture(t, localMember{name: "app", mode: fixtureServe, routes: []httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
		{Match: httproutes.MatchExact, Path: "/slow", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	}})
	project := fixture.project(t, "app")
	contract, unsupported, err := Derive(fixture.ws, project, "")
	if err != nil || unsupported != nil {
		t.Fatalf("Derive: %v %+v", err, unsupported)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		deadline := time.Now().Add(hangDetector)
		for time.Now().Before(deadline) {
			if strings.Contains(fixture.requests("app"), "GET /slow") {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	verdict := execute(t, ctx, contract, fixture.target(t, "app", hangDetector), Options{PollInterval: 5 * time.Millisecond, Unsupported: unsupported})

	if verdict.State != qualifyproto.StateCanceled {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=passed version-binding=passed smoke=canceled teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	if verdict.Requests[0].State != qualifyproto.StatePassed || verdict.Requests[1].State != qualifyproto.StateCanceled {
		t.Errorf("requests = %+v, want /items passed and /slow canceled", verdict.Requests)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v", verdict.Cleanup)
	}
	fixture.assertStopped(t, "app")
}

func TestNewLocalTarget_RefusesAWorktreeItCannotFingerprint(t *testing.T) {
	root := t.TempDir()
	// Never let discovery climb into a repository that happens to hold the
	// temporary directory.
	t.Setenv("GIT_CEILING_DIRECTORIES", filepath.Dir(root))
	project := &workspace.Project{ID: "/app", Name: "app", Path: "app"}
	if _, err := NewLocalTarget(LocalOptions{WorkspaceRoot: root, Project: project}); err == nil || !strings.Contains(err.Error(), "cannot be fingerprinted") {
		t.Errorf("a directory outside any git worktree: %v, want a refusal naming the fingerprint", err)
	}
	if _, err := NewLocalTarget(LocalOptions{WorkspaceRoot: root}); err == nil {
		t.Error("a local target without a project was accepted")
	}
}

// derivedRun derives a member's contract and its platform the way the qualify
// command does, so readiness follows the member's own route inventory.
func derivedRun(t *testing.T, fixture *localFixture, name string) (*qualifyproto.Contract, Options) {
	t.Helper()
	project := fixture.project(t, name)
	platform, err := ResolvePlatform(fixture.ws, project, "")
	if err != nil {
		t.Fatalf("ResolvePlatform: %v", err)
	}
	contract, unsupported, err := Derive(fixture.ws, project, platform.Prefix)
	if err != nil || unsupported != nil {
		t.Fatalf("Derive: %v %+v", err, unsupported)
	}
	opts := fastOptions()
	opts.PlatformPrefix, opts.ReadinessRoute = platform.Prefix, platform.ReadinessRoute
	return contract, opts
}

func TestLocalTarget_ReadinessIsTheApplicationsCompletedStartup(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-readiness-is-completed-startup",
		"a-local-target-without-a-readiness-route-passes-on-completed-startup")
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceManual}
	// The inventory declares no readiness route, and the member serves an
	// undeclared /readyz that answers ok at once: only the startup report may
	// let the smoke through.
	fixture := newLocalFixture(t, localMember{name: "app", mode: fixtureGatedStart, routes: []httproutes.Route{
		{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance},
	}})
	contract, opts := derivedRun(t, fixture, "app")
	if opts.ReadinessRoute {
		t.Fatal("the inventory declares no readiness route, yet one was resolved")
	}
	fixture.releaseOnceListening("app", 300*time.Millisecond)

	verdict := execute(t, context.Background(), contract, fixture.target(t, "app", hangDetector), opts)

	if verdict.State != qualifyproto.StatePassed {
		t.Fatalf("state = %s (%s)\n%+v", verdict.State, phaseStates(verdict), verdict.Phases)
	}
	// "started" is written just before the startup report: a smoke request
	// recorded ahead of it was sent to a workload still starting.
	if got := fixture.requests("app"); got != "started\nGET /items" {
		t.Errorf("member log = %q, want the startup report before the smoke", got)
	}
	if polls := fixture.readinessPolls("app"); polls != 0 {
		t.Errorf("the undeclared /readyz was polled %d times", polls)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v", verdict.Cleanup)
	}
	fixture.assertStopped(t, "app")
}

func TestLocalTarget_NoCompletedStartupIsTimedOutWithoutSmoke(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-readiness-is-completed-startup",
		"completed-startup-that-never-arrives-times-out-without-smoke")
	spectest.Proves(t, "cli/workload-qualification", "local-readiness-is-completed-startup",
		"a-declared-readiness-route-is-polled")
	provenance := httproutes.Provenance{Project: "app", SourceKind: httproutes.SourceManual}
	items := httproutes.Route{Match: httproutes.MatchExact, Path: "/items", Methods: []string{"GET"}, PublicEdge: true, Provenance: provenance}
	platformRoute := func(path string) httproutes.Route {
		return httproutes.Route{Match: httproutes.MatchExact, Path: path, Methods: []string{"GET"}, Provenance: provenance}
	}

	// A listening member that answers /readyz but never reports completed
	// startup, with an inventory that does not declare /readyz.
	silent := newLocalFixture(t, localMember{name: "app", mode: fixtureListenOnly, routes: []httproutes.Route{items}})
	contract, opts := derivedRun(t, silent, "app")
	opts.ReadyTimeout = time.Second
	verdict := execute(t, context.Background(), contract, silent.target(t, "app", hangDetector), opts)
	if verdict.State != qualifyproto.StateTimedOut {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=timed_out version-binding=not_run smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	if diagnostics := verdict.Phases[1].Diagnostics; len(diagnostics) != 1 || diagnostics[0].Code != qualifyproto.PhaseCodeNotReady ||
		!strings.Contains(diagnostics[0].Message, "never reported completed startup") {
		t.Errorf("readiness diagnostics = %+v", diagnostics)
	}
	if got, polls := silent.requests("app"), silent.readinessPolls("app"); got != "" || polls != 0 {
		t.Errorf("requests %q and %d readiness polls reached a workload that never completed startup", got, polls)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v", verdict.Cleanup)
	}
	silent.assertStopped(t, "app")

	// The same member with an inventory that declares GET /readyz and
	// /version: readiness polls the declared route, locally as for a URL.
	declared := newLocalFixture(t, localMember{name: "app", mode: fixtureListenOnly, routes: []httproutes.Route{
		items, platformRoute("/readyz"), platformRoute("/version"),
	}})
	contract, opts = derivedRun(t, declared, "app")
	if !opts.ReadinessRoute || opts.PlatformPrefix != "" {
		t.Fatalf("options = %+v, want the declared readiness route at the root", opts)
	}
	polled := execute(t, context.Background(), contract, declared.target(t, "app", hangDetector), opts)
	if polled.State != qualifyproto.StatePassed {
		t.Fatalf("declared route: state = %s (%s)", polled.State, phaseStates(polled))
	}
	if got, polls := declared.requests("app"), declared.readinessPolls("app"); got != "GET /items" || polls == 0 {
		t.Errorf("declared route: requests %q after %d readiness polls, want the smoke after a poll", got, polls)
	}
	declared.assertStopped(t, "app")
}

func TestLocalTarget_ATargetThatExitsBeforeCompletedStartupIsCompositionFailed(t *testing.T) {
	spectest.Proves(t, "cli/workload-qualification", "local-verdict-binds-to-the-exact-worktree",
		"a-composition-failure-names-the-member-and-still-tears-down")
	fixture := newLocalFixture(t, localMember{name: "app", mode: fixtureFailAfterListen})
	fixture.releaseOnceListening("app", 0)

	verdict := execute(t, context.Background(), contractFor(t, "/app", "GET /items"), fixture.target(t, "app", hangDetector), fastOptions())

	if verdict.State != qualifyproto.StateCompositionFailed {
		t.Fatalf("state = %s (%s)", verdict.State, phaseStates(verdict))
	}
	if got := phaseStates(verdict); got != "resolve-target=passed readiness=composition_failed version-binding=not_run smoke=not_run teardown=passed" {
		t.Errorf("phases = %s", got)
	}
	readiness := verdict.Phases[1]
	if len(readiness.Diagnostics) != 1 || readiness.Diagnostics[0].Code != qualifyproto.PhaseCodeCompositionFailed {
		t.Fatalf("readiness diagnostics = %+v", readiness.Diagnostics)
	}
	for _, want := range []string{compose.CodeMemberExited, "/app", "completed startup", "(phase " + compose.PhaseReadiness + ")"} {
		if !strings.Contains(readiness.Diagnostics[0].Message, want) {
			t.Errorf("the diagnostic does not name %q: %s", want, readiness.Diagnostics[0].Message)
		}
	}
	if strings.Contains(readiness.Diagnostics[0].Message, "failed after it started listening") {
		t.Errorf("the verdict diagnostic carries member output: %s", readiness.Diagnostics[0].Message)
	}
	if got := fixture.requests("app"); got != "" {
		t.Errorf("requests %q reached a workload whose startup failed", got)
	}
	if verdict.Cleanup == nil || verdict.Cleanup.State != qualifyproto.CleanupClean {
		t.Errorf("cleanup = %+v", verdict.Cleanup)
	}
	fixture.assertStopped(t, "app")
}
