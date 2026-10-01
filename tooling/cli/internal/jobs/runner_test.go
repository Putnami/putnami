package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	runner "go.putnami.dev/protocol/runner"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func writeNoopCommand(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "noop.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write no-op command: %v", err)
	}
	return path
}

// TestRunJob_RunnerModeDoesNotExposeInheritedCloudTokenToJobWithoutSideEffects
// is the exact regression for the hosted-CI capability boundary: a bearer
// inherited with the runner opt-in must not become ambient authority for an
// arbitrary repository-controlled job.
func TestRunJob_RunnerModeDoesNotExposeInheritedCloudTokenToJobWithoutSideEffects(t *testing.T) {
	t.Setenv(extensionproto.CloudTokenEnv, "test-cloud-bearer-must-stay-private")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "lint,test,build,validate,validate-workspace")

	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "build~probe-environment",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{{"verdict", "unset:" + extensionproto.CloudTokenEnv}})

	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success: inherited PUTNAMI_CLOUD_TOKEN reached a job with no side effects", result.Status)
	}
}

// TestRunJob_CustomCloudTraitWithoutAGatedPlanCannotReceiveCapability pins the
// trust gap in a trait-only transport design. A repository controls its
// manifest and can label an arbitrary custom command as cloud-side-effecting;
// a direct job invocation carries no scheduler proof that trusted release gates
// dominate it, so that declaration alone must not grant the captured bearer.
func TestRunJob_CustomCloudTraitWithoutAGatedPlanCannotReceiveCapability(t *testing.T) {
	t.Setenv(extensionproto.CloudTokenEnv, "custom-trait-must-not-authorize-cloud-capability")
	t.Setenv(extensionproto.CloudCapabilityAfterEnv, "lint,test,build,validate,validate-workspace")

	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/untrusted", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/untrusted",
			Name:          "custom-cloud-write",
			CommandName:   "custom-cloud-write",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
			Traits: extension.CommandTraits{
				SideEffects: extensionproto.SideEffectsCloud,
			},
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{{"verdict", "unset:" + extensionproto.CloudTokenEnv}})

	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success: a custom sideEffects trait received the capability without a gated-plan proof", result.Status)
	}
}

// TestMissingBinaryError covers the helpers that detect a missing-binary
// fork/exec error and render an actionable diagnostic.
func TestMissingBinaryError(t *testing.T) {
	if !missingCommandBinary(exec.ErrNotFound) {
		t.Error("exec.ErrNotFound should be detected as a missing binary")
	}
	if !missingCommandBinary(&os.PathError{Op: "fork/exec", Path: "/missing/bin", Err: os.ErrNotExist}) {
		t.Error("fork/exec ENOENT should be detected as a missing binary")
	}
	if missingCommandBinary(&os.PathError{Op: "chdir", Path: "/missing/cwd", Err: os.ErrNotExist}) {
		t.Error("a missing cwd must not be classified as a missing binary")
	}
	if missingCommandBinary(os.ErrPermission) {
		t.Error("a permission error must not be classified as a missing binary")
	}

	job := &ScheduledJob{Extension: &extension.ExtensionDescription{Name: "@putnami/cloud"}}
	msg := missingBinaryError(job, "/ws/.putnami/bin/extensions/acme-tool/bin/acme-tool", os.ErrNotExist).Error()
	for _, want := range []string{"@putnami/cloud", "putnami install", "cache clean"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in: %s", want, msg)
		}
	}
	// Graceful fallback when the job has no extension.
	if got := missingBinaryError(nil, "/x", os.ErrNotExist).Error(); !strings.Contains(got, "required extension binary") {
		t.Errorf("nil-extension fallback wording missing: %s", got)
	}
}

// TestRunJob_MissingExtensionBinaryGivesActionableError: a job whose command
// binary is absent (a dangling stable symlink, e.g. after a concurrent
// `cache clean --all` reaped the artifact) fails with the actionable diagnostic,
// not a raw "start subprocess … no such file or directory".
func TestRunJob_MissingExtensionBinaryGivesActionableError(t *testing.T) {
	wsRoot := t.TempDir()
	missing := filepath.Join(wsRoot, ".putnami", "bin", "extensions", "acme-tool", "bin", "acme-tool")

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{ID: "/site", Name: "site", Path: "."}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "publish~publish-migration",
			Kind:          "command",
			Command:       missing,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err == nil {
		t.Fatalf("expected an error for a missing extension binary, got result=%+v", result)
	}
	msg := err.Error()
	if strings.HasPrefix(msg, "start subprocess ") {
		t.Errorf("error should be the actionable diagnostic, not the raw start-subprocess error: %s", msg)
	}
	for _, want := range []string{"@putnami/widget", "putnami install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in: %s", want, msg)
		}
	}
}

