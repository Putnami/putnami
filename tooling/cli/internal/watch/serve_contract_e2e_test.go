package watch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/proctree"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The serve/restart conformance proof the dataset row demands.
//
// serve_ready_test.go already proves the CONSUMER: given a typed `ready` event,
// the watcher arms and re-arms. It hands that event to the renderer directly,
// which is the right shape for testing the signal but leaves the whole span
// between an extension's stdout and the serve loop untested — negotiation,
// subprocess spawn, JSONL parse, envelope version, renderer fan-out. Every one
// of those is a place a serve session can quietly stop hot-reloading, and a
// serve session that stops hot-reloading looks exactly like one that is idle.
//
// This test closes that span with a real subprocess:
//
//	manifest (contract 3, loaded by the real loader)
//	  → extension.Resolve / ExpandPipeline
//	  → jobs.RunJob (advertises PUTNAMI_RUNTIME_EVENTS, spawns, parses)
//	  → WatchRenderer.JobEvent
//	  → serveReadySignal → the serve loop arms → restart → arms again
//
// LIMITATION, stated because the issue asks for it: the workload is a fixture
// program, not `putnami serve` on a real @putnami/typescript app. Booting bun
// against a generated app inside a unit test needs a toolchain, an install and
// a port, and would trade this determinism for a flake. What the fixture does
// NOT invent is the wire format — the bytes it emits are the runtime protocol's
// own conformance fixture (fixtures/v2/valid/ready-server.jsonl), so a payload
// rename lands here rather than leaving this proof green against a shape nothing
// emits. The first-party wrappers' own emission is B6a's (the SDK forwarder in
// tooling/extension-sdk/jsonl, tested there); what is unproven anywhere is a
// full bun-serve round trip, which belongs to an e2e sample run.

// The serve workload is this test binary: with serveFixtureStreamEnv set,
// TestMain runs serveFixture instead of the tests, so the fixture spawns the
// same way on every platform.
const (
	serveFixtureStreamEnv = "PUTNAMI_WATCH_SERVE_FIXTURE_STREAM"
	serveFixturePIDsEnv   = "PUTNAMI_WATCH_SERVE_FIXTURE_PIDS"
)

func TestMain(m *testing.M) {
	if stream := os.Getenv(serveFixtureStreamEnv); stream != "" {
		os.Exit(serveFixture(stream, os.Getenv(serveFixturePIDsEnv)))
	}
	os.Exit(m.Run())
}

// serveFixture is the resident serve workload. It records its pid, answers at
// the runtime events version the CLI advertised, copies the stream to stdout
// and stays resident. The serve loop's cancel ends it; so does the end of the
// spawning process, which reparents it on Unix and closes its job on Windows.
func serveFixture(streamPath, pidPath string) int {
	parent := os.Getppid()
	if file, err := os.OpenFile(pidPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644); err == nil {
		_, _ = fmt.Fprintln(file, os.Getpid())
		_ = file.Close()
	}
	if os.Getenv(runtimeproto.AcceptedVersionEnv) != strconv.Itoa(runtimeproto.MaxKnownProtocolVersion) {
		fmt.Printf(`{"v":1,"type":"log","level":"error","message":"putnami did not advertise runtime events v%d"}`+"\n",
			runtimeproto.MaxKnownProtocolVersion)
		return 1
	}
	stream, err := os.ReadFile(streamPath)
	if err != nil {
		return 1
	}
	_, _ = os.Stdout.Write(stream)
	for os.Getppid() == parent {
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// findRepoRoot locates the workspace root so the runtime protocol's own
// conformance fixtures can be used as the workload's output.
func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "putnami.workspace.json")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skipf("could not find Putnami repo root from %s", start)
		}
		dir = parent
	}
}

