package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.putnami.dev/cli/model/extension"
	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/jobs/cachekey"
	"go.putnami.dev/tooling/cli/internal/jobs/planner"
	"go.putnami.dev/tooling/cli/internal/jobs/remotecache"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
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
		wantCode        features.UnconsultedReason
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
			wantCode: features.UnconsultedNotCacheable,
		},
		"a keyed report task with no entry was never reached": {
			outcome:         candidateOutcome{reportTasks: 1, keyed: 1},
			wantUnconsulted: true,
			wantReason:      "not selected, and the test task that writes its verification report has no cache entry for these inputs",
			wantCode:        features.UnconsultedNoEntry,
		},
		"one unanswered report task among several is enough": {
			outcome:         candidateOutcome{reportTasks: 2, keyed: 2, entries: 1},
			wantUnconsulted: true,
			wantReason:      "not selected, and the test task that writes its verification report has no cache entry for these inputs",
			wantCode:        features.UnconsultedNoEntry,
		},
		"a remote lookup that failed says so": {
			outcome: candidateOutcome{reportTasks: 1, keyed: 1,
				remoteFailure: remoteLookupFailure(errors.New("cache provider session ended"))},
			wantUnconsulted: true,
			wantReason: "not selected, and the remote cache lookup for the test task that writes its verification report failed: " +
				"cache provider session ended",
			wantCode: features.UnconsultedRemoteUnreachable,
		},
		"a remote lookup that ran out of time says so": {
			outcome: candidateOutcome{reportTasks: 1, keyed: 1,
				remoteFailure: remoteLookupFailure(context.DeadlineExceeded)},
			wantUnconsulted: true,
			wantReason:      "not selected, and the remote cache did not answer in time for the test task that writes its verification report",
			wantCode:        features.UnconsultedRemoteTimeout,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cause, unconsulted := testCase.outcome.unconsultedReason()
			if unconsulted != testCase.wantUnconsulted {
				t.Fatalf("unconsulted = %v, want %v (outcome %+v)", unconsulted, testCase.wantUnconsulted, testCase.outcome)
			}
			if cause.prose != testCase.wantReason {
				t.Errorf("reason = %q, want %q", cause.prose, testCase.wantReason)
			}
			if cause.code != testCase.wantCode {
				t.Errorf("code = %q, want %q", cause.code, testCase.wantCode)
			}
		})
	}
}