func TestRunJob_MissingCwdPreservesCwdDiagnostic(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "build~missing-cwd",
			Kind:          "command",
			Command:       writeNoopCommand(t, wsRoot),
			Cwd:           filepath.Join(wsRoot, "missing-cwd"),
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err == nil {
		t.Fatalf("expected an error for a missing cwd, got result=%+v", result)
	}
	msg := err.Error()
	if strings.Contains(msg, "shared artifact store") || strings.Contains(msg, "putnami install") {
		t.Fatalf("missing cwd should not be reported as a missing extension binary: %s", msg)
	}
	if !strings.Contains(msg, "start subprocess") || !strings.Contains(msg, "missing-cwd") {
		t.Fatalf("missing cwd diagnostic lost cwd context: %s", msg)
	}
}

func TestRunJob_RecordsSpawnToFirstJSONLEvent(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/site", Name: "site", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/widget", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/widget",
			Name:          "version~report",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{
		{"sleep", "0.02"},
		{"print", "not a runtime event"},
		{"print", `{"v":2,"type":"result","data":{"status":"success"}}`},
	})

	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if !result.FirstEventObserved {
		t.Fatal("FirstEventObserved = false, want first valid JSONL event recorded")
	}
	if result.SpawnToFirstEvent < 20*time.Millisecond {
		t.Errorf("SpawnToFirstEvent = %s, want delayed event latency", result.SpawnToFirstEvent)
	}
	if result.SpawnToFirstEvent > result.Duration {
		t.Errorf("SpawnToFirstEvent = %s exceeds task wall %s", result.SpawnToFirstEvent, result.Duration)
	}
}

func TestParseRawEvent_Valid(t *testing.T) {
	line := `{"v":2,"type":"phase","time":"2026-01-01T00:00:00Z","level":"info","message":"compile start","data":{"name":"compile","action":"start"}}`

	event, ok := parseRawEvent(line)
	if !ok {
		t.Fatal("parseRawEvent returned false for valid event")
	}
	if event.Version != runtimeproto.ProtocolVersion2 {
		t.Errorf("Version = %d, want %d", event.Version, runtimeproto.ProtocolVersion2)
	}
	if event.Type != "phase" {
		t.Errorf("Type = %q, want phase", event.Type)
	}
	if event.Data["name"] != "compile" {
		t.Errorf("Data[name] = %v, want compile", event.Data["name"])
	}
}

func TestParseRawEvent_InvalidVersion(t *testing.T) {
	for _, line := range []string{
		`{"v":0,"type":"phase"}`,
		`{"v":3,"type":"phase"}`,
		`{"type":"phase"}`,
	} {
		if _, ok := parseRawEvent(line); ok {
			t.Errorf("parseRawEvent(%s) should return false for an unknown protocol version", line)
		}
	}
}

// TestParseRawEvent_RejectsProtocolV1 is the former tolerate-v1 acceptance
// test, now converted to reject it. The CLI advertises v2 to every subprocess and now
// requires it: an extension can only reach this parser if its manifest declares
// CLI contract 3, and contract 3 requires an SDK that reads the advertisement.
// A v1 line therefore means the stream contradicts the manifest, which is not
// something to interpret.
func TestParseRawEvent_RejectsProtocolV1(t *testing.T) {
	for _, line := range []string{
		`{"v":1,"type":"log","level":"info","message":"hello"}`,
		`{"v":1,"type":"result","data":{"status":"success"}}`,
	} {
		if _, ok := parseRawEvent(line); ok {
			t.Errorf("parseRawEvent(%s) accepted a v1 line; v2 is required", line)
		}
	}
}

// TestParseRawEvent_AcceptsExactlyWhatTheCLIAdvertises is the invariant the two
// halves share: the acceptance test and the advertisement cannot drift, because
// a CLI that advertised one version and parsed another would make every
// well-behaved emitter wrong.
func TestParseRawEvent_AcceptsExactlyWhatTheCLIAdvertises(t *testing.T) {
	advertised := runtimeproto.NegotiatedVersion(
		strings.TrimPrefix(runtimeEventsAdvertisement(), runtimeproto.AcceptedVersionEnv+"="))
	line := fmt.Sprintf(`{"v":%d,"type":"log","level":"info","message":"hello"}`, advertised)
	if _, ok := parseRawEvent(line); !ok {
		t.Fatalf("the CLI advertises v%d but does not parse it", advertised)
	}
	for _, other := range []int{advertised - 1, advertised + 1} {
		other := fmt.Sprintf(`{"v":%d,"type":"log"}`, other)
		if _, ok := parseRawEvent(other); ok {
			t.Errorf("parseRawEvent(%s) accepted a version the CLI does not advertise", other)
		}
	}
}

