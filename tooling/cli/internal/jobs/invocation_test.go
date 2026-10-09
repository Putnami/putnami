package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/ownerperm"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// The fixture PROVIDER of this suite is three fixture scripts, and that is the
// point: nothing in the C7a machinery may know what kind of resource it is
// finalizing. The provisioning script writes a credential into the invocation's
// private artifact root and stamps the invocation id onto an "external
// resource"; the consuming script reads the credential; the finalizer removes
// the resource and appends one byte to a counter so exactly-once is countable.
const fixtureSecret = "postgres://fixture:s3kr1t@127.0.0.1:5432/db"

type invocationFixture struct {
	ws        *workspace.Workspace
	producer  *ScheduledJob
	consumer  *ScheduledJob
	finalizer *ScheduledJob
	planned   []*ScheduledJob
	// resource is the fixture provider's external resource: it exists between
	// setup and teardown and carries the non-secret invocation id.
	resource string
	// teardowns receives one byte per finalizer execution.
	teardowns string
	// consumed records what the consumer read, proving delivery of the artifact
	// root to the frontier.
	consumed string
	// identities collects the lease identity the finalizer found stamped on the
	// external resource it reclaimed.
	identities string
	// observed receives whatever the provisioning script recorded about the
	// workspace's invocations directory AT THE MOMENT IT RAN — which is how the
	// crash-recovery case proves reaping happened before provisioning.
	observed string
	// renderer is the surface the run's live rows are handed to. It is a fixture
	// member rather than a per-test construction so a case that inspects what the
	// terminal was shown uses the SHARED runner below instead of standing up a
	// second scheduler.
	renderer *mockRenderer
	// onSession, when set, receives the run's session records.
	onSession func(SessionRecord)
}

// newInvocationFixture builds a one-relation plan: setup → run, with a
// runOn:finally teardown that finalizes setup over the {run} frontier.
func newInvocationFixture(t *testing.T, setupBody, consumeBody fixtureScript) *invocationFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &invocationFixture{
		renderer:   &mockRenderer{},
		resource:   filepath.Join(root, "external-resource"),
		teardowns:  filepath.Join(root, "teardowns"),
		consumed:   filepath.Join(root, "consumed"),
		identities: filepath.Join(root, "reclaimed-identities"),
		observed:   filepath.Join(root, "observed-invocations"),
	}

	// Teardown is idempotent by construction (a missing resource is not an
	// error), exactly as the contract requires of a finalizer and of provider
	// cleanup.
	teardown := fixtureScript{
		{"append", "x", fixture.teardowns},
		{"append-file", fixture.resource, fixture.identities},
		{"remove", fixture.resource},
	}

	project := &workspace.Project{ID: "/proj", Name: "proj", Path: "."}
	// The extension is installed in a directory of its own: its installed tree
	// names it in every key, so it must not hold the workspace's files.
	ext := &extension.ExtensionDescription{
		Name: "@acme/provider",
		Path: t.TempDir(),
		Tasks: map[string]extension.TaskDefinition{
			"provision": {
				Kind: "command",
				Declares: &extensionproto.TaskDeclaration{
					Outputs: map[string]extensionproto.DeclaredOutput{
						"dsn": {
							Kind:      extensionproto.OutputKindRuntimeFile,
							Scope:     extensionproto.OutputScopeInvocation,
							Sensitive: true,
							Path:      "database/dsn.env",
						},
					},
					Effects: []string{extensionproto.EffectProcess},
				},
			},
			"consume":  {Kind: "command"},
			"teardown": {Kind: "command"},
		},
	}

	step := func(id, task string, dependsOn []string) extension.PipelineStep {
		return extension.PipelineStep{ID: id, Task: task, DependsOn: dependsOn}
	}
	finalizerStep := extension.PipelineStep{
		ID:    "teardown",
		Task:  "teardown",
		RunOn: extensionproto.StepRunOnFinally,
		Finalizes: &extensionproto.FinalizesRelation{
			Producer:  "setup",
			Consumers: []string{"run"},
		},
	}

	node := func(stepDef extension.PipelineStep, script fixtureScript, args []string, deps []string) *ScheduledJob {
		local := stepDef
		job := &ScheduledJob{
			Project:   project,
			Extension: ext,
			Step:      &local,
			JobDef: &extension.JobDefinition{
				Name:          extension.StepJobName("test", stepDef.ID),
				ExtensionName: ext.Name,
				CommandName:   "test",
				StepID:        stepDef.ID,
				Args:          args,
				TimeoutMs:     unboundedJobTimeoutMs,
			},
			DependsOn: deps,
		}
		fixtureTask(t, job.JobDef, script)
		task := ext.Tasks[stepDef.Task]
		task.Command = job.JobDef.Command
		ext.Tasks[stepDef.Task] = task
		return job
	}

	artifact := "{invocationArtifactRoot}/database/dsn.env"
	fixture.producer = node(step("setup", "provision", nil), setupBody,
		[]string{artifact, "{invocationArtifactRoot}", fixture.resource, fixture.observed}, nil)
	fixture.consumer = node(step("run", "consume", []string{"setup"}), consumeBody,
		[]string{artifact, fixture.consumed}, []string{"/proj:test~setup"})
	fixture.finalizer = node(finalizerStep, teardown, nil, nil)

	fixture.planned = []*ScheduledJob{fixture.producer, fixture.consumer, fixture.finalizer}
	fixture.ws = workspace.NewWorkspace(root, nil, []*workspace.Project{project})
	fixture.ws.Name = "test-ws"
	return fixture
}

// provisionBody writes the credential into the private artifact root and stamps
// the non-secret invocation id (the artifact root's owning directory) onto the
// external resource — the label a provider matches an orphan by.
var provisionBody = fixtureScript{{"provision"}}

// consumeBody reads the credential and records that it saw it, without ever
// echoing it.
var consumeBody = fixtureScript{{"consume"}}

func (f *invocationFixture) run(t *testing.T, ctx context.Context, cfg SchedulerConfig, cache *store.CacheManager) *SchedulerResult {
	t.Helper()
	return f.runWithParams(t, ctx, cfg, cache, nil)
}

func (f *invocationFixture) runWithParams(
	t *testing.T,
	ctx context.Context,
	cfg SchedulerConfig,
	cache *store.CacheManager,
	params map[string]any,
) *SchedulerResult {
	t.Helper()
	scheduler := newScheduler(f.ws, f.planned, params, cfg, f.renderer, cache)
	if f.onSession != nil {
		scheduler.setSessionEventHandler(f.onSession)
	}
	return scheduler.Run(ctx)
}

func (f *invocationFixture) teardownCount(t *testing.T) int {
	t.Helper()
	data, err := os.ReadFile(f.teardowns)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read teardown counter: %v", err)
	}
	return len(data)
}

// reclaimedIdentities returns the lease identities the finalizer read off the
// external resources it reclaimed.
func (f *invocationFixture) reclaimedIdentities(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(f.identities)
	if err != nil {
		return nil
	}
	return strings.Fields(string(data))
}

// TestInvocation_FinalizerRunsExactlyOnceOnSuccess is the baseline: the
// producer provisions, the frontier consumes, the finalizer runs once, and the
// private scratch is gone when the run ends.
func TestInvocation_FinalizerRunsExactlyOnceOnSuccess(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if got := result.Results[fixture.producer.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("producer result = %+v", got)
	}
	if got := result.Results[fixture.consumer.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("consumer result = %+v", got)
	}
	if got := result.Results[fixture.finalizer.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("finalizer result = %+v", got)
	}
	if !result.Success {
		t.Errorf("run should have succeeded: %+v", result.Session)
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times, want exactly once", got)
	}
	if data, err := os.ReadFile(fixture.consumed); err != nil || string(data) != "ok" {
		t.Errorf("consumer did not read the invocation artifact (%q, %v)", data, err)
	}
	if _, err := os.Stat(fixture.resource); !os.IsNotExist(err) {
		t.Errorf("finalizer left the external resource behind: %v", err)
	}
	if entries, err := os.ReadDir(store.InvocationsRoot(fixture.ws.Root)); err == nil && len(entries) != 0 {
		t.Errorf("invocation scratch survived the run: %v", entries)
	}
}

// TestInvocation_FinalizerExecutionResolvesInTheSessionLedger pins the
// physical-ledger contract for the scheduler path that does not use ordinary
// dispatch. A finalizer is a runOn:finally task, but its subprocess is no less
// real: the task's executionId must name a ledger entry just like every other
// task recorded in the session.
func TestInvocation_FinalizerExecutionResolvesInTheSessionLedger(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)
	if result.Session == nil {
		t.Fatal("run returned no session reduction")
	}
	finalizer := result.Results[fixture.finalizer.Key()]
	if finalizer == nil || finalizer.Execution == nil || finalizer.Execution.ID == "" {
		t.Fatalf("finalizer result = %+v, want a physical execution", finalizer)
	}

	declared := make(map[string]bool, len(result.Session.Executions))
	for _, execution := range result.Session.Executions {
		declared[execution.ID] = true
	}
	for key, task := range result.Results {
		if task == nil || task.Execution == nil || task.Execution.ID == "" {
			continue
		}
		if !declared[task.Execution.ID] {
			t.Errorf("session task %s references execution %q absent from the ledger: %+v",
				key, task.Execution.ID, result.Session.Executions)
		}
	}
}

// TestInvocation_ArtifactRootReachesOnlyTheRelation pins the delivery rule: the
// producer, the listed consumers and the finalizer get the locator, and nothing
// else in the plan does.
func TestInvocation_ArtifactRootReachesOnlyTheRelation(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	bystander := &ScheduledJob{
		Project:   fixture.producer.Project,
		Extension: fixture.producer.Extension,
		JobDef: &extension.JobDefinition{
			Name:          "test~unrelated",
			ExtensionName: fixture.producer.Extension.Name,
			CommandName:   "test",
			StepID:        "unrelated",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, bystander.JobDef, fixtureScript{{"exit", "0"}})
	fixture.planned = append(fixture.planned, bystander)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	for _, job := range []*ScheduledJob{fixture.producer, fixture.consumer, fixture.finalizer} {
		locator := job.InvocationLocator()
		if locator == nil || locator.ID == "" || locator.ArtifactRoot == "" {
			t.Fatalf("%s received no invocation locator", job.Key())
		}
		if !strings.HasSuffix(filepath.Dir(locator.ArtifactRoot), locator.ID) {
			t.Errorf("%s artifact root %q does not sit under its invocation id %q",
				job.Key(), locator.ArtifactRoot, locator.ID)
		}
	}
	if bystander.InvocationLocator() != nil {
		t.Error("an unrelated task received the invocation locator")
	}
	// The stamp travels onto the external resource: that is the lease identity a
	// provider matches an orphan by, and it is the invocation id, never the
	// credential.
	identities := fixture.reclaimedIdentities(t)
	want := fixture.producer.InvocationLocator().ID
	if len(identities) != 1 || identities[0] != want {
		t.Errorf("external resource carried %v, want the lease identity %q", identities, want)
	}
}

// TestInvocation_SetupFailureBlocksConsumersWithTypedCause is the setup-fail
// conformance case: the frontier never runs, it is told WHY in the closed
// vocabulary, and cleanup still happens because the producer started.
func TestInvocation_SetupFailureBlocksConsumersWithTypedCause(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, fixtureScript{{"exit", "9"}}, consumeBody)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if got := result.Results[fixture.producer.Key()]; got == nil || got.Status != "failed" {
		t.Fatalf("producer result = %+v, want failed", got)
	}
	blocked := result.Results[fixture.consumer.Key()]
	if blocked == nil || blocked.Status != "skipped" {
		t.Fatalf("consumer result = %+v, want skipped", blocked)
	}
	if blocked.Error == nil || blocked.Error.Code != extensionproto.FailureSensitiveSetupFailed {
		t.Fatalf("consumer diagnostic = %+v, want %s", blocked.Error, extensionproto.FailureSensitiveSetupFailed)
	}
	if !strings.Contains(blocked.Error.Message, fixture.producer.Key()) {
		t.Errorf("causal diagnostic %q does not name the producer", blocked.Error.Message)
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times after a setup failure, want exactly once", got)
	}
	if _, err := os.Stat(fixture.consumed); !os.IsNotExist(err) {
		t.Error("a blocked consumer executed against a resource that does not exist")
	}
}

