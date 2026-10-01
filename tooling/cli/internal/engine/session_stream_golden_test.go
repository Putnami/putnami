package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/fixtureproc"
	"go.putnami.dev/tooling/cli/internal/sessionreporter"
	"go.putnami.dev/tooling/cli/internal/sessionstream"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// sessionStreamBaseGolden is what reporterFixture(fixtureproc.Program{}), a
// task that succeeds at once, records with the session reporter selected,
// produced by the base recorder without the session event stream and
// normalized by normalizeRecordedSession. Every byte
// outside the declared volatile members is the base recorder's.
const sessionStreamBaseGolden = `session.json
{"protocolVersion":2,"sessionId":"volatile","startTime":"volatile","endTime":"volatile","commands":["test"],"selection":{"mode":"projects","scoped":true,"projects":["/app"]},"placement":{"requested":"local","actual":"local"},"run":{"outcome":"success","exitCode":0,"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":0,"cpu":{"allocatedMillicores":0,"allocatedSource":"volatile","allocatedMs":0,"actualMs":0,"executions":1}},"tasks":[{"identity":{"key":"/app:test~test","scope":"project","project":{"id":"/app","name":"app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@test/reporter","version":"1.0.0"}},"executionId":"volatile","status":"success","reuse":"none","exitCode":0,"durationMs":0,"taskWallMs":0}],"executions":"platform","environment":"platform","scheduler":{"parallel":{"mode":"auto","workers":1,"logicalCpu":0,"cpuCapacity":0,"memoryTotalMiB":0,"memoryUsableMiB":0,"memoryCapWorkers":0,"plannedJobs":1,"heavyJobPermille":0},"heavyJobRatio":0,"criticalPath":{"durationMs":0,"chain":[{"job":"/app:test~test","command":"test","durationMs":0}]},"cpuBudgets":[{"job":"/app:test~test","weight":1,"expectedCpuMs":0,"budget":0}],"resourceReservations":[{"job":"/app:test~test","cpu":1,"memoryMiB":512}]}}
events.jsonl
{"protocolVersion":2,"record":"task:event","time":"volatile","identity":{"key":"/workspace:session-events","scope":"workspace","project":{"id":"/workspace","name":"workspace"},"task":{"name":"session-events","command":"session","kind":"session-event"},"provider":{"extension":"@putnami/cli"}},"event":{"cpuBudget":"critical-path","cpuCapacity":0,"heavyJobPermille":0,"logicalCpu":0,"memoryCapWorkers":0,"memoryTotalMiB":0,"memoryUsableMiB":0,"mode":"auto","plannedJobs":1,"sessionId":"volatile","type":"scheduler:parallel","workers":1}}
{"protocolVersion":2,"record":"task:start","time":"volatile","identity":{"key":"/app:test~test","scope":"project","project":{"id":"/app","name":"app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@test/reporter","version":"1.0.0"}}}
{"protocolVersion":2,"record":"task:end","time":"volatile","identity":{"key":"/app:test~test","scope":"project","project":{"id":"/app","name":"app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@test/reporter","version":"1.0.0"}},"task":{"identity":{"key":"/app:test~test","scope":"project","project":{"id":"/app","name":"app"},"task":{"name":"test~test","command":"test","step":"test","kind":"test-exec"},"provider":{"extension":"@test/reporter","version":"1.0.0"}},"executionId":"volatile","status":"success","reuse":"none","exitCode":0,"durationMs":0,"taskWallMs":0}}
{"protocolVersion":2,"record":"session:end","time":"volatile","run":{"outcome":"success","exitCode":0,"counts":{"total":1,"succeeded":1,"failed":0,"canceled":0,"skipped":0},"reuse":{"localCache":0,"remoteCache":0,"coalesced":0},"durationMs":0},"machineOutput":{"mode":"normal","sanitization":"terminal-safe-redacted-v1","budget":{"maxBytes":1048576,"maxRecords":1024,"failureReserveBytes":262144,"failureReserveRecords":256,"finalReserveBytes":16384,"finalReserveRecords":1},"elided":{"ordinary":{"records":0,"bytes":0},"failure":{"records":0,"bytes":0}},"artifact":{"sessionId":"volatile","path":"events.jsonl","retention":"session"}}}
`