// TestParseRawEvent_AcceptsProtocolV2 is the acceptance half of the B6a
// negotiation: this CLI advertises that it accepts v2 (runner.go), so it must
// parse a v2 stream. Rejecting on version would drop the subprocess's ENTIRE
// stream — every log, the result included — not merely the readiness event,
// because the protocol forbids a stream from mixing versions.
func TestParseRawEvent_AcceptsProtocolV2(t *testing.T) {
	line := `{"v":2,"type":"ready","time":"2026-07-28T09:00:01.000Z","data":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":3000}],"durationMs":420}}`

	event, ok := parseRawEvent(line)
	if !ok {
		t.Fatal("a v2 readiness event must be accepted")
	}
	if event.Version != runtimeproto.ProtocolVersion2 {
		t.Errorf("Version = %d, want %d", event.Version, runtimeproto.ProtocolVersion2)
	}
	if event.Type != EventTypeReady {
		t.Errorf("Type = %q, want %q", event.Type, EventTypeReady)
	}
	ready, err := runtimeproto.ExtractReadyData(event.Data)
	if err != nil {
		t.Fatalf("readiness payload: %v", err)
	}
	if ready.Target != runtimeproto.ReadyTargetServer || len(ready.Endpoints) != 1 {
		t.Fatalf("readiness payload = %+v", ready)
	}
	if got := ready.Endpoints[0].URL(); got != "http://localhost:3000" {
		t.Errorf("endpoint = %q, want http://localhost:3000", got)
	}
}

// TestReadJSONLEvents_V2ServeStreamSurvives is the whole-stream guard: a serve
// subprocess that reports readiness stamps v2 on every line, and the CLI must
// keep every one of them — including the result — while carrying the readiness
// events through untouched for B6b to consume. Two ready events prove restart
// re-emission survives the reader.
func TestReadJSONLEvents_V2ServeStreamSurvives(t *testing.T) {
	lines := strings.Join([]string{
		`{"v":2,"type":"meta","data":{"extension":"@putnami/go","job":"serve"}}`,
		`{"v":2,"type":"log","level":"info","message":"⚡️ listening http://localhost:8080"}`,
		`{"v":2,"type":"ready","data":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":8080}]}}`,
		`{"v":2,"type":"log","level":"info","message":"⚡️ listening http://localhost:8080"}`,
		`{"v":2,"type":"ready","data":{"target":"server","endpoints":[{"scheme":"http","host":"localhost","port":8080}]}}`,
		`{"v":2,"type":"result","data":{"status":"success"}}`,
	}, "\n")

	events, result := readJSONLEvents(strings.NewReader(lines), nil, nil)

	if len(events) != 6 {
		t.Fatalf("len(events) = %d, want 6 — a v2 stream must not be dropped", len(events))
	}
	ready := 0
	for _, event := range events {
		if event.Type == EventTypeReady {
			ready++
		}
	}
	if ready != 2 {
		t.Errorf("carried %d ready events, want 2 (initial serve + restart)", ready)
	}
	if result == nil || result.Status != "success" {
		t.Fatalf("result = %+v, want the v2 result event to still be extracted", result)
	}
}

func TestParseRawEvent_MissingType(t *testing.T) {
	line := `{"v":1}`
	_, ok := parseRawEvent(line)
	if ok {
		t.Error("parseRawEvent should return false for missing type")
	}
}

func TestParseRawEvent_InvalidJSON(t *testing.T) {
	_, ok := parseRawEvent("not json")
	if ok {
		t.Error("parseRawEvent should return false for invalid JSON")
	}
}

func TestReadJSONLEvents_MultipleEvents(t *testing.T) {
	lines := strings.Join([]string{
		`{"v":2,"type":"meta","data":{"extension":"@putnami/typescript","job":"build"}}`,
		`{"v":2,"type":"phase","data":{"name":"compile","action":"start"}}`,
		`{"v":2,"type":"progress","data":{"current":1,"total":3,"label":"Compiling..."}}`,
		`{"v":2,"type":"phase","data":{"name":"compile","action":"end","status":"success","duration":150}}`,
		`{"v":2,"type":"result","data":{"status":"success","data":{"output":"/dist"}}}`,
	}, "\n")

	var received []RawJobEvent
	onEvent := func(e RawJobEvent) {
		received = append(received, e)
	}

	events, result := readJSONLEvents(strings.NewReader(lines), onEvent, nil)

	if len(events) != 5 {
		t.Errorf("len(events) = %d, want 5", len(events))
	}

	if len(received) != 5 {
		t.Errorf("onEvent called %d times, want 5", len(received))
	}

	if result == nil {
		t.Fatal("result is nil, want non-nil from result event")
	}
	if result.Status != "success" {
		t.Errorf("result.Status = %q, want success", result.Status)
	}
	if result.Data["output"] != "/dist" {
		t.Errorf("result.Data[output] = %v, want /dist", result.Data["output"])
	}
}

