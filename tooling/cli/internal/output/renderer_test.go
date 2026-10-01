package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func makeRendererTestJob() *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{Name: "my-app", Path: "packages/my-app"},
		Extension: &extension.ExtensionDescription{Name: "test-ext", Path: "/ext"},
		JobDef:    &extension.JobDefinition{Name: "build", ExtensionName: "test-ext"},
	}
}

func makeRendererDiagnostic(t *testing.T, severity runtimeproto.DiagnosticSeverity, message string) jobs.RawJobEvent {
	t.Helper()
	wire, err := json.Marshal(&runtimeproto.Event{
		V:        runtimeproto.MaxKnownProtocolVersion,
		Type:     runtimeproto.EventDiagnostic,
		Severity: &severity,
		Message:  message,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, ok := jobmodel.ParseRawEvent(string(wire))
	if !ok {
		t.Fatalf("typed diagnostic did not parse: %s", wire)
	}
	return event
}

func TestTextRenderer_SuccessOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Verbose: true})

	job := makeRendererTestJob()
	planned := []*jobs.ScheduledJob{job}

	r.Start(planned)
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{
		Status:   "success",
		Duration: 150 * time.Millisecond,
	})
	r.Finish(map[string]*jobs.JobResult{
		job.Key(): {Status: "success"},
	}, jobs.SessionOutcome{})

	output := errOut.String()
	if !strings.Contains(output, "done") {
		t.Errorf("output should contain 'done', got:\n%s", output)
	}
	if !strings.Contains(output, "1 succeeded") {
		t.Errorf("output should contain '1 succeeded', got:\n%s", output)
	}
}

func TestTextRenderer_FailureOutput(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "failure-presentation", "human-failure-output-is-prioritized-and-bounded")
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{})

	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{
		Status:   "failed",
		Duration: 200 * time.Millisecond,
		Error:    &jobs.JobError{Message: "compilation error"},
	})
	r.Finish(map[string]*jobs.JobResult{
		job.Key(): {Status: "failed"},
	}, jobs.SessionOutcome{})

	output := errOut.String()
	if !strings.Contains(output, "FAIL") {
		t.Errorf("output should contain 'FAIL', got:\n%s", output)
	}
	if !strings.Contains(output, "compilation error") {
		t.Errorf("output should contain error message, got:\n%s", output)
	}
	if !strings.Contains(output, "1/1 failed") {
		t.Errorf("output should contain '1/1 failed', got:\n%s", output)
	}
}

func TestTextRenderer_QuietMode(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Quiet: true})

	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{Status: "success", Duration: 100 * time.Millisecond})

	// In quiet mode, no per-job output before finish
	beforeFinish := errOut.String()
	if strings.Contains(beforeFinish, "my-app") {
		t.Errorf("quiet mode should suppress per-job output, got:\n%s", beforeFinish)
	}
}

func TestLifecycleRenderer_CompactSuccessAndVisibleDiagnostics(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewLifecycleRenderer(Config{Out: &out, Err: &errOut}, false)
	job := makeRendererTestJob()
	warning := makeRendererDiagnostic(t, runtimeproto.SeverityWarning, "optional tool unavailable")
	errorDiagnostic := makeRendererDiagnostic(t, runtimeproto.SeverityError, "provider reported an error")

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, warning)
	r.JobEvent(job, errorDiagnostic)
	r.JobComplete(job, &jobs.JobResult{Status: "success"})
	r.Finish(map[string]*jobs.JobResult{job.Key(): {Status: "success"}}, jobs.SessionOutcome{})

	got := errOut.String()
	if !strings.Contains(got, "warning: optional tool unavailable") {
		t.Fatalf("lifecycle warning was hidden: %q", got)
	}
	if !strings.Contains(got, "error: provider reported an error") {
		t.Fatalf("lifecycle error diagnostic was hidden on a successful job: %q", got)
	}
	for _, noise := range []string{"starting", "done", "succeeded", "Session"} {
		if strings.Contains(got, noise) {
			t.Errorf("compact lifecycle output contains %q: %q", noise, got)
		}
	}
}