// TestNoCacheRunConsultsNothingAndSaysWhy pins that --no-cache serves no stored
// byte and reports every candidate unconsulted with the accurate cause, so the
// resulting warning names the real reason rather than inventing a missing entry.
// The run's remote cache is never asked either.
func TestNoCacheRunConsultsNothingAndSaysWhy(t *testing.T) {
	t.Parallel()
	fixture := newSpecGateFixture(t, "passed")
	req := &Request{Commands: []string{"test"}}
	req.Global.NoCache = true
	remote := newRemoteEntryFake()
	source := newCachedObservationRecovery(req, fixture.ws, nil, nil).readingRemote(t.Context(), remote.resolve)
	if source == nil {
		t.Fatal("no recovery source was built for a --no-cache run")
	}
	recovery := source.RecoverReports(fixture.ws.Projects)
	if asked := remote.askedKeys(); len(asked) != 0 {
		t.Errorf("--no-cache asked the remote cache for %v", asked)
	}
	if len(recovery.Reports) != 0 {
		t.Errorf("--no-cache served %d cached report(s)", len(recovery.Reports))
	}
	if len(recovery.Unconsulted) != len(fixture.ws.Projects) {
		t.Fatalf("unconsulted = %+v, want every candidate", recovery.Unconsulted)
	}
	if recovery.Unconsulted[0].Reason != "--no-cache: no cached observation may be served" {
		t.Errorf("reason = %q, want the --no-cache cause", recovery.Unconsulted[0].Reason)
	}
	if recovery.Unconsulted[0].Code != features.UnconsultedNoCache {
		t.Errorf("code = %q, want %q", recovery.Unconsulted[0].Code, features.UnconsultedNoCache)
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
	planned []*scheduler.ScheduledJob
	keys    map[string]string
}

// newRecoveryPlanFixture builds a one-project workspace and keys its `test`
// plan exactly as RecoverReports does: the same extensions, the same command
// params, and the source's own run versions. withTestEnv places the closure
// file that activates the uncacheable test-environment steps.
func newRecoveryPlanFixture(t *testing.T, withTestEnv bool) *recoveryPlanFixture {
	t.Helper()
	return newRecoveryPlanFixtureFor(t, withTestEnv, "test")
}

// newRecoveryPlanFixtureFor is newRecoveryPlanFixture with the verification
// pipeline declared under command instead of `test`.
func newRecoveryPlanFixtureFor(t *testing.T, withTestEnv bool, command string) *recoveryPlanFixture {
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

	ext := recoveryTestExtension()
	if command != "test" {
		pipeline := ext.Jobs["test"]
		pipeline.Name = command
		ext.Jobs = map[string]*extension.JobDefinition{command: pipeline}
	}
	cache := store.NewCacheManager(store.NewLocalStore(filepath.Join(root, ".putnami", "store")))
	source := newCachedObservationRecovery(&Request{Commands: []string{command}}, ws,
		[]*extension.ExtensionDescription{ext}, cache)
	planned, err := planner.Plan(ws, []string{command}, ws.Projects, source.extensions, nil, nil, nil)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	keys, err := cachekey.PrecomputeKeys(ws, planned, nil, source.runVersions(), cache, cachekey.CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	return &recoveryPlanFixture{ws: ws, cache: cache, source: source, planned: planned, keys: keys}
}

// recoveryTestExtension mirrors the shape of the Go extension's `test`
// pipeline (go/extension/putnami.extension.json), with the verification kind
// the @putnami/dev vocabulary gives it: a cacheable generation prerequisite,
// an UNCACHEABLE test-environment step and its finalizer gated on
// infra/requirements.json in the closure, and the cacheable test step whose
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
				Traits:        extension.CommandTraits{Kind: extensionproto.CommandKindVerification},
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

// TestRecoverReportsPlansTheVerificationKindCommands pins recovery under a
// verification command that is not `test`: the lookup plans the command whose
// resolved traits declare the verification kind, keys it as a run of that
// command does, and returns the cached report from its declaring task's entry.
// Its prerequisite steps still answer for nothing, and the same command
// without the kind is not planned at all.
func TestRecoverReportsPlansTheVerificationKindCommands(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "declared-verification-producers",
		"cache-recovery-plans-the-verification-kind-commands")
	t.Parallel()
	const reportTask = "/billing:check~test"
	fixture := newRecoveryPlanFixtureFor(t, true, "check")
	if fixture.keys[reportTask] == "" || fixture.keys["/billing:check~generate"] == "" {
		t.Fatalf("the check plan must key its report and generate tasks, got %v", fixture.keys)
	}
	fixture.publish(t, "/billing:check~generate", nil, "")
	entry := fixture.publish(t, reportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Unconsulted) != 0 {
		t.Errorf("a candidate with a cached check report was reported unconsulted: %+v", recovery.Unconsulted)
	}
	if len(recovery.Reports) != 1 {
		t.Fatalf("reports = %+v, want exactly the candidate's cached check report", recovery.Reports)
	}
	report := recovery.Reports[0]
	if report.Project != recoveryProjectID || report.Task != reportTask {
		t.Errorf("report attributed to %s/%s, want %s/%s", report.Project, report.Task, recoveryProjectID, reportTask)
	}
	if want := filepath.Join(entry.FilesDir, recoveryReportOutput.ID); report.Path != want {
		t.Errorf("report path = %s, want the entry's own copy at %s", report.Path, want)
	}

	fixture.source.extensions[0].Jobs["check"].Traits = extension.CommandTraits{}
	if recovery := fixture.source.RecoverReports(fixture.ws.Projects); len(recovery.Reports) != 0 ||
		len(recovery.Unconsulted) != 0 {
		t.Errorf("recovery = %+v, want nothing planned for a check command without the verification kind", recovery)
	}
}

// remoteEntryFake stands in for the run's remote cache in the recovery's one
// remote operation. hits holds the entries a provider would serve, staged as an
// upload leaves them: payload blobs and descriptor in an exchange directory. A
// served hit publishes into the caller's store through the store's own
// MaterializeTaskEntry, as remotecache.RemoteCache.FetchTaskEntry does. err,
// when set, is the answer to every lookup.
type remoteEntryFake struct {
	mu     sync.Mutex
	hits   map[string]remoteEntryHit
	err    error
	asked  []string
	within []time.Duration
}

type remoteEntryHit struct {
	hit      store.RemoteTaskEntryHit
	exchange string
}

func newRemoteEntryFake() *remoteEntryFake {
	return &remoteEntryFake{hits: map[string]remoteEntryHit{}}
}

func (f *remoteEntryFake) resolve() remoteTaskEntries { return f }

func (f *remoteEntryFake) FetchTaskEntry(_ context.Context, within time.Duration, hash string,
	_ *scheduler.ScheduledJob, cm *store.CacheManager) (*store.TaskEntry, error) {
	f.mu.Lock()
	f.asked = append(f.asked, hash)
	f.within = append(f.within, within)
	hit, ok := f.hits[hash]
	err := f.err
	f.mu.Unlock()
	if err != nil || !ok {
		return nil, err
	}
	return cm.MaterializeTaskEntry(hit.hit, func(digest string) (io.ReadCloser, error) {
		path, ok := cache.BlobExchangePath(hit.exchange, digest)
		if !ok {
			return nil, fmt.Errorf("invalid digest %q", digest)
		}
		return os.Open(path)
	})
}

func (f *remoteEntryFake) askedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.asked)
}

