package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/machine"
	"go.putnami.dev/tooling/cli/internal/output"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// The v2 wire goldens: the ONLY machine documents the CLI emits since an
// earlier change deleted the v1 emitters.
//
// They were captured by B0g beside the v1 corpus, generated from the same two
// fixtures through the same production emission paths, differing only in whether
// the (now retired) PUTNAMI_MACHINE_OUTPUT opt-in was set. Both halves of that
// proof have since become unconditional: B1a made v2 the default, and B1b
// removed the alternative, so these goldens are driven through the SELECTION
// POINT (output.NewRenderer, not the concrete renderer) and each document is
// validated against the contract's own validator (protocolcli.ValidateDocument)
// BEFORE normalization — a golden can never pin a document the contract would
// reject.
//
// The ".v2" segment in the file names is historical: it distinguished a golden
// from its v1 sibling, and the names are kept so the corpus stays greppable
// against the slices that produced it.
//
// # Normalization
//
// Only fields production computes from the wall clock are replaced, each exactly
// once, and only after asserting the property the field exists for:
//
//   - the run wall is run.durationMs on every surface (the envelope, bounded
//     session:end record and session.json), always at indent 4 in an indented
//     document and always the ONLY durationMs at that indent — a task record's
//     sits at indent 6 and is injected by the fixture;
//   - session.json's endTime, which the writer stamps from time.Now; its
//     durationMs is then asserted to equal endTime minus the INJECTED startTime,
//     so the relationship survives the placeholder;
//   - JSONL's artifact session id, which comes from a real temp Session and is
//     replaced with the fixture id only after ValidateSessionStream proves the
//     live selection agrees with that session's complete events.jsonl;
//   - task:event records carry the PRODUCER's timestamp, not the moment the CLI
//     forwarded the event, so they are fully deterministic and are deliberately
//     left un-normalized. A regression that restamps them with time.Now fails
//     the golden instead of hiding behind a placeholder.

// v2GoldenName names a v2 golden after the fixture surface it pins.
func v2GoldenName(surface string) string {
	base, ext, _ := strings.Cut(surface, ".")
	return base + ".v2." + ext
}

var (
	// The run summary's durationMs, at four spaces: a task record's sits at six.
	// The trailing comma is optional because failures is the only member after
	// it and it is absent on a clean run.
	// The trailing comma is captured rather than assumed: failures is the only
	// member after it, so a clean run ends the object there.
	v2RunDurationRe = regexp.MustCompile(`(?m)^    "durationMs": (-?\d+)(,?)$`)
	// Compact stream lines: applied per line, and only to the records whose time
	// the CLI stamps itself.
	v2StreamTimeRe     = regexp.MustCompile(`"time":"([^"]*)"`)
	v2StreamDurationRe = regexp.MustCompile(`"durationMs":(-?\d+)`)
)

// ---------------------------------------------------------------------------
// One contract: no environment selects anything else
// ---------------------------------------------------------------------------

