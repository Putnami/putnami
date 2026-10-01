package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	distribution "go.putnami.dev/protocol/distribution"
	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func writeCapabilityCommand(t *testing.T, root, name, script string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write capability probe %s: %v", name, err)
	}
	return path
}

// capabilityJobTimeoutMs is the wall-clock budget every job in a capability
// test runs under: none.
//
// These tests assert a security contract — an adapter cannot arm a capability,
// an ordinary job never receives the bearer, an echoed bearer fails closed —
// and each probe forks /bin/sh to print one JSON line, microseconds of work.
// The helper used to hardcode a 5 s budget. A budget is wall clock, so on a
// loaded machine the fork alone could miss it: one measured run spent
// 5.003 s of wall clock for 578 µs of CPU, and the timed-out job reported
// status "failed" — the same value a real capability leak reports. That let
// machine load decide a security verdict, and a green run proved the contract
// while a red one proved nothing. Raising the number only moves the load at
// which the verdict flips, so the budget is removed instead of enlarged.
//
// A negative timeout is the manifest's own "unbounded" sentinel, resolved by
// ScheduledJob.EffectiveTimeoutMs and honored by every path that applies a
// budget: RunJob installs no deadline, applyTaskDeadline removes the deadline
// variable from the child environment, and batchGroupTimeoutMs keeps a group
// unbounded. No wall clock participates, so JobResult.TimedOut cannot be set on
// these probes. The Go test binary's own timeout remains the backstop, and it
// fails the package with a stack dump no assertion can read as a leak.
const capabilityJobTimeoutMs = -1

// capabilityResultDetail renders the fields that CLASSIFY a job result. Status
// alone cannot: a scheduler deadline, a spawn failure and a detected bearer
// leak all report "failed", so an assertion printing only the status leaves a
// reader unable to tell a host accident from a security regression. Every
// assertion in this file that can observe a job result prints this instead, so
// one run is enough to classify a failure.
func capabilityResultDetail(result *JobResult) string {
	if result == nil {
		return "<no result>"
	}
	detail := fmt.Sprintf(
		"status=%q timedOut=%t exitCode=%d duration=%s data=%v",
		result.Status, result.TimedOut, result.ExitCode, result.Duration, result.Data,
	)
	if result.Error == nil {
		return detail + " error=<nil>"
	}
	return detail + fmt.Sprintf(" error={message:%q code:%q}", result.Error.Message, result.Error.Code)
}

// capabilityExecutionFault names the HOST-EXECUTION outcome a job result
// carries, or "" when the result is a capability verdict a test may assert on.
// A deadline and a cancellation each keep an outcome of their own, so neither
// can stand in for a capability leak whatever the shared "failed" status says.
func capabilityExecutionFault(result *JobResult) string {
	switch {
	case result == nil:
		return "no result was recorded"
	case result.TimedOut:
		return "the job hit its scheduler deadline, an outcome of host load and not of any capability"
	case result.Status == "canceled":
		return "the job was canceled, so it never reported on its capabilities"
	default:
		return ""
	}
}

// capabilityJobResult returns the settled result of key, failing the test
// before any capability assertion reads it when the run produced an execution
// fault instead of a verdict.
func capabilityJobResult(t *testing.T, key string, result *JobResult) *JobResult {
	t.Helper()
	if fault := capabilityExecutionFault(result); fault != "" {
		t.Fatalf("job %s reported no capability verdict: %s: %s", key, fault, capabilityResultDetail(result))
	}
	return result
}

// capabilityProbe makes each job run script (fixtureTask). A test calls it once
// it has set the jobs' environment, and before it authorizes the plan.
func capabilityProbe(t *testing.T, script fixtureScript, jobs ...*ScheduledJob) {
	t.Helper()
	for _, job := range jobs {
		fixtureTask(t, job.JobDef, script)
	}
}

// Capability probe scripts: a gate that fails when it sees the bearer or the
// runner's AFTER list, results that always pass, fail or skip, and an effect
// that creates $PUTNAMI_EFFECT_MARKER before it passes.
var (
	unarmedGateProbe = fixtureScript{{"verdict", "unset:" + extensionproto.CloudTokenEnv, "unset:" + extensionproto.CloudCapabilityAfterEnv}}
	passProbe        = fixtureScript{{"print", `{"v":2,"type":"result","data":{"status":"success"}}`}}
	failProbe        = fixtureScript{{"print", `{"v":2,"type":"result","data":{"status":"failed"}}`}}
	skipProbe        = fixtureScript{{"print", `{"v":2,"type":"result","data":{"status":"skipped"}}`}}
	markerEffect     = fixtureScript{{"append-env", "", "$PUTNAMI_EFFECT_MARKER"}}.then(passProbe)
)

func capabilityTestJob(
	root string,
	project *workspace.Project,
	name string,
	commandName string,
	command string,
	sideEffects string,
	dependsOn ...string,
) *ScheduledJob {
	return &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: root},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          name,
			CommandName:   commandName,
			Kind:          "command",
			Command:       command,
			TimeoutMs:     capabilityJobTimeoutMs,
			Traits:        extension.CommandTraits{SideEffects: sideEffects},
		},
		DependsOn: append([]string(nil), dependsOn...),
	}
}

func withDeclaredCapabilityEffect(job *ScheduledJob, effect string) *ScheduledJob {
	job.Step = &extension.PipelineStep{Task: "effect-task"}
	job.Extension.Tasks = map[string]extension.TaskDefinition{
		"effect-task": {Declares: &extension.TaskDeclaration{Effects: []string{effect}}},
	}
	return job
}

func capturedCapabilityContext(t *testing.T, token, after string) context.Context {
	t.Helper()
	t.Setenv(extensionproto.CloudTokenEnv, token)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, after)
	return CaptureProcessCapabilities(context.Background())
}

// TestCapabilityProbesKeepHostLoadOutOfTheSecurityVerdict pins two guarantees
// together: the probes carry no wall-clock budget, and a result that
// did come from a deadline is classified as one rather than read as a leak.
func TestCapabilityProbesKeepHostLoadOutOfTheSecurityVerdict(t *testing.T) {
	job := capabilityTestJob(
		t.TempDir(),
		&workspace.Project{ID: "/site", Name: "site", Path: "."},
		"publish~push",
		"publish",
		"/unused",
		extensionproto.SideEffectsRegistry,
	)
	if got := job.EffectiveTimeoutMs(); got >= 0 {
		t.Fatalf("capability probe deadline = %d ms, want the unbounded sentinel so no wall clock decides a capability verdict", got)
	}
	prefix := extensionproto.TaskDeadlineMsEnv + "="
	for _, entry := range applyTaskDeadline([]string{"PATH=/bin"}, job) {
		if strings.HasPrefix(entry, prefix) {
			t.Fatalf("capability probe exported %q to its child; the probes must run without a deadline", entry)
		}
	}

	leak := &JobResult{Status: "failed", Error: &JobError{
		Message: "sensitive value detected",
		Code:    extensionproto.FailureSensitiveLeakDetected,
	}}
	if fault := capabilityExecutionFault(leak); fault != "" {
		t.Fatalf("a detected leak was classified as the execution fault %q", fault)
	}
	timedOut := &JobResult{Status: "failed", TimedOut: true, Error: &JobError{Message: "job timed out"}}
	if fault := capabilityExecutionFault(timedOut); fault == "" {
		t.Fatal("a timed-out job was accepted as a capability verdict, the exact confusion this guards against")
	}
	if detail := capabilityResultDetail(timedOut); !strings.Contains(detail, "timedOut=true") ||
		!strings.Contains(detail, "job timed out") {
		t.Fatalf("timeout detail = %q, want it to name TimedOut and the job error", detail)
	}
	for name, result := range map[string]*JobResult{
		"canceled": {Status: "canceled"},
		"absent":   nil,
	} {
		if fault := capabilityExecutionFault(result); fault == "" {
			t.Fatalf("%s result was accepted as a capability verdict", name)
		}
	}
}