// stageRemote publishes task's entry in a producer store of its own, at the
// key the plan derived, and stages it in remote as a provider serves it. The
// fixture's store keeps no entry for the task.
func (f *recoveryPlanFixture) stageRemote(t *testing.T, remote *remoteEntryFake, task string,
	outputs []store.DeclaredEntryOutput, report string) {
	t.Helper()
	hash := f.keys[task]
	if hash == "" {
		t.Fatalf("the plan derived no cache key for %s (keys %v)", task, f.keys)
	}
	producer := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	staging := t.TempDir()
	if report != "" {
		writeSpecGateFile(t, store.TaskStagingPath(staging, outputs[0]), report)
	}
	entry, err := producer.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key: hash, Result: &store.EntryResult{Status: "success"}, Outputs: outputs,
	})
	if err != nil {
		t.Fatalf("ingest %s in the producer store: %v", task, err)
	}
	transfer, err := store.NewTaskEntryTransfer(entry)
	if err != nil {
		t.Fatalf("prepare %s for the remote cache: %v", task, err)
	}
	exchange := t.TempDir()
	if err := producer.ExportManifestBlobs(transfer.Payload, exchange); err != nil {
		t.Fatalf("export %s payload: %v", task, err)
	}
	if err := store.StageExchangeBlob(exchange, transfer.DescriptorDigest, transfer.Descriptor); err != nil {
		t.Fatalf("stage %s descriptor: %v", task, err)
	}
	remote.mu.Lock()
	remote.hits[hash] = remoteEntryHit{
		hit:      store.RemoteTaskEntryHit{Key: hash, Result: entry.Result, Manifest: transfer.Manifest},
		exchange: exchange,
	}
	remote.mu.Unlock()
	if local, err := f.cache.LookupTaskEntry(hash); err != nil || local != nil {
		t.Fatalf("the fixture's store must miss %s for this scenario (entry %v, err %v)", task, local, err)
	}
}