// TestSessionStreamSubscribersKeepRecordedBytesIdenticalToBase is the
// acceptance golden. A run with no subscriber and a run with the live reporter
// subscriber plus the post-final ledger reader both record the base CLI's
// session.json and events.jsonl; only subscribers.json and the
// subscribers.lock that serializes its writers are added.
func TestSessionStreamSubscribersKeepRecordedBytesIdenticalToBase(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "stream-subscribers-leave-recorded-bytes-unchanged")
	t.Setenv(protocolcli.ParentSessionEnv, "")
	recorded := map[bool][2][]byte{}
	for _, subscribed := range []bool{false, true} {
		root, req := reporterFixture(t, fixtureproc.Program{})
		if !subscribed {
			t.Setenv(protocolcli.SessionReporterEnv, "")
		}
		result, err := New().Run(context.Background(), req, discardEvents{})
		if err != nil || result.ExitCode != ExitSuccess {
			t.Fatalf("subscribed=%v: exit=%d err=%v", subscribed, result.ExitCode, err)
		}
		id := workspace_state.NewSessionStore(root).LatestID()
		dir := filepath.Join(root, ".putnami", "sessions", id)
		session, events := readRecordedArtifacts(t, dir)
		assertRecordedLayout(t, session, events)
		if got := normalizeRecordedSession(t, session, events); got != sessionStreamBaseGolden {
			t.Fatalf("subscribed=%v: recorded session differs from the base CLI\n--- got\n%s\n--- base\n%s", subscribed, got, sessionStreamBaseGolden)
		}
		want := []string{"events.jsonl", "plan.json", "session.json"}
		if subscribed {
			want = []string{"events.jsonl", "plan.json", "reporting.json", "reporting.lock", "session.json", protocolcli.SessionSubscribersFileName, "subscribers.lock"}
			verifyReporterArtifacts(t, root, id)
			readLedger(t, dir, id, events)
			evidence, err := sessionstream.ReadEvidence(dir)
			if err != nil {
				t.Fatal(err)
			}
			stream := sessionstream.Position{Offset: int64(len(events)), Records: int64(bytes.Count(events, []byte("\n")))}
			if evidence.SessionID != id || evidence.Stream != stream || len(evidence.Subscribers) != 1 ||
				evidence.Subscribers[0] != (protocolcli.SessionSubscriberEvidence{Name: protocolcli.SessionReporterCommand, Evidence: protocolcli.SubscriberEvidenceDelivered, Acknowledged: stream}) {
				t.Fatalf("subscriber evidence = %+v, want the reporter delivered over %+v", evidence, stream)
			}
		}
		if got := sessionDirectoryNames(t, dir); !slices.Equal(got, want) {
			t.Fatalf("subscribed=%v: session directory = %v, want %v", subscribed, got, want)
		}
		// The ledger reader and the evidence write ran after finalization; the
		// artifacts they read are still the bytes the run recorded.
		afterSession, afterEvents := readRecordedArtifacts(t, dir)
		if !bytes.Equal(afterSession, session) || !bytes.Equal(afterEvents, events) {
			t.Fatalf("subscribed=%v: a subscriber changed a recorded artifact", subscribed)
		}
		recorded[subscribed] = [2][]byte{session, events}
	}
	if normalizeRecordedSession(t, recorded[false][0], recorded[false][1]) != normalizeRecordedSession(t, recorded[true][0], recorded[true][1]) {
		t.Fatal("the subscribed run recorded different artifacts than the run without subscribers")
	}
}

