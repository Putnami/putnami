package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// END-TO-END WIRE GOLDENS: the shared fixture harness and the retained
// version-1 session-event reader fixture.
//
// An earlier change captured this corpus from the code it was migrating,
// so the tally-loop migration had to land with the v1 bytes unmodified. That
// proof is spent: a later change DELETED the v1 emitters, and
// with them the four v1 golden pairs (output.json / output.jsonl for both
// fixtures) plus the v1 plan.json and session.json. The bytes they pinned are
// unreachable — no emitter can produce them — so keeping the files would pin
// nothing.
//
// What is left here is:
//
//   - the FIXTURE HARNESS the v2 corpus drives (wire_golden_v2_test.go):
//     decoding, materializing a run into the exact values production passes to
//     the renderers, the replay order, and the normalization/golden IO;
//   - the historical events.jsonl golden. New writers use the complete
//     sanitized v2 task stream, while sessions inspect/gate retain this
//     v1 job:end shape as an on-disk compatibility fixture. It is written
//     through AppendEvent, which remains only for that reader evidence.
//
// A real CLI subprocess was rejected on purpose: the suite runs with race
// (tooling/cli/putnami.json) where a re-exec costs ≈1s, and a
// subprocess run cannot be made to contain a guaranteed coalesced task, a
// remote-cache hit and a canceled task at once without becoming flaky.
//
// # Determinism
//
// Everything the fixture can inject IS injected — task statuses, reuse kind,
// per-task durations, error messages, project/job identity, runtime event
// payloads and their timestamps, session id, session start time, scheduler
// report, cache snapshot. Nothing is sorted or scrubbed after the fact.
//
// Only fields production computes from the wall clock are normalized, each
// after an assertion that pins the very thing the field is for, so drift cannot
// hide behind the placeholder; and each must match EXACTLY ONCE, so a field
// that disappears or forks fails the count instead of silently passing. The
// surviving list lives in wire_golden_v2_test.go's header, beside the documents
// that still carry them. Per-task durations, per-event times, taskWallMs and
// events.jsonl timestamps are NOT normalized: they come from the fixture and
// are pinned byte-for-byte.
//
// # What this corpus does NOT pin (state it, do not discover it in review)
//
//   - The current v2 session-event PRODUCER. Its complete live/artifact pair is
//     pinned in internal/output; this corpus reproduces only the retired v1 key
//     set so an existing session remains readable.
//   - Cache-hit / coalesced BRANCH PRECEDENCE. jobs.JobResult.MarkReuse makes
//     CacheHit and Coalesced mutually exclusive, and loadWireRun asserts the
//     fixture honors that, so no fixture task can set both.
//   - The git block of session.json: the writer captures it from the real
//     repository, so the corpus records a run with wsRoot "" (block omitted).
//     internal/workspace_state/session_test.go owns that path.

var updateWireGoldens = flag.Bool("update-wire-goldens", false,
	"rewrite internal/cli/testdata/wire/golden/* from the current emission path")

// Normalization sentinels. Deliberately unreachable values, so a reader can
// tell a placeholder from a measurement at a glance while the goldens stay
// decodable as the real wire structs.
const (
	wireGoldenTime       = "1970-01-01T00:00:00Z"
	wireGoldenDurationMs = -1
)

// ---------------------------------------------------------------------------
// Fixture decoding
//
// The fixture is a committed JSON document rather than Go literals for two
// reasons: the inputs of the proof are then bytes too (they cannot drift as a
// side effect of an unrelated edit), and the untyped payload maps
// (RawJobEvent.Data, SessionEvent.Data) are decoded rather than spelled out,
// which keeps this file clear of the untyped-map occurrences
// structural_baseline_test.go counts — including in comments, so this one
// deliberately does not name the type.
// ---------------------------------------------------------------------------

type wireFixture struct {
	Name      string             `json:"name"`
	Command   string             `json:"command"`
	Commands  []string           `json:"commands"`
	SessionID string             `json:"sessionId"`
	StartTime string             `json:"startTime"`
	Outcome   wireFixtureOutcome `json:"outcome"`
	Scheduler *jobs.TuningReport `json:"scheduler"`
	// Cache is the remote-cache snapshot embedded in session.json. Nil models a
	// local-only run, where the writer must omit the key entirely.
	Cache *jobs.CacheStatsSnapshot `json:"cache"`
	Tasks []wireFixtureTask        `json:"tasks"`
}

type wireFixtureOutcome struct {
	Aborted   bool   `json:"aborted"`
	AbortedBy string `json:"abortedBy"`
}

