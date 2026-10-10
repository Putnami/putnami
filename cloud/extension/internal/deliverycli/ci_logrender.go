// Human rendering of CI run logs.
//
// A CI run's log stream is not free text: nearly every line is one runner
// protocol record — a whole JSON object, several kilobytes wide, printed on a
// single line. Echoing those verbatim is what made `putnami cloud ci logs`
// unreadable without `jq`, and it is why the diagnostics that explain a red run
// were sitting in the output all along without anyone being able to see them.
//
// Two things are decoded here, and only for RENDERING:
//
//   - a compact human line per record (what ran, what it printed, what it
//     concluded, how long),
//   - the record's EFFECTIVE severity, which is the severity `--level` means.
//
// The second one is the subtle half. Every protocol line arrives at severity
// LOG, because that is the severity of the pipe that carried it; the severity a
// reader is asking about lives INSIDE the record, on each diagnostic. Filtering
// the outer one is why `--level error` answered "No logs found" about a run that
// had just failed four lint tasks.
//
// Nothing here is authoritative. An unrecognized or malformed record renders as
// its raw body — the information is never dropped, only the formatting is
// best-effort — and the report fold (internal/reportstream) remains the one
// parser whose output is ingested.
package deliverycli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ciSeverity is the OTLP SeverityNumber scale a log entry carries and
// `--level` filters on. Local: delivery-api's LogEntry declares severity as a
// plain int32, so its generated client has no constants for the levels. The
// provider cannot declare them yet: the framework's Go OpenAPI emitter writes
// every enum value as a JSON string, so an integer member cannot carry an
// enum. The values are the OTLP wire numbers.
type ciSeverity int32

// The severity thresholds, matching the OpenTelemetry SeverityNumber values.
const (
	ciSeverityUnset ciSeverity = 0
	ciSeverityTrace ciSeverity = 1
	ciSeverityDebug ciSeverity = 5
	ciSeverityInfo  ciSeverity = 9
	ciSeverityWarn  ciSeverity = 13
	ciSeverityError ciSeverity = 17
	ciSeverityFatal ciSeverity = 21
)

// ciLogRecord is the tolerant shape this renderer reads. Every member is
// optional: a stream from a newer contract must degrade to a raw line here, never
// to a dropped one.
type ciLogRecord struct {
	Record  string `json:"record"`
	Kind    string `json:"kind"`
	Command string `json:"command"`
	Status  string `json:"status"`
	Message string `json:"message"`

	Identity *ciLogIdentity `json:"identity"`
	Event    *ciLogEvent    `json:"event"`
	Task     *struct {
		Status     string `json:"status"`
		Reuse      string `json:"reuse"`
		DurationMs int64  `json:"durationMs"`
		ExitCode   int    `json:"exitCode"`
		Error      *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"task"`
	Run *struct {
		Outcome    string `json:"outcome"`
		ExitCode   int    `json:"exitCode"`
		DurationMs int64  `json:"durationMs"`
		Counts     *struct {
			Total     int `json:"total"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
			Canceled  int `json:"canceled"`
			Skipped   int `json:"skipped"`
		} `json:"counts"`
		Failures []struct {
			Diagnostics []struct {
				Severity string `json:"severity"`
			} `json:"diagnostics"`
		} `json:"failures"`
	} `json:"run"`
}

