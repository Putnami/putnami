package engine

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.putnami.dev/cli/model/extension"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// TestCandidateOutcomeSeparatesSilenceFromUnreachability is the guard on the
// one distinction this behavior rests on: only a candidate that could NOT be
// reached may excuse a still-missing check. A candidate whose entry exists and
// records the verification report EMPTY has answered — it ran for these inputs
// and observed nothing — and reading that as unreachable would excuse every
// genuine gap in a project that has no bound test yet.
func TestCandidateOutcomeSeparatesSilenceFromUnreachability(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		outcome         candidateOutcome
		wantUnconsulted bool
		wantReason      string
	}{
		"no report task at all is an answer": {
			outcome: candidateOutcome{},
		},
		"an entry recording an empty report is an answer": {
			outcome: candidateOutcome{reportTasks: 1, keyed: 1, entries: 1},
		},
		"every report task answered is an answer": {
			outcome: candidateOutcome{reportTasks: 2, keyed: 2, entries: 2},
		},
		"an uncacheable report task can never be recovered": {
			outcome:         candidateOutcome{reportTasks: 1},
			wantUnconsulted: true,
			wantReason: "not selected, and the test task that would write its verification report is not cacheable, " +
				"so no observation can be recovered",
		},
		"a keyed report task with no entry was never reached": {
			outcome:         candidateOutcome{reportTasks: 1, keyed: 1},
			wantUnconsulted: true,
			wantReason:      "not selected, and the test task that writes its verification report has no cache entry for these inputs",
		},
		"one unanswered report task among several is enough": {
			outcome:         candidateOutcome{reportTasks: 2, keyed: 2, entries: 1},
			wantUnconsulted: true,
			wantReason:      "not selected, and the test task that writes its verification report has no cache entry for these inputs",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reason, unconsulted := testCase.outcome.unconsultedReason()
			if unconsulted != testCase.wantUnconsulted {
				t.Fatalf("unconsulted = %v, want %v (outcome %+v)", unconsulted, testCase.wantUnconsulted, testCase.outcome)
			}
			if reason != testCase.wantReason {
				t.Errorf("reason = %q, want %q", reason, testCase.wantReason)
			}
		})
	}
}

// TestNoCacheRunConsultsNothingAndSaysWhy pins that --no-cache serves no stored
// byte and reports every candidate unconsulted with the accurate cause, so the
// resulting warning names the real reason rather than inventing a missing entry.
func TestNoCacheRunConsultsNothingAndSaysWhy(t *testing.T) {
	t.Parallel()
	fixture := newSpecGateFixture(t, "passed")
	req := &Request{Commands: []string{"test"}}
	req.Global.NoCache = true
	source := newCachedObservationRecovery(req, fixture.ws, nil, nil)
	if source == nil {
		t.Fatal("no recovery source was built for a --no-cache run")
	}
	recovery := source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Reports) != 0 {
		t.Errorf("--no-cache served %d cached report(s)", len(recovery.Reports))
	}
	if len(recovery.Unconsulted) != len(fixture.ws.Projects) {
		t.Fatalf("unconsulted = %+v, want every candidate", recovery.Unconsulted)
	}
	if recovery.Unconsulted[0].Reason != "--no-cache: no cached observation may be served" {
		t.Errorf("reason = %q, want the --no-cache cause", recovery.Unconsulted[0].Reason)
	}
}

// The tests below run RecoverReports against a REAL `test` plan: the
// planner expands a pipeline modeled on the Go and TypeScript manifests, the
// keying pass derives the real cache keys, and entries are published through
// the store's own ingest at those keys. Hand-built outcome values cannot catch
// a counting rule that asks the wrong steps; only a real plan carries the
// uncacheable test-environment step and the prerequisite steps that do.
const (
	recoveryProjectID    = "/billing"
	recoveryReportTask   = "/billing:test~test"
	recoveryGenerateTask = "/billing:test~generate"
	recoveryTestEnvTask  = "/billing:test~test-env"
	recoveryReportBytes  = `{"protocolVersion":1,"observations":[]}`
)

// recoveryReportOutput is the report output as the test task declares it,
// resolved the way the executor hands it to the store's ingest.
var recoveryReportOutput = store.DeclaredEntryOutput{
	ID:       "featureVerification",
	Kind:     extension.OutputKindFile,
	Root:     extension.OutputRootCommandOutput,
	Path:     features.VerificationReportFilename,
	Optional: true,
}

type recoveryPlanFixture struct {
	ws      *workspace.Workspace
	cache   *store.CacheManager
	source  *cachedObservationRecovery
	planned []*jobs.ScheduledJob
	keys    map[string]string
}