func TestLifecycleRenderer_QuietTakesPrecedenceAndShowsFailureDetails(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewLifecycleRenderer(Config{
		Out: &out, Err: &errOut, Quiet: true, Verbose: true, Debug: true, ServeMode: true,
	}, true)
	job := makeRendererTestJob()
	warning := makeRendererDiagnostic(t, runtimeproto.SeverityWarning, "optional tool unavailable")
	failed := &jobs.JobResult{Status: "failed", Error: &jobs.JobError{Message: "installer failed"}}

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, warning)
	r.JobComplete(job, failed)
	r.Finish(map[string]*jobs.JobResult{job.Key(): failed}, jobs.SessionOutcome{})

	got := errOut.String()
	if !strings.Contains(got, "warning: optional tool unavailable") ||
		!strings.Contains(got, "FAIL") || !strings.Contains(got, "installer failed") {
		t.Fatalf("quiet lifecycle failure lost details: %q", got)
	}
	for _, noise := range []string{"starting", "done", "succeeded", "Session"} {
		if strings.Contains(got, noise) {
			t.Errorf("quiet lifecycle output contains %q: %q", noise, got)
		}
	}
}

// TestJSONLRenderer_EmitsTheV2SessionStream replaces the v1 job:*/session:end
// assertions deleted with the JSONLRenderer itself. The stream this pins is
// what --output=jsonl now produces: version-stamped task:* records closed by
// session:end.
func TestJSONLRenderer_EmitsTheV2SessionStream(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(Config{Output: "jsonl", Out: &buf, Err: &buf})

	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, jobs.RawJobEvent{
		Version: 1,
		Type:    "phase",
		Data:    map[string]any{"name": "compile", "action": "start"},
	})
	r.JobComplete(job, &jobs.JobResult{
		Status:   "success",
		Duration: 100 * time.Millisecond,
	})
	r.Finish(map[string]*jobs.JobResult{
		job.Key(): {Status: "success"},
	}, jobs.SessionOutcome{})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected at least 3 JSONL lines, got %d", len(lines))
	}

	// Every line is valid JSON and says which contract it speaks.
	for i, line := range lines {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Errorf("line %d is not valid JSON: %v\nline: %s", i, err, line)
			continue
		}
		if parsed["protocolVersion"] != float64(protocolcli.ResultProtocolVersion) {
			t.Errorf("line %d carries protocolVersion %v, want %d — a record without it is version 1",
				i, parsed["protocolVersion"], protocolcli.ResultProtocolVersion)
		}
	}

	var firstLine map[string]any
	json.Unmarshal([]byte(lines[0]), &firstLine)
	if firstLine["record"] != protocolcli.RecordTaskStart {
		t.Errorf("first record = %v, want %q", firstLine["record"], protocolcli.RecordTaskStart)
	}

	var lastLine map[string]any
	json.Unmarshal([]byte(lines[len(lines)-1]), &lastLine)
	if lastLine["record"] != protocolcli.RecordSessionEnd {
		t.Errorf("last record = %v, want %q", lastLine["record"], protocolcli.RecordSessionEnd)
	}
	run, _ := lastLine["run"].(map[string]any)
	if run == nil || run["outcome"] != protocolcli.RunOutcomeSuccess {
		t.Errorf("session:end run = %v, want outcome %q", lastLine["run"], protocolcli.RunOutcomeSuccess)
	}
}