func TestScheduler_RegistryAndCloudReceiveExactCapabilityOnlyAfterSuccessfulLeaves(t *testing.T) {
	const after = "lint,test,build,validate,validate-workspace"
	for _, sideEffects := range []string{extensionproto.SideEffectsRegistry, extensionproto.SideEffectsCloud} {
		t.Run(sideEffects, func(t *testing.T) {
			root := t.TempDir()
			project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
			plan := make([]*ScheduledJob, 0, 6)
			gateKeys := make([]string, 0, 5)
			for _, commandName := range strings.Split(after, ",") {
				gate := capabilityTestJob(root, project, commandName+"~check", commandName, "", "")
				capabilityProbe(t, unarmedGateProbe, gate)
				plan = append(plan, gate)
				gateKeys = append(gateKeys, gate.Key())
			}
			effect := capabilityTestJob(root, project, "publish~push", "publish", "", sideEffects, gateKeys...)
			plan = append(plan, effect)
			exact := "exact-" + sideEffects + "-capability"
			effect.JobDef.Env = map[string]string{
				extensionproto.CloudTokenEnv:           "repository-value-must-not-win",
				extensionproto.CloudCapabilityAfterEnv: "repository-gate-must-not-reach-job",
				"PUTNAMI_EXPECTED_CLOUD_TOKEN":         exact,
			}
			capabilityProbe(t, fixtureScript{{"verdict",
				"same:" + extensionproto.CloudTokenEnv + "=PUTNAMI_EXPECTED_CLOUD_TOKEN",
				"unset:" + extensionproto.CloudCapabilityAfterEnv}}, effect)
			ctx := capturedCapabilityContext(t, exact, after)
			authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
			if err != nil {
				t.Fatalf("AuthorizeProcessCapabilities: %v", err)
			}
			if authorization == nil {
				t.Fatal("expected an opaque capability authorization")
			}

			result := RunPlan(ctx, RunRequest{
				Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
				Plan:      plan,
				Config: SchedulerConfig{
					MaxParallel: 1,
					NoCache:     true,
				},
				Renderer:                       &mockRenderer{},
				ProcessCapabilityAuthorization: authorization,
			})
			for _, gateKey := range gateKeys {
				if got := capabilityJobResult(t, gateKey, result.Results[gateKey]); got.Status != "success" {
					t.Fatalf("gate %s result: %s, want success without capability", gateKey, capabilityResultDetail(got))
				}
			}
			if got := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()]); got.Status != "success" {
				t.Fatalf("effect result: %s, want exact scoped capability", capabilityResultDetail(got))
			}
		})
	}
}

func TestScheduler_ProcessCapabilityEchoIsRedactedFromEveryPublishingSurface(t *testing.T) {
	const token = "adversarial-runtime-capability-bearer"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	gate := capabilityTestJob(root, project, "lint~check", "lint", "", "")
	capabilityProbe(t, fixtureScript{{"print", `{"v":2,"type":"result","status":"success"}`}}, gate)
	effect := capabilityTestJob(root, project, "publish~push", "publish", "", extensionproto.SideEffectsRegistry, gate.Key())
	capabilityProbe(t, fixtureScript{
		{"print-env", `{"v":2,"type":"$PUTNAMI_CLOUD_TOKEN","time":"$PUTNAMI_CLOUD_TOKEN","level":"$PUTNAMI_CLOUD_TOKEN","message":"message:$PUTNAMI_CLOUD_TOKEN","data":{"nested":["$PUTNAMI_CLOUD_TOKEN"]}}`},
		{"print-env", `{"v":2,"type":"result","time":"$PUTNAMI_CLOUD_TOKEN","level":"$PUTNAMI_CLOUD_TOKEN","message":"result:$PUTNAMI_CLOUD_TOKEN","status":"$PUTNAMI_CLOUD_TOKEN","data":{"status":"$PUTNAMI_CLOUD_TOKEN","data":{"leak":"$PUTNAMI_CLOUD_TOKEN"},"error":{"message":"error:$PUTNAMI_CLOUD_TOKEN","code":"$PUTNAMI_CLOUD_TOKEN"}}}`},
	}, effect)
	plan := []*ScheduledJob{gate, effect}
	ctx := capturedCapabilityContext(t, token, "lint")
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	renderer := &mockRenderer{}
	var sessions []SessionRecord
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config:    SchedulerConfig{MaxParallel: 1, NoCache: true, ContinueOnError: true},
		Renderer:  renderer,
		SessionEvents: func(record SessionRecord) {
			sessions = append(sessions, record)
		},
		ProcessCapabilityAuthorization: authorization,
	})
	effectResult := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()])
	if effectResult.Status != "failed" {
		t.Fatalf("capability echo result: %s, want fail-closed status", capabilityResultDetail(effectResult))
	}
	if effectResult.Error == nil || effectResult.Error.Code != extensionproto.FailureSensitiveLeakDetected {
		t.Fatalf("capability status leak result: %s, want code %s", capabilityResultDetail(effectResult), extensionproto.FailureSensitiveLeakDetected)
	}

	renderer.mu.Lock()
	rendered := append([]RawJobEvent(nil), renderer.jobEvents[effect.Key()]...)
	renderer.mu.Unlock()
	surfaces := []struct {
		name  string
		value any
	}{
		{name: "renderer", value: rendered},
		{name: "job result", value: effectResult},
		{name: "session", value: sessions},
		{name: "cache", value: entryResultFromJobResult(effectResult)},
	}
	for _, surface := range surfaces {
		encoded, err := json.Marshal(surface.value)
		if err != nil {
			t.Fatalf("marshal %s: %v", surface.name, err)
		}
		if strings.Contains(string(encoded), token) {
			t.Fatalf("process capability leaked through %s: %s", surface.name, encoded)
		}
	}
}