// newRecoveryPlanFixture builds a one-project workspace and keys its `test`
// plan exactly as RecoverReports does: the same extensions, the same command
// params, and the source's own run versions. withTestEnv places the closure
// file that activates the uncacheable test-environment steps.
func newRecoveryPlanFixture(t *testing.T, withTestEnv bool) *recoveryPlanFixture {
	t.Helper()
	root := t.TempDir()
	writeSpecGateFile(t, filepath.Join(root, "billing", "go.mod"), "module example.com/billing\n")
	writeSpecGateFile(t, filepath.Join(root, "billing", "invoice_test.go"), "package billing\n")
	if withTestEnv {
		writeSpecGateFile(t, filepath.Join(root, "billing", "infra", "requirements.json"), `{"protocolVersion":2}`)
	}
	project := &workspace.Project{ID: recoveryProjectID, Name: "@acme/billing", Path: "billing",
		Extensions: []string{"@acme/lang"}, Config: &wsproto.ProjectConfig{}}
	ws := workspace.NewWorkspace(root, &wsproto.Config{Name: "gate"}, []*workspace.Project{project})
	ws.Graph = workspace.BuildGraph(ws.Projects)

	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	source := newCachedObservationRecovery(&Request{Commands: []string{"test"}}, ws,
		[]*extension.ExtensionDescription{recoveryTestExtension()}, cache)
	planned, err := jobs.Plan(ws, []string{"test"}, ws.Projects, source.extensions, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys, err := jobs.PrecomputeKeys(ws, planned, nil, source.runVersions(), cache, jobs.CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	return &recoveryPlanFixture{ws: ws, cache: cache, source: source, planned: planned, keys: keys}
}

// recoveryTestExtension mirrors the shape of the Go extension's `test`
// pipeline (go/extension/putnami.extension.json): a cacheable generation
// prerequisite, an UNCACHEABLE test-environment step and its finalizer gated
// on infra/requirements.json in the closure, and the cacheable test step whose
// task alone declares the reserved verification report.
func recoveryTestExtension() *extension.ExtensionDescription {
	enabled, disabled := true, false
	sources := map[string]extension.TaskInputPort{"sources": {From: "project", Files: []string{"**/*.go"}}}
	closureGate := &extension.StepActivation{ClosureFiles: []string{"infra/requirements.json"}}
	return &extension.ExtensionDescription{
		Name:    "@acme/lang",
		Version: "1.0.0",
		Jobs: map[string]*extension.JobDefinition{
			"test": {
				ExtensionName: "@acme/lang",
				Name:          "test",
				Kind:          "command",
				Command:       "lang",
				Cache:         true,
				PipelineSteps: []extension.PipelineStep{
					{ID: "generate", Task: "lang-generate", DependsOn: []string{"^generate"}},
					{ID: "test-env", Task: "lang-test-env-up", DependsOn: []string{"generate"}, Activation: closureGate},
					{ID: "test", Task: "lang-test", DependsOn: []string{"generate", "test-env"}},
					{
						ID: "test-env-teardown", Task: "lang-test-env-down", RunOn: extensionproto.StepRunOnFinally,
						Finalizes:  &extensionproto.FinalizesRelation{Producer: "test-env", Consumers: []string{"test"}},
						Activation: closureGate,
					},
				},
			},
		},
		Tasks: map[string]extension.TaskDefinition{
			"lang-generate": {
				Kind: "command", Command: "lang", Inputs: sources,
				Cache:    &extension.TaskCachePolicy{Enabled: &enabled, Deterministic: true},
				Declares: &extension.TaskDeclaration{},
			},
			"lang-test-env-up": {
				Kind: "command", Command: "lang",
				Inputs:   map[string]extension.TaskInputPort{"requirements": {From: "closure", Files: []string{"infra/requirements.json"}}},
				Cache:    &extension.TaskCachePolicy{Enabled: &disabled},
				Declares: &extension.TaskDeclaration{},
			},
			"lang-test": {
				Kind: "command", Command: "lang", Inputs: sources,
				Cache: &extension.TaskCachePolicy{Enabled: &enabled},
				Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
					"featureVerification": {
						Kind:          extension.OutputKindFile,
						Root:          extension.OutputRootCommandOutput,
						Path:          features.VerificationReportFilename,
						OptionalEmpty: true,
					},
				}},
			},
			"lang-test-env-down": {
				Kind: "command", Command: "lang",
				Cache:    &extension.TaskCachePolicy{Enabled: &disabled},
				Declares: &extension.TaskDeclaration{},
			},
		},
	}
}

// publish ingests one task-owned entry at the key the plan derived for task.
// report is written into the staged report output when non-empty; an empty
// report leaves the optional output absent, which the store records EMPTY.
func (f *recoveryPlanFixture) publish(t *testing.T, task string, outputs []store.DeclaredEntryOutput, report string) *store.TaskEntry {
	t.Helper()
	hash := f.keys[task]
	if hash == "" {
		t.Fatalf("the plan derived no cache key for %s (keys %v)", task, f.keys)
	}
	staging := t.TempDir()
	if report != "" {
		path := store.TaskStagingPath(staging, outputs[0])
		writeSpecGateFile(t, path, report)
	}
	entry, err := f.cache.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:     hash,
		Result:  &store.EntryResult{Status: "success"},
		Outputs: outputs,
	})
	if err != nil {
		t.Fatalf("ingest %s: %v", task, err)
	}
	return entry
}

