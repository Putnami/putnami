package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// fakeHeldRuns is a machine worker's memory as the engine reads it: it offers
// offer to every engine run and records what each one keeps.
type fakeHeldRuns struct {
	offer   any
	takes   int
	peeks   int
	keeps   int
	record  any
	skipped bool
	// reused counts, per step, the engine runs that took it from the held
	// run (true) and those that computed it (false).
	reused map[string]map[bool]int
}

func (f *fakeHeldRuns) TakeRun(string) any {
	f.takes++
	return f.offer
}

func (f *fakeHeldRuns) PeekRun(string) any {
	f.peeks++
	return f.offer
}

func (f *fakeHeldRuns) Reused(_, step string, held bool) {
	if f.reused == nil {
		f.reused = map[string]map[bool]int{}
	}
	if f.reused[step] == nil {
		f.reused[step] = map[bool]int{}
	}
	f.reused[step][held]++
}

func (f *fakeHeldRuns) KeepRun(_ string, record any, skipped bool) {
	f.keeps++
	f.record, f.skipped = record, skipped
}

func (f *fakeHeldRuns) Witness(path string) string {
	return "stat of " + path
}

// serveHeldRuns installs memory as the worker's for the test, which therefore
// never runs in parallel.
func serveHeldRuns(t *testing.T, memory HeldRuns) {
	t.Helper()
	ServeHeldRuns(memory)
	t.Cleanup(func() { ServeHeldRuns(nil) })
}