func TestMachineRenderers_BoundedStreamOmitsButAggregateCarriesCacheSummary(t *testing.T) {
	var buf bytes.Buffer
	r := NewRenderer(Config{Output: "jsonl", Out: &buf, Err: &buf})
	job := makeRendererTestJob()
	cached := &jobs.JobResult{Status: "success", CacheHit: true}

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobComplete(job, cached)
	cache := &jobs.CacheStatsSnapshot{
		LocalHits: 1, LocalServedMs: 80, LocalKeysMs: 20,
		LocalBindingsMs: 50, LocalRestoreVerifyMs: 10, LocalSpawnedProcesses: 2,
	}
	r.Finish(map[string]*jobs.JobResult{job.Key(): cached}, jobs.SessionOutcome{Cache: cache})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var end protocolcli.SessionStreamRecord
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &end); err != nil {
		t.Fatal(err)
	}
	if end.Run == nil || end.Run.Reuse.LocalCache != 1 || end.MachineOutput == nil {
		t.Fatalf("bounded session:end = %+v / %+v", end.Run, end.MachineOutput)
	}
	if end.Run.Cache != nil {
		t.Fatalf("bounded session:end carried unbounded/general cache detail: %+v", end.Run.Cache)
	}

	var aggregate bytes.Buffer
	jsonRenderer := newMachineV2Renderer(&aggregate, "build", false)
	jsonRenderer.Start([]*jobs.ScheduledJob{job})
	jsonRenderer.Finish(map[string]*jobs.JobResult{job.Key(): cached}, jobs.SessionOutcome{Cache: cache})
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal(aggregate.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Run == nil || envelope.Run.Cache == nil || envelope.Run.Cache.Local == nil {
		t.Fatalf("aggregate omitted cache attribution: %+v", envelope.Run)
	}
	if got := envelope.Run.Cache.Local; got.Hits != 1 || got.ServedMs != 80 || got.SpawnedProcesses != 2 {
		t.Fatalf("aggregate local cache = %+v", got)
	}
}

func TestMachineV2Renderer_ProjectsDockerPublicationFactsOnBothMachineSurfaces(t *testing.T) {
	digestA := "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB := "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	packageA := dockerPublicationTestJob("/services/a", "a", "package~docker", "package")
	packageB := dockerPublicationTestJob("/services/b", "b", "package~docker", "package")
	publishA := dockerPublicationTestJob("/services/a", "a", "publish~docker", "publish")
	publishB := dockerPublicationTestJob("/services/b", "b", "publish~docker", "publish")
	planned := []*jobs.ScheduledJob{packageA, packageB, publishA, publishB}
	artifactA := dockerPublicationArtifact("ghcr.io/acme/a", "release-1", digestA, "miss", "pushed", false)
	artifactB := dockerPublicationArtifact("ghcr.io/acme/b", "release-1", digestB, "hit", "retagged", true)
	results := map[string]*jobs.JobResult{
		packageA.Key(): {Status: "success", TaskWall: 40 * time.Millisecond},
		packageB.Key(): {Status: "success", TaskWall: 60 * time.Millisecond},
		publishA.Key(): {Status: "success", Events: []jobs.RawJobEvent{artifactA}},
		publishB.Key(): {Status: "success", Events: []jobs.RawJobEvent{artifactB}},
	}

	var stream bytes.Buffer
	jsonl := newMachineV2Renderer(&stream, "publish", true)
	jsonl.SetPublishConcurrency(100)
	jsonl.Start(planned)
	jsonl.JobStart(publishA)
	jsonl.JobStart(publishB)
	jsonl.JobEvent(publishA, dockerPushPhase("start"))
	jsonl.JobEvent(publishB, dockerPushPhase("start"))
	jsonl.JobEvent(publishA, artifactA)
	jsonl.JobEvent(publishB, artifactB)
	jsonl.JobEvent(publishA, dockerPushPhase("end"))
	jsonl.JobEvent(publishB, dockerPushPhase("end"))
	jsonl.JobComplete(publishA, results[publishA.Key()])
	jsonl.JobComplete(publishB, results[publishB.Key()])
	jsonl.Finish(results, jobs.SessionOutcome{})

	var terminal protocolcli.SessionStreamRecord
	sawTypedArtifact := false
	for _, line := range strings.Split(strings.TrimSpace(stream.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode machine record: %v", err)
		}
		if record.Record == protocolcli.RecordTaskEvent && record.Event["kind"] == "published" {
			sawTypedArtifact = record.Event["imageDigest"] == digestA || record.Event["imageDigest"] == digestB
		}
		if record.Record == protocolcli.RecordSessionEnd {
			terminal = record
		}
	}
	if !sawTypedArtifact {
		t.Fatal("jsonl task:event did not carry the typed immutable Docker artifact facts")
	}
	if terminal.Run == nil || terminal.MachineOutput == nil || len(terminal.Run.Publications) != 0 {
		t.Fatalf("bounded jsonl terminal = run %+v / machine %+v", terminal.Run, terminal.MachineOutput)
	}
	if encoded, err := json.Marshal(terminal); err != nil {
		t.Fatal(err)
	} else if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionStreamRecord, encoded); len(violations) != 0 {
		t.Errorf("jsonl terminal violates the v2 contract: %+v", violations)
	}

	var aggregate bytes.Buffer
	jsonOutput := newMachineV2Renderer(&aggregate, "publish", false)
	jsonOutput.SetPublishConcurrency(100)
	jsonOutput.Start(planned)
	jsonOutput.Finish(results, jobs.SessionOutcome{})
	var envelope protocolcli.ResultV2
	if err := json.Unmarshal(aggregate.Bytes(), &envelope); err != nil {
		t.Fatalf("decode --output=json envelope: %v\n%s", err, aggregate.String())
	}
	if envelope.Run == nil || len(envelope.Run.Publications) != 2 {
		t.Fatalf("json envelope publications = %+v, want both workloads", envelope.Run)
	}
	if envelope.Run.Publications[0].Timings.BuildMs != 40 || envelope.Run.Publications[1].Timings.BuildMs != 60 {
		t.Errorf("json envelope build timings = %+v, want 40ms and 60ms", envelope.Run.Publications)
	}
	for _, publication := range envelope.Run.Publications {
		if publication.Concurrency.ConfiguredCap != 100 || publication.Concurrency.Effective != 0 {
			t.Errorf("json envelope without live phases = %+v, want configured 100 / observed 0", publication.Concurrency)
		}
	}
}