func TestScheduler_ProcessCapabilityInJSONKeysFailsClosedWithoutPublishingTheKey(t *testing.T) {
	const token = "json-key-runtime-capability-bearer"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	gate := capabilityTestJob(root, project, "lint~json-key-gate", "lint", "", "")
	capabilityProbe(t, fixtureScript{{"print", `{"v":2,"type":"result","status":"success"}`}}, gate)
	effect := capabilityTestJob(root, project, "publish~json-key", "publish", "", extensionproto.SideEffectsRegistry, gate.Key())
	capabilityProbe(t, fixtureScript{
		{"print-env", `{"v":2,"type":"log","data":{"$PUTNAMI_CLOUD_TOKEN":"top-level-live","nested":{"prefix-$PUTNAMI_CLOUD_TOKEN":"nested-live"}}}`},
		{"print-env", `{"v":2,"type":"result","data":{"status":"success","data":{"$PUTNAMI_CLOUD_TOKEN":"top-level-result","nested":{"prefix-$PUTNAMI_CLOUD_TOKEN":"nested-result"}}}}`},
	}, effect)
	plan := []*ScheduledJob{gate, effect}
	ctx := capturedCapabilityContext(t, token, "lint")
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	renderer := &mockRenderer{}
	var sessions []SessionRecord
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config:    SchedulerConfig{MaxParallel: 1, NoCache: true},
		Renderer:  renderer,
		SessionEvents: func(record SessionRecord) {
			sessions = append(sessions, record)
		},
		ProcessCapabilityAuthorization: authorization,
	})
	effectResult := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()])
	if effectResult.Status != "failed" || effectResult.Error == nil ||
		effectResult.Error.Code != extensionproto.FailureSensitiveLeakDetected {
		t.Fatalf("JSON-key capability leak result: %s, want fail-closed %s", capabilityResultDetail(effectResult), extensionproto.FailureSensitiveLeakDetected)
	}
	renderer.mu.Lock()
	rendered := append([]RawJobEvent(nil), renderer.jobEvents[effect.Key()]...)
	renderer.mu.Unlock()
	for _, surface := range []struct {
		name  string
		value any
	}{
		{name: "renderer", value: rendered},
		{name: "job result", value: effectResult},
		{name: "session", value: sessions},
		{name: "cache", value: entryResultFromJobResult(effectResult)},
	} {
		encoded, err := json.Marshal(surface.value)
		if err != nil {
			t.Fatalf("marshal %s: %v", surface.name, err)
		}
		if strings.Contains(string(encoded), token) {
			t.Fatalf("process capability JSON key leaked through %s: %s", surface.name, encoded)
		}
	}
}

func TestProcessCapabilityBatchAggregateResultIsRedactedWithoutLiveSink(t *testing.T) {
	const token = "batch-aggregate-capability-bearer"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := capabilityTestJob(root, project, "publish~batch", "publish", "/unused", extensionproto.SideEffectsRegistry)
	job.JobDef.Batchable = &extension.TaskBatchPolicy{Tool: "test-batcher"}
	ctx := capturedCapabilityContext(t, token, "lint")
	resultEvent, ok := parseRawEvent(fmt.Sprintf(
		`{"v":2,"type":"result","time":%q,"level":%q,"message":%q,"data":{"status":%q,"data":{"batchResults":[{"projectId":"/site","status":%q}]},"error":{"message":%q,"code":%q}}}`,
		token, token, token, token, token, token, token,
	))
	if !ok {
		t.Fatal("parse adversarial batch aggregate result event")
	}
	result := extractResult(resultEvent.Data)
	result.Events = []RawJobEvent{resultEvent}

	got := redactProcessCapabilityResult(ctx, job, result)
	if got.Status != "failed" || got.Error == nil || got.Error.Code != extensionproto.FailureSensitiveLeakDetected {
		t.Fatalf("batch aggregate capability status leak: %s, want fail-closed", capabilityResultDetail(got))
	}
	for _, surface := range []struct {
		name  string
		value any
	}{
		{name: "job result", value: got},
		{name: "retained events", value: got.Events},
		{name: "cache", value: entryResultFromJobResult(got)},
	} {
		encoded, err := json.Marshal(surface.value)
		if err != nil {
			t.Fatalf("marshal guarded batch aggregate %s: %v", surface.name, err)
		}
		if strings.Contains(string(encoded), token) {
			t.Fatalf("batch aggregate result guard leaked capability through %s without live sink: %s", surface.name, encoded)
		}
	}
}

func TestScheduler_ProcessCapabilityBatchNeverMixesProtectedAndOrdinaryMembers(t *testing.T) {
	const token = "batch-runtime-capability-bearer"
	for _, test := range []struct {
		name           string
		protectedIndex int
	}{
		{name: "protected leader", protectedIndex: 0},
		{name: "ordinary leader", protectedIndex: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			logPath := filepath.Join(t.TempDir(), "capability-batch-invocations")
			ws, batchJobs := makeBatchSchedulerFixture(t, "")
			for _, job := range batchJobs {
				job.JobDef.CommandName = "publish"
				job.JobDef.Env = map[string]string{"PUTNAMI_CAPABILITY_BATCH_LOG": logPath}
				capabilityProbe(t, fixtureScript{
					{"append-env", "$PWD|$" + extensionproto.CloudTokenEnv + "\n", "$PUTNAMI_CAPABILITY_BATCH_LOG"},
					{"print", `{"v":2,"type":"result","data":{"status":"OK","data":{"batchResults":[{"projectId":"/a","status":"OK"},{"projectId":"/b","status":"OK"}]}}}`},
				}, job)
				// The shared fixture leaves the budget at the CLI default; these
				// members take part in a capability assertion, so they run
				// unbounded like every other probe here (capabilityJobTimeoutMs).
				// batchGroupTimeoutMs keeps the whole group unbounded in turn.
				job.JobDef.TimeoutMs = capabilityJobTimeoutMs
			}
			protected := batchJobs[test.protectedIndex]
			protected.JobDef.Traits.SideEffects = extensionproto.SideEffectsRegistry

			gate := capabilityTestJob(ws.Root, batchJobs[0].Project, "lint~capability-gate", "lint", "", "")
			capabilityProbe(t, fixtureScript{{"print", `{"v":2,"type":"result","status":"success"}`}}, gate)
			for _, job := range batchJobs {
				job.DependsOn = []string{gate.Key()}
			}
			plan := append([]*ScheduledJob{gate}, batchJobs...)
			ctx := capturedCapabilityContext(t, token, "lint")
			authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
			if err != nil {
				t.Fatalf("AuthorizeProcessCapabilities: %v", err)
			}
			result := RunPlan(ctx, RunRequest{
				Workspace:                      ws,
				Plan:                           plan,
				Config:                         SchedulerConfig{MaxParallel: 1, NoCache: true},
				Renderer:                       &mockRenderer{},
				ProcessCapabilityAuthorization: authorization,
			})
			if !result.Success {
				for key, jobResult := range result.Results {
					t.Logf("result %s: %s", key, capabilityResultDetail(jobResult))
				}
				// Classify before the verdict: an execution fault on any planned
				// job is named as one, so the batch failure below always means
				// the capability regimes themselves.
				for _, planned := range plan {
					capabilityJobResult(t, planned.Key(), result.Results[planned.Key()])
				}
				t.Fatal("capability batch run failed")
			}
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read capability batch log: %v", err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) != 2 {
				t.Fatalf("capability regimes shared %d physical invocation(s), want 2: %q", len(lines), data)
			}
			for _, line := range lines {
				executionRoot, delivered, ok := strings.Cut(line, "|")
				if !ok {
					t.Fatalf("malformed capability batch log line %q", line)
				}
				hasProtected := strings.HasSuffix(executionRoot, string(os.PathSeparator)+protected.Project.Path)
				hasOrdinary := strings.HasSuffix(executionRoot, string(os.PathSeparator)+batchJobs[1-test.protectedIndex].Project.Path)
				if hasProtected == hasOrdinary {
					t.Fatalf("physical invocation crossed capability regimes: %q", line)
				}
				if hasProtected && delivered != token {
					t.Fatalf("protected invocation token = %q, want exact capability", delivered)
				}
				if hasOrdinary && delivered != "" {
					t.Fatalf("ordinary invocation received capability %q", delivered)
				}
			}
		})
	}
}

