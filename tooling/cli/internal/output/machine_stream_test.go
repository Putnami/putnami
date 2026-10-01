package output

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/protocol/distribution"
	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"

	jobmodel "go.putnami.dev/cli/model/jobs"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

func TestMachineStreamIsBoundedFailureFirstSanitizedAndCompletelyRecorded(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-machine-detail", "the-stream-is-bounded-failure-first-sanitized-and-recorded")
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(
		newMachineV2Renderer(&live, "build", true),
		session,
		false,
	)
	job := makeRendererTestJob()
	failed := &jobs.JobResult{
		Status:   string(jobs.TaskStatusFailed),
		Duration: 100 * time.Millisecond,
		Error:    &jobs.JobError{Message: "compile failed"},
	}

	renderer.Start([]*jobs.ScheduledJob{job})
	recorder, ok := renderer.(interface {
		RecordSessionEvent(jobs.SessionRecord)
	})
	if !ok {
		t.Fatalf("renderer %T does not retain session audit signals", renderer)
	}
	recorder.RecordSessionEvent(jobs.SessionRecord{Type: "invocation:reaped", Data: map[string]any{
		"invocationId":  "orphan-1",
		"Authorization": "session-secret",
	}})
	renderer.JobStart(job)
	for i := 0; i < 800; i++ {
		renderer.JobEvent(job, jobs.RawJobEvent{
			Version: 2,
			Type:    jobs.EventTypeLog,
			Data: map[string]any{
				"type":    "log",
				"level":   "info",
				"message": strings.Repeat("ordinary detail ", 140),
			},
		})
	}
	secret := "gh" + "p_" + strings.Repeat("a", 36)
	renderer.JobEvent(job, jobs.RawJobEvent{
		Version: 2,
		Type:    jobs.EventTypeLog,
		Data: map[string]any{
			"type":          "log",
			"level":         "error",
			"message":       "\x1b[31mfailure evidence\x1b[0m\x00",
			"Authorization": secret,
		},
	})
	renderer.JobComplete(job, failed)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): failed}, jobs.SessionOutcome{})
	session.Close()

	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("bounded stream violations: %+v", violations)
	}
	budget, _ := protocolcli.MachineOutputBudgetFor(protocolcli.MachineOutputModeNormal)
	if int64(live.Len()) > budget.MaxBytes {
		t.Fatalf("live bytes = %d, max = %d", live.Len(), budget.MaxBytes)
	}
	if bytes.Count(live.Bytes(), []byte{'\n'}) > budget.MaxRecords {
		t.Fatalf("live records exceed %d", budget.MaxRecords)
	}
	for name, data := range map[string][]byte{"live": live.Bytes(), "artifact": artifact} {
		if bytes.Contains(data, []byte{0x1b}) || bytes.Contains(data, []byte(secret)) {
			t.Errorf("%s contains unsanitized control or secret", name)
		}
	}
	if !bytes.Contains(live.Bytes(), []byte("failure evidence")) || !bytes.Contains(live.Bytes(), []byte("invocation:reaped")) || !bytes.Contains(live.Bytes(), []byte("[REDACTED]")) {
		t.Fatal("late failure evidence did not survive the ordinary-detail flood")
	}
	if len(artifact) <= live.Len() {
		t.Fatalf("artifact bytes = %d, live bytes = %d; omitted detail was not retained", len(artifact), live.Len())
	}

	var final protocolcli.BoundedSessionEndRecord
	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
	if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.MachineOutput.Elided.Ordinary.Records == 0 || final.MachineOutput.Elided.Failure.Records != 0 {
		t.Fatalf("elision accounting = %+v", final.MachineOutput.Elided)
	}
	if final.MachineOutput.Artifact.SessionID != session.ID {
		t.Fatalf("artifact ref = %+v, want session %s", final.MachineOutput.Artifact, session.ID)
	}
}