// heldRunFixture is a workspace with one project, app, whose build runs a
// cached task, and the request of `build --all` over it.
func heldRunFixture(t *testing.T) (string, Request) {
	t.Helper()
	root := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		path = filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("putnami.workspace.json", `{"name":"held-run","includes":["app","extension"]}`)
	write("app/putnami.json", `{"name":"app","extensions":["/extension"]}`)
	write("app/src.txt", "app\n")
	write("extension/putnami.json", `{"name":"@test/held"}`)
	enabled := true
	manifest, err := json.Marshal(extensionproto.Manifest{
		Name: "@test/held", Version: "1.0.0", CLIContract: 11,
		Commands: map[string]extensionproto.CommandDefinition{
			"build": {ActivationFiles: []string{"src.txt"}, Run: []extensionproto.PipelineStep{{ID: "build", Task: "build"}}},
		},
		Tasks: map[string]extensionproto.TaskDefinition{
			"build": {
				Kind: "command", Command: fixtureproc.Write(t, filepath.Join(t.TempDir(), "build"), fixtureproc.Program{}),
				Reads:    []extensionproto.ResourceRef{{ID: extensionproto.ResourceIDSources}},
				Cache:    &extensionproto.TaskCachePolicy{Enabled: &enabled, Deterministic: true, NoOutput: true},
				Declares: &extensionproto.TaskDeclaration{},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	write("extension/putnami.extension.json", string(manifest))
	workspace.InvalidateLoadCache(root)
	return root, Request{
		WorkspaceRoot: root, Config: wsproto.Load(root), Commands: []string{"build"},
		Global: GlobalFlags{All: true, Projects: "*"}, Stdout: io.Discard,
	}
}

// TestAWorkerAnswersARunFromWhatTheHeldRunLoadedAndPlanned pins the engine's
// side of CLI ADR 0116 §6a: in a worker, a run leaves what its phases 1 to 3
// computed; a later run of the same request offered it takes it instead of
// loading and planning, records its session as any run does, and leaves it
// again; a run of another request, or of a shape that cannot be held, loads
// and plans what is on disk and leaves nothing.
func TestAWorkerAnswersARunFromWhatTheHeldRunLoadedAndPlanned(t *testing.T) {
	root, req := heldRunFixture(t)
	memory := &fakeHeldRuns{}
	serveHeldRuns(t, memory)

	var result SessionResult
	var err error
	stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("first run: exit=%d err=%v\n%s", result.ExitCode, err, stderr)
	}
	held, ok := memory.record.(*heldRun)
	if memory.takes != 1 || memory.keeps != 1 || memory.skipped || !ok || !held.complete || len(held.run.planned) != 1 {
		t.Fatalf("first run left %+v, want one take and one kept record of its one-job plan", memory)
	}
	first := workspace_state.NewSessionStore(root).LatestID()

	// The project no longer uses the extension: a run that loads and plans
	// plans nothing. The worker's journal would refuse to offer the held run
	// now; this memory offers it anyway, which only the absence of a load and
	// a plan can answer with the held job.
	if err := os.WriteFile(filepath.Join(root, "app", "putnami.json"), []byte(`{"name":"app"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace.InvalidateLoadCache(root)
	memory.offer = held
	result, err = New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("answered run: exit=%d err=%v", result.ExitCode, err)
	}
	if len(result.Plan) != 1 || result.Plan[0] != held.run.planned[0] {
		t.Fatalf("answered run planned %v, want the held plan", result.Plan)
	}
	if !memory.skipped || memory.record != held {
		t.Fatalf("answered run kept %+v, want the held record again, answered", memory)
	}
	if phases := result.Session.Orchestration.Phases; len(phases) != 0 {
		t.Fatalf("answered run timed the engine phases %v, want none: it loaded and planned nothing", phases)
	}
	if latest := workspace_state.NewSessionStore(root).LatestID(); latest == first || latest == "" {
		t.Fatalf("answered run recorded no session of its own (latest %q, first %q)", latest, first)
	}

	for name, shape := range map[string]func(*Request){
		"another request":     func(r *Request) { r.Global.Verbose = true },
		"a project selection": func(r *Request) { r.Global.All, r.Global.Projects = false, "app" },
		"a run with --plan":   func(r *Request) { r.Global.Plan = true },
		"a run without cache": func(r *Request) { r.Global.NoCache = true },
		"a debug run":         func(r *Request) { r.Global.Debug = true },
		"a remote run":        func(r *Request) { r.Global.Where = "remote" },
	} {
		asked := req
		shape(&asked)
		memory.record, memory.skipped = "untouched", true
		result, _ := New().Run(context.Background(), asked, discardEvents{})
		if memory.skipped || memory.record != nil {
			t.Errorf("%s: kept %+v, want it computed and nothing kept", name, memory)
		}
		if len(result.Plan) == 1 && result.Plan[0] == held.run.planned[0] {
			t.Errorf("%s: answered from the held plan", name)
		}
	}
}

// TestAHeldRunWitnessesTheExtensionsItsDiscoveryRead pins what a run that
// may be held takes of its discovery for the worker (CLI ADR 0116 §6a): the
// stat of each extension manifest and root discovery read, and of each
// skipped extension's, which the worker checks before it offers the held run.
// A run that discovered a local-source extension outside the workspace's own
// path extensions whose runtime is prepared from its inputs is not held.
func TestAHeldRunWitnessesTheExtensionsItsDiscoveryRead(t *testing.T) {
	root, req := heldRunFixture(t)
	memory := &fakeHeldRuns{}
	serveHeldRuns(t, memory)
	var result SessionResult
	var err error
	stderr := captureStderr(t, func() { result, err = New().Run(context.Background(), req, discardEvents{}) })
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("run: exit=%d err=%v\n%s", result.ExitCode, err, stderr)
	}
	held, ok := memory.record.(*heldRun)
	if !ok {
		t.Fatalf("the run kept %v, want its hold", memory.record)
	}
	ext := filepath.Join(root, "extension")
	manifest := filepath.Join(ext, extensionproto.ManifestFilename)
	want := map[string]string{ext: memory.Witness(ext), manifest: memory.Witness(manifest)}
	if got := held.Witnesses(); !reflect.DeepEqual(got, want) {
		t.Fatalf("the hold witnesses %v, want the discovered extension's manifest and root %v", got, want)
	}

	outside := t.TempDir()
	prepared := &extension.ExtensionDescription{
		Name: "@test/outside", Path: outside, LocalSource: true,
		Runtime: &extensionproto.RuntimeDefinition{Executable: "bin/runtime", Prepare: &extensionproto.RuntimePrepare{Command: "make"}},
	}
	asked := &Request{WorkspaceRoot: root, hold: &heldRun{}}
	asked.witnessDiscovery(&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{prepared}})
	if asked.hold != nil {
		t.Fatal("a run that discovered a runtime prepared from sources outside the workspace kept its hold")
	}
	own := *prepared
	own.Path, own.RelPath = ext, "extension"
	asked = &Request{WorkspaceRoot: root, hold: &heldRun{}}
	asked.witnessDiscovery(&extension.DiscoveryResult{
		Extensions: []*extension.ExtensionDescription{&own},
		Skipped:    []extension.SkippedExtension{{Path: outside}},
	})
	skipped := filepath.Join(outside, extensionproto.ManifestFilename)
	if asked.hold == nil || asked.hold.witnesses[manifest] != memory.Witness(manifest) || asked.hold.witnesses[skipped] != memory.Witness(skipped) {
		t.Fatalf("a workspace path extension and a skipped one left the hold %+v, want both witnessed", asked.hold)
	}
}

// TestAHeldRunRecordsWhatPhasesOneToThreePrint pins the notices an answer
// prints again: a hold records what is written through it until it is
// complete, on stdout and stderr apart, and no hold records nothing.
func TestAHeldRunRecordsWhatPhasesOneToThreePrint(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	var none *heldRun
	if w := none.notices(&out, false); w != io.Writer(&out) {
		t.Fatal("no hold wrapped the writer")
	}
	hold := &heldRun{}
	_, _ = io.WriteString(hold.notices(&out, false), "selected\n")
	_, _ = io.WriteString(hold.notices(&out, true), "warning\n")
	hold.complete = true
	_, _ = io.WriteString(hold.notices(&out, false), "executed\n")
	if out.String() != "selected\nwarning\nexecuted\n" {
		t.Fatalf("written %q, want every notice", out.String())
	}
	if hold.stdout.String() != "selected\n" || hold.stderr.String() != "warning\n" {
		t.Fatalf("held stdout %q and stderr %q, want what was printed before the hold completed", hold.stdout.String(), hold.stderr.String())
	}
}

// TestAnAnsweredRunTakesTheVersionSnapshotTheHeldRunRead pins the version
// snapshot's part of CLI ADR 0116 §6a: in a worker, an engine run that reads
// its version snapshot from Git leaves it with what it computed; a later run
// the worker offers that run takes the snapshot instead of asking Git, and
// says so. A held run whose snapshot its adapter gave, or none, and a run
// offered nothing read Git.
func TestAnAnsweredRunTakesTheVersionSnapshotTheHeldRunRead(t *testing.T) {
	root, req := heldRunFixture(t)
	initCLISelectionGitRepo(t, root)
	memory := &fakeHeldRuns{}
	serveHeldRuns(t, memory)

	if result, err := New().Run(context.Background(), req, discardEvents{}); err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("first run: exit=%d err=%v", result.ExitCode, err)
	}
	held, ok := memory.record.(*heldRun)
	if !ok || !held.complete || !held.captured || held.snapshot == nil || held.snapshot.SHA == "" {
		t.Fatalf("first run kept %+v, want a complete record with the snapshot it read from Git", memory.record)
	}
	if memory.reused["versions"][false] != 1 || memory.reused["versions"][true] != 0 {
		t.Fatalf("first run reported %v, want its snapshot computed", memory.reused)
	}

	// A snapshot Git would never read: the answer asks what the held run
	// asked only when it took this one.
	marked := *held.snapshot
	marked.SHA, marked.Branch = "held00000000", "held"
	held.snapshot = &marked
	memory.offer, memory.reused = held, nil
	result, err := New().Run(context.Background(), req, discardEvents{})
	if err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("answered run: exit=%d err=%v", result.ExitCode, err)
	}
	if !memory.skipped || memory.reused["versions"][true] != 1 || memory.reused["versions"][false] != 0 {
		t.Fatalf("answered run: skipped=%v reused=%v, want it answered with the held snapshot", memory.skipped, memory.reused)
	}
	if again, ok := memory.record.(*heldRun); !ok || again.snapshot.SHA != "held00000000" || !again.captured {
		t.Fatalf("answered run kept %+v, want the held record, with its snapshot", memory.record)
	}

	for name, offer := range map[string]func() any{
		"nothing offered": func() any { return nil },
		"a snapshot its adapter gave": func() any {
			h := *held
			h.captured = false
			return &h
		},
		"no snapshot": func() any {
			h := *held
			h.snapshot = nil
			return &h
		},
		"another workspace": func() any {
			h := *held
			h.root = t.TempDir()
			return &h
		},
	} {
		memory.offer, memory.reused = offer(), nil
		if result, err := New().Run(context.Background(), req, discardEvents{}); err != nil || result.ExitCode != ExitSuccess {
			t.Fatalf("%s: exit=%d err=%v", name, result.ExitCode, err)
		}
		if memory.reused["versions"][true] != 0 || memory.reused["versions"][false] != 1 || memory.skipped {
			t.Errorf("%s: skipped=%v reused=%v, want the snapshot read from Git and the run computed", name, memory.skipped, memory.reused)
		}
	}

	// A snapshot the adapter gives is not Engine.Run's to read.
	given := req
	given.VersionSnapshot = &marked
	memory.offer, memory.reused = nil, nil
	peeks := memory.peeks
	if result, err := New().Run(context.Background(), given, discardEvents{}); err != nil || result.ExitCode != ExitSuccess {
		t.Fatalf("given snapshot: exit=%d err=%v", result.ExitCode, err)
	}
	if memory.peeks != peeks || len(memory.reused["versions"]) != 0 {
		t.Errorf("given snapshot: peeked %d times and reported %v, want neither", memory.peeks-peeks, memory.reused)
	}
}

// TestAnAnsweredRunReplaysWhatTheHeldRunsFinalizersDid pins what an answer
// holds of phase 4's finalizers (CLI ADR 0116 §6a): the spec gate alone, or
// none, and a gate only when its verdicts are a function of what the decision
// witnessed; and what the replay does: the held record with the answer's
// time, written beside the session, and its warnings and sanctions applied to
// the answered results as the gate applied them.
func TestAnAnsweredRunReplaysWhatTheHeldRunsFinalizersDid(t *testing.T) {
	record := &features.SpecVerificationRecord{GeneratedAt: "2020-01-01T00:00:00Z",
		Groups: []features.SpecVerificationGroup{{Project: "/app", Feature: "app/f", Blocked: true}}}
	collected := &specGateOutcome{record: record, collected: true}
	recovered := &cachedObservationRecovery{}
	recovered.plans.Add(1)
	for name, c := range map[string]struct {
		held *heldFinalizers
		want *heldFinalizers
	}{
		"no finalizer":                   {holdFinalizers(true, false, nil, nil), &heldFinalizers{}},
		"the gate":                       {holdFinalizers(true, true, collected, &cachedObservationRecovery{}), &heldFinalizers{gate: true, record: record}},
		"another finalizer":              {holdFinalizers(false, true, collected, nil), nil},
		"a gate that collected nothing":  {holdFinalizers(true, true, &specGateOutcome{}, nil), nil},
		"a gate that read the clock":     {holdFinalizers(true, true, &specGateOutcome{record: record, collected: true, clock: true}, nil), nil},
		"a gate that recovered an entry": {holdFinalizers(true, true, collected, recovered), nil},
	} {
		if !reflect.DeepEqual(c.held, c.want) {
			t.Errorf("%s: held %+v, want %+v", name, c.held, c.want)
		}
	}

	req := &Request{specGate: &specGateOutcome{}}
	results := map[string]*scheduler.JobResult{"/app:test": {Status: "success"}}
	(&heldFinalizers{gate: true, record: record}).replay(req)(results)
	replayed := req.specGate.record
	if !req.finalizersReplayed || replayed == nil || replayed == record || replayed.GeneratedAt == record.GeneratedAt ||
		record.GeneratedAt != "2020-01-01T00:00:00Z" || !reflect.DeepEqual(replayed.Groups, record.Groups) {
		t.Fatalf("replayed %+v (replayed %v), want a copy of the held record at this run's time", replayed, req.finalizersReplayed)
	}
	if sanction := results["/app:"+specGateResultTask]; sanction == nil || sanction.Status != "failed" {
		t.Errorf("results %v, want the held gate's sanction of /app", results)
	}

	failed := map[string]*scheduler.JobResult{"/app:test": {Status: "failed"}}
	(&heldFinalizers{gate: true, record: record}).replay(&Request{specGate: &specGateOutcome{}})(failed)
	if len(failed) != 1 {
		t.Errorf("a failed run's results %v, want no sanction added", failed)
	}
	none := &Request{specGate: &specGateOutcome{}}
	(&heldFinalizers{}).replay(none)(results)
	if !none.finalizersReplayed || none.specGate.record != nil {
		t.Errorf("no finalizer held: replayed %v, record %+v, want replayed and no record", none.finalizersReplayed, none.specGate.record)
	}
}