// requirePlanShape guards the fixture itself: the scenario only reproduces
// the case when the plan really carries (or really lacks) the uncacheable
// test-environment step next to a keyed report task.
func (f *recoveryPlanFixture) requirePlanShape(t *testing.T, wantTestEnv bool) {
	t.Helper()
	planned := make([]string, 0, len(f.planned))
	for _, job := range f.planned {
		planned = append(planned, job.Key())
	}
	if slices.Contains(planned, recoveryTestEnvTask) != wantTestEnv {
		t.Fatalf("test-env planned = %v, want %v (plan %v)", !wantTestEnv, wantTestEnv, planned)
	}
	if _, keyed := f.keys[recoveryTestEnvTask]; keyed {
		t.Fatalf("the test-env step is cache:false, yet the plan keyed it")
	}
	if f.keys[recoveryReportTask] == "" || f.keys[recoveryGenerateTask] == "" {
		t.Fatalf("the report and generate tasks must both be keyed, got %v", f.keys)
	}
}

// TestRecoverReportsReadsAnEmptyReportEntryAsAnAnswer is a regression test.
// The candidate's report task ran for these inputs and recorded the report
// EMPTY. The uncacheable test-env step beside it can never have an entry, and
// counting it made the candidate "unconsulted", which excused every check it
// failed to observe. Silence is an answer, so the candidate must be consulted.
func TestRecoverReportsReadsAnEmptyReportEntryAsAnAnswer(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, true)
	fixture.requirePlanShape(t, true)
	fixture.publish(t, recoveryGenerateTask, nil, "")
	fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, "")

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Reports) != 0 {
		t.Errorf("an entry that recorded the report empty yielded reports %+v", recovery.Reports)
	}
	if len(recovery.Unconsulted) != 0 {
		t.Errorf("an entry that recorded the report empty was read as unreachable: %+v", recovery.Unconsulted)
	}
}

// TestRecoverReportsReturnsAPresentReportInPlace pins the positive path on a
// real plan: the report task's entry carries the report, so the candidate
// returns it, located inside the entry and never materialized elsewhere.
func TestRecoverReportsReturnsAPresentReportInPlace(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, true)
	fixture.requirePlanShape(t, true)
	entry := fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Unconsulted) != 0 {
		t.Errorf("a candidate with a cached report was reported unconsulted: %+v", recovery.Unconsulted)
	}
	if len(recovery.Reports) != 1 {
		t.Fatalf("reports = %+v, want exactly the candidate's cached report", recovery.Reports)
	}
	report := recovery.Reports[0]
	if report.Project != recoveryProjectID || report.Task != recoveryReportTask {
		t.Errorf("report attributed to %s/%s, want %s/%s", report.Project, report.Task, recoveryProjectID, recoveryReportTask)
	}
	if want := filepath.Join(entry.FilesDir, recoveryReportOutput.ID); report.Path != want {
		t.Errorf("report path = %s, want the entry's own copy at %s", report.Path, want)
	}
	if got, err := os.ReadFile(report.Path); err != nil || string(got) != recoveryReportBytes {
		t.Errorf("report bytes = %q (err %v), want the captured report", got, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.ws.Root, ".putnami", "out", "billing", "test",
		features.VerificationReportFilename)); !os.IsNotExist(err) {
		t.Errorf("recovery materialized the report into the session output tree (stat err %v)", err)
	}
}

// TestRecoverReportsWithoutAReportEntryIsUnconsulted keeps the genuine
// unreachable case: the report task has no entry for these inputs, so nothing
// answered for the candidate. A prerequisite's entry does not stand in for it.
func TestRecoverReportsWithoutAReportEntryIsUnconsulted(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, true)
	fixture.requirePlanShape(t, true)
	fixture.publish(t, recoveryGenerateTask, nil, "")

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Reports) != 0 {
		t.Errorf("a candidate with no report entry yielded reports %+v", recovery.Reports)
	}
	if len(recovery.Unconsulted) != 1 || recovery.Unconsulted[0].Project != recoveryProjectID {
		t.Fatalf("unconsulted = %+v, want the candidate", recovery.Unconsulted)
	}
	const want = "not selected, and the test task that writes its verification report has no cache entry for these inputs"
	if got := recovery.Unconsulted[0].Reason; got != want {
		t.Errorf("reason = %q, want %q", got, want)
	}
}

// TestRecoverReportsIgnoresAPrerequisiteWithoutAnEntry isolates the second
// half of that: with no test-env step in the plan, the only thing missing is
// the generate prerequisite's entry. The store may evict it while keeping the
// report task's entry, and that eviction says nothing about what the test
// observed, so the candidate stays consulted.
func TestRecoverReportsIgnoresAPrerequisiteWithoutAnEntry(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, false)
	fixture.requirePlanShape(t, false)
	fixture.publish(t, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, "")
	if entry, err := fixture.cache.LookupTaskEntry(fixture.keys[recoveryGenerateTask]); err != nil || entry != nil {
		t.Fatalf("the generate prerequisite must have no entry for this scenario (entry %v, err %v)", entry, err)
	}

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Reports) != 0 || len(recovery.Unconsulted) != 0 {
		t.Errorf("recovery = %+v, want the candidate consulted and silent", recovery)
	}
}