func TestRunJob_NoCapturedCapabilityKeepsExistingTokenBehavior(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := capabilityTestJob(root, project, "publish~probe", "publish", "", extensionproto.SideEffectsCloud)
	job.JobDef.Env = map[string]string{
		extensionproto.CloudTokenEnv:           "job-owned-value",
		extensionproto.CloudCapabilityAfterEnv: "repository-owned-after-is-control-data",
	}
	capabilityProbe(t, fixtureScript{{"verdict",
		"equal:" + extensionproto.CloudTokenEnv + "=job-owned-value",
		"unset:" + extensionproto.CloudCapabilityAfterEnv}}, job)
	t.Setenv(extensionproto.CloudTokenEnv, "")

	result, err := RunJob(context.Background(), &workspace.Workspace{Root: root, Name: "test-ws"}, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if got := capabilityJobResult(t, job.Key(), result); got.Status != "success" {
		t.Fatalf("job token result: %s, want historical job token behavior with runner-only AFTER stripped", capabilityResultDetail(got))
	}
}

func TestRunJob_ProcessTokenAlonePreservesHistoricalMacBehavior(t *testing.T) {
	const token = "historical-mac-cloud-token"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := capabilityTestJob(root, project, "publish~probe", "publish", "", extensionproto.SideEffectsCloud)
	capabilityProbe(t, fixtureScript{{"verdict",
		"equal:" + extensionproto.CloudTokenEnv + "=" + token,
		"unset:" + extensionproto.CloudCapabilityAfterEnv}}, job)
	t.Setenv(extensionproto.CloudTokenEnv, token)
	// t.Setenv records and restores any caller value; remove the temporary value
	// so this is the exact historical token-only process contract.
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "")
	if err := os.Unsetenv(extensionproto.CloudCapabilityAfterEnv); err != nil {
		t.Fatalf("unset %s: %v", extensionproto.CloudCapabilityAfterEnv, err)
	}

	ctx := CaptureProcessCapabilities(context.Background())
	if got, present := os.LookupEnv(extensionproto.CloudTokenEnv); !present || got != token {
		t.Fatalf("ambient token after capture = %q, present %v; want historical value preserved", got, present)
	}
	result, err := RunJob(ctx, &workspace.Workspace{Root: root, Name: "test-ws"}, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if got := capabilityJobResult(t, job.Key(), result); got.Status != "success" {
		t.Fatalf("token-only result: %s, want token-only Mac publish/upgrade behavior preserved", capabilityResultDetail(got))
	}
}

func TestInternalReleaseSetProviderCapabilityRequiresExactPrivateGrant(t *testing.T) {
	const token = "exact-internal-provider-capability"
	const publicationsFile = "/private/tmp/exact-release-set-publications.json"
	const extensionName = "@putnami/cloud-provider"
	const version = "0.1.0"
	marker, err := json.Marshal(releaseSetProviderIdentity{
		ExtensionName: extensionName,
		Version:       version,
		Command:       distribution.ProviderCommandName,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(InternalReleaseSetProviderCapabilityEnv, string(marker))
	t.Setenv(extensionproto.CloudTokenEnv, token)
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "repo-control-must-not-survive")
	t.Setenv(runtimeproto.ReleaseSetPublishedImagesFileEnv, publicationsFile)
	t.Setenv("PUTNAMI_RELEASE_SET_MEMBERS_FILE", publicationsFile+".members")
	ctx := CaptureProcessCapabilities(context.Background())
	for _, name := range []string{
		InternalReleaseSetProviderCapabilityEnv,
		extensionproto.CloudTokenEnv,
		extensionproto.CloudCapabilityAfterEnv,
		runtimeproto.ReleaseSetPublishedImagesFileEnv,
		"PUTNAMI_RELEASE_SET_MEMBERS_FILE",
	} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("provider capture retained %s in the process environment", name)
		}
	}
	providerMode, err := ValidateReleaseSetProviderInvocation(ctx, []string{
		distribution.CloudCommand,
		distribution.ReleaseSetCommand,
		distribution.ResolveCommand,
		distribution.RequestFileFlag,
		filepath.Join(t.TempDir(), "request.json"),
	})
	if err != nil || !providerMode {
		t.Fatalf("provider invocation validation = (%v, %v), want (true, nil)", providerMode, err)
	}
	if err := ValidateReleaseSetProviderResolution(ctx, extensionName, version, distribution.ProviderCommandName, true); err != nil {
		t.Fatalf("validate exact provider resolution: %v", err)
	}

	root := t.TempDir()
	job := capabilityTestJob(
		root,
		&workspace.Project{ID: "/workspace", Name: "workspace", Path: "."},
		distribution.ProviderCommandName,
		distribution.ProviderCommandName,
		"/unused",
		"",
	)
	job.Extension.Name = extensionName
	job.Extension.Version = version
	hostileEnv := []string{
		extensionproto.CloudTokenEnv + "=manifest-owned",
		extensionproto.CloudCapabilityAfterEnv + "=manifest-owned",
		InternalReleaseSetProviderCapabilityEnv + "=manifest-owned",
		runtimeproto.ReleaseSetPublishedImagesFileEnv + "=/tmp/manifest-owned-publications.json",
		"PUTNAMI_RELEASE_SET_MEMBERS_FILE=/hostile",
	}
	assertProviderEnv := func(t *testing.T, env []string, wantToken, wantPublicationsFile string) {
		t.Helper()
		values := make(map[string]string, len(env))
		for _, entry := range env {
			if key, value, found := strings.Cut(entry, "="); found {
				values[key] = value
			}
		}
		if got := values[extensionproto.CloudTokenEnv]; got != wantToken {
			t.Fatalf("provider token = %q, want %q", got, wantToken)
		}
		wantMembers := ""
		if wantPublicationsFile != "" {
			wantMembers = wantPublicationsFile + ".members"
		}
		if values["PUTNAMI_RELEASE_SET_MEMBERS_FILE"] != wantMembers {
			t.Fatal("member evidence escaped exact provider grant")
		}
		if got := values[runtimeproto.ReleaseSetPublishedImagesFileEnv]; got != wantPublicationsFile {
			t.Fatalf("provider publications file = %q, want %q", got, wantPublicationsFile)
		}
		for _, name := range []string{extensionproto.CloudCapabilityAfterEnv, InternalReleaseSetProviderCapabilityEnv} {
			if _, present := values[name]; present {
				t.Fatalf("provider env retained control marker %s", name)
			}
		}
	}
	assertProviderEnv(t, scopeProcessCapabilities(ctx, hostileEnv, job), "", "")
	granted, err := GrantReleaseSetProviderJob(ctx, job)
	if err != nil {
		t.Fatalf("grant exact provider job: %v", err)
	}
	assertProviderEnv(t, scopeProcessCapabilities(granted, hostileEnv, job), token, publicationsFile)

	ordinary := *job
	ordinaryDef := *job.JobDef
	ordinaryDef.Name = "ordinary"
	ordinaryDef.CommandName = "ordinary"
	ordinary.JobDef = &ordinaryDef
	assertProviderEnv(t, scopeProcessCapabilities(granted, hostileEnv, &ordinary), "", "")
	if _, err := GrantReleaseSetProviderJob(ctx, &ordinary); err == nil {
		t.Fatal("ordinary job obtained an internal release-set provider grant")
	}
}