// readyStreamFixture returns the protocol's ready-server stream up to and
// including the `ready` line. The trailing `result` is dropped: a serve workload
// that has reported its result is finished, and this one has to stay resident.
func readyStreamFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(findRepoRoot(t), "protocols", "runtime", "fixtures", "v2", "valid", "ready-server.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read runtime protocol fixture: %v", err)
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.Contains(line, `"type":"result"`) {
			break
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		t.Fatal("ready-server fixture produced no lines")
	}
	if !strings.Contains(strings.Join(kept, "\n"), `"type":"ready"`) {
		t.Fatal("ready-server fixture carries no ready event; this proof would assert nothing")
	}
	return strings.Join(kept, "\n") + "\n"
}

// writeServeFixtureExtension stages a contract-3 extension whose serve command
// runs a resident workload, and returns the loaded description. Loading it
// through extension.LoadManifest is deliberate: the manifest has to clear the
// same contract-3 ladder a published artifact does, so this proof cannot pass on
// a manifest `putnami publish` would refuse to produce.
func writeServeFixtureExtension(t *testing.T, root, streamPath string) *extension.ExtensionDescription {
	t.Helper()

	extDir := filepath.Join(root, "fixture-extension")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("mkdir extension: %v", err)
	}

	// The workload answers at the version the CLI advertised, exactly as a
	// contract-3 extension must (protocols/runtime negotiation.go): a wrapper
	// that emits v1 into a v2 stream is a protocol violation, not an old build.
	//
	// Two guards keep the resident loop from outliving the suite (it used to
	// survive for DAYS when the test binary died without running cleanups —
	// a `go test` timeout panic or a SIGKILL reaps no children):
	//   - every spawn appends its PID to fixture.pids, and the t.Cleanup
	//     below kills each recorded process group as a backstop;
	//   - the fixture itself polls its parent and exits when it is gone, so
	//     even a hard-killed run leaves nothing behind (serveFixture).
	pidPath := filepath.Join(extDir, "fixture.pids")
	// Backstop reap: the jobs runner starts each spawn in its own process group,
	// so the group id is the fixture's own pid. A dead group is not an error,
	// which is exactly what we want to ignore.
	t.Cleanup(func() {
		data, err := os.ReadFile(pidPath)
		if err != nil {
			return
		}
		for _, field := range strings.Fields(string(data)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 1 {
				continue
			}
			_ = proctree.KillGroup(pid)
		}
	})

	manifest := fmt.Sprintf(`{
  "name": "@putnami/fixture-serve",
  "version": "0.0.0",
  "cliContract": %d,
  "commands": {
    "serve": {
      "description": "Run the fixture workload.",
      "run": [{"id": "serve", "task": "serve-run"}]
    }
  },
  "tasks": {
    "serve-run": {
      "description": "Long-lived fixture workload that announces typed readiness.",
      "kind": "command",
      "command": %q,
      "env": {%q: %q, %q: %q},
      "cache": false,
      "timeoutMs": -1,
      "declares": {"effects": ["network", "process"]}
    }
  }
}`, protocolcli.CurrentContract, os.Args[0],
		serveFixtureStreamEnv, streamPath, serveFixturePIDsEnv, pidPath)

	manifestPath := filepath.Join(extDir, extension.ManifestFilename)
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	loaded, err := extension.LoadManifest(manifestPath)
	if err != nil {
		t.Fatalf("the fixture extension does not load under CLI contract %d: %v", protocolcli.CurrentContract, err)
	}
	return extension.Resolve(loaded, extDir)
}

// readinessRecorder counts the typed readiness events that actually crossed the
// job boundary, so a passing restart cannot be credited to something other than
// the protocol.
type readinessRecorder struct {
	mu         sync.Mutex
	iterations int
	ready      int
	v1Ready    int
}

func (r *readinessRecorder) observe(event jobs.RawJobEvent) {
	if event.Type != jobs.EventTypeReady {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if event.Version == runtimeproto.ProtocolVersion2 {
		r.ready++
		return
	}
	r.v1Ready++
}

func (r *readinessRecorder) startIteration() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.iterations++
}

func (r *readinessRecorder) snapshot() (iterations, ready, v1Ready int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.iterations, r.ready, r.v1Ready
}