// TestInvocation_ArtifactMissingFailsSetup covers the other half of a broken
// setup: the task succeeded but did not write what it declared.
func TestInvocation_ArtifactMissingFailsSetup(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, fixtureScript{{"exit", "0"}}, consumeBody)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	got := result.Results[fixture.producer.Key()]
	if got == nil || got.Status != "failed" {
		t.Fatalf("producer result = %+v, want failed", got)
	}
	if got.Error == nil || got.Error.Code != extensionproto.FailureSensitiveArtifactMissing {
		t.Fatalf("producer diagnostic = %+v, want %s", got.Error, extensionproto.FailureSensitiveArtifactMissing)
	}
	if count := fixture.teardownCount(t); count != 1 {
		t.Errorf("finalizer ran %d times, want exactly once", count)
	}
}

// TestInvocation_FinalizerRunsAfterConsumerFailure is the consumer-fail
// conformance case. A failing frontier is the most common real leak: without
// the arm-on-start rule, the finalizer would be skipped as a dependent of
// failed work.
func TestInvocation_FinalizerRunsAfterConsumerFailure(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, fixtureScript{{"exit", "4"}})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if got := result.Results[fixture.consumer.Key()]; got == nil || got.Status != "failed" {
		t.Fatalf("consumer result = %+v, want failed", got)
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times after a consumer failure, want exactly once", got)
	}
	if _, err := os.Stat(fixture.resource); !os.IsNotExist(err) {
		t.Errorf("the external resource leaked past a failing consumer: %v", err)
	}
	if result.Success {
		t.Error("a failing consumer must fail the run")
	}
}

// TestInvocation_FinalizerRunsAfterCancellation is the cancellation
// conformance case: the run's context is dead, so cleanup runs under its own
// bounded context or does not run at all.
func TestInvocation_FinalizerRunsAfterCancellation(t *testing.T) {
	fixture := newInvocationFixture(t, provisionBody, fixtureScript{{"sleep", "30"}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(fixture.resource); err == nil {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
	}()
	defer cancel()

	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	if got := fixture.teardownCount(t); got != 1 {
		t.Fatalf("finalizer ran %d times after cancellation, want exactly once", got)
	}
	if _, err := os.Stat(fixture.resource); !os.IsNotExist(err) {
		t.Errorf("the external resource leaked past cancellation: %v", err)
	}
	if got := result.Results[fixture.finalizer.Key()]; got == nil || got.Status != "success" {
		t.Errorf("finalizer result = %+v; cleanup must not inherit the cancellation that triggered it", got)
	}
	if entries, err := os.ReadDir(store.InvocationsRoot(fixture.ws.Root)); err == nil && len(entries) != 0 {
		t.Errorf("invocation scratch survived a canceled run: %v", entries)
	}
}

// TestInvocation_LeakingConsumerFailsClosed pins the fail-closed guard: a task
// that emits the credential is failed with the typed code and the value is
// withheld from every surface the result reaches.
func TestInvocation_LeakingConsumerFailsClosed(t *testing.T) {
	t.Parallel()
	// stderr is the surface a real leak takes: a task that dies while printing
	// its own configuration puts the captured text straight into the result's
	// error message, which is rendered, recorded, and stored in a cache entry.
	leaky := fixtureScript{{"cat-to-stderr", "$1"}, {"exit", "5"}}
	fixture := newInvocationFixture(t, provisionBody, leaky)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	scheduler := newScheduler(fixture.ws, fixture.planned, nil,
		SchedulerConfig{MaxParallel: 2, NoCache: true}, &mockRenderer{}, nil)
	var recorded []SessionRecord
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		recorded = append(recorded, record)
	})
	result := scheduler.Run(ctx)

	got := result.Results[fixture.consumer.Key()]
	if got == nil || got.Status != "failed" {
		t.Fatalf("leaking consumer result = %+v, want failed", got)
	}
	if got.Error == nil || got.Error.Code != extensionproto.FailureSensitiveLeakDetected {
		t.Fatalf("leak diagnostic = %+v, want %s", got.Error, extensionproto.FailureSensitiveLeakDetected)
	}
	if strings.Contains(got.Error.Message, fixtureSecret) {
		t.Fatal("the leak diagnostic republished the value it exists to withhold")
	}
	for _, record := range recorded {
		for key, value := range record.Data {
			text, ok := value.(string)
			if ok && strings.Contains(text, fixtureSecret) {
				t.Fatalf("session record %s.%s carried the sensitive value", record.Type, key)
			}
		}
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times, want exactly once", got)
	}
}

// leakyProvisionBody is a provisioner that ECHOES the credential it just
// generated onto its live event stream — a driver printing the binding it
// created, a script logging what it wrote. It is the one stream that arrives
// BEFORE any needle can exist, because needles come from what the producer
// wrote and nothing is written until it exits.
var leakyProvisionBody = provisionBody.then(fixtureScript{{"log", "bound " + fixtureSecret}})

// TestInvocation_ProducerOutputIsWithheldUntilItCanBeRedacted pins the
// confinement window the producer's own output opens.
//
// The guard's needles are derived from the artifacts the producer WROTE, which
// exist only once it has exited. Its live events therefore reach the renderer
// before a single needle exists, so forwarding them as they arrive put the
// generated credential on the terminal — and into any renderer that persists
// what it displayed — with nothing able to redact it. The same window closed
// over the producer's own RESULT: guarding it before validation matched its
// error, data and events against an empty set and published them.
//
// Both halves are one property: nothing a sensitive-artifact producer emits is
// published before the needles derived from what it produced exist.
func TestInvocation_ProducerOutputIsWithheldUntilItCanBeRedacted(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, leakyProvisionBody, consumeBody)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var records []SessionRecord
	var mu sync.Mutex
	fixture.onSession = func(record SessionRecord) {
		mu.Lock()
		records = append(records, record)
		mu.Unlock()
	}
	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	// THE LIVE STREAM. Not one event the renderer was handed — for any job —
	// carried the credential.
	fixture.renderer.mu.Lock()
	rendered := fixture.renderer.jobEvents
	fixture.renderer.mu.Unlock()
	releasedRedacted := false
	for key, events := range rendered {
		for _, event := range events {
			payload, err := json.Marshal(event)
			if err != nil {
				t.Fatalf("render event of %s: %v", key, err)
			}
			if strings.Contains(string(payload), fixtureSecret) {
				t.Fatalf("the renderer was handed %s's live event carrying the credential: %s", key, payload)
			}
			if strings.Contains(string(payload), redactedMarker) {
				releasedRedacted = true
			}
		}
	}
	// Withholding is not dropping: the producer's output still reaches the
	// renderer, redacted, once the needles exist.
	if !releasedRedacted {
		t.Error("the producer's withheld events were never released; suppressing output is not the fix")
	}

	// THE PRODUCER'S OWN RESULT. It emitted the credential, so the fail-closed
	// guard applies to it exactly as it does to a consumer.
	producer := result.Results[fixture.producer.Key()]
	if producer == nil || producer.Status != "failed" {
		t.Fatalf("producer result = %+v, want failed: it published the credential it generated", producer)
	}
	if producer.Error == nil || producer.Error.Code != extensionproto.FailureSensitiveLeakDetected {
		t.Fatalf("producer diagnostic = %+v, want %s", producer.Error, extensionproto.FailureSensitiveLeakDetected)
	}

	// EVERY PUBLISHED SURFACE, as the crash-recovery case checks them.
	mu.Lock()
	defer mu.Unlock()
	for _, surface := range publishedSurfaces(t, records, result) {
		if strings.Contains(surface.text, fixtureSecret) {
			t.Errorf("%s leaked the credential: %s", surface.name, surface.text)
		}
	}
	// And the resource is still torn down: confinement never costs cleanup.
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times, want exactly once", got)
	}
}

// TestInvocation_LateLeakAfterNoisyStreamFailsClosed pins the event verdict's
// independence from bounded JobResult.Events. Both the producer's private
// pre-needle spool and a consumer's live redaction path must observe every
// event, publish the complete sanitized stream, and remember a late hit after
// the result's ordinary retention partition is exhausted.
func TestInvocation_LateLeakAfterNoisyStreamFailsClosed(t *testing.T) {
	t.Parallel()
	const eventCount = 900
	flood := fixtureScript{{"flood", strconv.Itoa(eventCount)}, {"log", "late " + fixtureSecret}}

	for _, tc := range []struct {
		name      string
		setupBody fixtureScript
		runBody   fixtureScript
		target    func(*invocationFixture) *ScheduledJob
	}{
		{
			name:      "producer replay",
			setupBody: provisionBody.then(flood),
			runBody:   consumeBody,
			target:    func(f *invocationFixture) *ScheduledJob { return f.producer },
		},
		{
			name:      "live consumer",
			setupBody: provisionBody,
			runBody:   consumeBody.then(flood),
			target:    func(f *invocationFixture) *ScheduledJob { return f.consumer },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newInvocationFixture(t, tc.setupBody, tc.runBody)
			target := tc.target(fixture)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var records []SessionRecord
			fixture.onSession = func(record SessionRecord) { records = append(records, record) }
			result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

			got := result.Results[target.Key()]
			if got == nil || got.Status != "failed" || got.Error == nil ||
				got.Error.Code != extensionproto.FailureSensitiveLeakDetected {
				t.Fatalf("late-leaking result = %+v, want %s", got, extensionproto.FailureSensitiveLeakDetected)
			}
			if len(got.Events) >= eventCount {
				t.Fatalf("retained events = %d, want bounded projection below %d-event flood", len(got.Events), eventCount)
			}

			fixture.renderer.mu.Lock()
			events := append([]RawJobEvent(nil), fixture.renderer.jobEvents[target.Key()]...)
			fixture.renderer.mu.Unlock()
			ordinary, redacted := 0, false
			for _, event := range events {
				if strings.HasPrefix(event.Message, "ordinary-") {
					ordinary++
				}
				if strings.Contains(event.Message, redactedMarker) {
					redacted = true
				}
				if strings.Contains(event.Message, fixtureSecret) {
					t.Fatalf("renderer received secret in event: %+v", event)
				}
			}
			if ordinary != eventCount || !redacted {
				t.Fatalf("sanitized stream ordinary=%d redacted=%v, want complete %d-event flood plus redacted leak",
					ordinary, redacted, eventCount)
			}
			for _, surface := range publishedSurfaces(t, records, result) {
				if strings.Contains(surface.text, fixtureSecret) {
					t.Fatalf("%s leaked late credential: %s", surface.name, surface.text)
				}
			}
		})
	}
}