func TestInternalReleaseSetProviderCapabilityRedactsBearerWithoutManifestSideEffects(t *testing.T) {
	const token = "provider-output-capability-must-be-redacted"
	const extensionName = "@putnami/cloud-provider"
	const version = "0.1.0"
	marker, err := json.Marshal(releaseSetProviderIdentity{
		ExtensionName: extensionName,
		Version:       version,
		Command:       distribution.ProviderCommandName,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(InternalReleaseSetProviderCapabilityEnv, string(marker))
	t.Setenv(extensionproto.CloudTokenEnv, token)
	ctx := CaptureProcessCapabilities(context.Background())
	job := capabilityTestJob(
		t.TempDir(),
		&workspace.Project{ID: "/workspace", Name: "workspace", Path: "."},
		distribution.ProviderCommandName,
		distribution.ProviderCommandName,
		"/unused",
		"", // The reserved provider is authorized by its private grant, not a manifest trait.
	)
	job.Extension.Name = extensionName
	job.Extension.Version = version
	granted, err := GrantReleaseSetProviderJob(ctx, job)
	if err != nil {
		t.Fatalf("grant exact provider job: %v", err)
	}

	event := RawJobEvent{Type: EventTypeLog, Message: "provider:" + token, Data: extension.ParamMap{"token": token}}
	var rendered RawJobEvent
	processCapabilityEventSink(granted, job, func(got RawJobEvent) { rendered = got })(event)
	result := redactProcessCapabilityResult(granted, job, &JobResult{
		Status: "success",
		Data:   extension.ParamMap{"token": token},
		Events: []RawJobEvent{event},
	})
	for _, surface := range []struct {
		name  string
		value any
	}{
		{name: "renderer", value: rendered},
		{name: "result", value: result},
	} {
		encoded, marshalErr := json.Marshal(surface.value)
		if marshalErr != nil {
			t.Fatalf("marshal %s: %v", surface.name, marshalErr)
		}
		if strings.Contains(string(encoded), token) {
			t.Fatalf("reserved provider bearer leaked through %s without a sideEffects trait: %s", surface.name, encoded)
		}
	}
}

func TestRunJob_RunnerAfterWithoutBearerRejectsManifestOwnedToken(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := capabilityTestJob(root, project, "custom-cloud-write", "custom-cloud-write", "", extensionproto.SideEffectsCloud)
	job.JobDef.Env = map[string]string{extensionproto.CloudTokenEnv: "manifest-owned-token-must-not-bypass-gate"}
	capabilityProbe(t, fixtureScript{{"verdict", "unset:" + extensionproto.CloudTokenEnv}}, job)
	t.Setenv(extensionproto.CloudTokenEnv, "")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "lint,test,build,validate,validate-workspace")

	result, err := RunJob(context.Background(), &workspace.Workspace{Root: root, Name: "test-ws"}, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if got := capabilityJobResult(t, job.Key(), result); got.Status != "success" {
		t.Fatalf("manifest-token result: %s, want runner AFTER to suppress a manifest-owned token without a bearer", capabilityResultDetail(got))
	}
}

func TestRunPlan_DirectAdapterCannotArmCapability(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := capabilityTestJob(root, project, "custom-cloud-write", "custom-cloud-write", "", extensionproto.SideEffectsCloud)
	capabilityProbe(t, unarmedGateProbe, job)
	ctx := capturedCapabilityContext(t, "direct-adapter-must-not-receive-token", "lint,test,build,validate,validate-workspace")

	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      []*ScheduledJob{job},
		Config:    SchedulerConfig{MaxParallel: 1, NoCache: true},
		Renderer:  &mockRenderer{},
		// Deliberately no ProcessCapabilityAuthorization: adapters cannot derive
		// authority from a manifest-owned sideEffects trait.
	})
	if got := capabilityJobResult(t, job.Key(), result.Results[job.Key()]); got.Status != "success" {
		t.Fatalf("direct adapter result: %s, want no capability exposure", capabilityResultDetail(got))
	}
}

func TestAuthorizeProcessCapabilities_FailsClosedOnIncompleteFunctionalGate(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop.sh", "#!/bin/sh\nexit 0\n")

	tests := []struct {
		name  string
		after string
		plan  func() []*ScheduledJob
		want  string
	}{
		{
			name:  "missing command",
			after: "lint,test",
			plan: func() []*ScheduledJob {
				lint := capabilityTestJob(root, project, "lint~check", "lint", command, "")
				effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry, lint.Key())
				return []*ScheduledJob{lint, effect}
			},
			want: `no planner proof for no-op gate command "test"`,
		},
		{
			name:  "missing leaf",
			after: "lint",
			plan: func() []*ScheduledJob {
				one := capabilityTestJob(root, project, "lint~one", "lint", command, "")
				two := capabilityTestJob(root, project, "lint~two", "lint", command, "")
				effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry, one.Key())
				return []*ScheduledJob{one, two, effect}
			},
			want: "is not functionally after required leaf",
		},
		{
			name:  "serialize edge is not a functional gate",
			after: "lint",
			plan: func() []*ScheduledJob {
				lint := capabilityTestJob(root, project, "lint~check", "lint", command, "")
				effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry)
				effect.SerializeAfter = []string{lint.Key()}
				return []*ScheduledJob{lint, effect}
			},
			want: "is not functionally after required leaf",
		},
		{
			name:  "cycle",
			after: "lint",
			plan: func() []*ScheduledJob {
				one := capabilityTestJob(root, project, "lint~one", "lint", command, "")
				two := capabilityTestJob(root, project, "lint~two", "lint", command, "", one.Key())
				one.DependsOn = []string{two.Key()}
				effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsCloud, two.Key())
				return []*ScheduledJob{one, two, effect}
			},
			want: "possible dependency cycle",
		},
		{
			name:  "missing dependency",
			after: "lint",
			plan: func() []*ScheduledJob {
				lint := capabilityTestJob(root, project, "lint~check", "lint", command, "", "/missing:lint~check")
				effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsCloud, lint.Key())
				return []*ScheduledJob{lint, effect}
			},
			want: "unresolved job dependencies",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := capturedCapabilityContext(t, "resource-scoped-test-token", test.after)
			if _, err := AuthorizeProcessCapabilities(ctx, test.plan(), true); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want to contain %q", err, test.want)
			}
		})
	}
}