func TestMachineStreamVerboseChangesCapacityNotVerdict(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-machine-detail", "normal-suppresses-and-verbose-admits-debug-detail")
	run := func(mode string) ([]byte, protocolcli.BoundedSessionEndRecord) {
		t.Helper()
		var out bytes.Buffer
		renderer := newMachineV2RendererMode(&out, "build", true, mode)
		job := makeRendererTestJob()
		result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
		renderer.Start([]*jobs.ScheduledJob{job})
		for i := 0; i < 1200; i++ {
			renderer.JobEvent(job, jobs.RawJobEvent{Data: map[string]any{
				"type": "log", "level": "info", "message": "bounded",
			}})
		}
		renderer.JobComplete(job, result)
		renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
		lines := bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'})
		var final protocolcli.BoundedSessionEndRecord
		if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
			t.Fatal(err)
		}
		return out.Bytes(), final
	}

	normal, normalFinal := run(protocolcli.MachineOutputModeNormal)
	verbose, verboseFinal := run(protocolcli.MachineOutputModeVerbose)
	if bytes.Count(normal, []byte{'\n'}) >= bytes.Count(verbose, []byte{'\n'}) {
		t.Fatalf("normal records = %d, verbose records = %d", bytes.Count(normal, []byte{'\n'}), bytes.Count(verbose, []byte{'\n'}))
	}
	if normalFinal.Run.Outcome != verboseFinal.Run.Outcome || normalFinal.Run.ExitCode != verboseFinal.Run.ExitCode || normalFinal.Run.Counts != verboseFinal.Run.Counts {
		t.Fatalf("mode changed verdict: normal=%+v verbose=%+v", normalFinal.Run, verboseFinal.Run)
	}
	if normalFinal.MachineOutput.Elided.Ordinary.Records == 0 || verboseFinal.MachineOutput.Elided.Ordinary.Records != 0 {
		t.Fatalf("unexpected elision: normal=%+v verbose=%+v", normalFinal.MachineOutput.Elided, verboseFinal.MachineOutput.Elided)
	}
}

func TestMachineStreamNormalSuppressesDebugTranscriptAndVerboseRetainsIt(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-machine-detail", "normal-suppresses-and-verbose-admits-debug-detail")
	type stream struct {
		live     []byte
		artifact []byte
		final    protocolcli.BoundedSessionEndRecord
	}
	run := func(verbose bool) stream {
		t.Helper()
		var live bytes.Buffer
		session, err := workspace_state.NewSession(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		renderer := WithSessionRecording(
			newMachineV2Renderer(&live, "test", true),
			session,
			verbose,
		)
		job := makeRendererTestJob()
		result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
		event := func(raw string) jobs.RawJobEvent {
			t.Helper()
			parsed, ok := jobmodel.ParseRawEvent(raw)
			if !ok {
				t.Fatalf("parse runtime event: %s", raw)
			}
			return parsed
		}
		renderer.Start([]*jobs.ScheduledJob{job})
		renderer.JobStart(job)
		renderer.JobEvent(job, event(`{"v":2,"type":"log","level":"debug","message":"passing test transcript"}`))
		renderer.JobEvent(job, event(`{"v":2,"type":"summary","message":"1/1 passed"}`))
		renderer.JobComplete(job, result)
		renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
		session.Close()

		artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
		if err != nil {
			t.Fatal(err)
		}
		if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
			t.Fatalf("bounded stream violations: %+v", violations)
		}
		lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
		var final protocolcli.BoundedSessionEndRecord
		if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
			t.Fatal(err)
		}
		return stream{live: append([]byte(nil), live.Bytes()...), artifact: artifact, final: final}
	}

	normal := run(false)
	if bytes.Contains(normal.live, []byte("passing test transcript")) {
		t.Fatal("normal live stream contains debug-level passing test detail")
	}
	if !bytes.Contains(normal.live, []byte("1/1 passed")) {
		t.Fatal("normal live stream omitted the outcome summary")
	}
	if !bytes.Contains(normal.artifact, []byte("passing test transcript")) {
		t.Fatal("normal session artifact omitted the complete test transcript")
	}
	if normal.final.MachineOutput.Elided.Ordinary.Records != 1 || normal.final.MachineOutput.Elided.Ordinary.Bytes == 0 {
		t.Fatalf("normal debug elision accounting = %+v", normal.final.MachineOutput.Elided.Ordinary)
	}

	verbose := run(true)
	if !bytes.Contains(verbose.live, []byte("passing test transcript")) {
		t.Fatal("verbose live stream omitted debug-level passing test detail")
	}
	if verbose.final.MachineOutput.Elided.Ordinary != (protocolcli.MachineOutputElision{}) {
		t.Fatalf("verbose ordinary elision = %+v, want zero", verbose.final.MachineOutput.Elided.Ordinary)
	}
	if normal.final.Run.Outcome != verbose.final.Run.Outcome || normal.final.Run.ExitCode != verbose.final.Run.ExitCode || normal.final.Run.Counts != verbose.final.Run.Counts {
		t.Fatalf("mode changed verdict: normal=%+v verbose=%+v", normal.final.Run, verbose.final.Run)
	}
}