// TestRecoverReportsFetchesAnEntryOnlyTheRemoteCacheHolds is the hosted-runner
// case: the attester's report task ran in another run, which published its
// entry to the remote cache only. On the local miss the recovery fetches that
// entry into the local store and reads the report in place there. Only the
// report task is asked, nothing reaches the workspace, and a second lookup is
// served locally.
func TestRecoverReportsFetchesAnEntryOnlyTheRemoteCacheHolds(t *testing.T) {
	spectest.Proves(t, "cli/job-planning-execution", "spec-gate-evidence-scope",
		"an-unplanned-attester-resolves-from-its-cache-entry")
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, true)
	fixture.requirePlanShape(t, true)
	remote := newRemoteEntryFake()
	fixture.stageRemote(t, remote, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
	fixture.source.readingRemote(t.Context(), remote.resolve)

	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Unconsulted) != 0 {
		t.Errorf("a candidate the remote cache answered for was reported unconsulted: %+v", recovery.Unconsulted)
	}
	if len(recovery.Reports) != 1 {
		t.Fatalf("reports = %+v, want the candidate's report from the remote entry", recovery.Reports)
	}
	report := recovery.Reports[0]
	if report.Project != recoveryProjectID || report.Task != recoveryReportTask {
		t.Errorf("report attributed to %s/%s, want %s/%s", report.Project, report.Task, recoveryProjectID, recoveryReportTask)
	}
	entry, err := fixture.cache.LookupTaskEntry(fixture.keys[recoveryReportTask])
	if err != nil || entry == nil {
		t.Fatalf("the fetched entry is not in the local store (entry %v, err %v)", entry, err)
	}
	if want := filepath.Join(entry.FilesDir, recoveryReportOutput.ID); report.Path != want {
		t.Errorf("report path = %s, want the local store's copy at %s", report.Path, want)
	}
	if relative, err := filepath.Rel(fixture.cache.StoreRoot(), report.Path); err != nil || strings.HasPrefix(relative, "..") {
		t.Errorf("report path %s is outside the store %s", report.Path, fixture.cache.StoreRoot())
	}
	if got, err := os.ReadFile(report.Path); err != nil || string(got) != recoveryReportBytes {
		t.Errorf("report bytes = %q (err %v), want the captured report", got, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.ws.Root, ".putnami", "out")); !os.IsNotExist(err) {
		t.Errorf("recovery wrote the session output tree (stat err %v)", err)
	}
	if asked := remote.askedKeys(); !slices.Equal(asked, []string{fixture.keys[recoveryReportTask]}) {
		t.Errorf("remote lookups = %v, want only the report task's key %s", asked, fixture.keys[recoveryReportTask])
	}
	if within := remote.within[0]; within <= 0 || within > specGateRemoteLookupTimeout {
		t.Errorf("remote lookup bound = %s, want a positive bound of at most %s", within, specGateRemoteLookupTimeout)
	}

	if again := fixture.source.RecoverReports(fixture.ws.Projects); len(again.Reports) != 1 {
		t.Fatalf("second recovery = %+v, want the report from the local store", again)
	}
	if asked := remote.askedKeys(); len(asked) != 1 {
		t.Errorf("a locally held entry was asked of the remote cache again: %v", asked)
	}
}

// TestRecoverReportsKeepsARemoteMissUnconsulted pins that a miss in both caches
// keeps the existing reason, and that the remote cache is asked for a key at
// most once per gate.
func TestRecoverReportsKeepsARemoteMissUnconsulted(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, true)
	fixture.requirePlanShape(t, true)
	fixture.publish(t, recoveryGenerateTask, nil, "")
	remote := newRemoteEntryFake()
	fixture.source.readingRemote(t.Context(), remote.resolve)

	for range 2 {
		recovery := fixture.source.RecoverReports(fixture.ws.Projects)
		if len(recovery.Reports) != 0 || len(recovery.Unconsulted) != 1 {
			t.Fatalf("recovery = %+v, want the candidate unconsulted", recovery)
		}
		const want = "not selected, and the test task that writes its verification report has no cache entry for these inputs"
		if got := recovery.Unconsulted[0].Reason; got != want {
			t.Errorf("reason = %q, want %q", got, want)
		}
	}
	if asked := remote.askedKeys(); !slices.Equal(asked, []string{fixture.keys[recoveryReportTask]}) {
		t.Errorf("remote lookups = %v, want the report task's key once", asked)
	}
}

// TestRecoverReportsSaysWhyTheRemoteCacheDidNotAnswer pins the reasons of a
// remote lookup that ran out of time or failed: the candidate stays
// unconsulted, and the reason names the remote cache rather than claim the
// entry does not exist.
func TestRecoverReportsSaysWhyTheRemoteCacheDidNotAnswer(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		err  error
		want string
	}{
		"a lookup that ran out of time": {
			err:  fmt.Errorf("provider op restore: %w", context.DeadlineExceeded),
			want: "not selected, and the remote cache did not answer in time for the test task that writes its verification report",
		},
		"a lookup that failed": {
			err: errors.New("cache provider session ended"),
			want: "not selected, and the remote cache lookup for the test task that writes its verification report failed: " +
				"cache provider session ended",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRecoveryPlanFixture(t, false)
			remote := newRemoteEntryFake()
			remote.err = testCase.err
			fixture.source.readingRemote(t.Context(), remote.resolve)

			recovery := fixture.source.RecoverReports(fixture.ws.Projects)
			if len(recovery.Reports) != 0 || len(recovery.Unconsulted) != 1 {
				t.Fatalf("recovery = %+v, want the candidate unconsulted", recovery)
			}
			if got := recovery.Unconsulted[0].Reason; got != testCase.want {
				t.Errorf("reason = %q, want %q", got, testCase.want)
			}
			if asked := remote.askedKeys(); len(asked) != 1 {
				t.Errorf("remote lookups = %v, want one", asked)
			}
		})
	}
}