func TestAuthorizeProcessCapabilities_TaskDeclaredEffectsAreProtectedAndPlanBound(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "declared-effect.sh", "#!/bin/sh\nexit 0\n")
	lint := capabilityTestJob(root, project, "lint~check", "lint", command, "")
	effect := withDeclaredCapabilityEffect(
		capabilityTestJob(root, project, "custom~publish", "custom", command, "", lint.Key()),
		extensionproto.EffectRegistry,
	)
	ctx := capturedCapabilityContext(t, "task-declared-effect-token", "lint")
	plan := []*ScheduledJob{lint, effect}

	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil || authorization == nil {
		t.Fatalf("task-declared registry effect was not authorized through its functional gate: authorization=%v err=%v", authorization, err)
	}
	effect.Extension.Tasks["effect-task"] = extension.TaskDefinition{
		Declares: &extension.TaskDeclaration{Effects: []string{extensionproto.EffectCloud}},
	}
	if runtime := newProcessCapabilityRuntime(authorization, plan); runtime != nil {
		t.Fatal("authorization survived mutation of the task-declared external effect")
	}

	ungated := withDeclaredCapabilityEffect(
		capabilityTestJob(root, project, "custom~ungated", "custom", command, ""),
		extensionproto.EffectRegistry,
	)
	if _, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{lint, ungated}, true); err == nil ||
		!strings.Contains(err.Error(), "is not functionally after required leaf") {
		t.Fatalf("ungated task-declared effect authorization error = %v", err)
	}
}

func TestAuthorizeProcessCapabilities_RequiresPlannerProvenNoopGatesOnEveryProtectedJob(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop-gate.sh", "#!/bin/sh\nexit 0\n")
	lint := capabilityTestJob(root, project, "lint~check", "lint", command, "")

	t.Run("proven noop is accepted and bound to the exact plan", func(t *testing.T) {
		effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry, lint.Key())
		effect.SessionPrerequisiteNoopGates = []string{"test"}
		effect.SelectedProjects = []*workspace.Project{{ID: "/A", Name: "A", Path: "A"}}
		plan := []*ScheduledJob{lint, effect}
		ctx := capturedCapabilityContext(t, "resource-scoped-noop-token", "lint,test")

		authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
		if err != nil {
			t.Fatalf("AuthorizeProcessCapabilities: %v", err)
		}
		if authorization == nil || newProcessCapabilityRuntime(authorization, plan) == nil {
			t.Fatal("planner-proven no-op gate did not produce a runtime authorization")
		}

		effect.SelectedProjects = append(effect.SelectedProjects, &workspace.Project{ID: "/B", Name: "B", Path: "B"})
		if runtime := newProcessCapabilityRuntime(authorization, plan); runtime != nil {
			t.Fatal("authorization survived widening the planner-proven workspace selection")
		}
		effect.SelectedProjects = effect.SelectedProjects[:1]

		effect.SessionPrerequisiteNoopGates = []string{"test", "validate"}
		if runtime := newProcessCapabilityRuntime(authorization, plan); runtime != nil {
			t.Fatal("authorization survived mutation of the planner-proven no-op facts")
		}
	})

	t.Run("one unproven protected job keeps the missing gate closed", func(t *testing.T) {
		proven := capabilityTestJob(root, project, "publish~proven", "publish", command, extensionproto.SideEffectsRegistry, lint.Key())
		proven.SessionPrerequisiteNoopGates = []string{"test"}
		unproven := capabilityTestJob(root, project, "publish~unproven", "publish", command, extensionproto.SideEffectsCloud, lint.Key())
		ctx := capturedCapabilityContext(t, "resource-scoped-noop-token", "lint,test")

		if _, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{lint, proven, unproven}, true); err == nil ||
			!strings.Contains(err.Error(), unproven.Key()) {
			t.Fatalf("error = %v, want the unproven protected job to keep gate test fail-closed", err)
		}
	})
}

func TestAuthorizeProcessCapabilities_PropagatesNoopProofThroughEveryFunctionalBranch(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop-release.sh", "#!/bin/sh\nexit 0\n")
	lint := capabilityTestJob(root, project, "lint~check", "lint", command, "")
	publishImage := capabilityTestJob(
		root, project, "publish~image", "publish", command, extensionproto.SideEffectsRegistry, lint.Key(),
	)
	publishConfig := capabilityTestJob(
		root, project, "publish~config", "publish", command, extensionproto.SideEffectsCloud, lint.Key(),
	)
	publishImage.SessionPrerequisiteNoopGates = []string{"validate"}
	publishConfig.SessionPrerequisiteNoopGates = []string{"validate"}
	release := capabilityTestJob(
		root, project, "deploy~release", "deploy", command, extensionproto.SideEffectsCloud,
		publishImage.Key(), publishConfig.Key(),
	)
	plan := []*ScheduledJob{lint, publishImage, publishConfig, release}
	ctx := capturedCapabilityContext(t, "resource-scoped-release-token", "lint,validate")

	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	if authorization == nil || newProcessCapabilityRuntime(authorization, plan) == nil {
		t.Fatal("a protected descendant did not inherit the no-op proof carried by every functional branch")
	}

	ordinaryWithoutProof := capabilityTestJob(root, project, "package~unproven", "package", command, "", lint.Key())
	release.DependsOn = append(release.DependsOn, ordinaryWithoutProof.Key())
	if _, err := AuthorizeProcessCapabilities(ctx, append(plan, ordinaryWithoutProof), true); err == nil ||
		!strings.Contains(err.Error(), release.Key()) {
		t.Fatalf("error = %v, want the unproven functional branch to keep %s fail-closed", err, release.Key())
	}

	serializeOnlyRelease := capabilityTestJob(
		root, project, "deploy~serialize-only", "deploy", command, extensionproto.SideEffectsCloud,
	)
	serializeOnlyRelease.SerializeAfter = []string{publishImage.Key(), publishConfig.Key()}
	if _, err := AuthorizeProcessCapabilities(
		ctx, []*ScheduledJob{lint, publishImage, publishConfig, serializeOnlyRelease}, true,
	); err == nil || !strings.Contains(err.Error(), serializeOnlyRelease.Key()) {
		t.Fatalf("error = %v, want serialization-only branches to keep %s fail-closed", err, serializeOnlyRelease.Key())
	}
}