func TestMachineStreamReservesNormalizedFailedResultAfterOrdinaryExhaustion(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "bounded-machine-detail", "failure-capacity-is-reserved-after-exhaustion")
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, false)
	job := makeRendererTestJob()
	failed := &jobs.JobResult{Status: string(jobs.TaskStatusFailed), Error: &jobs.JobError{Message: "tests failed"}}

	renderer.Start([]*jobs.ScheduledJob{job})
	for i := 0; i < 800; i++ {
		renderer.JobEvent(job, jobs.RawJobEvent{Data: map[string]any{
			"type": "log", "level": "info", "message": "ordinary",
		}})
	}
	resultEvent, ok := jobmodel.ParseRawEvent(`{"v":2,"type":"result","data":{"status":"FAILED"}}`)
	if !ok {
		t.Fatal("runtime v2 failed result did not parse")
	}
	if _, nested := resultEvent.Data["data"]; nested || resultEvent.Data["status"] != "FAILED" {
		t.Fatalf("normalized result event = %#v, want flat FAILED status", resultEvent.Data)
	}
	renderer.JobEvent(job, resultEvent)
	renderer.JobComplete(job, failed)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): failed}, jobs.SessionOutcome{})
	session.Close()

	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(live.Bytes(), []byte(`"status":"FAILED","type":"result"`)) {
		t.Fatal("normalized failed result did not consume the failure reserve")
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("normalized failed result stream violations: %+v", violations)
	}
}