func TestReadJSONLEvents_ObservesBeforeMappingAndRendering(t *testing.T) {
	lines := strings.Join([]string{
		"not a runtime event",
		`{"v":2,"type":"log","message":"ready"}`,
	}, "\n")
	var order []string
	mapper := func(event RawJobEvent) RawJobEvent {
		order = append(order, "map")
		return event
	}
	onEvent := func(RawJobEvent) {
		order = append(order, "render")
	}
	observe := func() {
		order = append(order, "observe")
	}

	events, _ := readJSONLEventsObserved(strings.NewReader(lines), onEvent, mapper, observe)
	if len(events) != 1 {
		t.Fatalf("events = %d, want one valid event", len(events))
	}
	if got := strings.Join(order, ","); got != "observe,map,render" {
		t.Fatalf("callback order = %q, want observe,map,render", got)
	}
}

func TestReadJSONLEvents_SkipsInvalidLines(t *testing.T) {
	lines := strings.Join([]string{
		"not a json line",
		`{"v":2,"type":"log","data":{"level":"info","message":"hello"}}`,
		`{"incomplete json`,
		`{"v":99,"type":"wrong_version"}`,
		`{"v":2,"type":"result","data":{"status":"success"}}`,
	}, "\n")

	events, result := readJSONLEvents(strings.NewReader(lines), nil, nil)

	if len(events) != 2 {
		t.Errorf("len(events) = %d, want 2 (only valid events)", len(events))
	}

	if result == nil || result.Status != "success" {
		t.Error("should extract result from valid result event")
	}
}

func TestReadJSONLEvents_FailedResult(t *testing.T) {
	lines := `{"v":2,"type":"result","data":{"status":"failed","error":{"message":"build failed","code":"BUILD_ERROR"}}}`

	_, result := readJSONLEvents(strings.NewReader(lines), nil, nil)

	if result == nil {
		t.Fatal("result is nil")
	}
	if result.Status != "failed" {
		t.Errorf("Status = %q, want failed", result.Status)
	}
	if result.Error == nil {
		t.Fatal("Error is nil")
	}
	if result.Error.Message != "build failed" {
		t.Errorf("Error.Message = %q, want build failed", result.Error.Message)
	}
	if result.Error.Code != "BUILD_ERROR" {
		t.Errorf("Error.Code = %q, want BUILD_ERROR", result.Error.Code)
	}
}

func TestReadJSONLEvents_LargeGeneratedAssetResult(t *testing.T) {
	line := `{"v":2,"type":"result","data":{"status":"success","data":{"exports":{"api-loader":"./api/.api-application.gen","static-loader":"./static/.static.gen"},"assets":{"public/install.sh":"/project/.gen/public/install.sh"},"manifest":"` + strings.Repeat("x", 2*1024*1024) + `"}}}`

	events, result := readJSONLEvents(strings.NewReader(line), nil, nil)

	if len(events) != 0 {
		t.Fatalf("len(events) = %d, want oversized detail omitted from bounded retention", len(events))
	}
	if result == nil {
		t.Fatal("result is nil, want non-nil from large result event")
	}
	if result.Status != "success" {
		t.Fatalf("result.Status = %q, want success", result.Status)
	}
	exports, ok := result.Data["exports"].(map[string]any)
	if !ok {
		t.Fatalf("result.Data[exports] = %T, want map[string]any", result.Data["exports"])
	}
	if exports["api-loader"] != "./api/.api-application.gen" {
		t.Errorf("api-loader export = %v, want ./api/.api-application.gen", exports["api-loader"])
	}
	if exports["static-loader"] != "./static/.static.gen" {
		t.Errorf("static-loader export = %v, want ./static/.static.gen", exports["static-loader"])
	}
	assets, ok := result.Data["assets"].(map[string]any)
	if !ok {
		t.Fatalf("result.Data[assets] = %T, want map[string]any", result.Data["assets"])
	}
	if assets["public/install.sh"] != "/project/.gen/public/install.sh" {
		t.Fatalf("assets[public/install.sh] = %v, want /project/.gen/public/install.sh", assets["public/install.sh"])
	}
}