// ciLogEvent is what a `task:event` record carries: one thing a running task
// reported. A `log` event is a line the task printed, with its level. A
// `diagnostic` event is a finding, with its severity and where it points.
type ciLogEvent struct {
	Type     string `json:"type"`
	Level    string `json:"level"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Location *struct {
		File string `json:"file"`
		Line int    `json:"line"`
	} `json:"location"`
}

type ciLogIdentity struct {
	Project struct {
		Name string `json:"name"`
	} `json:"project"`
	Task struct {
		Name string `json:"name"`
	} `json:"task"`
}

// ciNoisyRecordKeys are the members the generic fallback renderer drops: run
// plumbing that repeats on every single line and tells a reader nothing they did
// not already know from the command they typed.
var ciNoisyRecordKeys = map[string]struct{}{
	"runId":           {},
	"run_id":          {},
	"protocolVersion": {},
	"identity":        {},
	"time":            {},
	// The entry already carries its own timestamp column, and the logger name is
	// the same on every line of a given stream.
	"timestamp": {},
	"logger":    {},
}

// ciRenderLogEntry renders one log entry for humans: a decoded protocol record
// as a compact line, anything else as the entry's own body.
func ciRenderLogEntry(entry CILogEntry) string {
	body := strings.TrimSpace(entry.Body)
	rendered, ok := ciRenderRecordBody(body)
	if !ok {
		return ciFormatLogLine(entry)
	}
	return ciFormatLogLineWith(entry, rendered)
}

// ciRenderRecordBody turns one protocol record body into a compact human line.
// The bool is false for a body this renderer does not speak, and the caller then
// prints the raw body — an unreadable line is strictly better than a lost one.
func ciRenderRecordBody(body string) (string, bool) {
	if body == "" || body[0] != '{' {
		return "", false
	}
	var rec ciLogRecord
	if json.Unmarshal([]byte(body), &rec) != nil {
		return "", false
	}
	switch {
	case rec.Record == "task:end" && rec.Task != nil:
		return ciRenderTaskEnd(rec), true
	case rec.Record == "task:start":
		return "task start   " + ciRecordIdentity(rec.Identity), true
	case rec.Record == "task:event" && rec.Event != nil:
		return ciRenderTaskEvent(body, rec), true
	case rec.Record == "session:end" && rec.Run != nil:
		return ciRenderSessionEnd(rec), true
	case rec.Kind != "" || rec.Command != "":
		return ciRenderGenericRecord(body, rec, ""), true
	case rec.Message != "":
		// A structured log line rather than a protocol record — the runner's own
		// narration and the ci-worker's JSON logger both take this shape. The message
		// IS the line; the surrounding envelope is what made it unreadable.
		return ciRenderGenericRecord(body, rec, rec.Message), true
	}
	return "", false
}

// ciRenderTaskEnd renders one finished task: what ran, how it ended, how long it
// took, and whether it was replayed from cache rather than executed.
func ciRenderTaskEnd(rec ciLogRecord) string {
	parts := []string{
		fmt.Sprintf("%-12s", "task "+ciCompactStatus(rec.Task.Status)),
		ciRecordIdentity(rec.Identity),
	}
	if rec.Task.DurationMs > 0 {
		parts = append(parts, ciFormatDurationMS(rec.Task.DurationMs))
	}
	if reuse := strings.TrimSpace(rec.Task.Reuse); reuse != "" && reuse != "none" {
		parts = append(parts, reuse)
	}
	// The task error rides along only when it SAYS something the status has not.
	// The gate's generic "task failed" beside a status of `failed` is the same fact
	// twice, and a line that repeats itself reads as two findings.
	if rec.Task.Error != nil {
		if msg := strings.TrimSpace(rec.Task.Error.Message); msg != "" && !ciEchoesStatus(msg, rec.Task.Status) {
			parts = append(parts, "— "+msg)
		}
	}
	return strings.Join(parts, "  ")
}

// ciRenderTaskEvent renders one thing a running task reported as
// `<project:task> <message>`: the line the task printed, under the name of the
// task that printed it. These records are most of a run's log since the engine
// ships its event file, so the line must read like the task's own output.
//
// A diagnostic adds where it points. An event with no message (a metric, a
// phase mark) renders as its type and its scalar members.
func ciRenderTaskEvent(body string, rec ciLogRecord) string {
	label := ciEventIdentity(rec.Identity)
	message := strings.TrimSpace(rec.Event.Message)
	if message == "" {
		return strings.TrimSpace(label + " " + ciRenderEventFacts(body, rec.Event.Type))
	}
	if loc := rec.Event.Location; loc != nil && strings.TrimSpace(loc.File) != "" {
		where := strings.TrimSpace(loc.File)
		if loc.Line > 0 {
			where += ":" + strconv.Itoa(loc.Line)
		}
		message += "  (" + where + ")"
	}
	return strings.TrimSpace(label + " " + message)
}

// ciRenderEventFacts renders a message-less event as its type and its scalar
// members, the same compact form the generic record line uses.
func ciRenderEventFacts(body, eventType string) string {
	head := strings.TrimSpace(eventType)
	if head == "" {
		head = "event"
	}
	var envelope struct {
		Event map[string]json.RawMessage `json:"event"`
	}
	if json.Unmarshal([]byte(body), &envelope) != nil {
		return head
	}
	keys := make([]string, 0, len(envelope.Event))
	for k := range envelope.Event {
		if k != "type" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	parts := []string{head}
	for _, k := range keys {
		if v, ok := ciScalarString(envelope.Event[k]); ok {
			parts = append(parts, k+"="+v)
		}
	}
	return strings.Join(parts, " ")
}

// ciEventIdentity renders the task an event came from as `<project:task>`,
// degrading to whichever half the record carried, or to nothing.
func ciEventIdentity(id *ciLogIdentity) string {
	if id == nil {
		return ""
	}
	project := strings.TrimSpace(id.Project.Name)
	task := strings.TrimSpace(id.Task.Name)
	switch {
	case project != "" && task != "":
		return "<" + project + ":" + task + ">"
	case project != "" || task != "":
		return "<" + project + task + ">"
	default:
		return ""
	}
}

// ciRenderSessionEnd renders the gate's own verdict plus the task census behind
// it. The census is the line that explains a red run's shape: a fail-fast gate
// cancels most of its work, so "4 failed" alongside "617 canceled" says the run
// stopped early rather than that 617 things broke.
func ciRenderSessionEnd(rec ciLogRecord) string {
	line := "session end  " + ciCompactStatus(rec.Run.Outcome)
	if c := rec.Run.Counts; c != nil {
		var census []string
		for _, part := range []struct {
			label string
			n     int
		}{
			{"succeeded", c.Succeeded}, {"failed", c.Failed},
			{"canceled", c.Canceled}, {"skipped", c.Skipped},
		} {
			if part.n > 0 {
				census = append(census, strconv.Itoa(part.n)+" "+part.label)
			}
		}
		if len(census) > 0 {
			line += "  " + strings.Join(census, ", ") + " of " + strconv.Itoa(c.Total)
		}
	}
	if rec.Run.DurationMs > 0 {
		line += "  " + ciFormatDurationMS(rec.Run.DurationMs)
	}
	return line
}

// ciRenderGenericRecord renders a record this file has no specific shape for —
// the runner's own framing markers, the nested CLI command records, and plain
// structured log lines — as a heading plus its scalar members. Nested objects and
// arrays are omitted rather than flattened: they are what made the raw line
// unreadable, and the ones that carry findings are rendered by the diagnostics
// section instead.
func ciRenderGenericRecord(body string, rec ciLogRecord, head string) string {
	if head == "" {
		head = strings.TrimSpace(rec.Kind)
	}
	if head == "" {
		head = strings.TrimSpace(rec.Command)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(body), &fields) != nil {
		return head
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if _, noisy := ciNoisyRecordKeys[k]; noisy || k == "kind" || k == "command" || k == "message" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if v, ok := ciScalarString(fields[k]); ok {
			parts = append(parts, k+"="+v)
		}
	}
	if len(parts) == 0 {
		return head
	}
	return head + "  " + strings.Join(parts, " ")
}

// ciScalarString renders a JSON scalar for the generic line. Objects and arrays
// report false and are skipped.
func ciScalarString(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	switch trimmed[0] {
	case '{', '[':
		return "", false
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		if s == "" {
			return "", false
		}
		return s, true
	default:
		return trimmed, true
	}
}

// ciRecordIdentity renders a task identity as `project task`, degrading to
// whichever half the record carried.
func ciRecordIdentity(id *ciLogIdentity) string {
	if id == nil {
		return "-"
	}
	project := strings.TrimSpace(id.Project.Name)
	task := strings.TrimSpace(id.Task.Name)
	switch {
	case project != "" && task != "":
		return project + "  " + task
	case project != "":
		return project
	case task != "":
		return task
	default:
		return "-"
	}
}

// ciEchoesStatus reports whether an error message adds nothing to the status word
// already rendered beside it ("task failed" next to `failed`).
func ciEchoesStatus(message, status string) bool {
	m := strings.ToLower(strings.TrimSpace(message))
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return false
	}
	return m == s || m == "task "+s || m == s+" task"
}

// ciCompactStatus normalizes the runner's status words to a fixed width so a
// column of them stays scannable.
func ciCompactStatus(status string) string {
	s := strings.TrimSpace(status)
	if s == "" {
		return "?"
	}
	return s
}

// ciFormatLogLineWith renders one entry with a REPLACEMENT body, reusing the
// timestamp/severity prefix so a decoded line and a raw one align in the same
// column layout.
func ciFormatLogLineWith(entry CILogEntry, body string) string {
	replaced := entry
	replaced.Body = body
	return ciFormatLogLine(replaced)
}

// ciEntrySeverity is the severity `--level` filters on: the worst severity the
// entry actually asserts.
//
// For a protocol record that is the worst severity among the findings INSIDE it
// (a lint task reporting an `error` diagnostic is an error line, whatever the
// pipe stamped it), plus the record's own outcome — a failed task or a failed
// session is an error even when it carried no diagnostics at all. For everything
// else it is the entry's own severity, which is the honest reading for a genuinely
// free-text line such as a crashed runner's output.
func ciEntrySeverity(entry CILogEntry) ciSeverity {
	body := strings.TrimSpace(entry.Body)
	if body == "" || body[0] != '{' {
		return entry.Severity
	}
	var rec ciLogRecord
	if json.Unmarshal([]byte(body), &rec) != nil {
		return entry.Severity
	}
	worst := entry.Severity
	raise := func(sev ciSeverity) {
		if sev > worst {
			worst = sev
		}
	}
	if rec.Task != nil && ciIsFailureWord(rec.Task.Status) {
		raise(ciSeverityError)
	}
	if rec.Run != nil {
		if ciIsFailureWord(rec.Run.Outcome) {
			raise(ciSeverityError)
		}
		for i := range rec.Run.Failures {
			for j := range rec.Run.Failures[i].Diagnostics {
				raise(ciParseDiagnosticSeverity(rec.Run.Failures[i].Diagnostics[j].Severity))
			}
		}
	}
	if ciIsFailureWord(rec.Status) {
		raise(ciSeverityError)
	}
	if rec.Record == "task:event" && rec.Event != nil {
		// A task's own line states its level, a finding states its severity. An
		// unknown word raises nothing.
		if level, ok := ciParseLevel(rec.Event.Level); ok {
			raise(level)
		}
		if strings.TrimSpace(rec.Event.Severity) != "" {
			raise(ciParseDiagnosticSeverity(rec.Event.Severity))
		}
	}
	return worst
}

// ciIsFailureWord reports whether a runner status word means "this did not
// succeed". `canceled` and `skipped` are deliberately NOT failures: a fail-fast
// gate cancels most of its tasks, and rendering those as errors would bury the
// handful that actually broke.
func ciIsFailureWord(word string) bool {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "failure", "failed", "error", "errored", "timed_out", "preempted":
		return true
	default:
		return false
	}
}

// ciParseDiagnosticSeverity maps a diagnostic's own severity word onto the log
// severity scale. An unknown word maps to the bottom of the scale rather than to
// a guessed level.
func ciParseDiagnosticSeverity(word string) ciSeverity {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "fatal":
		return ciSeverityFatal
	case "error":
		return ciSeverityError
	case "warning", "warn":
		return ciSeverityWarn
	case "info", "information", "hint":
		return ciSeverityInfo
	default:
		return ciSeverityTrace
	}
}

// ciParseLevel maps a `--level` word onto the severity scale, reporting false for
// a word that is not a level at all.
func ciParseLevel(word string) (ciSeverity, bool) {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case "trace":
		return ciSeverityTrace, true
	case "debug":
		return ciSeverityDebug, true
	case "info":
		return ciSeverityInfo, true
	case "warn", "warning":
		return ciSeverityWarn, true
	case "error":
		return ciSeverityError, true
	case "fatal":
		return ciSeverityFatal, true
	default:
		return ciSeverityUnset, false
	}
}