// TestRecoverReportsAsksNoRemoteCacheItCannotWaitFor pins the two cases that
// skip the remote lookup: the run's context ended, or the gate's remote
// budget is spent. Neither asks, and the reason says which.
func TestRecoverReportsAsksNoRemoteCacheItCannotWaitFor(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	cases := map[string]struct {
		ctx    context.Context
		budget time.Duration
		want   string
	}{
		"the run ended": {
			ctx: ended, budget: specGateRemoteBudget,
			want: "not selected, and the remote cache lookup for the test task that writes its verification report failed: " +
				"context canceled",
		},
		"the budget is spent": {
			ctx: t.Context(), budget: 0,
			want: "not selected, and the remote cache did not answer in time for the test task that writes its verification report",
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newRecoveryPlanFixture(t, false)
			remote := newRemoteEntryFake()
			fixture.stageRemote(t, remote, recoveryReportTask, []store.DeclaredEntryOutput{recoveryReportOutput}, recoveryReportBytes)
			fixture.source.readingRemote(testCase.ctx, remote.resolve)
			fixture.source.remoteBudget = testCase.budget

			recovery := fixture.source.RecoverReports(fixture.ws.Projects)
			if asked := remote.askedKeys(); len(asked) != 0 {
				t.Errorf("remote lookups = %v, want none", asked)
			}
			if len(recovery.Reports) != 0 || len(recovery.Unconsulted) != 1 {
				t.Fatalf("recovery = %+v, want the candidate unconsulted", recovery)
			}
			if got := recovery.Unconsulted[0].Reason; got != testCase.want {
				t.Errorf("reason = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestRunObservationRecoveryAsksTheRemoteTheSessionRecords pins the gate's
// wiring: a run that is not hosted records its remote cache when execution
// starts, after the spec gate is assembled, and the run's recovery source
// resolves that remote cache at lookup time.
func TestRunObservationRecoveryAsksTheRemoteTheSessionRecords(t *testing.T) {
	t.Parallel()
	fixture := newRecoveryPlanFixture(t, false)
	req := &Request{Commands: []string{"test"}}
	source := runObservationRecovery(t.Context(), req, fixture.ws, fixture.source.extensions, fixture.cache)
	if source == nil || source.remote == nil {
		t.Fatal("the run's recovery source reads no remote cache")
	}
	if remote := source.remote(); remote != nil {
		t.Fatalf("before execution starts the source resolved %#v, want none", remote)
	}
	recorded := &remotecache.RemoteCache{}
	req.executionRemote = recorded
	if remote := source.remote(); remote != remoteTaskEntries(recorded) {
		t.Fatalf("the source resolved %#v, want the remote cache the session recorded", remote)
	}
}

// TestRecoverReportsWithoutARunRemoteReadsTheLocalStoreOnly pins the run that
// carries no remote cache: the request resolves to a nil interface, never to
// a nil pointer the lookup would call, and the candidate keeps the local
// reason.
func TestRecoverReportsWithoutARunRemoteReadsTheLocalStoreOnly(t *testing.T) {
	t.Parallel()
	req := &Request{}
	if remote := req.executionRemoteEntries(); remote != nil {
		t.Fatalf("a request without a remote cache resolved to %#v", remote)
	}
	fixture := newRecoveryPlanFixture(t, false)
	fixture.source.readingRemote(t.Context(), req.executionRemoteEntries)
	recovery := fixture.source.RecoverReports(fixture.ws.Projects)
	if len(recovery.Unconsulted) != 1 || !strings.HasSuffix(recovery.Unconsulted[0].Reason, "has no cache entry for these inputs") {
		t.Fatalf("recovery = %+v, want the candidate unconsulted for a missing entry", recovery)
	}
}