// TestSessionStreamKilledSubscriberKeepsTheVerdictAndRecordsPartial kills the
// real reporter provider after it acknowledged part of the stream. The run keeps
// its own verdict, the reporter's evidence is partial with the unacknowledged
// records counted as lost, and a replay afterwards delivers the rest.
func TestSessionStreamKilledSubscriberKeepsTheVerdictAndRecordsPartial(t *testing.T) {
	spectest.Proves(t, "cli/native-session-reporting", "engine-lifecycle", "a-killed-subscriber-keeps-the-verdict-and-records-partial-evidence")
	for _, exit := range []int{0, 1} {
		t.Run(strconv.Itoa(exit), func(t *testing.T) {
			root, req := reporterFixture(t, fixtureproc.Program{WaitFor: []string{"release"}, Exit: exit})
			killed := make(chan error, 1)
			go func() { killed <- killProviderAfterFirstAcknowledgement(root) }()
			var result SessionResult
			var err error
			stderr := captureStderr(t, func() { result, err = New().Run(liveReportingContext(), req, discardEvents{}) })
			if killErr := <-killed; killErr != nil {
				t.Fatalf("kill: %v", killErr)
			}
			if err != nil || result.ExitCode != exit {
				t.Fatalf("verdict changed by a killed subscriber: exit=%d err=%v stderr=%s", result.ExitCode, err, stderr)
			}
			if !strings.Contains(stderr, "session reporting incomplete") {
				t.Fatalf("a killed reporter was not diagnosed: %s", stderr)
			}
			id := workspace_state.NewSessionStore(root).LatestID()
			dir := filepath.Join(root, ".putnami", "sessions", id)
			session, events := readRecordedArtifacts(t, dir)
			var document protocolcli.SessionFile
			if err := json.Unmarshal(session, &document); err != nil {
				t.Fatal(err)
			}
			wantOutcome := protocolcli.RunOutcomeSuccess
			if exit != 0 {
				wantOutcome = protocolcli.RunOutcomeFailure
			}
			if document.Run.Outcome != wantOutcome || document.Run.ExitCode != exit {
				t.Fatalf("recorded verdict = %s/%d, want %s/%d", document.Run.Outcome, document.Run.ExitCode, wantOutcome, exit)
			}
			evidence, err := sessionstream.ReadEvidence(dir)
			if err != nil {
				t.Fatal(err)
			}
			records := int64(bytes.Count(events, []byte("\n")))
			reporter := evidence.Subscribers[0]
			if len(evidence.Subscribers) != 1 || reporter.Name != protocolcli.SessionReporterCommand ||
				reporter.Evidence != protocolcli.SubscriberEvidencePartial || reporter.Acknowledged.Offset == 0 ||
				reporter.Lost == 0 || evidence.Stream != (sessionstream.Position{Offset: int64(len(events)), Records: records}) {
				t.Fatalf("evidence after a kill = %+v, want the reporter partial with lost records", evidence)
			}

			if err := os.Remove(filepath.Join(root, "dead")); err != nil {
				t.Fatal(err)
			}
			t.Setenv(protocolcli.SessionReporterEnv, "@test/reporter")
			t.Setenv(protocolcli.SessionReporterTokenEnv, "reporter-test-secret")
			if err := New().ReplaySession(context.Background(), root, req.Config, id); err != nil {
				t.Fatalf("replay after a kill: %v", err)
			}
			verifyReporterArtifacts(t, root, id)
			evidence, err = sessionstream.ReadEvidence(dir)
			if err != nil || evidence.Subscribers[0].Evidence != protocolcli.SubscriberEvidenceDelivered || evidence.Subscribers[0].Lost != 0 {
				t.Fatalf("evidence after replay = %+v, %v; want delivered", evidence, err)
			}
		})
	}
}