// TestServeLoop_TypedReadinessFromARealJobArmsAndReArms is the end-to-end serve
// proof: a real subprocess emits the protocol's readiness stream, the CLI parses
// it, and the serve loop arms on it — twice, because a one-shot signal that
// forgot to re-arm leaves the second server running with no hot reload at all.
//
// The watcher is armed ONLY after readiness, which is what makes a restart
// evidence rather than coincidence: if the ready event never arrived, the loop
// would never snapshot, never see the file change, and never restart.
func TestServeLoop_TypedReadinessFromARealJobArmsAndReArms(t *testing.T) {
	stream := readyStreamFixture(t)

	root := t.TempDir()
	streamPath := filepath.Join(root, "ready.jsonl")
	if err := os.WriteFile(streamPath, []byte(stream), 0o644); err != nil {
		t.Fatalf("write ready stream: %v", err)
	}
	appSrc := filepath.Join(root, "packages", "app", "src")
	if err := os.MkdirAll(appSrc, 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}

	desc := writeServeFixtureExtension(t, root, streamPath)
	project := &workspace.Project{ID: "/packages/app", Name: "app", Path: "packages/app"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{project})
	ws.Name = "serve-e2e"

	recorder := &readinessRecorder{}
	runIteration := func(ctx context.Context, _ []*workspace.Project, sink jobs.Renderer) IterationResult {
		recorder.startIteration()
		job := &jobs.ScheduledJob{
			Project:   project,
			Extension: desc,
			JobDef:    desc.Jobs["serve"],
		}
		result, err := jobs.RunJob(ctx, ws, job, nil, nil, nil, func(event jobs.RawJobEvent) {
			recorder.observe(event)
			sink.JobEvent(job, event)
		})
		if err != nil {
			return IterationResult{ExitCode: protocolcli.ExitFailure}
		}
		// The serve loop's own cancel is what ends every iteration here, and
		// "canceled" is neither a success nor a failure — reporting it as
		// aborted is what the engine seam does with a SIGTERMed serve job.
		if result.Status == "canceled" {
			return IterationResult{Aborted: true}
		}
		if result.Status == "success" {
			return IterationResult{ExitCode: protocolcli.ExitSuccess}
		}
		return IterationResult{ExitCode: protocolcli.ExitFailure}
	}

	out := &syncBuffer{}
	session := &Session{
		cfg: SessionConfig{
			Workspace:        ws,
			SelectedProjects: []*workspace.Project{project},
			Renderer:         &fakeRenderer{},
			RunIteration:     runIteration,
			CommandParams:    map[string]any{"port": freePort(t)},
			ServeMode:        true,
		},
		// The watcher root is the workspace root, not the app directory: it
		// reports paths relative to the root it watched, and those relative
		// paths are what the change→project mapping resolves against.
		watcher: NewWatcher(WatcherConfig{
			Roots:        []string{root},
			PollInterval: 5 * time.Millisecond,
			Debounce:     10 * time.Millisecond,
		}),
		watchRend: NewWatchRenderer(&fakeRenderer{}, out, true),
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- session.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("serve loop did not stop within 10s of cancel")
		}
	}()

	// Two restarts, each driven by its own change. The write is retried under a
	// new filename because the loop re-snapshots when it arms: a file created
	// before that snapshot is part of the baseline, not a change.
	for restart := 1; restart <= 2; restart++ {
		deadline := time.Now().Add(20 * time.Second)
		for attempt := 0; ; attempt++ {
			if strings.Count(out.String(), "restarting server") >= restart {
				break
			}
			if time.Now().After(deadline) {
				iterations, ready, _ := recorder.snapshot()
				t.Fatalf("restart %d never happened — typed readiness did not travel from the job to the watcher.\n"+
					"iterations started: %d, v2 ready events parsed: %d\noutput:\n%s",
					restart, iterations, ready, out.String())
			}
			name := filepath.Join(appSrc, "change-"+strconv.Itoa(restart)+"-"+strconv.Itoa(attempt)+".ts")
			if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
				t.Fatalf("write file: %v", err)
			}
			time.Sleep(25 * time.Millisecond)
		}
	}

	// "restarting server" is printed BEFORE the replacement iteration starts, so
	// the last one's readiness is still in flight when the loop above exits.
	// Wait for it: every serve iteration must announce readiness, including the
	// one no further change will restart.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ready, _ := recorder.snapshot(); ready >= 3 {
			break
		}
		if time.Now().After(deadline) {
			iterations, ready, _ := recorder.snapshot()
			t.Fatalf("typed v2 ready events parsed = %d over %d iterations, want one per iteration (>= 3)",
				ready, iterations)
		}
		time.Sleep(20 * time.Millisecond)
	}

	iterations, ready, v1Ready := recorder.snapshot()
	if iterations < 3 {
		t.Errorf("serve iterations = %d, want 3 (initial serve + 2 restarts)", iterations)
	}
	// Each armed iteration must have been armed by a readiness event that
	// actually crossed the subprocess boundary — not by an iteration exiting,
	// which the loop answers with passive watching rather than a restart.
	if ready < 3 {
		t.Errorf("typed v2 ready events parsed = %d, want one per serve iteration (>= 3)", ready)
	}
	if v1Ready != 0 {
		t.Errorf("%d readiness events arrived on a v1 envelope; only v2 admits the type", v1Ready)
	}
}