func TestReadJSONLEvents_BoundsRetentionWithoutDroppingCallbacksOrVerdict(t *testing.T) {
	lines := make([]string, 0, 903)
	for i := 0; i < 900; i++ {
		lines = append(lines, fmt.Sprintf(`{"v":2,"type":"log","level":"info","message":"ordinary-%d"}`, i))
	}
	lines = append(lines,
		`{"v":2,"type":"log","level":"error","message":"late failure evidence"}`,
		`{"v":2,"type":"meta","data":{"extension":"@putnami/go","job":"build"}}`,
		`{"v":2,"type":"result","data":{"status":"FAILED","error":{"message":"build failed"}}}`,
	)
	callbackCount := 0
	events, result := readJSONLEvents(strings.NewReader(strings.Join(lines, "\n")), func(RawJobEvent) {
		callbackCount++
	}, nil)

	if callbackCount != len(lines) {
		t.Fatalf("callback count = %d, want %d complete observed events", callbackCount, len(lines))
	}
	budget, _ := protocolcli.MachineOutputBudgetFor(protocolcli.MachineOutputModeNormal)
	ordinaryCap := budget.MaxRecords - budget.FailureReserveRecords - budget.FinalReserveRecords
	const failureEvidence = 2 // the late error log and normalized FAILED result
	if len(events) > ordinaryCap+failureEvidence {
		t.Fatalf("retained events = %d, want at most %d ordinary + %d failure-priority records",
			len(events), ordinaryCap, failureEvidence)
	}
	sawFailureLog := false
	sawFailedResult := false
	for _, event := range events {
		if event.Level == "error" && event.Message == "late failure evidence" {
			sawFailureLog = true
		}
		status, _ := event.Data["status"].(string)
		if event.Type == EventTypeResult && normalizeStatus(status) == string(TaskStatusFailed) {
			sawFailedResult = true
		}
	}
	if !sawFailureLog {
		t.Fatal("late failure event did not survive ordinary-event exhaustion")
	}
	if !sawFailedResult {
		t.Fatal("normalized FAILED result did not survive ordinary-event exhaustion")
	}
	if result == nil || result.Status != string(TaskStatusFailed) || result.Error == nil || result.Error.Message != "build failed" {
		t.Fatalf("result = %+v, verdict extraction must be independent of retention", result)
	}
	if !result.EmittedMeta {
		t.Fatal("runtime meta handshake changed when its detail record was outside retention")
	}
}

func TestBoundedJobEventsClassifiesExplicitPayloadBeforeStructProjection(t *testing.T) {
	ordinary := make([]RawJobEvent, 900)
	for i := range ordinary {
		ordinary[i] = RawJobEvent{Version: 2, Type: EventTypeLog, Level: "info", Message: "ordinary"}
	}

	explicitFailure := RawJobEvent{Version: 2, Type: EventTypeLog, Level: "info",
		Data: map[string]any{"type": EventTypeDiagnostic, "severity": "error", "message": "opaque failure"}}
	retained := BoundedJobEvents(append(ordinary, explicitFailure))
	if !slices.ContainsFunc(retained, func(event RawJobEvent) bool { return event.Data["message"] == "opaque failure" }) {
		t.Fatal("explicit failure payload was overwritten by the RawJobEvent compatibility fields")
	}

	explicitOrdinary := RawJobEvent{Version: 2, Type: EventTypeDiagnostic, Level: "error",
		Data: map[string]any{"type": EventTypeLog, "level": "info", "message": "opaque ordinary"}}
	retained = BoundedJobEvents(append(ordinary, explicitOrdinary))
	if slices.ContainsFunc(retained, func(event RawJobEvent) bool { return event.Data["message"] == "opaque ordinary" }) {
		t.Fatal("compatibility fields overrode explicit ordinary payload and consumed failure reserve")
	}
}

func TestNormalizeStatus(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"success", "success"},
		{"succeeded", "success"},
		{"OK", "success"},
		{"ok", "success"},
		{"failed", "failed"},
		{"failure", "failed"},
		{"error", "failed"},
		{"FAILED", "failed"},
		{"skipped", "skipped"},
		{"skip", "skipped"},
		{"SKIP", "skipped"},
		{"unknown", "unknown"},
	}

	for _, tt := range tests {
		got := normalizeStatus(tt.input)
		if got != tt.want {
			t.Errorf("normalizeStatus(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestReadJSONLEvents_EmptyInput(t *testing.T) {
	events, result := readJSONLEvents(strings.NewReader(""), nil, nil)
	if len(events) != 0 {
		t.Errorf("empty input should produce 0 events, got %d", len(events))
	}
	if result != nil {
		t.Error("empty input should produce nil result")
	}
}

func TestReadJSONLEvents_OnlyBlankLines(t *testing.T) {
	events, result := readJSONLEvents(strings.NewReader("\n\n  \n"), nil, nil)
	if len(events) != 0 {
		t.Errorf("blank-line input should produce 0 events, got %d", len(events))
	}
	if result != nil {
		t.Error("blank-line input should produce nil result")
	}
}

func TestReadJSONLEvents_NoResultEvent(t *testing.T) {
	lines := strings.Join([]string{
		`{"v":2,"type":"log","data":{"level":"info","message":"starting"}}`,
		`{"v":2,"type":"phase","data":{"name":"compile","action":"start"}}`,
	}, "\n")

	events, result := readJSONLEvents(strings.NewReader(lines), nil, nil)
	if len(events) != 2 {
		t.Errorf("len(events) = %d, want 2", len(events))
	}
	if result != nil {
		t.Error("no result event should produce nil result")
	}
}

func TestRunJob_NonZeroExitOverridesSkippedResult(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "fake-cloud-publish-config.sh")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"log\",\"level\":\"error\",\"message\":\"Invalid or expired refresh token. Run `putnami cloud login`.\"}'\n" +
		"printf '%s\\n' '{\"v\":2,\"type\":\"result\",\"data\":{\"status\":\"SKIP\"}}'\n" +
		"exit 7\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake publish script: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{ID: "/auth/server", Name: "auth/server", Path: "."}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/cloud", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/cloud",
			Name:          "publish~cloud-publish-config",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", result.ExitCode)
	}
	if len(result.Events) == 0 || !strings.Contains(result.Events[0].Message, "Invalid or expired refresh token") {
		t.Fatalf("auth failure event not retained: %+v", result.Events)
	}
}