type wireFixtureTask struct {
	Key            string   `json:"key"`
	ProjectID      string   `json:"projectId"`
	ProjectName    string   `json:"projectName"`
	Extension      string   `json:"extension"`
	Job            string   `json:"job"`
	InternalName   string   `json:"internalName"`
	Cacheable      bool     `json:"cacheable"`
	DependsOn      []string `json:"dependsOn"`
	SerializeAfter []string `json:"serializeAfter"`
	// ReportedToRenderer is false for a task that reached the result map without
	// ever being announced to the renderer — the case output/json.go handles in
	// appendMissingResultEntries, and which the jsonl stream reports only in its
	// session:end counts.
	ReportedToRenderer  bool                     `json:"reportedToRenderer"`
	Status              string                   `json:"status"`
	DurationMs          int64                    `json:"durationMs"`
	TaskWallMs          int64                    `json:"taskWallMs"`
	CPUTimeMs           int64                    `json:"cpuTimeMs"`
	SpawnToFirstEventMs int64                    `json:"spawnToFirstEventMs"`
	FirstEventObserved  bool                     `json:"firstEventObserved"`
	CacheHit            bool                     `json:"cacheHit"`
	Coalesced           bool                     `json:"coalesced"`
	Reuse               jobs.ReuseKind           `json:"reuse"`
	Error               *wireFixtureError        `json:"error"`
	Events              []wireFixtureEvent       `json:"events"`
	SessionEvent        *wireFixtureSessionEvent `json:"sessionEvent"`
}

type wireFixtureError struct {
	Message string `json:"message"`
	Code    string `json:"code"`
}