func TestMachineStreamEmitsOneCanonicalReleaseSetResultBeforeSyntheticTerminal(t *testing.T) {
	const resultKey = "putnami:publish~release-set"
	outcome := releaseSetRendererTestOutcome("a")

	for _, saturated := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "ordinary-budget-exhausted"}[saturated], func(t *testing.T) {
			var live bytes.Buffer
			session, err := workspace_state.NewSession(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			renderer := WithSessionRecording(newMachineV2Renderer(&live, "publish", true), session, false)
			job := makeRendererTestJob()
			result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
			releaseResult := &jobs.JobResult{
				Status: string(jobs.TaskStatusSuccess),
				Data:   extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: outcome},
			}

			renderer.Start([]*jobs.ScheduledJob{job})
			if saturated {
				for i := 0; i < 800; i++ {
					renderer.JobEvent(job, jobs.RawJobEvent{Data: extension.ParamMap{
						"type": "log", "level": "info", "message": strings.Repeat("ordinary publish detail ", 100),
					}})
				}
			}
			renderer.JobComplete(job, result)
			renderer.Finish(map[string]*jobs.JobResult{job.Key(): result, resultKey: releaseResult}, jobs.SessionOutcome{})
			session.Close()

			artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
			if err != nil {
				t.Fatal(err)
			}
			if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
				t.Fatalf("release-set stream violations: %+v", violations)
			}

			liveRecords, liveLines := parseOuterV2Records(t, live.Bytes())
			artifactRecords, artifactLines := parseOuterV2Records(t, artifact)
			liveIndex, liveEventLine := soleReleaseSetOuterEvent(t, liveRecords, liveLines)
			artifactIndex, artifactEventLine := soleReleaseSetOuterEvent(t, artifactRecords, artifactLines)
			if !bytes.Equal(liveEventLine, artifactEventLine) {
				t.Fatalf("stdout/artifact release-set records differ\nstdout:   %s\nartifact: %s", liveEventLine, artifactEventLine)
			}
			if liveIndex >= len(liveRecords)-1 || liveRecords[len(liveRecords)-1].Record != protocolcli.RecordSessionEnd {
				t.Fatal("release-set result was not emitted before stdout session:end")
			}

			identity := jobs.TaskIdentityOfKey(resultKey)
			if got := artifactRecords[artifactIndex].Identity; got == nil || *got != identity {
				t.Fatalf("release-set identity = %#v, want canonical fallback %#v", got, identity)
			}
			endIndex := -1
			for i, record := range artifactRecords {
				if record.Record == protocolcli.RecordTaskEnd && record.Identity != nil && record.Identity.Key == identity.Key {
					endIndex = i
					break
				}
			}
			if endIndex < 0 || artifactIndex >= endIndex || endIndex >= len(artifactRecords)-1 {
				t.Fatalf("release-set ordering: event=%d task:end=%d records=%d", artifactIndex, endIndex, len(artifactRecords))
			}

			payload, err := json.Marshal(runtimeproto.ResultData{
				Status: runtimeproto.ResultOK,
				Data:   extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: &outcome},
			})
			if err != nil {
				t.Fatal(err)
			}
			wantEvent, err := json.Marshal(runtimeproto.Event{V: runtimeproto.ProtocolVersion, Type: runtimeproto.EventResult, Data: payload})
			if err != nil {
				t.Fatal(err)
			}
			var wantEventObject extension.ParamMap
			if err := json.Unmarshal(wantEvent, &wantEventObject); err != nil {
				t.Fatal(err)
			}
			wantEvent, err = json.Marshal(wantEventObject)
			if err != nil {
				t.Fatal(err)
			}
			gotEvent, err := json.Marshal(artifactRecords[artifactIndex].Event)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(gotEvent, wantEvent) {
				t.Fatalf("inner event = %s, want canonical runtime v1 %s", gotEvent, wantEvent)
			}
		})
	}
}

func TestMachineStreamReleaseSetResultFailsClosed(t *testing.T) {
	valid := releaseSetRendererTestOutcome("b")
	tests := []struct {
		name    string
		results map[string]*jobs.JobResult
	}{
		{name: "zero", results: map[string]*jobs.JobResult{"publish": {Status: string(jobs.TaskStatusSuccess)}}},
		{name: "dry-run", results: nil},
		{name: "duplicate", results: map[string]*jobs.JobResult{
			"publish-a": {Status: string(jobs.TaskStatusSuccess), Data: extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: valid}},
			"publish-b": {Status: string(jobs.TaskStatusSuccess), Data: extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: valid}},
		}},
		{name: "failed-result", results: map[string]*jobs.JobResult{
			"publish": {Status: string(jobs.TaskStatusFailed), Data: extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: valid}},
		}},
		{name: "malformed", results: map[string]*jobs.JobResult{
			"publish": {Status: string(jobs.TaskStatusSuccess), Data: extension.ParamMap{runtimeproto.ReleaseSetResultDataKey: extension.ParamMap{"protocolVersion": 1}}},
		}},
		{name: "cas-conflict", results: map[string]*jobs.JobResult{
			"publish": {Status: string(jobs.TaskStatusFailed), Error: &jobs.JobError{Message: "release-set channel CAS conflict"}},
		}},
	}

	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			var live bytes.Buffer
			renderer := WithoutSessionArtifact(newMachineV2Renderer(&live, "publish", true))
			renderer.Start(nil)
			renderer.Finish(testCase.results, jobs.SessionOutcome{})
			records, lines := parseOuterV2Records(t, live.Bytes())
			for i, record := range records {
				if isReleaseSetOuterEvent(record) {
					t.Fatalf("failure path emitted release-set evidence at record %d: %s", i, lines[i])
				}
			}
		})
	}
}