// TestRunJob_NoConfigSchemaScan proves the deletion of the config-schema
// preflight is real and observable.
//
// The fixture is exactly the shape the old gate failed: an infra marker, a Go
// source importing go.putnami.dev/config and instantiating config.Config[T],
// no schema/config.json and no .gen/config-schema.json, run under a `deploy`
// job — the verb whose default traits carried `preflight: config-schema`. Core
// no longer parses the workload's imports or stats its infra markers to decide
// whether an artifact it does not own must exist; a task that needs another
// task's output declares a required `from: "task"` input port and the planner
// enforces it (plan_producers.go).
func TestRunJob_NoConfigSchemaScan(t *testing.T) {
	wsRoot := t.TempDir()
	appDir := filepath.Join(wsRoot, "delivery", "workloads", "cache-server")
	if err := os.MkdirAll(filepath.Join(appDir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "infra", "runtime.json"), []byte(`{"ingress":{"public":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configSrc := "package config\n\n" +
		"import pconfig \"go.putnami.dev/config\"\n\n" +
		"type ServerCfg struct { Port int `json:\"port\" env:\"PORT\"` }\n\n" +
		"var ServerConfig = pconfig.Config[ServerCfg](\"cacheServer\")\n"
	if err := os.WriteFile(filepath.Join(appDir, "config.go"), []byte(configSrc), 0o644); err != nil {
		t.Fatal(err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{
		ID:   "/delivery/workloads/cache-server",
		Name: "delivery/cache-server",
		Path: "delivery/workloads/cache-server",
	}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/cloud", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/cloud",
			Name:          "deploy~cloud-release",
			Kind:          "command",
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{{"exit", "0"}})

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q error = %+v, want success: core must not inspect the workload for a config schema", result.Status, result.Error)
	}
	for _, event := range result.Events {
		if strings.Contains(event.Message, "config schema") {
			t.Fatalf("core emitted a config-schema diagnostic: %+v", event)
		}
	}
}

func TestPrepareJobInvocation_RebasesWorkspaceExtensionRootFromRelPath(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."},
		Extension: &extension.ExtensionDescription{
			Name:    "@putnami/go",
			Path:    filepath.Join(filepath.Dir(wsRoot), "other-workspace", "go", "extension"),
			RelPath: "/go/extension",
		},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/go",
			Name:          "build~config-merge",
			Kind:          "command",
			Command:       "{extensionRoot}/bin/custom-run",
			Args:          []string{"{extensionRoot}/cmd/putnami-go"},
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	_, cancel, inv, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareJobInvocation: %v", err)
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()

	wantRoot := filepath.Join(wsRoot, "go", "extension")
	if inv.jobCtx.Extension.Root != wantRoot {
		t.Fatalf("context extension root = %q, want %q", inv.jobCtx.Extension.Root, wantRoot)
	}
	// The expansion keeps the manifest's "/" after {extensionRoot}, which
	// Windows accepts in a program path, so the paths compare cleaned.
	if filepath.Clean(inv.command) != filepath.Join(wantRoot, "bin", "custom-run") {
		t.Fatalf("command = %q, want %q", inv.command, filepath.Join(wantRoot, "bin", "custom-run"))
	}
	if len(inv.args) == 0 {
		t.Fatal("args is empty, want expanded extension command arg")
	}
	if filepath.Clean(inv.args[0]) != filepath.Join(wantRoot, "cmd", "putnami-go") {
		t.Fatalf("args[0] = %q, want %q", inv.args[0], filepath.Join(wantRoot, "cmd", "putnami-go"))
	}
}

func TestPrepareJobInvocation_ExpandsSelectedProjectTemplates(t *testing.T) {
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project: &workspace.Project{ID: "test-ws", Name: "test-ws", Path: "."},
		SelectedProjects: []*workspace.Project{
			{ID: "/apps/api", Name: "api", Path: "apps/api"},
			{ID: "/sites/web", Name: "web", Path: "sites/web"},
		},
		Extension: &extension.ExtensionDescription{Name: "@putnami/cloud", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/cloud",
			Name:          "deploy",
			Kind:          "command",
			Command:       "/bin/echo",
			Args:          []string{"{selectedProjects}", "{selectedProjectPaths}"},
			Env:           map[string]string{"SELECTED": "{selectedProjectIDs}"},
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	_, cancel, inv, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareJobInvocation: %v", err)
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()

	if got, want := inv.args[0], "api,web"; got != want {
		t.Fatalf("args[0] = %q, want %q", got, want)
	}
	if got, want := inv.args[1], "apps/api,sites/web"; got != want {
		t.Fatalf("args[1] = %q, want %q", got, want)
	}
	if !slices.Contains(inv.env, "SELECTED=/apps/api,/sites/web") {
		// Never the whole env: it starts from this process's environment.
		t.Fatalf("env SELECTED = %q, want the selected project IDs", envLastValue(inv.env, "SELECTED"))
	}
}

func TestRunJob_StderrErrorDoesNotAppendJSONLHint(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "failing-lint.sh")
	script := "#!/bin/sh\n" +
		"echo 'lint failed because config is invalid' >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake lint script: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "lint~check",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.Error == nil || result.Error.Message != "lint failed because config is invalid" {
		t.Fatalf("unexpected error message: %+v", result.Error)
	}
	if strings.Contains(result.Error.Message, "--output=jsonl") {
		t.Fatalf("runner should not append renderer hints to job errors: %q", result.Error.Message)
	}
}

func TestRunJob_SignalKillWithEmptyStderrSurfacesWaitError(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "self-kill.sh")
	// Kill self with SIGKILL before writing anything to stdout/stderr, mirroring
	// a subprocess torn down by a signal (OOM, SIGBUS, torn code signature).
	script := "#!/bin/sh\n" +
		"kill -KILL $$\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write self-kill script: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	project := &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."}
	job := &ScheduledJob{
		Project:   project,
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test",
			Name:          "build~self-kill",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if result.Error == nil {
		t.Fatalf("Error is nil; want wait error surfaced (e.g. \"signal: killed\")")
	}
	if !strings.Contains(result.Error.Message, "signal: killed") {
		t.Fatalf("Error.Message = %q, want it to contain %q", result.Error.Message, "signal: killed")
	}
}