func dockerPublicationTestJob(projectID, projectName, taskName, command string) *jobs.ScheduledJob {
	return &jobs.ScheduledJob{
		Project:   &workspace.Project{ID: projectID, Name: projectName},
		Extension: &extension.ExtensionDescription{Name: "docker-test"},
		JobDef: &extension.JobDefinition{
			Name:         taskName,
			InternalName: taskName,
			CommandName:  command,
			StepID:       "docker",
		},
		Step: &extension.PipelineStep{Task: command + "-docker"},
	}
}

func dockerPublicationArtifact(name, version, digest, cacheOutcome, contentStatus string, digestReused bool) jobs.RawJobEvent {
	return jobs.RawJobEvent{
		Version: 2,
		Type:    jobs.EventTypeArtifact,
		Data: map[string]any{
			"id": "docker", "name": name, "kind": "published", "path": name + "@" + digest,
			"registry": "docker", "targetRegistry": "ghcr.io", "version": version, "tags": []any{"edge"},
			"contentStatus": contentStatus, "cacheOutcome": cacheOutcome, "imageDigest": digest, "immutableRef": name + "@" + digest,
			"digestVerified": true, "digestReused": digestReused,
			"publishTimings": map[string]any{
				"cacheLookupMs": 2, "cacheTransferMs": 5, "registryPushMs": 5,
				"referencePublishMs": 1, "digestResolveMs": 1,
			},
		},
	}
}

func dockerPushPhase(action string) jobs.RawJobEvent {
	return jobs.RawJobEvent{Version: 2, Type: jobs.EventTypePhase, Data: map[string]any{"name": "docker-push", "action": action}}
}