func releaseSetRendererTestOutcome(hexDigit string) distribution.ReleaseSetPublishOutcome {
	hex := strings.Repeat(hexDigit, 64)
	ref := distribution.ReleaseSetRef{ID: "rs_" + hex, Digest: "sha256:" + hex}
	return distribution.ReleaseSetPublishOutcome{
		ProtocolVersion: distribution.ProtocolVersion,
		Namespace:       "putnami",
		Ref:             ref,
		// One publication, several channels, one immutable set: every channel
		// this release advanced reports the same ref at its own generation.
		Channels: map[string]*distribution.ChannelHead{
			"canary": {Ref: ref, Generation: 5},
			"next":   {Ref: ref, Generation: 2},
		},
	}
}

func parseOuterV2Records(t *testing.T, stream []byte) ([]protocolcli.SessionStreamRecord, [][]byte) {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(stream), []byte{'\n'})
	records := make([]protocolcli.SessionStreamRecord, 0, len(lines))
	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		var record protocolcli.SessionStreamRecord
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("parse outer v2 record %s: %v", line, err)
		}
		records = append(records, record)
	}
	return records, lines
}

func soleReleaseSetOuterEvent(
	t *testing.T,
	records []protocolcli.SessionStreamRecord,
	lines [][]byte,
) (int, []byte) {
	t.Helper()
	index := -1
	for i, record := range records {
		if !isReleaseSetOuterEvent(record) {
			continue
		}
		if index >= 0 {
			t.Fatalf("duplicate release-set task:event at records %d and %d", index, i)
		}
		index = i
	}
	if index < 0 {
		t.Fatal("outer v2 stream has no release-set task:event")
	}
	return index, lines[index]
}

func isReleaseSetOuterEvent(record protocolcli.SessionStreamRecord) bool {
	if record.Record != protocolcli.RecordTaskEvent || record.Event["type"] != string(runtimeproto.EventResult) {
		return false
	}
	result, _ := record.Event["data"].(extension.ParamMap)
	data, _ := result["data"].(extension.ParamMap)
	_, present := data[runtimeproto.ReleaseSetResultDataKey]
	return present
}