func TestReadJSONLEvents_NilHandler(t *testing.T) {
	line := `{"v":2,"type":"log","data":{"level":"info","message":"test"}}`
	events, _ := readJSONLEvents(strings.NewReader(line), nil, nil)
	if len(events) != 1 {
		t.Errorf("expected 1 event, got %d", len(events))
	}
}

func TestParseRawEvent_AllEventTypes(t *testing.T) {
	types := []string{
		EventTypeLog, EventTypeProgress, EventTypeArtifact,
		EventTypeDiagnostic, EventTypeMetric, EventTypePhase,
		EventTypeSummary, EventTypeResult, EventTypeMeta,
	}
	for _, typ := range types {
		line := `{"v":2,"type":"` + typ + `"}`
		event, ok := parseRawEvent(line)
		if !ok {
			t.Errorf("parseRawEvent should accept type %q", typ)
			continue
		}
		if event.Type != typ {
			t.Errorf("Type = %q, want %q", event.Type, typ)
		}
	}
}

func TestParseRawEvent_WithMessageAndLevel(t *testing.T) {
	line := `{"v":2,"type":"log","level":"warn","message":"deprecated API"}`
	event, ok := parseRawEvent(line)
	if !ok {
		t.Fatal("parseRawEvent returned false")
	}
	if event.Level != "warn" {
		t.Errorf("Level = %q, want warn", event.Level)
	}
	if event.Message != "deprecated API" {
		t.Errorf("Message = %q, want deprecated API", event.Message)
	}
}

func TestExtractResult(t *testing.T) {
	tests := []struct {
		name       string
		data       map[string]any
		wantStatus string
		wantData   bool
		wantError  bool
	}{
		{
			name:       "success with data",
			data:       map[string]any{"status": "success", "data": map[string]any{"key": "val"}},
			wantStatus: "success",
			wantData:   true,
		},
		{
			name:       "failed with error",
			data:       map[string]any{"status": "failed", "error": map[string]any{"message": "oops"}},
			wantStatus: "failed",
			wantError:  true,
		},
		{
			name:       "skipped",
			data:       map[string]any{"status": "skipped"},
			wantStatus: "skipped",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractResult(tt.data)
			if result.Status != tt.wantStatus {
				t.Errorf("Status = %q, want %q", result.Status, tt.wantStatus)
			}
			if tt.wantData && result.Data == nil {
				t.Error("Data is nil, want non-nil")
			}
			if tt.wantError && result.Error == nil {
				t.Error("Error is nil, want non-nil")
			}
		})
	}
}