// TestRedactData_ScrubsNestedPayloadsAtAnyDepth pins that the redactor's reach
// is the payload's shape, not one level of it.
//
// An event's data is whatever a task reported: rows, records, findings,
// connection details. `{"records":[{"dsn":"…"}]}` and `{"rows":[["…"]]}` are
// ordinary shapes, and a redactor that descends into maps but reads an array
// one level deep — and only when the member is DIRECTLY a string — neither
// scrubs them nor reports the leak, so the value reaches the live stream and
// the stored result while the task is reported green.
func TestRedactData_ScrubsNestedPayloadsAtAnyDepth(t *testing.T) {
	t.Parallel()
	needles := []string{fixtureSecret}
	data := map[string]any{
		"records": []any{map[string]any{"dsn": "connect with " + fixtureSecret}},
		"rows":    []any{[]any{fixtureSecret}},
		"deep":    map[string]any{"pages": []any{[]any{map[string]any{"url": fixtureSecret}}}},
		"count":   float64(3),
	}
	// Hold the containers the caller holds: the redactor mutates in place, so
	// these references must see the scrubbed values.
	rows := data["rows"].([]any)

	if !redactData(data, needles) {
		t.Fatal("a nested payload leak was not reported")
	}
	rendered, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("render payload: %v", err)
	}
	if strings.Contains(string(rendered), fixtureSecret) {
		t.Fatalf("scrubbed payload still carries the value: %s", rendered)
	}
	if got := rows[0].([]any)[0].(string); got != redactedMarker {
		t.Errorf("the caller's own slice was not scrubbed in place: %q", got)
	}
	if got := data["count"]; got != float64(3) {
		t.Errorf("a non-string member was rewritten: %v", got)
	}

	clean := map[string]any{"rows": []any{[]any{map[string]any{"note": "nothing to see"}}}}
	if redactData(clean, needles) {
		t.Error("a clean nested payload reported a leak")
	}
}

// TestInvocation_AStepInTwoFrontiersIsRefusedRatherThanGivenAWinner is the
// defense-in-depth half of the one-invocation-per-step rule (manifest
// validation owns the authoring half: `shared-finalizer-consumer`).
//
// A step receives exactly ONE invocation locator and a relation holds exactly
// one blocking and guard state, so a plan that lists one step in two relations'
// consumer frontiers has no execution — only a choice of which relation wins,
// which silently hands the step one relation's private artifact tree while the
// other relation's confinement applies to nothing.
func TestInvocation_AStepInTwoFrontiersIsRefusedRatherThanGivenAWinner(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	sibling := func(model *ScheduledJob, step extension.PipelineStep) *ScheduledJob {
		local := step
		def := *model.JobDef
		def.Name = extension.StepJobName("test", step.ID)
		def.StepID = step.ID
		return &ScheduledJob{
			Project:   model.Project,
			Extension: model.Extension,
			Step:      &local,
			JobDef:    &def,
		}
	}
	secondProducer := sibling(fixture.producer, extension.PipelineStep{ID: "setup-2", Task: "provision"})
	secondFinalizer := sibling(fixture.finalizer, extension.PipelineStep{
		ID:    "teardown-2",
		Task:  "teardown",
		RunOn: extensionproto.StepRunOnFinally,
		Finalizes: &extensionproto.FinalizesRelation{
			Producer:  "setup-2",
			Consumers: []string{"run"},
		},
	})
	planned := append(append([]*ScheduledJob(nil), fixture.planned...), secondProducer, secondFinalizer)

	// PLAN TIME: the run refuses to start, naming the step and both relations.
	err := validateInvocationFrontiers(planned)
	if err == nil {
		t.Fatal("a plan that puts one step in two finalizes relations was accepted")
	}
	for _, want := range []string{
		fixture.consumer.Key(), fixture.producer.Key(), secondProducer.Key(),
		fixture.finalizer.Key(), secondFinalizer.Key(),
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("diagnostic %q does not name %s", err, want)
		}
	}

	// RUNTIME: a plan that reaches the scheduler anyway blocks the shared step
	// with the typed cause instead of picking a winner for it.
	runtime := newInvocationRuntime(fixture.ws, planned)
	if runtime == nil {
		t.Fatal("the plan declares two relations")
	}
	cause := runtime.blockedError(fixture.consumer)
	if cause == nil || cause.Code != extensionproto.FailureSensitiveSetupFailed {
		t.Fatalf("shared consumer diagnostic = %+v, want %s", cause, extensionproto.FailureSensitiveSetupFailed)
	}
	if !strings.Contains(cause.Message, fixture.producer.Key()) ||
		!strings.Contains(cause.Message, secondProducer.Key()) {
		t.Errorf("causal diagnostic %q does not name both relations", cause.Message)
	}
	for _, producer := range []*ScheduledJob{fixture.producer, secondProducer} {
		relation := runtime.byProducer[producer.Key()]
		if relation == nil {
			t.Fatalf("%s resolved no relation", producer.Key())
		}
		relation.mu.Lock()
		refused := relation.setupFailed
		relation.mu.Unlock()
		if !refused {
			t.Errorf("%s's relation still believes its frontier is runnable", producer.Key())
		}
	}
}

// TestInvocation_ProducerFailingBeforeItStartsBlocksItsFrontier pins that the
// frontier is blocked by a producer that never provisioned, whatever stage of
// its lifecycle it died in and whatever --continue-on-error says.
//
// Only a producer that reached its subprocess records a verdict through
// closeTask. A producer can fail EARLIER — a command output that cannot be
// prepared, a preBuild hook that fails, a scratch that cannot be created — and
// those failures reach the scheduler on the ordinary completion path. Left
// unobserved there, `--continue-on-error` read them as an unrelated failure and
// dispatched the consumer with no invocation locator at all, against a resource
// that was never provisioned.
func TestInvocation_ProducerFailingBeforeItStartsBlocksItsFrontier(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)

	// Declaring a command-output artifact makes prepareTask prepare the command
	// directory FIRST — before the hook, before the scratch, before any
	// subprocess. Planting a file where that directory belongs fails the
	// producer at the earliest point of its lifecycle. The consumer declares no
	// command output, so it is untouched and remains free to run.
	provision := fixture.producer.Extension.Tasks["provision"]
	provision.Declares.Outputs["report"] = extensionproto.DeclaredOutput{
		Kind:          extensionproto.OutputKindFile,
		Root:          extensionproto.OutputRootCommandOutput,
		Path:          "report.txt",
		OptionalEmpty: true,
	}
	fixture.producer.Extension.Tasks["provision"] = provision

	blocker := filepath.Join(fixture.ws.Root, ".putnami", "out", "test")
	if err := os.MkdirAll(filepath.Dir(blocker), 0o755); err != nil {
		t.Fatalf("prepare command output root: %v", err)
	}
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("plant the command-output blocker: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := fixture.run(t, ctx,
		SchedulerConfig{MaxParallel: 2, NoCache: true, ContinueOnError: true}, nil)

	produced := result.Results[fixture.producer.Key()]
	if produced == nil || produced.Status != "failed" {
		t.Fatalf("producer result = %+v, want a pre-start failure", produced)
	}
	blocked := result.Results[fixture.consumer.Key()]
	if blocked == nil || blocked.Status != "skipped" {
		t.Fatalf("consumer result = %+v, want skipped: --continue-on-error does not mean "+
			"run against a resource nobody provisioned", blocked)
	}
	if blocked.Error == nil || blocked.Error.Code != extensionproto.FailureSensitiveSetupFailed {
		t.Fatalf("consumer diagnostic = %+v, want %s", blocked.Error, extensionproto.FailureSensitiveSetupFailed)
	}
	if _, err := os.Stat(fixture.consumed); !os.IsNotExist(err) {
		t.Error("the consumer executed against an invocation that was never armed")
	}
	// Nothing was provisioned, so there is nothing to tear down.
	if got := fixture.teardownCount(t); got != 0 {
		t.Errorf("finalizer ran %d times for a producer that never started, want never", got)
	}
}