// TestJSONLRenderer_TaskEndCarriesTheTasksDiagnostics pins the one thing a
// task:end record exists to report beyond a count: the diagnostics the task
// produced. The contract requires them (protocols/cli/doc/02-result-v2.md,
// doc/08-output-and-rendering.md § Record fields) and session.json's record for
// the SAME task carries them, so a streamed record built from the counting-only
// projection made two documents describing one run disagree — silently, because
// diagnostics is an omitempty member and an empty one looks exactly like a task
// that produced none.
func TestJSONLRenderer_TaskEndCarriesTheTasksDiagnostics(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "located-diagnostics", "locations-normalize-and-survive-to-results")
	var buf bytes.Buffer
	r := NewRenderer(Config{Output: "jsonl", Out: &buf, Err: &buf})

	job := makeRendererTestJob()
	// The event is decoded from its wire bytes rather than spelled as a literal
	// payload: this is the shape jobs.parseRawEvent leaves behind, and the
	// record extraction under test reads exactly that.
	event := jobs.RawJobEvent{Version: 2, Type: "diagnostic", Time: "2026-01-01T00:00:05Z"}
	if err := json.Unmarshal([]byte(`{
		"severity": "error",
		"code": "ASSERT",
		"message": "expected 3, got 4",
		"location": {"file": "internal/sum/sum_test.go", "line": 42, "column": 3}
	}`), &event.Data); err != nil {
		t.Fatalf("decode diagnostic payload: %v", err)
	}
	failed := &jobs.JobResult{
		Status:   string(jobs.TaskStatusFailed),
		Duration: 100 * time.Millisecond,
		Error:    &jobs.JobError{Message: "2 assertions failed"},
		Events:   []jobs.RawJobEvent{event},
	}

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobEvent(job, event)
	r.JobComplete(job, failed)
	r.Finish(map[string]*jobs.JobResult{job.Key(): failed}, jobs.SessionOutcome{})

	var end protocolcli.SessionStreamRecord
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("stream line is not a v2 record: %v\nline: %s", err, line)
		}
		if record.Record == protocolcli.RecordTaskEnd {
			end = record
		}
	}
	if end.Task == nil {
		t.Fatal("no task:end record carried a terminal task result")
	}
	if len(end.Task.Diagnostics) != 1 {
		t.Fatalf("task:end carried %d diagnostics, want 1 — the streamed record dropped what "+
			"session.json records for the same task", len(end.Task.Diagnostics))
	}
	got := end.Task.Diagnostics[0]
	if got.Severity != "error" || got.Code != "ASSERT" || got.Message != "expected 3, got 4" {
		t.Errorf("task:end diagnostic = %+v, want the emitted error diagnostic", got)
	}
	if got.File != "internal/sum/sum_test.go" || got.Line != 42 || got.Column != 3 {
		t.Errorf("task:end diagnostic location = %s:%d:%d, want internal/sum/sum_test.go:42:3",
			got.File, got.Line, got.Column)
	}
}

func TestCloudLoggingRenderer_EmitsStructuredJSON(t *testing.T) {
	var buf bytes.Buffer
	r := NewCloudLoggingRenderer(&buf)

	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)
	r.JobComplete(job, &jobs.JobResult{
		Status:   "success",
		Duration: 100 * time.Millisecond,
	})
	r.Finish(map[string]*jobs.JobResult{
		job.Key(): {Status: "success"},
	}, jobs.SessionOutcome{})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 lines, got %d", len(lines))
	}

	// Each line should have severity + message
	for i, line := range lines {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
			continue
		}
		if _, ok := entry["severity"]; !ok {
			t.Errorf("line %d: missing severity field", i)
		}
		if _, ok := entry["message"]; !ok {
			t.Errorf("line %d: missing message field", i)
		}
	}
}

func TestTextRenderer_DebugEvents(t *testing.T) {
	var out, errOut bytes.Buffer
	r := NewTextRenderer(&out, &errOut, TextRendererConfig{Debug: true, Verbose: true})

	job := makeRendererTestJob()

	r.Start([]*jobs.ScheduledJob{job})
	r.JobStart(job)

	r.JobEvent(job, jobs.RawJobEvent{
		Version: 1, Type: "phase",
		Data: map[string]any{"name": "compile", "action": "start"},
	})
	r.JobEvent(job, jobs.RawJobEvent{
		Version: 1, Type: "progress",
		Data: map[string]any{"current": float64(1), "total": float64(3), "label": "Compiling..."},
	})
	r.JobEvent(job, jobs.RawJobEvent{
		Version: 1, Type: "metric",
		Data: map[string]any{"name": "binary-size", "value": float64(4096), "unit": "bytes"},
	})

	output := errOut.String()
	if !strings.Contains(output, "[phase]") {
		t.Errorf("debug output should contain [phase], got:\n%s", output)
	}
	if !strings.Contains(output, "[progress]") {
		t.Errorf("debug output should contain [progress], got:\n%s", output)
	}
	if !strings.Contains(output, "[metric]") {
		t.Errorf("debug output should contain [metric], got:\n%s", output)
	}
}