func TestRunJob_ExportsCPUBudgetAndCapturesCPUTime(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh unavailable on this platform")
	}
	t.Setenv("GOMAXPROCS", "") // register cleanup, then force the unset path
	os.Unsetenv("GOMAXPROCS")

	wsRoot := t.TempDir()
	scriptPath := filepath.Join(wsRoot, "report-budget.sh")
	// Echo the budget env vars back as a log event; burnCPUShell keeps the
	// captured CPUTime measurably positive (see test_helpers_test.go).
	script := "#!/bin/sh\n" +
		burnCPUShell() +
		"printf '%s\\n' \"{\\\"v\\\":2,\\\"type\\\":\\\"log\\\",\\\"level\\\":\\\"info\\\",\\\"message\\\":\\\"budget=$PUTNAMI_CPU_BUDGET gomaxprocs=$GOMAXPROCS\\\"}\"\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/proj", Name: "proj", Path: "."},
		Extension: &extension.ExtensionDescription{Name: goExtensionName, Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: goExtensionName,
			Name:          "lint",
			Kind:          "command",
			Command:       scriptPath,
			TimeoutMs:     unboundedJobTimeoutMs,
		},
		CPUBudget: 3,
	}

	result, err := RunJob(t.Context(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success (error: %+v)", result.Status, result.Error)
	}
	if len(result.Events) == 0 || !strings.Contains(result.Events[0].Message, "budget=3 gomaxprocs=3") {
		t.Fatalf("budget env not exported to subprocess: %+v", result.Events)
	}
	if result.CPUTime <= 0 {
		t.Fatalf("CPUTime = %v, want > 0", result.CPUTime)
	}
	if result.Execution == nil || result.Execution.Concurrency != job.CPUBudget {
		t.Fatalf("execution = %+v, want physical ledger concurrency equal to granted budget %d", result.Execution, job.CPUBudget)
	}
}

// PUTNAMI_SOURCE_REVISION and PUTNAMI_SOURCE_COMMIT_TIME are this run's input:
// the CLI reads them into the version snapshot and hands every job the bound
// version through its job context. A job process never inherits them, so a
// test suite a gate runs reads its fixtures' own HEAD, and a nested `putnami`
// run stamps its own checkout, even when a runner exported the pair for the
// whole session. Not parallel: it sets the environment.
func TestRunJob_NeverForwardsTheBoundSourceRevision(t *testing.T) {
	t.Setenv(putnamigit.SourceRevisionEnv, testRevision)
	t.Setenv(putnamigit.SourceCommitTimeEnv, "1767323045")

	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@putnami/test", Path: wsRoot},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@putnami/test", Name: "test~probe-environment", Kind: "command",
			TimeoutMs: unboundedJobTimeoutMs,
		},
	}
	fixtureTask(t, job.JobDef, fixtureScript{{"verdict", "unset:" + putnamigit.SourceRevisionEnv, "unset:" + putnamigit.SourceCommitTimeEnv}})
	result, err := RunJob(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if result.Status != "success" {
		t.Fatalf("status = %q, want success: the bound source revision reached a job process", result.Status)
	}
}

// The credential-provider choice holds for the CLI process that made it: a
// job that runs the CLI again starts with the provider off unless its own
// command line turns it on.
func TestPrepareJobInvocation_NeverForwardsTheProvidersChoice(t *testing.T) {
	spectest.Proves(t, "cli/credential-provider", "providers-are-opt-in", "choice-holds-for-one-process")
	t.Setenv(ProvidersEnv, "install,publish")
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@fixture/gate", Path: filepath.Join(wsRoot, "ext")},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@fixture/gate", Name: "build", Kind: "command", Command: "/bin/true",
			TimeoutMs: unboundedJobTimeoutMs,
		},
	}
	_, cancel, inv, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareJobInvocation: %v", err)
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()
	for _, entry := range inv.env {
		if strings.HasPrefix(entry, ProvidersEnv+"=") {
			t.Fatalf("job environment forwards the providers choice: %s", entry)
		}
	}
}

// The bound-request channel of a portable execution names the request the
// pinned entrypoint consumes; it is control data for THAT process only. A task
// that called the CLI again while it was set would be hijacked into a second
// bound execution of the same snapshot, so the job environment must
// never carry it — not from the inherited process environment, and not from a
// manifest that restates it.
func TestPrepareJobInvocation_NeverForwardsTheBoundRequestChannel(t *testing.T) {
	t.Setenv(runner.BoundRequestEnv, filepath.Join(t.TempDir(), "request.json"))
	wsRoot := t.TempDir()
	ws := &workspace.Workspace{Root: wsRoot, Name: "test-ws"}
	job := &ScheduledJob{
		Project:   &workspace.Project{ID: "/pkg", Name: "pkg", Path: "."},
		Extension: &extension.ExtensionDescription{Name: "@fixture/gate", Path: filepath.Join(wsRoot, "ext")},
		JobDef: &extension.JobDefinition{
			ExtensionName: "@fixture/gate", Name: "build", Kind: "command", Command: "/bin/true",
			TimeoutMs: unboundedJobTimeoutMs,
		},
	}
	_, cancel, inv, err := prepareJobInvocation(context.Background(), ws, job, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("prepareJobInvocation: %v", err)
	}
	defer cancel()
	defer func() { _ = os.Remove(inv.contextFile) }()
	for _, entry := range inv.env {
		if strings.HasPrefix(entry, runner.BoundRequestEnv+"=") {
			t.Fatalf("job environment forwards the bound-request channel: %s", entry)
		}
	}
}