// killProviderAfterFirstAcknowledgement waits until the reporter's durable
// checkpoint holds an acknowledged events chunk, SIGKILLs the provider, marks
// it dead so a respawn cannot recover, and releases the waiting task.
//
// The provider that acknowledged wrote provider.pid before it read its first
// chunk, and the reporter runs one provider at a time, so the pid names a live
// process unless the reporter already stopped it. A checkpoint that records a
// delivery error says the reporter did, and the kill would then prove nothing:
// that is a failure, not a process that is already gone.
func killProviderAfterFirstAcknowledgement(root string) error {
	defer func() { _ = os.WriteFile(filepath.Join(root, "release"), nil, 0o600) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		checkpoints, _ := filepath.Glob(filepath.Join(root, ".putnami", "sessions", "*", sessionreporter.StateFile))
		if len(checkpoints) != 1 {
			continue
		}
		data, err := os.ReadFile(checkpoints[0])
		var state struct {
			Events struct {
				Offset int64 `json:"offset"`
			} `json:"events"`
			Error string `json:"error"`
		}
		if err != nil || json.Unmarshal(data, &state) != nil || state.Events.Offset == 0 {
			continue
		}
		if state.Error != "" {
			return fmt.Errorf("the reporter stopped before the kill: checkpoint error %q", state.Error)
		}
		pid, err := os.ReadFile(filepath.Join(root, "provider.pid"))
		if err != nil {
			return err
		}
		n, err := strconv.Atoi(string(pid))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(root, "dead"), nil, 0o600); err != nil {
			return err
		}
		provider, err := os.FindProcess(n)
		if err != nil {
			return fmt.Errorf("find provider %d: %w", n, err)
		}
		if err := provider.Kill(); err != nil {
			return fmt.Errorf("kill provider %d: %w", n, err)
		}
		return nil
	}
	return fmt.Errorf("the reporter never acknowledged an events chunk")
}

func readRecordedArtifacts(t *testing.T, dir string) (session, events []byte) {
	t.Helper()
	session, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil {
		t.Fatal(err)
	}
	events, err = os.ReadFile(filepath.Join(dir, sessionstream.EventsFile))
	if err != nil {
		t.Fatal(err)
	}
	return session, events
}

// readLedger is the post-final reader: subscribe from 0 on the finalized log and
// read every record. The records, joined with their LFs, are the file.
func readLedger(t *testing.T, dir, id string, events []byte) {
	t.Helper()
	log, err := sessionstream.Open(dir, id)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := log.Subscribe("ledger", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ledger.Close() }()
	var joined []byte
	for ordinal := int64(0); ; ordinal++ {
		record, ok, err := ledger.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if !record.Terminated || record.Position.Records != ordinal || record.Position.Offset != int64(len(joined)) {
			t.Fatalf("ledger record %d = %+v", ordinal, record.Position)
		}
		joined = append(append(joined, record.Data...), '\n')
	}
	if !bytes.Equal(joined, events) {
		t.Fatal("the ledger reader did not read the recorded stream")
	}
}

// assertRecordedLayout pins the bytes the normalizer does not: session.json is
// the two-space MarshalIndent layout, and every events.jsonl record is one
// compact JSON document ending in LF.
func assertRecordedLayout(t *testing.T, session, events []byte) {
	t.Helper()
	var compact, indented bytes.Buffer
	if err := json.Compact(&compact, session); err != nil {
		t.Fatal(err)
	}
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(indented.Bytes(), session) {
		t.Fatal("session.json layout differs from the recorder's indentation")
	}
	if len(events) == 0 || events[len(events)-1] != '\n' {
		t.Fatal("events.jsonl does not end with a record terminator")
	}
	for _, line := range bytes.Split(bytes.TrimSuffix(events, []byte("\n")), []byte("\n")) {
		compact.Reset()
		if err := json.Compact(&compact, line); err != nil || !bytes.Equal(compact.Bytes(), line) {
			t.Fatalf("events.jsonl record is not one compact JSON document: %s", line)
		}
	}
}

func sessionDirectoryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}

// recordedVolatileNumbers are the members whose NUMBER value is measured per
// run or per machine: wall and CPU clocks (the cache's served and binding
// times included), and the host's CPU and memory
// capacity the scheduler sized itself from. Only a number is replaced, so a
// member that shares a name with an object (machineOutput.budget) stays pinned.
var recordedVolatileNumbers = map[string]bool{
	"durationMs": true, "taskWallMs": true, "allocatedMs": true, "actualMs": true,
	"spawnToFirstEventMs": true, "expectedCpuMs": true, "logicalCpu": true,
	"cpuCapacity": true, "memoryTotalMiB": true, "memoryUsableMiB": true,
	"memoryCapWorkers": true, "allocatedMillicores": true, "budget": true,
	"servedMs": true, "bindingsMs": true, "localServedMs": true, "localBindingsMs": true,
}