// --- NewRenderer factory ---

// TestNewRenderer_MachineFormatsAreV2Only pins the selection point once the v1
// renderers were gone: both machine formats are the v2 renderer, and the
// retired PUTNAMI_MACHINE_OUTPUT variable selects nothing — a run that still sets it to
// "v1" gets v2 documents like every other run, because there is no v1 emitter
// left to select.
func TestNewRenderer_MachineFormatsAreV2Only(t *testing.T) {
	for _, format := range []string{"json", "jsonl"} {
		for _, value := range []string{"", "v1", "V1", "v2"} {
			t.Run(format+"/"+machine.RetiredSelectionEnv+"="+value, func(t *testing.T) {
				t.Setenv(machine.RetiredSelectionEnv, value)
				r := NewRenderer(Config{Output: format})
				if _, ok := r.(*machineV2Renderer); !ok {
					t.Errorf("NewRenderer(%s) with %s=%q = %T, want the v2 machine renderer",
						format, machine.RetiredSelectionEnv, value, r)
				}
			})
		}
	}
}

func TestNewRenderer_CloudLogging(t *testing.T) {
	cfg := Config{Output: "cloud-logging"}
	r := NewRenderer(cfg)
	if _, ok := r.(*CloudLoggingRenderer); !ok {
		t.Errorf("NewRenderer(cloud-logging) = %T, want *CloudLoggingRenderer", r)
	}
}

func TestNewRenderer_AutoDetect_KService(t *testing.T) {
	t.Setenv("K_SERVICE", "my-cloud-service")
	cfg := Config{Output: ""}
	r := NewRenderer(cfg)
	if _, ok := r.(*CloudLoggingRenderer); !ok {
		t.Errorf("NewRenderer with K_SERVICE = %T, want *CloudLoggingRenderer", r)
	}
}

func TestNewRenderer_AutoDetect_Quiet(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	cfg := Config{Output: "", Quiet: true}
	r := NewRenderer(cfg)
	if _, ok := r.(*TextRenderer); !ok {
		t.Errorf("NewRenderer(quiet) = %T, want *TextRenderer", r)
	}
}

func TestNewRenderer_AutoDetect_Debug(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	cfg := Config{Output: "", Debug: true}
	r := NewRenderer(cfg)
	if _, ok := r.(*TextRenderer); !ok {
		t.Errorf("NewRenderer(debug) = %T, want *TextRenderer", r)
	}
}

func TestNewRenderer_AutoDetect_Verbose(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	cfg := Config{Output: "", Verbose: true}
	r := NewRenderer(cfg)
	if _, ok := r.(*TextRenderer); !ok {
		t.Errorf("NewRenderer(verbose) = %T, want *TextRenderer", r)
	}
}

func TestNewRenderer_AutoDetect_NoTTY(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	// In test environments there's no TTY, so ShouldUseLiveRenderer() == false.
	// The renderer should fall back to TextRenderer.
	cfg := Config{Output: ""}
	r := NewRenderer(cfg)
	// Either TextRenderer or LiveRenderer is valid depending on the environment.
	switch r.(type) {
	case *TextRenderer, *LiveRenderer:
		// ok
	default:
		t.Errorf("NewRenderer(auto) = %T, want *TextRenderer or *LiveRenderer", r)
	}
}

func TestStructuredOutput(t *testing.T) {
	cases := map[string]bool{
		"jsonl":         true,
		"cloud-logging": true,
		"":              false, // auto — human renderers own stdout
		"text":          false,
		"json":          true,
	}
	for output, want := range cases {
		if got := StructuredOutput(output); got != want {
			t.Errorf("StructuredOutput(%q) = %v, want %v", output, got, want)
		}
	}
}