// TestWireGoldenV2_IsTheOnlyContract drives the SELECTION POINT
// (output.NewRenderer) with the retired variable in every state that used to
// mean something, and asserts the same v2 bytes each time. It replaces B0g's
// opt-in gate, whose second half compared against the v1 goldens B1b
// deleted.
//
// The value table is the retired opt-out's, deliberately: a rollback lever that
// quietly came back — or a residual branch that fired on one spelling — would
// produce a versionless document for exactly these inputs and nothing else.
func TestWireGoldenV2_IsTheOnlyContract(t *testing.T) {
	for _, format := range []string{"json", "jsonl"} {
		for _, value := range []string{"", "v1", "V1 ", "v2", "latest"} {
			t.Run(format+"/"+machine.RetiredSelectionEnv+"="+value, func(t *testing.T) {
				t.Setenv(machine.RetiredSelectionEnv, value)
				run := loadWireRun(t, "mixed")
				got := renderWireRun(t, run, format)
				if !bytes.Contains(got, []byte(`"protocolVersion"`)) {
					t.Fatalf("%s=%q produced a document with no protocolVersion — a document WITHOUT it is version 1:\n%s",
						machine.RetiredSelectionEnv, value, got)
				}
				var normalized []byte
				if format == "json" {
					assertValidV2Document(t, protocolcli.DocumentResultEnvelope, got)
					normalized = normalizeMachineV2JSON(t, got)
				} else {
					assertValidV2Stream(t, got)
					normalized = normalizeMachineV2JSONL(t, got)
				}
				assertWireGoldenV2(t, v2GoldenName("mixed.output."+format), normalized)
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Goldens, one per surface
// ---------------------------------------------------------------------------

// TestWireGoldenV2_OutputJSON pins the --output=json envelope: the versioned
// document, the typed run summary, and the abort precedence the v1 envelope
// deliberately inverted (the aborted fixture reported "failure" with exitCode
// 130 in v1, and reports "aborted" with exitCode 130 here).
func TestWireGoldenV2_OutputJSON(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mixed", "aborted"} {
		t.Run(name, func(t *testing.T) {
			run := loadWireRun(t, name)
			got := renderWireRun(t, run, "json")
			assertValidV2Document(t, protocolcli.DocumentResultEnvelope, got)
			assertWireGoldenV2(t, v2GoldenName(name+".output.json"),
				normalizeMachineV2JSON(t, got))
		})
	}
}

// TestWireGoldenV2_OutputJSONL pins the bounded v2 session stream: task:start /
// task:event / task:end / session:end, each carrying the typed identity, with
// the fixed budget, sanitization and artifact contract closing the stream.
func TestWireGoldenV2_OutputJSONL(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"mixed", "aborted"} {
		t.Run(name, func(t *testing.T) {
			run := loadWireRun(t, name)
			got := renderWireRun(t, run, "jsonl")
			assertValidV2Stream(t, got)
			assertWireGoldenV2(t, v2GoldenName(name+".output.jsonl"),
				normalizeMachineV2JSONL(t, got))
		})
	}
}

// TestWireGoldenV2_SessionFiles pins the two recorded session documents, written
// through the real workspace_state writer — the only session writer left.
// events.jsonl is written incrementally through renderer callbacks rather than
// by FinalizeV2; each JSONL golden above validates its live bytes against that
// real artifact. Historical v1 session-event goldens remain reader compatibility
// fixtures.
func TestWireGoldenV2_SessionFiles(t *testing.T) {
	t.Parallel()
	run := loadWireRun(t, "mixed")

	session, err := workspace_state.NewSession(t.TempDir())
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	t.Cleanup(session.Close)
	session.ID = run.fixture.SessionID
	session.StartTime = run.startTime

	if err := session.WritePlanV2(
		machine.SessionPlanFile(session.ID, run.fixture.Commands, run.planned)); err != nil {
		t.Fatalf("write v2 plan: %v", err)
	}

	view := machine.RunFrom(
		jobs.ReduceRun(run.planned, run.results, run.outcome), run.planned, run.results)
	var cache any
	if run.fixture.Cache != nil {
		cache = run.fixture.Cache
	}
	if err := session.FinalizeV2(
		view.SessionFile(run.fixture.Commands, run.fixture.Scheduler, cache), "", ""); err != nil {
		t.Fatalf("finalize v2: %v", err)
	}

	plan := readSessionFile(t, session, "plan.json")
	assertValidV2Document(t, protocolcli.DocumentSessionPlanFile, plan)
	assertWireGoldenV2(t, v2GoldenName("mixed.plan.json"), plan)

	recorded := readSessionFile(t, session, "session.json")
	assertValidV2Document(t, protocolcli.DocumentSessionFile, recorded)
	assertWireGoldenV2(t, v2GoldenName("mixed.session.json"),
		normalizeMachineV2SessionJSON(t, recorded, run.startTime))
}

// TestWireGoldenV2_MCPResult pins the fourth surface. The MCP document has no
// v1 golden to sit beside — run_jobs was never captured here — so this is both
// its first pin and the proof that the run summary is ONE type across all four
// surfaces: these counts are byte-identical to the envelope's.
func TestWireGoldenV2_MCPResult(t *testing.T) {
	t.Parallel()
	run := loadWireRun(t, "mixed")
	view := machine.RunFrom(
		jobs.ReduceRun(run.planned, run.results, run.outcome), run.planned, run.results)

	document := marshalIndentedV2(t, view.MCPRun(run.fixture.Commands))
	assertValidV2Document(t, protocolcli.DocumentMCPResult, document)
	assertWireGoldenV2(t, "mixed.v2.mcp.json", document)

	plan := marshalIndentedV2(t, machine.MCPPlan(run.fixture.Commands, run.planned))
	assertValidV2Document(t, protocolcli.DocumentMCPResult, plan)
	assertWireGoldenV2(t, "mixed.v2.mcp-plan.json", plan)
}

// TestWireGoldenV2_EncodesTheSettledSemantics is the semantic half of the
// corpus: the goldens above pin the BYTES, this pins the two decisions those
// bytes are supposed to encode (protocols/cli doc/02-result-v2.md). Both are
// cases where v2 must disagree with the v1 wire, so an emitter that projected
// the v1 verdict onto the v2 shape would still produce a schema-valid document
// and would still pass every structural check.
func TestWireGoldenV2_EncodesTheSettledSemantics(t *testing.T) {
	t.Parallel()
	t.Run("abort wins over failure", func(t *testing.T) {
		run := loadWireRun(t, "aborted")
		// The v1 envelope reports an interrupted run with failures as a plain
		// failure; make sure the fixture really is that case before asserting.
		reduced := jobs.ReduceRun(run.planned, run.results, run.outcome)
		if !reduced.Aborted {
			t.Fatal("the aborted fixture stopped being aborted")
		}
		summary := machine.RunFrom(reduced, run.planned, run.results).Summary(0)
		if summary.Outcome != protocolcli.RunOutcomeAborted {
			t.Errorf("outcome = %q, want %q", summary.Outcome, protocolcli.RunOutcomeAborted)
		}
		if summary.AbortedBy != protocolcli.AbortedByUser {
			t.Errorf("abortedBy = %q, want %q", summary.AbortedBy, protocolcli.AbortedByUser)
		}
		if summary.ExitCode != protocolcli.ExitSignal {
			t.Errorf("exitCode = %d, want %d", summary.ExitCode, protocolcli.ExitSignal)
		}
	})

	t.Run("unified success is strict", func(t *testing.T) {
		// One task, failed, restored from cache. Before B1a this was the single
		// input v1's lenient session:end predicate and the strict rule disagreed
		// on; the lenient spelling is deleted, and this case remains so the
		// unified verdict cannot drift back. Positive control: the fresh
		// histogram still ignores the reused failure — leniency would be
		// re-derivable from it — while the verdict does not.
		run := loadWireRun(t, "mixed")
		job := run.planned[0]
		results := map[string]*jobs.JobResult{job.Key(): {
			Status:   string(jobs.TaskStatusFailed),
			CacheHit: true,
			Reuse:    jobs.ReuseLocalCache,
			Error:    &jobs.JobError{Message: "restored failure"},
		}}
		reduced := jobs.ReduceRun([]*jobs.ScheduledJob{job}, results, jobs.SessionOutcome{})
		if reduced.Fresh.Failed != 0 || reduced.Status.Failed != 1 {
			t.Fatalf("fresh.failed = %d / status.failed = %d, want 0 / 1 — "+
				"this case no longer separates fresh from status accounting",
				reduced.Fresh.Failed, reduced.Status.Failed)
		}
		if reduced.Success() {
			t.Fatal("the unified strict predicate green-lit a reused failure")
		}
		summary := machine.RunFrom(reduced, []*jobs.ScheduledJob{job}, results).Summary(0)
		if summary.Outcome != protocolcli.RunOutcomeFailure {
			t.Errorf("outcome = %q for a reused failure, want %q",
				summary.Outcome, protocolcli.RunOutcomeFailure)
		}
		if summary.Counts.Failed != 1 || summary.Reuse.LocalCache != 1 {
			t.Errorf("counts.failed = %d / reuse.localCache = %d, want 1 / 1 — "+
				"reuse must be counted APART from the verdict",
				summary.Counts.Failed, summary.Reuse.LocalCache)
		}
		if len(summary.Failures) != 1 {
			t.Fatalf("failures = %d, want 1: a counted failure cannot be omitted from the list",
				len(summary.Failures))
		}
		if key := summary.Failures[0].Identity.Key; key != job.Key() {
			t.Errorf("failure identity key = %q, want the plan key %q", key, job.Key())
		}
	})
}

// ---------------------------------------------------------------------------
// Driving and validating
// ---------------------------------------------------------------------------

// renderWireRun drives a fixture through the renderer output.NewRenderer
// SELECTS for the given format, which is what puts the opt-in itself under
// test rather than the v2 renderer alone.
func renderWireRun(t *testing.T, run *wireRun, format string) []byte {
	t.Helper()
	var buf bytes.Buffer
	renderer := output.NewRenderer(output.Config{
		Output:  format,
		Command: run.fixture.Command,
		Out:     &buf,
		Err:     &buf,
	})
	var session *workspace_state.Session
	if format == "jsonl" {
		var err error
		session, err = workspace_state.NewSession(t.TempDir())
		if err != nil {
			t.Fatalf("new machine-output session: %v", err)
		}
		t.Cleanup(session.Close)
		renderer = output.WithSessionRecording(renderer, session, false)
	}
	run.drive(renderer)
	got := append([]byte(nil), buf.Bytes()...)
	if session == nil {
		return got
	}
	artifact := readSessionFile(t, session, protocolcli.MachineOutputArtifactPath)
	if violations := protocolcli.ValidateSessionStream(got, artifact); len(violations) != 0 {
		t.Fatalf("golden live/artifact stream mismatch: %+v", violations)
	}
	if count := bytes.Count(got, []byte(session.ID)); count != 1 {
		t.Fatalf("live artifact session id occurs %d times, want exactly once", count)
	}
	return bytes.Replace(got, []byte(session.ID), []byte(run.fixture.SessionID), 1)
}

func marshalIndentedV2(t *testing.T, document any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatalf("marshal v2 document: %v", err)
	}
	return append(data, '\n')
}

// assertValidV2Document runs the contract's own validator over the RAW bytes,
// before normalization: the placeholders a golden carries (a -1 duration) are
// not values the contract allows, so validating the golden instead would either
// fail or force the placeholders to be legal — and a legal placeholder is one
// no reader can distinguish from a measurement.
func assertValidV2Document(t *testing.T, kind protocolcli.DocumentKind, data []byte) {
	t.Helper()
	violations := protocolcli.ValidateDocument(kind, bytes.TrimSpace(data))
	if len(violations) == 0 {
		return
	}
	var detail strings.Builder
	for _, violation := range violations {
		detail.WriteString("\n  " + violation.Path + ": " + violation.Code)
	}
	t.Errorf("%s document violates the v2 contract:%s\n%s", kind, detail.String(), data)
}

func assertValidV2Stream(t *testing.T, data []byte) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("the v2 stream is empty")
	}
	for _, line := range lines {
		assertValidV2Document(t, protocolcli.DocumentSessionStreamRecord, []byte(line))
	}
	if !strings.Contains(lines[len(lines)-1], `"record":"session:end"`) {
		t.Errorf("the stream does not end with session:end: %s", lines[len(lines)-1])
	}
}