// wireFixtureEvent is one runtime event AFTER jobs.parseRawEvent has folded the
// flat top-level fields into Data. parseRawEvent is unexported, so the fixture
// reproduces its output shape (type/time/level/message are present both at the
// top level and inside data, exactly as the parser leaves them).
//
// Version is 2 on every fixture event, and loadWireRun enforces it: since B6c
// the parser accepts runtimeproto.MaxKnownProtocolVersion and NOTHING else, so a
// v1 event is not a stale input this corpus tolerates — it is an event
// parseRawEvent would have rejected, i.e. a state no production result can be
// in. The version never reaches a document (parseRawEvent excludes "v" from
// Data), which is exactly why a wrong stamp here would have gone unnoticed by
// every golden.
type wireFixtureEvent struct {
	Version int             `json:"v"`
	Type    string          `json:"type"`
	Time    string          `json:"time"`
	Level   string          `json:"level"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// wireFixtureSessionEvent is one recorded session event. Its Data mirrors
// jobs.Scheduler.emitSessionJobEnd key-for-key; that method is unexported in
// internal/jobs, so this corpus pins the session WRITER, not the producer.
type wireFixtureSessionEvent struct {
	Time string          `json:"time"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// wireRun is a fixture materialized into the exact values production passes to
// the renderers and the session writer.
type wireRun struct {
	fixture   wireFixture
	planned   []*jobs.ScheduledJob
	results   map[string]*jobs.JobResult
	outcome   jobs.SessionOutcome
	startTime time.Time
	events    map[string][]jobs.RawJobEvent
	session   []workspace_state.SessionEvent
}

func loadWireRun(t *testing.T, name string) *wireRun {
	t.Helper()

	path := filepath.Join("testdata", "wire", name+".fixture.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	var fixture wireFixture
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode fixture %s: %v", path, err)
	}

	startTime, err := time.Parse(time.RFC3339Nano, fixture.StartTime)
	if err != nil {
		t.Fatalf("fixture %s startTime: %v", path, err)
	}

	run := &wireRun{
		fixture:   fixture,
		results:   make(map[string]*jobs.JobResult, len(fixture.Tasks)),
		outcome:   jobs.SessionOutcome{Aborted: fixture.Outcome.Aborted, AbortedBy: fixture.Outcome.AbortedBy},
		startTime: startTime,
		events:    make(map[string][]jobs.RawJobEvent, len(fixture.Tasks)),
	}

	for i := range fixture.Tasks {
		task := fixture.Tasks[i]
		job := &jobs.ScheduledJob{
			Project:   &workspace.Project{ID: task.ProjectID, Name: task.ProjectName},
			Extension: &extension.ExtensionDescription{Name: task.Extension},
			JobDef: &extension.JobDefinition{
				Name:         task.Job,
				InternalName: task.InternalName,
				Cache:        task.Cacheable,
			},
			DependsOn:      task.DependsOn,
			SerializeAfter: task.SerializeAfter,
		}
		// The fixture cannot lie about identity: Key() is what every surface
		// keys on, so a mismatch would silently detach the goldens from the
		// plan and the result map.
		if job.Key() != task.Key {
			t.Fatalf("fixture task %d: Key() = %q, fixture says %q", i, job.Key(), task.Key)
		}
		if _, exists := run.results[task.Key]; exists {
			t.Fatalf("fixture task %d: duplicate key %q", i, task.Key)
		}

		result := &jobs.JobResult{
			Status:             task.Status,
			Duration:           time.Duration(task.DurationMs) * time.Millisecond,
			TaskWall:           time.Duration(task.TaskWallMs) * time.Millisecond,
			CPUTime:            time.Duration(task.CPUTimeMs) * time.Millisecond,
			SpawnToFirstEvent:  time.Duration(task.SpawnToFirstEventMs) * time.Millisecond,
			FirstEventObserved: task.FirstEventObserved,
			CacheHit:           task.CacheHit,
			Coalesced:          task.Coalesced,
			Reuse:              task.Reuse,
		}
		if task.Error != nil {
			result.Error = &jobs.JobError{Message: task.Error.Message, Code: task.Error.Code}
		}
		// The fixture states all three encodings (the Reuse kind and the two
		// booleans) and this asserts they agree — the invariant
		// jobs.JobResult.MarkReuse exists to hold. An earlier change exported that
		// writer with the result model, so the fixture COULD call it; it
		// deliberately does not, because a fixture built through the writer
		// could no longer disagree with itself and the check would pass
		// vacuously. Stating the encodings independently is what keeps a
		// fixture from describing a result production cannot produce.
		if got := result.ReuseKind(); got != task.Reuse {
			t.Fatalf("fixture task %q: reuse %q disagrees with the cacheHit/coalesced booleans (%q)",
				task.Key, task.Reuse, got)
		}

		// The events go on the RESULT, not only into the replay: production
		// results carry the stream the subprocess already parsed, and every
		// structured record a v2 document reports (a failure's diagnostics
		// above all) is extracted from exactly that field by
		// jobs.TaskResultOf. A harness that only replayed them through
		// JobEvent made every golden vacuous on records — no document in the
		// corpus could show a diagnostic, whatever the emitters did.
		events := decodeFixtureEvents(t, task)
		result.Events = events

		run.planned = append(run.planned, job)
		run.results[task.Key] = result
		if task.ReportedToRenderer {
			run.events[task.Key] = events
		}
		if task.SessionEvent != nil {
			event := workspace_state.SessionEvent{
				Time:   task.SessionEvent.Time,
				Type:   task.SessionEvent.Type,
				JobKey: task.Key,
			}
			if len(task.SessionEvent.Data) > 0 {
				if err := json.Unmarshal(task.SessionEvent.Data, &event.Data); err != nil {
					t.Fatalf("fixture task %q session event data: %v", task.Key, err)
				}
			}
			run.session = append(run.session, event)
		}
	}
	return run
}

func decodeFixtureEvents(t *testing.T, task wireFixtureTask) []jobs.RawJobEvent {
	t.Helper()
	if len(task.Events) == 0 {
		return nil
	}
	decoded := make([]jobs.RawJobEvent, 0, len(task.Events))
	for i, event := range task.Events {
		// A fixture event models parseRawEvent's OUTPUT, and the parser drops
		// anything that is not the one version it knows, so a fixture event
		// stamped otherwise describes a stream the CLI would never have parsed.
		if event.Version != runtimeproto.MaxKnownProtocolVersion {
			t.Fatalf("fixture task %q event %d: v=%d, but parseRawEvent only yields v=%d — "+
				"this event could not have reached a result",
				task.Key, i, event.Version, runtimeproto.MaxKnownProtocolVersion)
		}
		raw := jobs.RawJobEvent{
			Version: event.Version,
			Type:    event.Type,
			Time:    event.Time,
			Level:   event.Level,
			Message: event.Message,
		}
		if len(event.Data) > 0 {
			if err := json.Unmarshal(event.Data, &raw.Data); err != nil {
				t.Fatalf("fixture task %q event %d data: %v", task.Key, i, err)
			}
		}
		decoded = append(decoded, raw)
	}
	return decoded
}

// drive replays the run through a renderer in the same order jobs.Scheduler
// does: Start once, then per task JobStart → JobEvent* → JobComplete, then one
// Finish with the whole result map and the session outcome.
func (r *wireRun) drive(renderer jobs.Renderer) {
	renderer.Start(r.planned)
	for i, job := range r.planned {
		if !r.fixture.Tasks[i].ReportedToRenderer {
			continue
		}
		renderer.JobStart(job)
		for _, event := range r.events[job.Key()] {
			renderer.JobEvent(job, event)
		}
		renderer.JobComplete(job, r.results[job.Key()])
	}
	renderer.Finish(r.results, r.outcome)
}

// ---------------------------------------------------------------------------
// Goldens
// ---------------------------------------------------------------------------

// TestWireGoldenSessionEvents pins the historical version-1 events.jsonl shape.
// internal/commands/sessions/sessions_helpers.go still parses it, so a drift
// here either breaks an existing session or silently reads it differently.
//
// plan.json and session.json used to be pinned beside it; they are v2 documents
// now and their goldens live in wire_golden_v2_test.go.
func TestWireGoldenSessionEvents(t *testing.T) {
	t.Parallel()
	run := loadWireRun(t, "mixed")

	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(session.Close)
	// The id and start time are generated from time.Now plus crypto/rand.
	// Overwriting them before anything is written is injection, not scrubbing:
	// the bytes below are the writer's own formatting of these exact values.
	session.ID = run.fixture.SessionID
	session.StartTime = run.startTime

	for _, event := range run.session {
		if err := session.AppendEvent(event); err != nil {
			t.Fatalf("append event %q: %v", event.JobKey, err)
		}
	}

	assertWireGolden(t, "mixed.events.jsonl", readSessionFile(t, session, "events.jsonl"))
}

// TestWireGoldenFixtureCoverage keeps the corpus honest: the mixed fixture must
// keep exercising every outcome class and record type the issue names, so a
// later edit cannot quietly reduce the goldens to a warm all-success run.
func TestWireGoldenFixtureCoverage(t *testing.T) {
	t.Parallel()
	run := loadWireRun(t, "mixed")

	required := map[string]bool{
		"fresh success":         false,
		"fresh failure":         false,
		"failure error":         false,
		"diagnostic event":      false,
		"artifact event":        false,
		"metric event":          false,
		"local cache hit":       false,
		"remote cache hit":      false,
		"coalesced":             false,
		"skipped":               false,
		"canceled":              false,
		"result-only task":      false,
		"multiple projects":     false,
		"session event payload": false,
	}
	projects := make(map[string]bool)

	for _, task := range run.fixture.Tasks {
		projects[task.ProjectName] = true
		switch {
		case task.Reuse == jobs.ReuseLocalCache:
			required["local cache hit"] = true
		case task.Reuse == jobs.ReuseRemoteCache:
			required["remote cache hit"] = true
		case task.Reuse == jobs.ReuseCoalesced:
			required["coalesced"] = true
		case task.Status == string(jobs.TaskStatusSuccess):
			required["fresh success"] = true
		case task.Status == string(jobs.TaskStatusFailed):
			required["fresh failure"] = true
		case task.Status == string(jobs.TaskStatusSkipped):
			required["skipped"] = true
		case task.Status == string(jobs.TaskStatusCanceled):
			required["canceled"] = true
		}
		if task.Error != nil && task.Error.Message != "" {
			required["failure error"] = true
		}
		if !task.ReportedToRenderer {
			required["result-only task"] = true
		}
		if task.SessionEvent != nil && len(task.SessionEvent.Data) > 0 {
			required["session event payload"] = true
		}
		for _, event := range task.Events {
			switch event.Type {
			case "diagnostic":
				required["diagnostic event"] = true
			case "artifact":
				required["artifact event"] = true
			case "metric":
				required["metric event"] = true
			}
		}
	}
	required["multiple projects"] = len(projects) > 1

	for _, name := range sortedCoverageKeys(required) {
		if !required[name] {
			t.Errorf("mixed fixture no longer covers %q — the wire goldens stop proving that case", name)
		}
	}

	// One fresh failure, kept deliberately. The canonical reducer produces
	// failures in PLAN order, so the v2 corpus would tolerate more; the fixture
	// stays at one because the v1 session summary it was captured against walked
	// the result MAP, and re-widening it here would silently change what the
	// surviving goldens are comparable to.
	fresh := 0
	for _, task := range run.fixture.Tasks {
		if task.Status == string(jobs.TaskStatusFailed) && !task.CacheHit && !task.Coalesced {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("mixed fixture has %d fresh failures; exactly 1 keeps the corpus comparable", fresh)
	}

	// NON-VACUITY. Every v2 surface reports a failure's diagnostics — the
	// envelope's and session:end's run.failures, task:end, and session.json's
	// task record — and all four read them off the failed task's own events. A
	// failure that carries none makes each of those members ABSENT in every
	// golden, so the whole corpus would pass while proving nothing about the
	// one field an agent reads a failed run for. That is exactly the state B1r
	// found the corpus in.
	for _, task := range run.fixture.Tasks {
		if task.Status != string(jobs.TaskStatusFailed) {
			continue
		}
		diagnostics := 0
		for _, event := range task.Events {
			if event.Type == "diagnostic" {
				diagnostics++
			}
		}
		if diagnostics == 0 {
			t.Errorf("failed task %q carries no diagnostic event — the goldens then pin "+
				"failures with NO diagnostics member, which is a vacuous corpus, not a passing one",
				task.Key)
		}
	}
}

func sortedCoverageKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	// Small fixed set; insertion sort keeps this free of a sort import shuffle.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

func readSessionFile(t *testing.T, session *workspace_state.Session, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(session.Dir(), name))
	if err != nil {
		t.Fatalf("read session %s: %v", name, err)
	}
	return data
}

// ---------------------------------------------------------------------------
// Normalization — see the file header for the full list and rationale
// ---------------------------------------------------------------------------

// sessionEndTimeRe matches the recorded session's endTime, which the writer
// stamps from time.Now and no caller can inject. It is the one v1-era
// normalization the v2 session-file golden still needs (the v1 run-duration and
// stream-time regexes went with the emitters B1b deleted).
var sessionEndTimeRe = regexp.MustCompile(`(?m)^  "endTime": "([^"]*)",$`)

// replaceOnce validates and replaces the single occurrence of re in data. The
// exactly-once requirement is half the guarantee: if a migration drops the
// field, or grows a second one, this fails instead of quietly normalizing
// nothing.
func replaceOnce(
	t *testing.T,
	data []byte,
	re *regexp.Regexp,
	replacement string,
	what string,
	validate func(string) error,
) []byte {
	t.Helper()
	matches := re.FindAllSubmatch(data, -1)
	if len(matches) != 1 {
		t.Fatalf("%s: expected exactly 1 occurrence of %s, found %d — the field moved, vanished, or forked",
			what, re, len(matches))
	}
	if err := validate(string(matches[0][1])); err != nil {
		t.Fatalf("%s: %v — the normalized field itself drifted", what, err)
	}
	return re.ReplaceAll(data, []byte(replacement))
}

// assertRFC3339Nano pins the timestamp LAYOUT, which is the part of a
// normalized time field that is still a wire contract.
func assertRFC3339Nano(value string) error {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return fmt.Errorf("%q is not RFC3339Nano: %w", value, err)
	}
	if formatted := parsed.Format(time.RFC3339Nano); formatted != value {
		return fmt.Errorf("%q does not round-trip through RFC3339Nano (got %q)", value, formatted)
	}
	return nil
}