// recordedVolatileStrings are the members whose STRING value names this run
// or this host: clocks, the random session id, and the process-wide execution
// counter.
var recordedVolatileStrings = map[string]bool{
	"time": true, "startTime": true, "endTime": true, "sessionId": true,
	"executionId": true, "allocatedSource": true,
}

// recordedPlatformSubtrees are replaced whole. Their member SET depends on the
// platform (cgroup, PSI and block-IO counters exist on Linux only), so neither
// their values nor their shape can be pinned by a committed golden.
var recordedPlatformSubtrees = map[string]bool{"environment": true, "executions": true}

// recordedAmbientMembers are removed: they describe the checkout, the parent
// CLI or the machine load the test happens to run under, never the work the
// fixture recorded. The scheduler omits readyWait when every ready job started
// within the same millisecond, so a loaded machine adds it and an idle one
// does not.
var recordedAmbientMembers = map[string]bool{"git": true, "tree": true, "parentSessionId": true, "readyWait": true}

// normalizeRecordedDocument re-emits one recorded JSON document with every
// member in its recorded order and every value byte-for-byte, except the
// declared volatile members above. The output is compact, so a caller compares
// layout separately (see assertRecordedLayout).
func normalizeRecordedDocument(t *testing.T, data []byte) []byte {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var out bytes.Buffer
	if err := normalizeRecordedValue(decoder, &out, ""); err != nil {
		t.Fatalf("normalize recorded document: %v\n%s", err, data)
	}
	if _, err := decoder.Token(); err != io.EOF {
		t.Fatalf("recorded document has trailing data: %s", data)
	}
	return out.Bytes()
}

func normalizeRecordedValue(decoder *json.Decoder, out *bytes.Buffer, member string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch value := token.(type) {
	case json.Delim:
		if recordedPlatformSubtrees[member] {
			if err := skipRecordedValue(decoder); err != nil {
				return err
			}
			out.WriteString(`"platform"`)
			return nil
		}
		return normalizeRecordedContainer(decoder, out, value)
	case json.Number:
		if recordedVolatileNumbers[member] {
			out.WriteString("0")
			return nil
		}
		out.WriteString(value.String())
	case string:
		if recordedVolatileStrings[member] {
			value = "volatile"
		}
		encoded, _ := json.Marshal(value)
		out.Write(encoded)
	case bool:
		fmt.Fprint(out, value)
	case nil:
		out.WriteString("null")
	}
	return nil
}

func normalizeRecordedContainer(decoder *json.Decoder, out *bytes.Buffer, open json.Delim) error {
	object := open == '{'
	out.WriteRune(rune(open))
	first := true
	for decoder.More() {
		member := ""
		if object {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			member, _ = key.(string)
			if recordedAmbientMembers[member] {
				if err := skipRecordedMember(decoder); err != nil {
					return err
				}
				continue
			}
		}
		if !first {
			out.WriteByte(',')
		}
		first = false
		if object {
			encoded, _ := json.Marshal(member)
			out.Write(encoded)
			out.WriteByte(':')
		}
		if err := normalizeRecordedValue(decoder, out, member); err != nil {
			return err
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return err
	}
	out.WriteRune(rune(closing.(json.Delim)))
	return nil
}

func skipRecordedMember(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if _, ok := token.(json.Delim); ok {
		return skipRecordedValue(decoder)
	}
	return nil
}

// skipRecordedValue consumes the rest of a container whose opening delimiter
// was already read.
func skipRecordedValue(decoder *json.Decoder) error {
	depth := 1
	for depth > 0 {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			if delim == '{' || delim == '[' {
				depth++
			} else {
				depth--
			}
		}
	}
	return nil
}

// normalizeRecordedSession normalizes a recorded session.json and a recorded
// events.jsonl into one comparable text: the session document, then one
// normalized line per recorded event line.
func normalizeRecordedSession(t *testing.T, session, events []byte) string {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("session.json\n")
	out.Write(normalizeRecordedDocument(t, session))
	out.WriteString("\nevents.jsonl\n")
	for _, line := range bytes.SplitAfter(events, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		out.Write(normalizeRecordedDocument(t, bytes.TrimSuffix(line, []byte("\n"))))
		out.WriteByte('\n')
	}
	return out.String()
}