// ---------------------------------------------------------------------------
// Normalization
// ---------------------------------------------------------------------------

func normalizeMachineV2JSON(t *testing.T, data []byte) []byte {
	t.Helper()
	return replaceOnce(t, data, v2RunDurationRe,
		`    "durationMs": `+strconv.Itoa(wireGoldenDurationMs)+`${2}`,
		"v2 run.durationMs", assertWallMillis)
}

func normalizeMachineV2JSONL(t *testing.T, data []byte) []byte {
	t.Helper()
	lines := strings.Split(string(data), "\n")
	markers := make(map[string]int, 3)
	subSecond := make(map[string]int, 3)
	for i, line := range lines {
		record := v2StreamRecord(line)
		switch record {
		case "task:start", "task:end", "session:end":
			markers[record]++
			lines[i] = string(replaceOnce(t, []byte(line), v2StreamTimeRe,
				`"time":"`+wireGoldenTime+`"`, "v2 "+record+" time",
				func(value string) error {
					if err := assertRFC3339Nano(value); err != nil {
						return err
					}
					if strings.Contains(value, ".") {
						subSecond[record]++
					}
					return nil
				}))
		}
		if record == "session:end" {
			lines[i] = string(replaceOnce(t, []byte(lines[i]), v2StreamDurationRe,
				`"durationMs":`+strconv.Itoa(wireGoldenDurationMs),
				"v2 session:end run.durationMs", assertWallMillis))
		}
	}
	// Same pin as the v1 corpus: across every record of a name, at least one
	// stamp must carry a sub-second fraction, or the stream silently downgraded
	// from RFC3339Nano to a layout that still round-trips.
	for _, record := range []string{"task:start", "task:end"} {
		if markers[record] > 0 && subSecond[record] == 0 {
			t.Fatalf("no %s timestamp carried a sub-second fraction across %d records",
				record, markers[record])
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// normalizeMachineV2SessionJSON replaces the two fields FinalizeV2 stamps from
// the wall clock. As in v1, durationMs is asserted to be endTime minus the
// INJECTED startTime, so the relationship survives the placeholder.
func normalizeMachineV2SessionJSON(t *testing.T, data []byte, start time.Time) []byte {
	t.Helper()

	var endTime time.Time
	data = replaceOnce(t, data, sessionEndTimeRe,
		`  "endTime": "`+wireGoldenTime+`",`, "v2 session.json endTime",
		func(value string) error {
			if err := assertRFC3339Nano(value); err != nil {
				return err
			}
			endTime, _ = time.Parse(time.RFC3339Nano, value)
			return nil
		})

	return replaceOnce(t, data, v2RunDurationRe,
		`    "durationMs": `+strconv.Itoa(wireGoldenDurationMs)+`${2}`,
		"v2 session.json run.durationMs",
		func(value string) error {
			millis, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return err
			}
			if want := endTime.Sub(start).Milliseconds(); millis != want {
				return fmt.Errorf("run.durationMs %d is not endTime-startTime (%d ms)", millis, want)
			}
			return nil
		})
}

// v2StreamRecord reports the record a compact v2 stream line carries.
func v2StreamRecord(line string) string {
	const marker = `"record":"`
	_, rest, found := strings.Cut(line, marker)
	if !found {
		return ""
	}
	name, _, _ := strings.Cut(rest, `"`)
	return name
}

// ---------------------------------------------------------------------------
// Golden IO
// ---------------------------------------------------------------------------

func assertWireGoldenV2(t *testing.T, name string, got []byte) {
	t.Helper()

	path := filepath.Join("testdata", "wire", "golden", name)
	if *updateWireGoldens && strings.Contains(name, ".v2.") {
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
		"These are the CLI's only machine documents: a failure means the emitted contract\n"+
		"changed. Check it against protocols/cli/doc/02-result-v2.md — and against the\n"+
		"migration section there, which is what consumers were told — before regenerating.",
		name, path, firstWireDiff(want, got))
}