// TestInvocation_AllHitFrontierPrunesSetupAndFinalizer pins negotiation before
// materialization: when every consumer is already cached, nothing is
// provisioned and nothing is torn down.
func TestInvocation_AllHitFrontierPrunesSetupAndFinalizer(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	// The consumer becomes cacheable by declaring an ordinary durable output;
	// the producer stays uncacheable, as the contract requires of any task with
	// an invocation-scoped output.
	fixture.consumer.Step.Task = "consume-cached"
	fixture.consumer.JobDef.Cache = true
	fixture.consumer.Extension.Tasks["consume-cached"] = extension.TaskDefinition{
		Kind:    "command",
		Command: fixture.consumer.JobDef.Command,
		Declares: &extensionproto.TaskDeclaration{
			Outputs: map[string]extensionproto.DeclaredOutput{
				"report": {Kind: extensionproto.OutputKindFile, Path: "report.txt", OptionalEmpty: true},
			},
		},
	}

	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	// The consumer's key folds in the producing action's digest, so the plan's
	// relations must be resolved before the key the test publishes at can match
	// the one the scheduler looks up.
	resolveInvocationRelations(fixture.planned)
	keys, err := PrecomputeKeys(fixture.ws, fixture.planned, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	hash := keys[fixture.consumer.Key()]
	if hash == "" {
		t.Fatal("the consumer must be cacheable for this case to mean anything")
	}
	if _, err := cache.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
		Key:      hash,
		Result:   &store.EntryResult{Status: "success"},
		Metadata: &store.EntryMetadata{Extension: "@acme/provider", Task: "test~run", Project: "proj"},
	}); err != nil {
		t.Fatalf("publish consumer entry: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2}, cache)

	if got := result.Results[fixture.producer.Key()]; got == nil || got.Status != "skipped" {
		t.Fatalf("producer result = %+v, want skipped by an all-hit frontier", got)
	}
	if got := result.Results[fixture.finalizer.Key()]; got == nil || got.Status != "skipped" {
		t.Fatalf("finalizer result = %+v, want skipped", got)
	}
	if got := fixture.teardownCount(t); got != 0 {
		t.Errorf("finalizer ran %d times for a pruned relation, want never", got)
	}
	if _, err := os.Stat(fixture.resource); !os.IsNotExist(err) {
		t.Error("a pruned relation provisioned its resource anyway")
	}
	if entries, err := os.ReadDir(store.InvocationsRoot(fixture.ws.Root)); err == nil && len(entries) != 0 {
		t.Errorf("a pruned relation created an invocation scratch: %v", entries)
	}
}

// TestInvocation_PruneIfPreservesImperativeSideEffects is a regression test.
// `putnami test --infra-down` is an imperative request carried by the
// producer's params: even when the test consumer is a local cache hit, setup
// must run the requested teardown and arm its finalizer. The same manifest
// relation must remain prunable for an ordinary warm test run.
func TestInvocation_PruneIfPreservesImperativeSideEffects(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		infraDown  bool
		wantPruned bool
	}{
		{
			name:       "ordinary warm frontier still prunes",
			expression: "!params.infra-down",
			infraDown:  false,
			wantPruned: true,
		},
		{
			name:       "explicit teardown keeps lifecycle",
			expression: "!params.infra-down",
			infraDown:  true,
			wantPruned: false,
		},
		{
			name:       "malformed predicate fails safe",
			expression: "params.infra-down ==",
			infraDown:  true,
			wantPruned: false,
		},
		{
			name:       "missing and malformed predicate fails safe",
			expression: "params.missing ==",
			infraDown:  true,
			wantPruned: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			params := map[string]any{"infra-down": tt.infraDown}
			fixture := newInvocationFixture(t, provisionBody, consumeBody)
			fixture.finalizer.Step.Finalizes.PruneIf = tt.expression
			fixture.consumer.Step.Task = "consume-cached"
			fixture.consumer.JobDef.Cache = true
			fixture.consumer.Extension.Tasks["consume-cached"] = extension.TaskDefinition{
				Kind:    "command",
				Command: fixture.consumer.JobDef.Command,
				Declares: &extensionproto.TaskDeclaration{
					Outputs: map[string]extensionproto.DeclaredOutput{
						"report": {
							Kind:          extensionproto.OutputKindFile,
							Path:          "report.txt",
							OptionalEmpty: true,
						},
					},
				},
			}

			cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
			resolveInvocationRelations(fixture.planned)
			keys, err := PrecomputeKeys(fixture.ws, fixture.planned, params, nil, cache, CacheBypass{})
			if err != nil {
				t.Fatalf("PrecomputeKeys: %v", err)
			}
			hash := keys[fixture.consumer.Key()]
			if hash == "" {
				t.Fatal("the consumer must be cacheable for this case to mean anything")
			}
			if _, err := cache.IngestTaskEntry(t.TempDir(), store.TaskEntrySpec{
				Key:      hash,
				Result:   &store.EntryResult{Status: "success"},
				Metadata: &store.EntryMetadata{Extension: "@acme/provider", Task: "test~run", Project: "proj"},
			}); err != nil {
				t.Fatalf("publish consumer entry: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result := fixture.runWithParams(t, ctx, SchedulerConfig{MaxParallel: 2}, cache, params)

			consumer := result.Results[fixture.consumer.Key()]
			if consumer == nil || consumer.Status != "success" || !consumer.CacheHit {
				t.Fatalf("consumer result = %+v, want cached success", consumer)
			}
			producer := result.Results[fixture.producer.Key()]
			finalizer := result.Results[fixture.finalizer.Key()]
			if tt.wantPruned {
				if producer == nil || producer.Status != "skipped" {
					t.Fatalf("producer result = %+v, want pruned", producer)
				}
				if finalizer == nil || finalizer.Status != "skipped" {
					t.Fatalf("finalizer result = %+v, want pruned", finalizer)
				}
				if got := fixture.teardownCount(t); got != 0 {
					t.Fatalf("pruned finalizer ran %d times, want never", got)
				}
				return
			}

			if producer == nil || producer.Status != "success" {
				t.Fatalf("producer result = %+v, want the imperative action to run", producer)
			}
			if finalizer == nil || finalizer.Status != "success" {
				t.Fatalf("finalizer result = %+v, want armed cleanup", finalizer)
			}
			if got := fixture.teardownCount(t); got != 1 {
				t.Fatalf("finalizer ran %d times, want exactly once", got)
			}
			if _, err := os.Stat(fixture.resource); !os.IsNotExist(err) {
				t.Fatalf("imperative lifecycle left its resource behind: %v", err)
			}
		})
	}
}

// TestInvocation_PrunedRelationStillServesTheConsumersCachedResult is a
// repair for the other half of the pruning contract.
//
// Pruning is decided BY the consumers: the relation is pruned precisely because
// every consumer's entry is already in the local store. A consumer must
// therefore be SERVED that entry — its stored verdict replayed and its declared
// outputs materialized — not reported "skipped" alongside the producer it made
// unnecessary. Matching the prune through the participants map (producer +
// consumers + finalizer) reported the consumer skipped on every warm run of
// every project with a finalizes relation: no cached verdict, no restored
// coverage output, for exactly the runs the pruning exists to make cheap.
func TestInvocation_PrunedRelationStillServesTheConsumersCachedResult(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	fixture.consumer.Step.Task = "consume-cached"
	fixture.consumer.JobDef.Cache = true
	fixture.consumer.Extension.Tasks["consume-cached"] = extension.TaskDefinition{
		Kind:    "command",
		Command: fixture.consumer.JobDef.Command,
		Declares: &extensionproto.TaskDeclaration{
			Outputs: map[string]extensionproto.DeclaredOutput{
				"report": {Kind: extensionproto.OutputKindFile, Path: "report.txt"},
			},
		},
	}

	cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
	resolveInvocationRelations(fixture.planned)
	keys, err := PrecomputeKeys(fixture.ws, fixture.planned, nil, nil, cache, CacheBypass{})
	if err != nil {
		t.Fatalf("PrecomputeKeys: %v", err)
	}
	hash := keys[fixture.consumer.Key()]
	if hash == "" {
		t.Fatal("the consumer must be cacheable for this case to mean anything")
	}

	// Publish the entry a previous run would have left: a green verdict AND the
	// declared output's bytes.
	const restored = "coverage: 100%\n"
	staging := t.TempDir()
	spec := store.DeclaredEntryOutput{ID: "report", Kind: extensionproto.OutputKindFile, Path: "report.txt"}
	stagedAt := store.TaskStagingPath(staging, spec)
	if err := os.MkdirAll(filepath.Dir(stagedAt), 0o755); err != nil {
		t.Fatalf("stage report: %v", err)
	}
	if err := os.WriteFile(stagedAt, []byte(restored), 0o644); err != nil {
		t.Fatalf("write staged report: %v", err)
	}
	if _, err := cache.IngestTaskEntry(staging, store.TaskEntrySpec{
		Key:      hash,
		Result:   &store.EntryResult{Status: "success"},
		Metadata: &store.EntryMetadata{Extension: "@acme/provider", Task: "test~run", Project: "proj"},
		Outputs:  []store.DeclaredEntryOutput{spec},
	}); err != nil {
		t.Fatalf("publish consumer entry: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2}, cache)

	// The pruned half is unchanged: nothing was provisioned, nothing torn down.
	if got := result.Results[fixture.producer.Key()]; got == nil || got.Status != "skipped" {
		t.Fatalf("producer result = %+v, want skipped by an all-hit frontier", got)
	}
	if got := result.Results[fixture.finalizer.Key()]; got == nil || got.Status != "skipped" {
		t.Fatalf("finalizer result = %+v, want skipped", got)
	}
	if got := fixture.teardownCount(t); got != 0 {
		t.Errorf("finalizer ran %d times for a pruned relation, want never", got)
	}

	// The consumer is SERVED, not skipped.
	served := result.Results[fixture.consumer.Key()]
	if served == nil || served.Status != "success" {
		t.Fatalf("consumer result = %+v, want the cached success it was pruned for", served)
	}
	if !served.CacheHit {
		t.Errorf("consumer result = %+v, want a cache hit; a pruned consumer must not be reported skipped", served)
	}
	// And its declared output is back on disk: a warm run that serves a verdict
	// without restoring what the verdict was recorded with is the same bug.
	data, err := os.ReadFile(filepath.Join(fixture.ws.Root, "report.txt"))
	if err != nil || string(data) != restored {
		t.Errorf("declared output was not restored (%q, %v)", data, err)
	}
	if _, err := os.Stat(fixture.consumed); !os.IsNotExist(err) {
		t.Error("the consumer executed instead of being served its entry")
	}
}

// TestInvocation_ConsumerKeyFollowsTheProducingAction pins the cache-identity
// rule: a consumer's key varies with the producing ACTION and never with the
// secret's content.
func TestInvocation_ConsumerKeyFollowsTheProducingAction(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	fixture.consumer.JobDef.Cache = true
	fixture.consumer.Extension.Tasks["consume"] = extension.TaskDefinition{
		Kind:     "command",
		Command:  fixture.consumer.JobDef.Command,
		Declares: &extensionproto.TaskDeclaration{},
	}
	resolveInvocationRelations(fixture.planned)

	consumerKey := func() string {
		t.Helper()
		// A fresh manager each time: the memoized file-hash cache would otherwise
		// hide a content change the key is supposed to see.
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		keys, err := PrecomputeKeys(fixture.ws, fixture.planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys[fixture.consumer.Key()]
	}

	baseKey := consumerKey()
	if baseKey == "" {
		t.Fatal("consumer produced no cache key")
	}
	if again := consumerKey(); again != baseKey {
		t.Fatalf("consumer key is not deterministic: %q then %q", baseKey, again)
	}

	// The secret's own bytes are NOT an input. Materializing a credential where
	// the declaration actually puts it — the invocation's private scratch —
	// must leave every key exactly where it was. That is the difference between
	// keying on the action and keying on what the action produced.
	scratch, err := store.NewInvocationScratch(fixture.ws.Root, "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	artifact, err := scratch.Prepare("database/dsn.env", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.WriteFile(artifact, []byte("DATABASE_URL=postgres://other:pw@h/db\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	if withSecret := consumerKey(); withSecret != baseKey {
		t.Errorf("consumer key moved with the secret's content: %q, want %q", withSecret, baseKey)
	}

	// The producing ACTION is an input. Re-declaring the provisioning task —
	// which is what its task-contract digest reports — moves the consumer's key
	// even though the consumer's own inputs are untouched.
	fixture.producer.JobDef.ContractDigest = "provisioning-task-v2"
	if changed := consumerKey(); changed == baseKey {
		t.Error("the consumer's cache key ignored a change to the producing action")
	}

	// And the fold is what does it: a consumer with no producing action keys
	// differently again.
	fixture.consumer.InvocationProducer = nil
	if blind := consumerKey(); blind == baseKey {
		t.Error("the producing action's digest never reached the consumer's key")
	}
}

// TestInvocation_ConsumerKeyFollowsTheProducersClosure is a repair.
//
// A consumer folds the PRODUCING ACTION's digest, and that digest is only as
// complete as the producer's declared inputs. The producer of a database test
// environment reads its whole dependency CLOSURE's committed
// infra/requirements.json (dbtestenv.ClosureDatabases walks
// project.dependencyClosure), so a requirements-only change in a DEPENDENCY
// changes the environment the consumer's tests run against. Before the closure
// input existed, the producer declared only its OWN manifest, the digest never
// moved, and the consumer served a stored verdict against a different world.
//
// The negative half matters just as much: an unrelated file in the same
// dependency must NOT move the key, or the fold degenerates into "any change
// anywhere in the closure" and every warm run of a workload goes cold.
func TestInvocation_ConsumerKeyFollowsTheProducersClosure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"default","engine":"postgres"}]}`)
	writeProjectFile(t, root, "lib", "unrelated.txt", "before\n")
	writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")

	lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
	app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
	ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
	ws.Graph = workspace.BuildGraph(ws.Projects)

	ext := &extension.ExtensionDescription{
		Name: "@acme/provider",
		Path: t.TempDir(),
		Tasks: map[string]extension.TaskDefinition{
			"provision": {Kind: "command", Command: "/bin/true", Declares: &extensionproto.TaskDeclaration{
				Outputs: map[string]extensionproto.DeclaredOutput{
					"dsn": {
						Kind:      extensionproto.OutputKindRuntimeFile,
						Scope:     extensionproto.OutputScopeInvocation,
						Sensitive: true,
						Path:      "database/dsn.env",
					},
				},
			}},
			"consume":  {Kind: "command", Command: "/bin/true", Declares: &extensionproto.TaskDeclaration{}},
			"teardown": {Kind: "command", Command: "/bin/true"},
		},
	}

	node := func(step extension.PipelineStep, cache bool, key *extensionproto.TaskCacheKey) *ScheduledJob {
		local := step
		job := &ScheduledJob{
			Project:   app,
			Extension: ext,
			Step:      &local,
			JobDef: &extension.JobDefinition{
				Name:          extension.StepJobName("test", step.ID),
				ExtensionName: ext.Name,
				CommandName:   "test",
				StepID:        step.ID,
				Command:       "/bin/true",
				Cache:         cache,
			},
		}
		if key != nil {
			job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: key}
		}
		return job
	}

	// The producer declares the closure input the runtime actually reads.
	producer := node(extension.PipelineStep{ID: "setup", Task: "provision"}, false,
		&extensionproto.TaskCacheKey{ClosureFiles: []string{"infra/requirements.json"}})
	// The consumer keys on its own sources, as the real `test-exec` does, so an
	// unrelated file cannot move its key through its OWN patterns and the
	// assertions below observe only the producing action's fold.
	consumer := node(extension.PipelineStep{ID: "run", Task: "consume", DependsOn: []string{"setup"}}, true,
		&extensionproto.TaskCacheKey{Files: []string{"**/*.go"}})
	consumer.DependsOn = []string{producer.Key()}
	finalizer := node(extension.PipelineStep{
		ID:        "teardown",
		Task:      "teardown",
		RunOn:     extensionproto.StepRunOnFinally,
		Finalizes: &extensionproto.FinalizesRelation{Producer: "setup", Consumers: []string{"run"}},
	}, false, nil)

	planned := []*ScheduledJob{producer, consumer, finalizer}
	resolveInvocationRelations(planned)
	if consumer.InvocationProducer != producer {
		t.Fatal("the fixture must resolve the finalizes relation for the fold to exist")
	}

	consumerKey := func() string {
		t.Helper()
		// A fresh manager each time: the memoized file-hash cache would otherwise
		// hide the content change the key is supposed to see.
		cache := store.NewCacheManager(store.NewLocalStore(t.TempDir()))
		keys, err := PrecomputeKeys(ws, planned, nil, nil, cache, CacheBypass{})
		if err != nil {
			t.Fatalf("PrecomputeKeys: %v", err)
		}
		return keys[consumer.Key()]
	}

	baseKey := consumerKey()
	if baseKey == "" {
		t.Fatal("consumer produced no cache key")
	}
	if again := consumerKey(); again != baseKey {
		t.Fatalf("consumer key is not deterministic: %q then %q", baseKey, again)
	}

	// An unrelated file in the same dependency is not an input.
	writeProjectFile(t, root, "lib", "unrelated.txt", "after\n")
	if unrelated := consumerKey(); unrelated != baseKey {
		t.Errorf("an unrelated dependency file moved the consumer's key: %q, want %q", unrelated, baseKey)
	}
	// Nor is an unrelated file in the PRODUCER's own project. This is the guard
	// against the empty-pattern fallback: the file-hash helper reads an empty
	// pattern set as "hash the whole project tree", so a producer whose only file
	// input is a closure port must still name its patterns project-relatively or
	// every source edit would drag the test verdict cold.
	writeProjectFile(t, root, "app", "unrelated.txt", "after\n")
	if unrelated := consumerKey(); unrelated != baseKey {
		t.Errorf("an unrelated file in the producer's own project moved the consumer's key: %q, want %q",
			unrelated, baseKey)
	}

	// The DEPENDENCY's committed requirements are.
	writeProjectFile(t, root, "lib", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"reports","engine":"postgres"}]}`)
	changed := consumerKey()
	if changed == baseKey {
		t.Error("a requirements-only change in a dependency left the consumer's key unmoved; " +
			"the stored verdict would be served against a different provisioned environment")
	}

	// So are the SEED project's own: the closure includes the project itself.
	writeProjectFile(t, root, "app", "infra/requirements.json",
		`{"protocolVersion":2,"databases":[{"name":"own","engine":"postgres"}]}`)
	if seeded := consumerKey(); seeded == changed {
		t.Error("the seed project's own requirements are part of its closure and must move the key")
	}
}