// assertWallMillis pins that a normalized duration is still a non-negative
// integer count of milliseconds, and still plausibly a measurement of this
// test rather than, say, a nanosecond count that slipped through.
func assertWallMillis(value string) error {
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("%q is not an integer: %w", value, err)
	}
	if millis < 0 {
		return errors.New("duration is negative")
	}
	if millis > int64(10*time.Minute/time.Millisecond) {
		return fmt.Errorf("duration %d ms is not a wall time this test could have produced", millis)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Golden IO
// ---------------------------------------------------------------------------

func assertWireGolden(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", "wire", "golden", name)
	if *updateWireGoldens {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create golden dir: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (create with: go test ./internal/cli/ -run WireGolden -update-wire-goldens)", path, err)
	}
	if bytes.Equal(want, got) {
		return
	}
	t.Errorf("%s drifted from its golden %s.\n%s\n\n"+
		"This is persisted version-1 session history that `putnami sessions` still reads.\n"+
		"A failure means the compatibility projection moved; do not update it until the\n"+
		"sessions readers still read the historical session identically.\n",
		name, path, firstWireDiff(want, got))
}

// firstWireDiff reports the first differing line so a failure names the drift
// instead of dumping two whole wire captures into the test log.
func firstWireDiff(want, got []byte) string {
	wantLines := strings.Split(string(want), "\n")
	gotLines := strings.Split(string(got), "\n")
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		var wantLine, gotLine string
		if i < len(wantLines) {
			wantLine = wantLines[i]
		}
		if i < len(gotLines) {
			gotLine = gotLines[i]
		}
		if wantLine != gotLine {
			return "first difference at line " + strconv.Itoa(i+1) +
				":\n  want: " + wantLine + "\n  got:  " + gotLine
		}
	}
	return "captures differ only in trailing bytes"
}

// streamEventName, which read the v1 `event` discriminator off a compact jsonl
// line, went with the v1 stream. Its v2 counterpart is v2StreamRecord in
// wire_golden_v2_test.go, which reads `record` instead.