func TestAuthorizeProcessCapabilities_IgnoresRedundantDirectAncestorWithoutNoopProof(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop-release-ancestor.sh", "#!/bin/sh\nexit 0\n")
	packageImage := capabilityTestJob(root, project, "package~image", "package", command, "")
	publishImage := capabilityTestJob(
		root, project, "publish~image", "publish", command, extensionproto.SideEffectsRegistry,
		packageImage.Key(),
	)
	publishImage.SessionPrerequisiteNoopGates = []string{"validate"}
	release := capabilityTestJob(
		root, project, "deploy~release", "deploy", command, extensionproto.SideEffectsCloud,
		packageImage.Key(), publishImage.Key(),
	)
	plan := []*ScheduledJob{packageImage, publishImage, release}
	ctx := capturedCapabilityContext(t, "resource-scoped-release-token", "validate")

	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	if authorization == nil || newProcessCapabilityRuntime(authorization, plan) == nil {
		t.Fatal("a redundant direct ancestor bypassed the proven descendant branch")
	}
}

func TestAuthorizeProcessCapabilities_UsesFinalFunctionalLeaves(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop.sh", "#!/bin/sh\nexit 0\n")
	rootGate := capabilityTestJob(root, project, "lint~prepare", "lint", command, "")
	leafGate := capabilityTestJob(root, project, "lint~check", "lint", command, "", rootGate.Key())
	effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry, leafGate.Key())
	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", "lint")

	authorization, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{rootGate, leafGate, effect}, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	if authorization == nil {
		t.Fatal("expected authorization from the final functional leaf")
	}
}

func TestAuthorizeProcessCapabilities_IgnoresActiveFinalizerAsGateLeaf(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "noop.sh", "#!/bin/sh\nexit 0\n")
	leaf := capabilityTestJob(root, project, "test~run", "test", command, "")
	finalizer := capabilityTestJob(root, project, "test~teardown", "test", command, "")
	finalizer.Step = &extension.PipelineStep{
		ID:    "teardown",
		Task:  "teardown",
		RunOn: extensionproto.StepRunOnFinally,
		Finalizes: &extensionproto.FinalizesRelation{
			Producer:  "setup",
			Consumers: []string{"run"},
		},
	}
	effect := capabilityTestJob(root, project, "publish~push", "publish", command, extensionproto.SideEffectsRegistry, leaf.Key())
	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", "test")

	authorization, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{leaf, finalizer, effect}, true)
	if err != nil {
		t.Fatalf("active finalizer must not replace or block the functional gate leaf: %v", err)
	}
	if authorization == nil {
		t.Fatal("expected authorization from the ordinary functional test leaf")
	}
}

func TestScheduler_FailedGateNeverGrantsCapabilityWithContinueOnError(t *testing.T) {
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	marker := filepath.Join(root, "effect-ran")
	gate := capabilityTestJob(root, project, "lint~check", "lint", "", "")
	capabilityProbe(t, failProbe, gate)
	effect := capabilityTestJob(root, project, "publish~push", "publish", "", extensionproto.SideEffectsCloud, gate.Key())
	effect.JobDef.Env = map[string]string{"PUTNAMI_EFFECT_MARKER": marker}
	capabilityProbe(t, markerEffect, effect)
	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", "lint")
	authorization, err := AuthorizeProcessCapabilities(ctx, []*ScheduledJob{gate, effect}, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}

	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      []*ScheduledJob{gate, effect},
		Config: SchedulerConfig{
			MaxParallel:     1,
			ContinueOnError: true,
			NoCache:         true,
		},
		Renderer:                       &mockRenderer{},
		ProcessCapabilityAuthorization: authorization,
	})
	if got := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()]); got.Status != "skipped" {
		t.Fatalf("effect result: %s, want skipped after failed gate", capabilityResultDetail(got))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("side-effecting job ran after failed gate; marker stat error = %v", err)
	}
}

func TestScheduler_FailedNonGateAncestorNeverGrantsCapabilityWithContinueOnError(t *testing.T) {
	const after = "lint,test,build,validate,validate-workspace"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	marker := filepath.Join(root, "effect-ran")
	plan := make([]*ScheduledJob, 0, 8)
	dependencies := make([]string, 0, 6)
	for _, commandName := range strings.Split(after, ",") {
		gate := capabilityTestJob(root, project, commandName+"~check", commandName, "", "")
		capabilityProbe(t, passProbe, gate)
		plan = append(plan, gate)
		dependencies = append(dependencies, gate.Key())
	}
	configJob := capabilityTestJob(root, project, "config-merge~merge", "config-merge", "", "")
	capabilityProbe(t, failProbe, configJob)
	packageJob := capabilityTestJob(root, project, "package~assemble", "package", "", "", configJob.Key())
	capabilityProbe(t, passProbe, packageJob)
	plan = append(plan, configJob, packageJob)
	dependencies = append(dependencies, packageJob.Key())
	effect := capabilityTestJob(root, project, "publish~push", "publish", "", extensionproto.SideEffectsRegistry, dependencies...)
	effect.JobDef.Env = map[string]string{"PUTNAMI_EFFECT_MARKER": marker}
	capabilityProbe(t, markerEffect, effect)
	plan = append(plan, effect)

	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", after)
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config: SchedulerConfig{
			MaxParallel:     1,
			ContinueOnError: true,
			NoCache:         true,
		},
		Renderer:                       &mockRenderer{},
		ProcessCapabilityAuthorization: authorization,
	})
	if got := capabilityJobResult(t, packageJob.Key(), result.Results[packageJob.Key()]); got.Status != "success" {
		t.Fatalf("package result: %s, want continue-on-error to reach the protected job through a successful direct ancestor", capabilityResultDetail(got))
	}
	if got := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()]); got.Status != "skipped" {
		t.Fatalf("effect result: %s, want skipped after failed non-gate functional ancestor", capabilityResultDetail(got))
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("side-effecting job ran after failed non-gate ancestor; marker stat error = %v", err)
	}
}

// TestScheduler_InJobSkippedNonGateAncestorStillGrantsCapability pins a CI
// failure mode: under the capability gate a protected publish job sits
// behind ancestors such as package~tidy or
// config-merge that legitimately finish with an in-job "skipped" result (exit
// 0, no error). Those are complete terminal results, not gaps in the gate, so
// the grant must still be issued. A laptop publish has no gate and never saw
// the denial.
func TestScheduler_InJobSkippedNonGateAncestorStillGrantsCapability(t *testing.T) {
	const after = "lint,test,build,validate"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	marker := filepath.Join(root, "effect-ran")
	plan := make([]*ScheduledJob, 0, 8)
	dependencies := make([]string, 0, 6)
	for _, commandName := range strings.Split(after, ",") {
		gate := capabilityTestJob(root, project, commandName+"~check", commandName, "", "")
		capabilityProbe(t, passProbe, gate)
		plan = append(plan, gate)
		dependencies = append(dependencies, gate.Key())
	}
	tidyJob := capabilityTestJob(root, project, "package~tidy", "package", "", "")
	capabilityProbe(t, skipProbe, tidyJob)
	packageJob := capabilityTestJob(root, project, "package~docker", "package", "", "", tidyJob.Key())
	capabilityProbe(t, passProbe, packageJob)
	plan = append(plan, tidyJob, packageJob)
	dependencies = append(dependencies, packageJob.Key())
	effect := capabilityTestJob(root, project, "publish~docker", "publish", "", extensionproto.SideEffectsRegistry, dependencies...)
	effect.JobDef.Env = map[string]string{"PUTNAMI_EFFECT_MARKER": marker}
	capabilityProbe(t, markerEffect, effect)
	plan = append(plan, effect)

	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", after)
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	result := RunPlan(ctx, RunRequest{
		Workspace: &workspace.Workspace{Root: root, Name: "test-ws"},
		Plan:      plan,
		Config: SchedulerConfig{
			MaxParallel: 1,
			NoCache:     true,
		},
		Renderer:                       &mockRenderer{},
		ProcessCapabilityAuthorization: authorization,
	})
	if got := capabilityJobResult(t, tidyJob.Key(), result.Results[tidyJob.Key()]); got.Status != "skipped" || got.Error != nil {
		t.Fatalf("tidy result: %s, want an in-job skipped result with no error", capabilityResultDetail(got))
	}
	if got := capabilityJobResult(t, packageJob.Key(), result.Results[packageJob.Key()]); got.Status != "success" {
		t.Fatalf("package result: %s, want success", capabilityResultDetail(got))
	}
	if got := capabilityJobResult(t, effect.Key(), result.Results[effect.Key()]); got.Status != "success" {
		t.Fatalf("effect result: %s, want the protected job to run after an in-job skipped ancestor", capabilityResultDetail(got))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("side-effecting job did not run; marker stat error = %v", err)
	}
}