// TestClosureInputsDigest_IsIndependentOfTheCheckoutDirectory pins that a
// closure key input travels: two checkouts of the same content produce the same
// digest, so a remote entry stays shareable between a developer and CI. (The
// other cross-project key mechanism, ExtraFiles, hashes ABSOLUTE paths, which is
// exactly why this digest does not use it.)
func TestClosureInputsDigest_IsIndependentOfTheCheckoutDirectory(t *testing.T) {
	t.Parallel()
	build := func(root string) (*workspace.Workspace, *ScheduledJob) {
		writeProjectFile(t, root, "lib", "infra/requirements.json", `{"protocolVersion":2}`)
		writeProjectFile(t, root, "app", "go.mod", "module example.com/app\n")
		lib := &workspace.Project{ID: "/lib", Name: "lib", Path: "lib"}
		app := &workspace.Project{ID: "/app", Name: "app", Path: "app", Dependencies: []string{"lib"}}
		ws := workspace.NewWorkspace(root, nil, []*workspace.Project{lib, app})
		ws.Graph = workspace.BuildGraph(ws.Projects)
		job := &ScheduledJob{
			Project: app,
			JobDef: &extension.JobDefinition{
				Name: "test~setup",
				TaskCachePolicy: &extension.TaskCachePolicy{
					Key: &extensionproto.TaskCacheKey{ClosureFiles: []string{"infra/requirements.json"}},
				},
			},
		}
		return ws, job
	}

	leftWS, leftJob := build(t.TempDir())
	rightWS, rightJob := build(t.TempDir())
	left, leftErr := closureInputsDigest(leftWS, leftJob, store.NewCacheManager(store.NewLocalStore(t.TempDir())))
	right, rightErr := closureInputsDigest(rightWS, rightJob, store.NewCacheManager(store.NewLocalStore(t.TempDir())))
	if leftErr != nil || rightErr != nil {
		t.Fatalf("closure inputs: %v, %v", leftErr, rightErr)
	}
	if left == "" {
		t.Fatal("a declared closure input produced no digest")
	}
	if left != right {
		t.Errorf("closure digest varies with the checkout directory: %q vs %q", left, right)
	}

	// A task that declares no closure input pays nothing and folds nothing.
	bare := &ScheduledJob{Project: leftJob.Project, JobDef: &extension.JobDefinition{Name: "test~run"}}
	if got, err := closureInputsDigest(leftWS, bare, store.NewCacheManager(store.NewLocalStore(t.TempDir()))); err != nil || got != "" {
		t.Errorf("undeclared closure digest = %q, want empty", got)
	}
}

// TestInvocation_ArmPublishesANonSecretLease pins what arming writes to disk:
// the crash-recovery record, and nothing a credential could travel in.
func TestInvocation_ArmPublishesANonSecretLease(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	runtime := newInvocationRuntime(fixture.ws, fixture.planned)
	if runtime == nil {
		t.Fatal("the fixture plan declares a finalizer")
	}
	t.Cleanup(func() {
		for _, rel := range runtime.relations {
			rel.discard()
		}
	})

	reaped, err := runtime.arm(fixture.producer, "producing-action-digest")
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if len(reaped) != 0 {
		t.Errorf("a fresh workspace reaped %+v", reaped)
	}

	leases := runtime.leases()
	if len(leases) != 1 {
		t.Fatalf("leases = %+v, want exactly one", leases)
	}
	lease := leases[0]
	if lease.Provider != "@acme/provider" || lease.ActionDigest != "producing-action-digest" ||
		lease.PID != os.Getpid() || lease.ID != fixture.producer.InvocationLocator().ID {
		t.Fatalf("lease = %+v", lease)
	}

	// Arming also RESERVED the declared sensitive artifact, so the producer
	// cannot create it at the process umask.
	artifact := filepath.Join(fixture.producer.InvocationLocator().ArtifactRoot, "database", "dsn.env")
	info, err := os.Stat(artifact)
	if err != nil {
		t.Fatalf("sensitive artifact was not reserved: %v", err)
	}
	assertPrivateArtifact(t, artifact, info)

	// Arming is exactly-once: a second call must not replace the private tree
	// the producer is already writing into.
	if _, err := runtime.arm(fixture.producer, "producing-action-digest"); err != nil {
		t.Fatalf("second arm: %v", err)
	}
	if again := runtime.leases(); len(again) != 1 || again[0].ID != lease.ID {
		t.Errorf("re-arming replaced the invocation: %+v", again)
	}
}