// TestServeFixtureAnswersTheAdvertisedProtocolVersion is the negative control
// for the test above. If the CLI stopped advertising PUTNAMI_RUNTIME_EVENTS, the
// fixture would exit 1 instead of emitting readiness — and the restart assertion
// would fail with a timeout that says nothing about why. This runs the same job
// once, without a watch loop, and names the cause directly.
func TestServeFixtureAnswersTheAdvertisedProtocolVersion(t *testing.T) {
	root := t.TempDir()
	streamPath := filepath.Join(root, "ready.jsonl")
	if err := os.WriteFile(streamPath, []byte(readyStreamFixture(t)), 0o644); err != nil {
		t.Fatalf("write ready stream: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packages", "app"), 0o755); err != nil {
		t.Fatalf("mkdir app: %v", err)
	}
	desc := writeServeFixtureExtension(t, root, streamPath)

	project := &workspace.Project{ID: "/packages/app", Name: "app", Path: "packages/app"}
	ws := workspace.NewWorkspace(root, &wsproto.Config{}, []*workspace.Project{project})
	ws.Name = "serve-e2e"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	readyCh := make(chan jobs.RawJobEvent, 1)
	jobCtx, stop := context.WithCancel(ctx)
	defer stop()
	runDone := make(chan struct{})
	// Cancellation only REQUESTS teardown; RunJob still has to SIGTERM the
	// process group and collect the exit. Returning from the test before that
	// completes is how the fixture used to outlive the whole test binary, so
	// wait for the goroutine after stop().
	defer func() {
		stop()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("serve job did not finish within 30s of cancel; the fixture may still be running")
		}
	}()
	go func() {
		defer close(runDone)
		_, _ = jobs.RunJob(jobCtx, ws, &jobs.ScheduledJob{
			Project:   project,
			Extension: desc,
			JobDef:    desc.Jobs["serve"],
		}, nil, nil, nil, func(event jobs.RawJobEvent) {
			if event.Type == jobs.EventTypeReady {
				select {
				case readyCh <- event:
				default:
				}
			}
		})
	}()

	select {
	case event := <-readyCh:
		if event.Version != runtimeproto.ProtocolVersion2 {
			t.Fatalf("ready event arrived at v%d, want v%d", event.Version, runtimeproto.ProtocolVersion2)
		}
		data, err := runtimeproto.ExtractReadyData(event.Data)
		if err != nil {
			t.Fatalf("readiness payload: %v", err)
		}
		if data.Target != runtimeproto.ReadyTargetServer {
			t.Errorf("ready target = %q, want %q", data.Target, runtimeproto.ReadyTargetServer)
		}
		if len(data.Endpoints) == 0 {
			t.Error("a server readiness claim must carry the address it bound")
		}
	case <-ctx.Done():
		t.Fatal("the serve job never reported typed readiness; the CLI's " +
			runtimeproto.AcceptedVersionEnv + " advertisement is the first thing to check")
	}
}