func TestSanitizedMachineRecordPreservesNestedIntegerLexemeAboveFloatPrecision(t *testing.T) {
	const exact = int64(9007199254740993)
	record := protocolcli.SessionStreamRecord{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		Record:          protocolcli.RecordTaskEvent,
		Time:            "2026-08-21T00:00:00Z",
		Event: map[string]any{
			"type": "metric",
			"nested": map[string]any{
				"exact": exact,
			},
		},
	}
	line, err := sanitizedMachineRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(line, []byte(`"exact":9007199254740993`)) {
		t.Fatalf("sanitized record changed integer lexeme: %s", line)
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var decoded map[string]any
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	event := decoded["event"].(map[string]any)
	nested := event["nested"].(map[string]any)
	if got := nested["exact"].(json.Number).String(); got != "9007199254740993" {
		t.Fatalf("nested integer = %s, want exact source lexeme", got)
	}
}

func TestMachineBatchStreamRecordsPhysicalEventsOnceUnderWorkspaceIdentity(t *testing.T) {
	var live bytes.Buffer
	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	renderer := WithSessionRecording(newMachineV2Renderer(&live, "test", true), session, false)
	job := makeRendererTestJob()
	result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
	renderer.Start([]*jobs.ScheduledJob{job})
	batchRenderer, ok := renderer.(interface {
		BatchJobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)
		BatchMemberJobEvent(*jobs.ScheduledJob, jobs.RawJobEvent)
	})
	if !ok {
		t.Fatalf("renderer %T does not implement batch recording", renderer)
	}
	for i := 0; i < 800; i++ {
		batchRenderer.BatchJobEvent(job, jobs.RawJobEvent{Data: map[string]any{
			"type": "log", "level": "info", "message": "physical-batch-event",
		}})
	}
	batchRenderer.BatchMemberJobEvent(job, jobs.RawJobEvent{Data: map[string]any{
		"type": "log", "level": "info", "message": "projected-member-duplicate",
	}})
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	session.Close()

	artifact, err := os.ReadFile(filepath.Join(session.Dir(), protocolcli.MachineOutputArtifactPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(artifact, []byte("physical-batch-event")); got != 800 {
		t.Fatalf("artifact physical batch events = %d, want all 800", got)
	}
	for name, data := range map[string][]byte{"live": live.Bytes(), "artifact": artifact} {
		if bytes.Contains(data, []byte("projected-member-duplicate")) {
			t.Fatalf("%s contains duplicate member projection", name)
		}
		if !bytes.Contains(data, []byte(`"scope":"workspace","project":{"id":"/workspace","name":"workspace"}`)) {
			t.Fatalf("%s does not use the stable workspace batch identity", name)
		}
	}
	if violations := protocolcli.ValidateSessionStream(live.Bytes(), artifact); len(violations) != 0 {
		t.Fatalf("batch stream violations: %+v", violations)
	}
}

func TestMachineStreamWithoutArtifactFallsBackWithoutFalseReference(t *testing.T) {
	var live bytes.Buffer
	renderer := WithoutSessionArtifact(newMachineV2Renderer(&live, "build", true))
	job := makeRendererTestJob()
	result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
	renderer.Start([]*jobs.ScheduledJob{job})
	for i := 0; i < 1100; i++ {
		renderer.JobEvent(job, jobs.RawJobEvent{Data: map[string]any{
			"type": "log", "level": "info", "message": "complete fallback",
		}})
	}
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	if got := bytes.Count(live.Bytes(), []byte("complete fallback")); got != 1100 {
		t.Fatalf("fallback events = %d, want complete 1100", got)
	}
	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
	var final protocolcli.SessionStreamRecord
	if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.Run == nil || final.MachineOutput != nil || bytes.Contains(lines[len(lines)-1], []byte("unrecorded")) {
		t.Fatalf("fallback final makes a false artifact claim: %s", lines[len(lines)-1])
	}
}

func TestMachineStreamSurfacesArtifactAppendFailureWithoutBoundedClaim(t *testing.T) {
	var live bytes.Buffer
	renderer := newMachineV2Renderer(&live, "build", true)
	renderer.configureMachineSession("session", func([]byte) error {
		return os.ErrPermission
	}, protocolcli.MachineOutputModeNormal)
	job := makeRendererTestJob()
	result := &jobs.JobResult{Status: string(jobs.TaskStatusSuccess)}
	renderer.Start([]*jobs.ScheduledJob{job})
	for i := 0; i < 1100; i++ {
		renderer.JobEvent(job, jobs.RawJobEvent{Data: map[string]any{"type": "log", "message": "event"}})
	}
	renderer.JobComplete(job, result)
	renderer.Finish(map[string]*jobs.JobResult{job.Key(): result}, jobs.SessionOutcome{})
	if err := SessionRecordingError(renderer); err == nil {
		t.Fatal("artifact append failure was not surfaced")
	}
	if got := bytes.Count(live.Bytes(), []byte(`"record":"task:event"`)); got != 1100 {
		t.Fatalf("fallback events = %d, want complete 1100", got)
	}
	lines := bytes.Split(bytes.TrimSpace(live.Bytes()), []byte{'\n'})
	var final protocolcli.SessionStreamRecord
	if err := json.Unmarshal(lines[len(lines)-1], &final); err != nil {
		t.Fatal(err)
	}
	if final.MachineOutput != nil || final.Run == nil {
		t.Fatalf("append failure final makes a bounded artifact claim: %s", lines[len(lines)-1])
	}
}