// TestInvocation_FinalizerFailureIsReportedWithoutChangingTheOutcome pins the
// two halves of `sensitive.finalizer_failed`: a leak is never swallowed, and a
// finalizer can neither rescue nor condemn the work it tears down.
func TestInvocation_FinalizerFailureIsReportedWithoutChangingTheOutcome(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	fixtureTask(t, fixture.finalizer.JobDef, fixtureScript{
		{"append", "x", fixture.teardowns},
		{"stderr", "cannot reach the provider"},
		{"exit", "6"},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := fixture.run(t, ctx, SchedulerConfig{MaxParallel: 2, NoCache: true}, nil)

	got := result.Results[fixture.finalizer.Key()]
	if got == nil || got.Status != "failed" {
		t.Fatalf("finalizer result = %+v, want failed", got)
	}
	if got.Error == nil || got.Error.Code != extensionproto.FailureSensitiveFinalizerFailed {
		t.Fatalf("finalizer diagnostic = %+v, want %s", got.Error, extensionproto.FailureSensitiveFinalizerFailed)
	}
	if !result.Success {
		t.Error("a failed finalizer changed the invocation's outcome")
	}
	if result.Session.Status.Failed != 0 {
		t.Errorf("failed task count = %d; the finalizer must not vote on the verdict", result.Session.Status.Failed)
	}
	if got := fixture.teardownCount(t); got != 1 {
		t.Errorf("finalizer ran %d times, want exactly once", got)
	}
}

// TestResolveInvocationRelations_DropsAnIncompleteFrontier pins the resolution
// rule: a relation whose producer or consumer frontier is not in the plan is not
// half-wired, because a finalizer that cannot see its whole frontier has no
// defined moment to run at.
func TestResolveInvocationRelations_DropsAnIncompleteFrontier(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)

	complete := resolveInvocationRelations(fixture.planned)
	if len(complete) != 1 {
		t.Fatalf("resolved %d relations, want 1", len(complete))
	}
	if complete[0].producer != fixture.producer || complete[0].finalizer != fixture.finalizer ||
		len(complete[0].consumers) != 1 || complete[0].consumers[0] != fixture.consumer {
		t.Fatalf("relation resolved onto the wrong nodes: %+v", complete[0])
	}
	if fixture.consumer.InvocationProducer != fixture.producer {
		t.Error("the consumer's producing action was not stamped for cache-key derivation")
	}

	withoutConsumer := []*ScheduledJob{fixture.producer, fixture.finalizer}
	if got := resolveInvocationRelations(withoutConsumer); len(got) != 0 {
		t.Errorf("resolved %d relations with an absent consumer, want none", len(got))
	}
	withoutProducer := []*ScheduledJob{fixture.consumer, fixture.finalizer}
	if got := resolveInvocationRelations(withoutProducer); len(got) != 0 {
		t.Errorf("resolved %d relations with an absent producer, want none", len(got))
	}
}

// TestSchedulableJobs_HoldsFinalizersOutOfTheDAG pins the reason a finalizer is
// never dispatched: it has no dependsOn by contract, so a DAG would run it
// first.
func TestSchedulableJobs_HoldsFinalizersOutOfTheDAG(t *testing.T) {
	t.Parallel()
	fixture := newInvocationFixture(t, provisionBody, consumeBody)
	schedulable := schedulableJobs(fixture.planned)
	if len(schedulable) != 2 {
		t.Fatalf("schedulable = %d jobs, want 2", len(schedulable))
	}
	for _, job := range schedulable {
		if job == fixture.finalizer {
			t.Fatal("a runOn:finally step reached the dependency DAG")
		}
	}
	plain := []*ScheduledJob{fixture.producer, fixture.consumer}
	if got := schedulableJobs(plain); len(got) != 2 {
		t.Errorf("a plan with no finalizer must be returned unchanged, got %d", len(got))
	}
}

func TestRedactSensitive_ReplacesEveryOccurrenceAndReports(t *testing.T) {
	t.Parallel()
	needles := []string{"/private/inv-1/dsn.env", fixtureSecret}
	text := "connecting with " + fixtureSecret + " from /private/inv-1/dsn.env and " + fixtureSecret
	got, leaked := redactSensitive(text, needles)
	if !leaked {
		t.Fatal("a leaked value was not reported")
	}
	if strings.Contains(got, fixtureSecret) || strings.Contains(got, "/private/inv-1/dsn.env") {
		t.Fatalf("redacted text still carries the value: %q", got)
	}
	if _, leaked := redactSensitive("nothing to see", needles); leaked {
		t.Error("clean text reported a leak")
	}
	if _, leaked := redactSensitive(fixtureSecret, nil); leaked {
		t.Error("a relation with no sensitive artifact reported a leak")
	}
}

// TestInvocation_LeakGuardLeavesDeclaredArtifactPathsPublishable covers both
// shapes. First: the TypeScript test task's JUnit path repeats a JSON
// leaf from the sensitive bindings artifact, and that leaf is safe in the
// declared JUnit artifact's path. Second: an artifact value equal to the
// project's own name or path never becomes a needle at all — workspace
// structure appears in ordinary output by design ("Tests passed for
// <project>"), so needling it turns every green run into a leak verdict. The
// exact private locator and every credential-derived needle must still fail
// closed on all other event surfaces.
func TestInvocation_LeakGuardLeavesDeclaredArtifactPathsPublishable(t *testing.T) {
	t.Parallel()
	const projectPath = "surfaces/workloads/marketing"
	scratch, err := store.NewInvocationScratch(t.TempDir(), "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	sensitiveArtifactPath, err := scratch.Prepare("database/bindings.json", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	document := `{"schema":"` + projectPath + `","app":"marketing","engine":"postgres","dsn":"` + fixtureSecret + `"}`
	if err := os.WriteFile(sensitiveArtifactPath, []byte(document), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/" + projectPath, Path: projectPath},
		Extension: &extension.ExtensionDescription{Tasks: map[string]extension.TaskDefinition{
			"test-run": {Declares: &extension.TaskDeclaration{Outputs: map[string]extension.DeclaredOutput{
				"junit": {
					Kind: extension.OutputKindFile,
					Root: extension.OutputRootCommandOutput,
					Path: "results.junit.xml",
				},
			}}},
		}},
		JobDef: &extension.JobDefinition{Name: "test~test"},
		Step:   &extension.PipelineStep{Task: "test-run"},
	}
	needles := sensitiveNeedles(scratch, "database/bindings.json",
		(&invocationRelation{producer: job}).publicStructure())
	for _, identifier := range []string{projectPath, "marketing"} {
		if containsNeedle(needles, identifier) {
			t.Fatalf("needles %v carry the workspace identifier %q — structure is not payload", needles, identifier)
		}
	}
	if !containsNeedle(needles, fixtureSecret) {
		t.Fatalf("needles %v are missing the credential leaf %q", needles, fixtureSecret)
	}
	runtime := &invocationRuntime{participants: map[string]*invocationRelation{
		job.Key(): {needles: needles},
	}}

	for _, junitPath := range []string{
		projectPath + "/test/results.junit.xml",
		".putnami/out/" + projectPath + "/test/results.junit.xml",
	} {
		junitEvent, ok := parseRawEvent(`{"v":2,"type":"artifact","id":"junit","name":"JUnit Report","kind":"report","path":"` + junitPath + `"}`)
		if !ok {
			t.Fatal("parse JUnit artifact event")
		}
		publishable := runtime.guardResult(job, &JobResult{Status: "success", Events: []RawJobEvent{junitEvent}})
		if publishable.Status != "success" {
			t.Fatalf("JUnit artifact result = %+v, want publishable success", publishable)
		}
		if got := publishable.Events[0].Data["path"]; got != junitPath {
			t.Errorf("JUnit path = %q, want unchanged %q", got, junitPath)
		}
	}

	// The extension's own summary line carries the project name by design
	// (putnami-ts test.go: `"Tests passed for "+ctx.Project.Name`). Workspace
	// structure must never be a needle, or every green run of a project whose
	// test env mentions its own name becomes a leak verdict.
	summaryEvent := RawJobEvent{Type: EventTypeLog, Message: "Tests passed for " + projectPath}
	summary := runtime.guardResult(job, &JobResult{Status: "success", Events: []RawJobEvent{summaryEvent}})
	if summary.Status != "success" {
		t.Fatalf("project-name summary log = %+v, want publishable success", summary)
	}
	if summary.Events[0].Message != summaryEvent.Message {
		t.Errorf("summary log = %q, want unredacted %q", summary.Events[0].Message, summaryEvent.Message)
	}

	// A driver stack trace names its own package — which is also the artifact's
	// engine value. Structural vocabulary must not needle.
	stackEvent := RawJobEvent{Type: EventTypeLog,
		Message: "at ErrorResponse (/ws/node_modules/.bun/postgres@3.4.9/node_modules/postgres/src/connection.js:815:30)"}
	stack := runtime.guardResult(job, &JobResult{Status: "success", Events: []RawJobEvent{stackEvent}})
	if stack.Status != "success" {
		t.Fatalf("driver stack-trace log = %+v, want publishable success", stack)
	}
	if stack.Events[0].Message != stackEvent.Message {
		t.Errorf("stack log = %q, want unredacted %q", stack.Events[0].Message, stackEvent.Message)
	}

	// The locator is encoded as a JSON string: a Windows path's backslashes
	// spliced into the line are invalid escapes, and the event never parses.
	pathEvent, ok := parseRawEvent(`{"v":2,"type":"artifact","id":"bindings","path":` + jsonString(t, sensitiveArtifactPath) + `}`)
	if !ok {
		t.Fatal("parse sensitive artifact event")
	}
	declaredIDPathEvent, ok := parseRawEvent(`{"v":2,"type":"artifact","id":"junit","path":` + jsonString(t, sensitiveArtifactPath) + `}`)
	if !ok {
		t.Fatal("parse mismatched declared artifact event")
	}
	for _, tc := range []struct {
		name   string
		event  RawJobEvent
		needle string
	}{
		{name: "exact sensitive artifact locator", event: pathEvent, needle: sensitiveArtifactPath},
		{name: "declared artifact id with sensitive path", event: declaredIDPathEvent, needle: sensitiveArtifactPath},
		{name: "credential bytes", event: RawJobEvent{Type: EventTypeLog, Message: fixtureSecret}, needle: fixtureSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := runtime.guardResult(job, &JobResult{Status: "success", Events: []RawJobEvent{tc.event}})
			if result.Status != "failed" || result.Error == nil || result.Error.Code != extensionproto.FailureSensitiveLeakDetected {
				t.Fatalf("leak result = %+v, want %s", result, extensionproto.FailureSensitiveLeakDetected)
			}
			rendered, err := json.Marshal(result.Events)
			if err != nil {
				t.Fatalf("marshal redacted events: %v", err)
			}
			// rendered is JSON, where the needle appears as encoded string content.
			encodedNeedle := strings.TrimSuffix(strings.TrimPrefix(jsonString(t, tc.needle), `"`), `"`)
			if strings.Contains(string(rendered), encodedNeedle) || !strings.Contains(string(rendered), redactedMarker) {
				t.Errorf("redacted events = %s, want %q withheld", rendered, tc.needle)
			}
		})
	}
}

// jsonString is value encoded as a JSON string literal, quotes included.
func jsonString(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode %q: %v", value, err)
	}
	return string(encoded)
}

func TestSensitiveNeedles_CoversPathAndBytes(t *testing.T) {
	t.Parallel()
	scratch, err := store.NewInvocationScratch(t.TempDir(), "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	path, err := scratch.Prepare("database/dsn.env", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.WriteFile(path, []byte("DATABASE_URL="+fixtureSecret+"\nPGPASSWORD=\"s3kr1t-pw\"\n"), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	needles := sensitiveNeedles(scratch, "database/dsn.env", nil)
	want := []string{path, fixtureSecret, "DATABASE_URL=" + fixtureSecret, "s3kr1t-pw"}
	for _, needle := range want {
		if !containsNeedle(needles, needle) {
			t.Errorf("needles %v are missing %q", needles, needle)
		}
	}
	for _, needle := range needles {
		if len(needle) < sensitiveNeedleMin {
			t.Errorf("needle %q is shorter than the false-positive floor", needle)
		}
	}
}

// TestSensitiveNeedles_CoversAJSONArtifactsLeaves is a repair.
//
// The only real sensitive artifact in this workspace is
// dbtestenv's database/bindings.json — a SINGLE-LINE JSON document. A single
// line is the whole file, so the line rule and the `KEY=value` rule both
// degenerate to the document needle, and a task that prints the password or the
// DSN it just parsed defeats the bytes half of the guard entirely.
func TestSensitiveNeedles_CoversAJSONArtifactsLeaves(t *testing.T) {
	t.Parallel()
	scratch, err := store.NewInvocationScratch(t.TempDir(), "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	path, err := scratch.Prepare("database/bindings.json", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The shape dbtestenv.SynthesizeBinding writes: one line, structured
	// connection, plus a DSN-carrying member for the url half of the rule.
	const password = "s3kr1t-local-password"
	const dsn = "postgres://putnami:" + password + "@127.0.0.1:54321/putnami_test"
	document := `{"protocolVersion":1,"mode":"auto","databases":{"default":{"engine":"postgres",` +
		`"schema":"public","connection":{"host":"127.0.0.1","port":54321,"user":"putnami",` +
		`"password":"` + password + `","database":"putnami_test","dsn":"` + dsn + `"}}}}`
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	needles := sensitiveNeedles(scratch, "database/bindings.json", nil)
	for _, want := range []string{path, document, password, dsn} {
		if !containsNeedle(needles, want) {
			t.Errorf("needles %v are missing %q", needles, want)
		}
	}
	// Member NAMES are structure, not payload: matching them would fail a task
	// for printing a field name. Values under STRUCTURAL names are the same
	// vocabulary — an engine or database name appears in node_modules stack
	// frames and migration logs by design — so only
	// credential-named members and userinfo-bearing URLs mint byte needles.
	for _, name := range []string{"protocolVersion", "connection", "databases", "putnami_test", "postgres"} {
		if containsNeedle(needles, name) {
			t.Errorf("needle set carries the structural vocabulary %q", name)
		}
	}
	for _, needle := range needles {
		if len(needle) < sensitiveNeedleMin {
			t.Errorf("needle %q is shorter than the false-positive floor", needle)
		}
	}

	// The guard is one matcher, so what the needles cover is what a leak is: a
	// task printing only the password loses control of it and must fail.
	if _, leaked := redactSensitive("connected as putnami/"+password, needles); !leaked {
		t.Error("a task printing only the password defeated the bytes half of the guard")
	}
	if _, leaked := redactSensitive("connected to "+dsn, needles); !leaked {
		t.Error("a task printing only the DSN defeated the bytes half of the guard")
	}
}

// --- the sample cap bounds the READ, not the guard ---

// captureInvocationWarnings redirects the default logger for one test and returns what it
// was written. slog is the channel the surrounding scheduler degrades on
// (remote.go, task_capture.go), so it is where a partially covered artifact has
// to become visible.
func captureInvocationWarnings(t *testing.T) func() string {
	t.Helper()
	var logs strings.Builder
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return logs.String
}

func newNeedleScratch(t *testing.T) *store.InvocationScratch {
	t.Helper()
	scratch, err := store.NewInvocationScratch(t.TempDir(), "@acme/provider", "digest")
	if err != nil {
		t.Fatalf("NewInvocationScratch: %v", err)
	}
	t.Cleanup(func() { _ = scratch.Discard() })
	return scratch
}

// TestSensitiveNeedles_CoverAnArtifactLargerThanTheSampleCap covers the case
// where an artifact exceeds the sample cap.
//
// A sensitive artifact over sensitiveSampleCap used to yield its PATH and
// nothing else, so every value inside it — the DSN a consumer connects with,
// the key it loads — was absent from the needle set and reached the live event
// stream and the stored result verbatim, with the task reported green. A PEM
// chain plus its key clears 64 KiB without difficulty, and `cert` is
// credential vocabulary, so this is the shape the cap was silently dropping.
func TestSensitiveNeedles_CoverAnArtifactLargerThanTheSampleCap(t *testing.T) {
	scratch := newNeedleScratch(t)
	path, err := scratch.Prepare("database/creds.env", true)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	const password = "l4rge-artifact-password"
	const dsn = "postgres://putnami:" + password + "@127.0.0.1:54321/putnami_test"
	const armor = "-----BEGIN CERTIFICATE-----"
	var document strings.Builder
	document.WriteString("DATABASE_URL=" + dsn + "\n")
	document.WriteString(armor + "\n")
	for i := 0; document.Len() < 3*sensitiveSampleCap; i++ {
		fmt.Fprintf(&document, "certbody%06dAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\n", i)
	}
	document.WriteString("-----END CERTIFICATE-----\n")
	if err := os.WriteFile(path, []byte(document.String()), 0o600); err != nil {
		t.Fatalf("write artifact: %v", err)
	}

	logs := captureInvocationWarnings(t)
	needles := sensitiveNeedles(scratch, "database/creds.env", nil)

	for _, want := range []string{path, "DATABASE_URL=" + dsn, dsn, password} {
		if !containsNeedle(needles, want) {
			t.Errorf("needles are missing %q — a value past the cap is still a value", want)
		}
	}
	// The whole-document needle is the ONE the cap legitimately drops: the guard
	// never read the document, so a needle equal to its prefix matches nothing a
	// task could print except by accident.
	if containsNeedle(needles, strings.TrimSpace(document.String())) {
		t.Error("a truncated sample must not mint a whole-document needle")
	}
	// PEM armor is the document's shape, identical in every certificate ever
	// written: shared vocabulary cannot distinguish a
	// leak from a green run.
	if containsNeedle(needles, armor) {
		t.Error("PEM armor became a needle; every unrelated certificate would trip the guard")
	}
	if _, leaked := redactSensitive("connected to "+dsn, needles); !leaked {
		t.Error("a task echoing the DSN embedded in a large artifact was not detected")
	}

	// Fail VISIBLY: a guard that could not cover an artifact says which one.
	warning := logs()
	if !strings.Contains(warning, "leak guard could not fully sample") ||
		!strings.Contains(warning, "database/creds.env") {
		t.Errorf("the degradation was silent or did not name the artifact: %s", warning)
	}
	// ...and says it without publishing the very thing it is protecting.
	if strings.Contains(warning, path) || strings.Contains(warning, password) {
		t.Errorf("the warning carried the artifact's private path or bytes: %s", warning)
	}
}

// A truncated read that ends exactly on a newline still contains that complete
// line. Trimming the sample before locating the last newline erased the
// delimiter and misclassified the fully-read line as a partial record, leaving
// a credential at the budget boundary out of the guard.
func TestNeedleCollector_TruncatedSampleKeepsBoundaryLine(t *testing.T) {
	t.Parallel()
	const secret = "boundary-secret-value"
	const secretLine = "TOKEN=" + secret + "\n"
	prefixLen := sensitiveSampleCap - len(secretLine)
	if prefixLen < 2 {
		t.Fatal("test fixture does not leave room for a prefix")
	}
	sample := strings.Repeat("x", prefixLen-1) + "\n" + secretLine
	if len(sample) != sensitiveSampleCap {
		t.Fatalf("sample length = %d, want %d", len(sample), sensitiveSampleCap)
	}

	collector := &needleCollector{seen: make(map[string]bool)}
	collector.derive(sample, true)
	if !containsNeedle(collector.needles, secret) {
		t.Fatalf("needles missed the fully sampled credential at the truncation boundary")
	}
}

// TestSensitiveNeedles_CoverADirectoryArtifact covers a directory artifact.
//
// store.InvocationScratch.Harden already walks and chmods a sensitive artifact
// that is a TREE, so `{"kind":"directory","sensitive":true}` is a shape a
// provider may declare — a generated credentials tree of ca.pem, client.key and
// env. The guard used to see info.IsDir() and return the directory path alone,
// so a consumer that printed `cat $CREDS/env` matched no needle at all.
func TestSensitiveNeedles_CoverADirectoryArtifact(t *testing.T) {
	scratch := newNeedleScratch(t)
	root, err := scratch.Resolve("database/creds")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "certs"), 0o700); err != nil {
		t.Fatalf("create artifact tree: %v", err)
	}
	const password = "tree-artifact-password"
	const dsn = "postgres://putnami:" + password + "@127.0.0.1:54321/putnami_test"
	const key = "MIIEowIBAAKCAQEAtreeprivatekeymaterial0123456789abcdef"
	writeTreeFile(t, filepath.Join(root, "env"), "DATABASE_URL="+dsn+"\n")
	writeTreeFile(t, filepath.Join(root, "certs", "client.key"),
		"-----BEGIN PRIVATE KEY-----\n"+key+"\n-----END PRIVATE KEY-----\n")

	logs := captureInvocationWarnings(t)
	needles := sensitiveNeedles(scratch, "database/creds", nil)

	for _, want := range []string{root, dsn, password, key} {
		if !containsNeedle(needles, want) {
			t.Errorf("needles %v are missing %q from inside the tree", needles, want)
		}
	}
	if _, leaked := redactSensitive("loaded key "+key, needles); !leaked {
		t.Error("a task echoing a key from inside a sensitive tree was not detected")
	}
	// A contained file's PATH needs no needle of its own: it carries the tree's
	// path as a prefix, which is already one.
	if _, leaked := redactSensitive("cat "+filepath.Join(root, "env"), needles); !leaked {
		t.Error("a task echoing a path inside the sensitive tree was not detected")
	}
	// A tree the guard covered completely degrades nowhere, so it says nothing.
	if warning := logs(); warning != "" {
		t.Errorf("a fully sampled tree warned anyway: %s", warning)
	}
}