// TestProcessCapabilityRuntime_SkipWithCauseNeverGrantsCapability is the
// symmetric guard for the in-job skip admission: a "skipped" result that
// carries a cause was synthesized by the scheduler (skipJob after a failed or
// blocked dependency, or the pruned-invocation path), not reported by the
// task, and must keep the gate closed exactly like a failure.
func TestProcessCapabilityRuntime_SkipWithCauseNeverGrantsCapability(t *testing.T) {
	const after = "lint,test,build,validate"
	root := t.TempDir()
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	command := writeCapabilityCommand(t, root, "pass.sh", `#!/bin/sh
printf '%s\n' '{"v":2,"type":"result","data":{"status":"success"}}'
`)
	plan := make([]*ScheduledJob, 0, 8)
	dependencies := make([]string, 0, 6)
	for _, commandName := range strings.Split(after, ",") {
		gate := capabilityTestJob(root, project, commandName+"~check", commandName, command, "")
		plan = append(plan, gate)
		dependencies = append(dependencies, gate.Key())
	}
	tidyJob := capabilityTestJob(root, project, "package~tidy", "package", command, "")
	packageJob := capabilityTestJob(root, project, "package~docker", "package", command, "", tidyJob.Key())
	plan = append(plan, tidyJob, packageJob)
	dependencies = append(dependencies, packageJob.Key())
	effect := capabilityTestJob(root, project, "publish~docker", "publish", command, extensionproto.SideEffectsRegistry, dependencies...)
	plan = append(plan, effect)

	ctx := capturedCapabilityContext(t, "resource-scoped-test-token", after)
	authorization, err := AuthorizeProcessCapabilities(ctx, plan, true)
	if err != nil {
		t.Fatalf("AuthorizeProcessCapabilities: %v", err)
	}
	runtime := newProcessCapabilityRuntime(authorization, plan)
	if runtime == nil {
		t.Fatal("expected a runtime authorization for the gated plan")
	}
	settled := func(tidy *JobResult) map[string]*JobResult {
		results := make(map[string]*JobResult, len(plan))
		for _, job := range plan {
			if job.Key() != effect.Key() {
				results[job.Key()] = &JobResult{Status: "success"}
			}
		}
		results[tidyJob.Key()] = tidy
		return results
	}
	var mu sync.Mutex

	for name, tidy := range map[string]*JobResult{
		"scheduler skip after a failed dependency": {Status: "skipped", Error: &JobError{Message: "dependency failed"}},
		"pruned invocation resource":               {Status: "skipped", Error: &JobError{Message: "every consumer of this invocation-scoped resource was served from cache"}},
		"failed":                                   {Status: "failed"},
		"absent":                                   nil,
	} {
		results := settled(tidy)
		if tidy == nil {
			delete(results, tidyJob.Key())
		}
		if runtime.authorizeReady(effect, results, &mu) {
			t.Fatalf("%s: protected job received its grant behind ancestor result %+v", name, tidy)
		}
	}
	if !runtime.authorizeReady(effect, settled(&JobResult{Status: "skipped"}), &mu) {
		t.Fatal("protected job was denied its grant behind an in-job skipped ancestor")
	}
}

func TestCapturedCloudCapabilityStaysOutOfAmbientPlanContextAndCacheKey(t *testing.T) {
	const first = "first-private-cloud-capability"
	const after = "lint,test,build,validate,validate-workspace"
	ctx := capturedCapabilityContext(t, first, after)
	for _, name := range []string{extensionproto.CloudTokenEnv, extensionproto.CloudCapabilityAfterEnv} {
		if _, present := os.LookupEnv(name); present {
			t.Fatalf("%s remained in the ambient process environment", name)
		}
	}

	ws := makeExecutorTestWorkspace(t)
	job := cacheableJob("build~capability-probe", "/proj", "proj", "proj")
	job.JobDef.TaskCachePolicy = &extension.TaskCachePolicy{Key: &extension.TaskCacheKey{
		// Even a repository that explicitly names the transports as env inputs
		// sees absence during key computation: injection occurs only at exec.
		Env: []string{extensionproto.CloudTokenEnv, extensionproto.CloudCapabilityAfterEnv},
	}}

	planBytes, err := json.Marshal([]*ScheduledJob{job})
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	contextBytes, err := json.Marshal(BuildJobContext(ws, job, nil, nil, nil))
	if err != nil {
		t.Fatalf("marshal job context: %v", err)
	}
	for surface, value := range map[string]string{
		"in-memory context diagnostic": fmt.Sprint(ctx),
		"plan":                         string(planBytes),
		"job context":                  string(contextBytes),
	} {
		if strings.Contains(value, first) || strings.Contains(value, after) {
			t.Fatalf("captured capability control entered %s", surface)
		}
	}

	cacheFor := func() *store.CacheManager {
		return store.NewCacheManager(store.NewLocalStore(filepath.Join(t.TempDir(), "store")))
	}
	firstKey, err := computeJobCacheHash(ws, job, nil, nil, cacheFor(), nil)
	if err != nil {
		t.Fatalf("compute first cache key: %v", err)
	}

	const second = "second-private-cloud-capability"
	secondCtx := capturedCapabilityContext(t, second, after)
	secondKey, err := computeJobCacheHash(ws, job, nil, nil, cacheFor(), nil)
	if err != nil {
		t.Fatalf("compute second cache key: %v", err)
	}
	if firstKey != secondKey {
		t.Fatalf("cloud capability changed cache key: %s != %s", firstKey, secondKey)
	}
	if strings.Contains(fmt.Sprint(secondCtx), second) || strings.Contains(fmt.Sprint(secondCtx), after) {
		t.Fatal("captured capability entered a context diagnostic")
	}
}