// TestSensitiveNeedles_KeepStructureScopingForDirectoryNeedles is a
// regression test: the rules that keep the guard from failing GREEN runs are
// keyed on the candidate and on the relation, never on the artifact's declared
// name, so a file inside a directory artifact must go through exactly the same
// scoping a single-file artifact does.
func TestSensitiveNeedles_KeepStructureScopingForDirectoryNeedles(t *testing.T) {
	t.Parallel()
	scratch := newNeedleScratch(t)
	root, err := scratch.Resolve("database/creds")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("create artifact tree: %v", err)
	}
	const password = "scoped-tree-password"
	const dsn = "postgres://putnami:" + password + "@127.0.0.1:54321/putnami_test"
	writeTreeFile(t, filepath.Join(root, "bindings.json"),
		`{"protocolVersion":1,"databases":{"default":{"engine":"postgres","schema":"public",`+
			`"connection":{"host":"127.0.0.1","user":"putnami","password":"`+password+`",`+
			`"database":"putnami_test","dsn":"`+dsn+`"}}}}`)
	// A test env carries the consuming project's own name.
	writeTreeFile(t, filepath.Join(root, "project"), "@acme/api-service\n")

	public := map[string]bool{"@acme/api-service": true, "acme": true, "api-service": true}
	needles := sensitiveNeedles(scratch, "database/creds", public)

	for _, want := range []string{dsn, password} {
		if !containsNeedle(needles, want) {
			t.Errorf("needles %v are missing the credential %q", needles, want)
		}
	}
	// Workspace structure is not payload, whichever file inside the tree
	// happens to spell it.
	if containsNeedle(needles, "@acme/api-service") {
		t.Error("a tree file equal to a participant's own name became a needle")
	}
	// Only credential-named members and userinfo-bearing URLs mint byte
	// needles; structural enumerators stay shared vocabulary.
	for _, structural := range []string{"postgres", "putnami_test", "protocolVersion", "connection", "databases"} {
		if containsNeedle(needles, structural) {
			t.Errorf("a tree file's structural vocabulary %q became a needle", structural)
		}
	}
	if _, leaked := redactSensitive("Tests passed for @acme/api-service", needles); leaked {
		t.Error("an ordinary summary line was read as a leak — this does not hold for tree needles")
	}
}

// TestSensitiveNeedles_ReportEveryBoundThatBites pins the visibility half: each
// bound that stops the walk names the artifact in a warning, and the shared
// budget is spent smallest-file-first so a blob cannot starve the DSN next to
// it purely by lexical order.
func TestSensitiveNeedles_ReportEveryBoundThatBites(t *testing.T) {
	t.Run("byte budget", func(t *testing.T) {
		scratch := newNeedleScratch(t)
		root, err := scratch.Resolve("database/creds")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create artifact tree: %v", err)
		}
		const password = "budget-tree-password"
		const dsn = "postgres://putnami:" + password + "@127.0.0.1:54321/putnami_test"
		// Two blobs sort BEFORE the credential lexically and together exceed the
		// artifact's whole budget.
		for _, name := range []string{"a.blob", "b.blob"} {
			var blob strings.Builder
			for i := 0; blob.Len() < sensitiveSampleCap; i++ {
				fmt.Fprintf(&blob, "blobline%06dBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB\n", i)
			}
			writeTreeFile(t, filepath.Join(root, name), blob.String())
		}
		writeTreeFile(t, filepath.Join(root, "env"), "DATABASE_URL="+dsn+"\n")

		logs := captureInvocationWarnings(t)
		needles := sensitiveNeedles(scratch, "database/creds", nil)

		if !containsNeedle(needles, dsn) || !containsNeedle(needles, password) {
			t.Errorf("needles %v lost the smallest file's credential to two blobs", needles)
		}
		if warning := logs(); !strings.Contains(warning, "byte budget") ||
			!strings.Contains(warning, "database/creds") {
			t.Errorf("an exhausted budget was silent or unattributed: %s", warning)
		}
	})

	t.Run("file count", func(t *testing.T) {
		scratch := newNeedleScratch(t)
		root, err := scratch.Resolve("database/creds")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create artifact tree: %v", err)
		}
		for i := 0; i <= sensitiveTreeFileCap; i++ {
			writeTreeFile(t, filepath.Join(root, fmt.Sprintf("secret-%03d.env", i)),
				fmt.Sprintf("TOKEN=tree-token-value-%03d\n", i))
		}

		logs := captureInvocationWarnings(t)
		needles := sensitiveNeedles(scratch, "database/creds", nil)

		if !containsNeedle(needles, "tree-token-value-000") {
			t.Errorf("needles %v missed the files the guard did sample", needles)
		}
		if warning := logs(); !strings.Contains(warning, "more files than the guard samples") ||
			!strings.Contains(warning, "database/creds") {
			t.Errorf("an exhausted file allowance was silent or unattributed: %s", warning)
		}
	})

	t.Run("depth", func(t *testing.T) {
		scratch := newNeedleScratch(t)
		root, err := scratch.Resolve("database/creds")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		deep := filepath.Join(root, "a", "b", "c", "d", "e")
		if err := os.MkdirAll(deep, 0o700); err != nil {
			t.Fatalf("create artifact tree: %v", err)
		}
		writeTreeFile(t, filepath.Join(deep, "buried.env"), "TOKEN=buried-token-value\n")

		logs := captureInvocationWarnings(t)
		needles := sensitiveNeedles(scratch, "database/creds", nil)

		if containsNeedle(needles, "buried-token-value") {
			t.Error("the guard descended past its depth bound")
		}
		if warning := logs(); !strings.Contains(warning, "deeper than the guard descends") ||
			!strings.Contains(warning, "database/creds") {
			t.Errorf("a truncated descent was silent or unattributed: %s", warning)
		}
	})
}

// TestSensitiveNeedles_DecideSymlinksByPosition pins the deliberate asymmetry.
//
// The DECLARED artifact is followed to a regular file: those bytes are what the
// consumers read through the declared path, so not following would drop the
// needles for the exact value being handed out. A link INSIDE a tree is not
// followed: its target is not the declared artifact, and following it could take
// the guard out of the private root and mint needles from arbitrary files.
func TestSensitiveNeedles_DecideSymlinksByPosition(t *testing.T) {
	t.Run("the declared artifact is followed", func(t *testing.T) {
		scratch := newNeedleScratch(t)
		const dsn = "postgres://putnami:linked-password@127.0.0.1:54321/putnami_test"
		target := filepath.Join(t.TempDir(), "real.env")
		writeTreeFile(t, target, "DATABASE_URL="+dsn+"\n")

		path, err := scratch.Resolve("database/link.env")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("create artifact directory: %v", err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatalf("symlink artifact: %v", err)
		}
		if needles := sensitiveNeedles(scratch, "database/link.env", nil); !containsNeedle(needles, dsn) {
			t.Errorf("needles %v missed the value the declared path hands out", needles)
		}
	})

	t.Run("a link inside the tree is not", func(t *testing.T) {
		scratch := newNeedleScratch(t)
		const outside = "OUTSIDE_TOKEN=outside-the-private-root"
		target := filepath.Join(t.TempDir(), "outside.env")
		writeTreeFile(t, target, outside+"\n")

		root, err := scratch.Resolve("database/creds")
		if err != nil {
			t.Fatalf("Resolve: %v", err)
		}
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("create artifact tree: %v", err)
		}
		if err := os.Symlink(target, filepath.Join(root, "escape.env")); err != nil {
			t.Fatalf("symlink inside tree: %v", err)
		}

		logs := captureInvocationWarnings(t)
		needles := sensitiveNeedles(scratch, "database/creds", nil)
		if containsNeedle(needles, outside) {
			t.Error("the walk followed a symlink out of the private root")
		}
		if warning := logs(); !strings.Contains(warning, "symlink inside the artifact tree") ||
			!strings.Contains(warning, "database/creds") {
			t.Errorf("a skipped symlink was silent or unattributed: %s", warning)
		}
	})
}

func writeTreeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// TestUrlUserinfoPassword_OnlyMatchesURLs pins that the userinfo rule cannot
// invent a credential out of ordinary text: url.Parse accepts almost anything,
// so the scheme check is what keeps the guard from failing tasks at random.
func TestUrlUserinfoPassword_OnlyMatchesURLs(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"postgres://user:hunter2@host:5432/db": "hunter2",
		"postgres://user@host:5432/db":         "",
		"host:5432/db":                         "",
		"user:hunter2@host":                    "",
		"":                                     "",
	}
	for candidate, want := range cases {
		if got := urlUserinfoPassword(candidate); got != want {
			t.Errorf("urlUserinfoPassword(%q) = %q, want %q", candidate, got, want)
		}
	}
}

func TestFinalizerCleanupContext_IsIndependentOnlyAfterCancellation(t *testing.T) {
	t.Parallel()
	job := &ScheduledJob{JobDef: &extension.JobDefinition{TimeoutMs: 1_000}}

	live, cancelLive := context.WithCancel(context.Background())
	defer cancelLive()
	shared, release := finalizerCleanupContext(live, job)
	release()
	if shared != live {
		t.Error("a live run's finalizer must share the run's cancellation")
	}

	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()
	cleanup, releaseCleanup := finalizerCleanupContext(dead, job)
	defer releaseCleanup()
	if cleanup.Err() != nil {
		t.Fatal("cleanup after cancellation inherited the cancellation that triggered it")
	}
	deadline, ok := cleanup.Deadline()
	if !ok || time.Until(deadline) > 2*time.Second {
		t.Errorf("cleanup context deadline = %v (ok=%v), want the job's bounded budget", deadline, ok)
	}
}

func containsNeedle(needles []string, want string) bool {
	for _, needle := range needles {
		if needle == want {
			return true
		}
	}
	return false
}

// --- crash recovery: SIGKILL, then the next invocation ---
//
// An earlier stage proved the machinery in isolation: a lease survives a kill and
// store.ReapOrphanInvocations removes the orphan. This is where a real PROVIDER
// arrives, so this is the case that has to hold end to end — a killed run left a
// live credential on disk, and the NEXT run must remove it before it provisions
// anything of its own, without any surface ever naming what it removed.

const (
	// killedProviderRootEnv hands the child the workspace root to claim an
	// invocation under; its presence is also what turns the helper test on.
	killedProviderRootEnv = "PUTNAMI_TEST_KILLED_PROVIDER_ROOT"
	// killedProviderHandshakeEnv is the file the child publishes its invocation
	// id to, so the parent can kill it at a known point.
	killedProviderHandshakeEnv = "PUTNAMI_TEST_KILLED_PROVIDER_HANDSHAKE"
)

// killedSecret is the credential the KILLED run leaked onto disk. It is
// deliberately different from fixtureSecret so an assertion can tell which run's
// value it found.
const killedSecret = "postgres://killed:0rph4ned@127.0.0.1:5432/db"

// TestKilledProviderHoldsUntilKilled is the child half. It claims an invocation
// under the parent's workspace, writes a credential into the private tree
// exactly as a database provisioner would, publishes its id, and blocks — with
// no cleanup path at all, because a SIGKILL would not run one anyway.
func TestKilledProviderHoldsUntilKilled(t *testing.T) {
	t.Parallel()
	root := os.Getenv(killedProviderRootEnv)
	if root == "" {
		t.Skip("child-only helper; driven by TestInvocation_KilledProviderIsReapedBeforeTheNextProvisions")
	}
	scratch, err := store.NewInvocationScratch(root, "@acme/provider", "killed-action-digest")
	if err != nil {
		t.Fatalf("child NewInvocationScratch: %v", err)
	}
	credential, err := scratch.Prepare("database/dsn.env", true)
	if err != nil {
		t.Fatalf("child reserve credential: %v", err)
	}
	if err := os.WriteFile(credential, []byte("DATABASE_URL="+killedSecret+"\n"), 0o600); err != nil {
		t.Fatalf("child write credential: %v", err)
	}
	if err := os.WriteFile(os.Getenv(killedProviderHandshakeEnv), []byte(scratch.ID()), 0o600); err != nil {
		t.Fatalf("child handshake: %v", err)
	}
	time.Sleep(time.Minute)
}

// startAndKillProvider runs the child helper against root, waits for it to claim
// an invocation, SIGKILLs it, and returns the orphan's id and directory.
func startAndKillProvider(t *testing.T, root string) (string, string) {
	t.Helper()
	handshake := filepath.Join(t.TempDir(), "child-invocation-id")

	child := exec.Command(os.Args[0], "-test.run=^TestKilledProviderHoldsUntilKilled$", "-test.timeout=120s")
	child.Env = append(os.Environ(),
		killedProviderRootEnv+"="+root,
		killedProviderHandshakeEnv+"="+handshake,
	)
	if err := child.Start(); err != nil {
		t.Fatalf("start the provider that will be killed: %v", err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })

	deadline := time.Now().Add(60 * time.Second)
	var id string
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(handshake); err == nil && len(data) > 0 {
			id = string(data)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("the child never claimed an invocation")
	}

	if err := child.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL the owning process: %v", err)
	}
	_ = child.Wait()

	dir := filepath.Join(store.InvocationsRoot(root), id)
	// The kill must leave the whole tree behind — that is the state recovery has
	// to deal with, and a test that silently lost it would prove nothing.
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "database", "dsn.env")); err != nil {
		t.Fatalf("the killed run's credential did not survive the kill: %v", err)
	}
	return id, dir
}

// TestInvocation_KilledProviderIsReapedBeforeTheNextProvisions is the slice's
// real crash test.
//
// It pins four things at once, and each one is a different failure:
//
//   - RECOVERY HAPPENS. The killed run's private tree — credential included — is
//     gone after the next invocation, without a GC interval or a timer.
//   - RECOVERY PRECEDES PROVISIONING. The provisioning task itself records what
//     the invocations directory held at the moment it ran, and the orphan is
//     already absent from that listing. Reaping afterwards would leave a run
//     racing its predecessor's leftovers.
//   - IT IS REPORTED. The recovery surfaces as an `invocation:reaped` session
//     record carrying the typed recovery code, so an operator can tell a
//     reclaimed orphan from a resource that never existed.
//   - NOTHING LEAKS. Neither the killed run's credential nor this run's appears
//     in any published surface: the reaped lease, the session records, the task
//     results, their events, or the recorded renderer rows.
func TestInvocation_KilledProviderIsReapedBeforeTheNextProvisions(t *testing.T) {
	// The provisioning script records the invocations directory it observed, so
	// the ordering assertion reads what the task saw rather than what the test
	// assumes. $2 is {invocationArtifactRoot} = <root>/.putnami/invocations/<id>/artifacts.
	observingProvisionBody := fixtureScript{{"observe-invocations"}}.then(provisionBody)
	fixture := newInvocationFixture(t, observingProvisionBody, consumeBody)

	orphanID, orphanDir := startAndKillProvider(t, fixture.ws.Root)

	var records []SessionRecord
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	scheduler := newScheduler(fixture.ws, fixture.planned, nil,
		SchedulerConfig{MaxParallel: 2, NoCache: true}, &mockRenderer{}, nil)
	scheduler.setSessionEventHandler(func(record SessionRecord) {
		mu.Lock()
		records = append(records, record)
		mu.Unlock()
	})
	result := scheduler.Run(ctx)

	if got := result.Results[fixture.consumer.Key()]; got == nil || got.Status != "success" {
		t.Fatalf("consumer result = %+v, want success", got)
	}

	// RECOVERY HAPPENS.
	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Errorf("the killed run's private tree survived the next invocation: %v", err)
	}

	// RECOVERY PRECEDES PROVISIONING.
	observed, err := os.ReadFile(fixture.observed)
	if err != nil {
		t.Fatalf("the provisioning task recorded nothing: %v", err)
	}
	// The listing must name THIS run's own invocation, or the absence of the
	// orphan below would prove only that the probe read the wrong directory.
	live := fixture.producer.InvocationLocator()
	if live == nil || !strings.Contains(string(observed), live.ID) {
		t.Fatalf("the observed listing does not contain the live invocation %+v:\n%s", live, observed)
	}
	if strings.Contains(string(observed), orphanID) {
		t.Errorf("the orphan %s was still present when provisioning ran:\n%s", orphanID, observed)
	}

	// IT IS REPORTED.
	mu.Lock()
	defer mu.Unlock()
	var reaped []SessionRecord
	for _, record := range records {
		if record.Type == "invocation:reaped" {
			reaped = append(reaped, record)
		}
	}
	if len(reaped) != 1 {
		t.Fatalf("invocation:reaped records = %d, want exactly one for the killed run", len(reaped))
	}
	if got := reaped[0].Data["invocationId"]; got != orphanID {
		t.Errorf("reaped invocationId = %v, want the killed run's %s", got, orphanID)
	}
	if got := reaped[0].Data["code"]; got != extensionproto.RecoverySensitiveLeaseReaped {
		t.Errorf("recovery code = %v, want %s", got, extensionproto.RecoverySensitiveLeaseReaped)
	}
	if got := reaped[0].Data["actionDigest"]; got != "killed-action-digest" {
		t.Errorf("reaped actionDigest = %v, want the killed producing action", got)
	}

	// NOTHING LEAKS — every published surface, against BOTH credentials.
	for _, surface := range publishedSurfaces(t, records, result) {
		for _, secret := range []string{killedSecret, fixtureSecret} {
			if strings.Contains(surface.text, secret) {
				t.Errorf("%s leaked a credential: %s", surface.name, surface.text)
			}
		}
	}
}

// publishedSurface is one rendered surface plus the name a failure reports it by.
type publishedSurface struct {
	name string
	text string
}

// publishedSurfaces renders every place a run's facts end up: the session
// records (events.jsonl), the canonical result model (task results, their
// errors, their events, the reduction the exit code and every renderer read),
// and the reaped leases inside those records.
func publishedSurfaces(t *testing.T, records []SessionRecord, result *SchedulerResult) []publishedSurface {
	t.Helper()
	render := func(name string, value any) publishedSurface {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("render %s: %v", name, err)
		}
		return publishedSurface{name: name, text: string(data)}
	}
	surfaces := []publishedSurface{
		render("session records", records),
		render("task results", result.Results),
		render("canonical session", result.Session),
	}
	for key, jobResult := range result.Results {
		surfaces = append(surfaces, render("result events of "+key, jobResult.Events))
	}
	return surfaces
}

// assertPrivateArtifact checks that only the current user may access a reserved
// sensitive artifact: mode 0600 on Unix, and on Windows, where mode bits
// restrict nobody, an access list that admits the current user alone.
func assertPrivateArtifact(t *testing.T, path string, info os.FileInfo) {
	t.Helper()
	if runtime.GOOS != "windows" {
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("reserved sensitive artifact mode = %04o, want 0600", got)
		}
		return
	}
	private, err := ownerperm.OwnerOnly(path)
	if err != nil {
		t.Fatalf("OwnerOnly(%s): %v", path, err)
	}
	if !private {
		t.Errorf("reserved sensitive artifact %s admits more than the current user", path)
	}
}
