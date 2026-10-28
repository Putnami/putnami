// Package reportstream parses the JSONL event stream `putnami … --output=jsonl`
// writes and folds it into the delivery report batch (tests, coverage, builds)
// that `putnami cloud ci report` submits to the records ingest.
//
// TWO PUTNAMI CONTRACTS ARE SPOKEN, detected per line:
//
//   - v1 — the frozen protocols/runtime session stream: job:start / job:event /
//     job:end / session:end, discriminated by an "event" member and carrying no
//     version. Fixture: testdata/lint-test-build.jsonl.
//   - v2 — protocols/cli's SessionStreamRecord: a "record" member of
//     task:start / task:event / task:end / session:end plus
//     "protocolVersion": 2, with a TYPED identity per task record. It became the
//     CLI default in putnami 0.1.0-405dcd542, which is what made this parser fold
//     an empty batch and sink the aggregate CI check. Fixture:
//     testdata/v2-lint-test-build.jsonl, with the same run captured under
//     PUTNAMI_MACHINE_OUTPUT=v1 in testdata/v1-twin-lint-test-build.jsonl so the
//     two mappings are pinned against each other.
//
// `ci report` is how CI that runs outside Putnami's hosted runners submits
// these reports. A bounded v2 stdout drops ordinary records once its live
// budget is spent, so a large run's stdout can lack most of its test and
// coverage results. FoldRun (artifact.go) therefore folds the complete
// events.jsonl that the stream's final session:end names, and falls back to
// the stdout fold, with a note, when that file is absent or does not verify.
//
// The parser is tolerant and versioned against those real captured streams:
// unknown event and record types are skipped and counted, malformed or oversized
// lines are skipped and counted, and nothing is ever fatal — a broken stream
// yields a smaller batch plus honest stream stats, never an error. Only
// structured summary data is folded in (counts, names, file:line references,
// durations); log lines and artifact contents are never ingested — those belong
// to Observability.
//
// The report payload types are local twins of the records reports feature
// contract held by the delivery API. This CLI module deliberately stays
// off the server dependency tree (it compiles under GOWORK=off), so the JSON
// keys are pinned by TestReportBatchWireKeys in the parent package instead of
// a shared import.
package reportstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
)

// ContractVersion identifies the revision of THIS PARSER's contract — not the
// version of the stream it happened to read. It travels with the batch as parse
// provenance so the server can tell which client contract produced the
// summaries. Revision "3" adds task start/finish clocks while retaining the v1
// and v2 stream mappings; the server treats it as an opaque string.
const ContractVersion = "3"

// PutnamiProtocolVersionV2 is the protocolVersion stamp this parser maps onto
// the v1 fold. A line carrying a "record" member with any other stamp is a
// versioned session record from a contract revision we do not speak: it is
// counted as an unknown event and skipped, never guessed at.
const PutnamiProtocolVersionV2 = 2

// MaxLineBytes caps a single JSONL line of the wrapped command's stdout. Longer
// lines are discarded whole and counted as oversized — a runaway line must
// never balloon the parser.
const MaxLineBytes = 1 << 20

// MaxArtifactLineBytes caps a single line of a session artifact. It is the
// bound the putnami CLI puts on one task event line it accepts from a
// subprocess (cli-model jobs, maxJobEventBytes), so a result the CLI kept in
// the complete record is not discarded here. The bounded stdout keeps
// MaxLineBytes.
const MaxArtifactLineBytes = 16 << 20

// Stats is the parse provenance for one stream.
type Stats struct {
	// Lines counts every non-empty line seen (including skipped ones).
	Lines int
	// MalformedLines counts lines that were not a JSON event object.
	MalformedLines int
	// UnknownEvents counts recognized-shaped lines this contract version does
	// not know: an unknown v1 event or job:event type, an unknown v2 record, or
	// a versioned record stamped with a protocolVersion other than
	// PutnamiProtocolVersionV2. They are skipped, never fatal.
	UnknownEvents int
	// OversizedLines counts lines discarded for exceeding the parser's line
	// cap: MaxLineBytes, or MaxArtifactLineBytes for a session artifact.
	OversizedLines int
	// ElidedLines counts the records the producer left out of the folded
	// stream, as its bounded session:end reports them. FoldRun sets it; it is
	// zero for a batch folded from the complete session artifact.
	ElidedLines int
	// Source names the stream FoldRun folded the batch from: SourceStdout or
	// SourceSessionArtifact. It is empty on a bare Parser's batch.
	Source string
}

// TestsReport twins reports.TestsReport.
type TestsReport struct {
	Projects []ProjectTests `json:"projects"`
}

// ProjectTests twins reports.ProjectTests.
type ProjectTests struct {
	Project      string        `json:"project"`
	Passed       int           `json:"passed"`
	Failed       int           `json:"failed"`
	Skipped      int           `json:"skipped"`
	Total        int           `json:"total"`
	FailingCases []FailingCase `json:"failingCases,omitempty"`
}

// FailingCase twins reports.FailingCase.
type FailingCase struct {
	Name    string `json:"name,omitempty"`
	Message string `json:"message,omitempty"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// CoverageReport twins reports.CoverageReport. The client never sets delta
// fields — the server resolves baselines best-effort at ingest time.
type CoverageReport struct {
	Projects []ProjectCoverage `json:"projects"`
}

// ProjectCoverage twins reports.ProjectCoverage (minus the server-computed
// delta).
type ProjectCoverage struct {
	Project           string         `json:"project"`
	CoveredStatements int            `json:"coveredStatements"`
	TotalStatements   int            `json:"totalStatements"`
	Percentage        float64        `json:"percentage"`
	Files             []FileCoverage `json:"files,omitempty"`
}

// FileCoverage twins reports.FileCoverage.
type FileCoverage struct {
	File              string  `json:"file"`
	CoveredStatements int     `json:"coveredStatements"`
	TotalStatements   int     `json:"totalStatements"`
	Percentage        float64 `json:"percentage"`
}

// BuildsReport twins reports.BuildsReport.
type BuildsReport struct {
	Tasks []TaskBuild `json:"tasks"`
}

// TaskBuild twins reports.TaskBuild.
type TaskBuild struct {
	Project string `json:"project"`
	Task    string `json:"task"`
	Status  string `json:"status"`
	// Reuse preserves protocol v2's exact provenance. CacheHit stays as the
	// backwards-compatible binary projection, but cannot distinguish coalesced
	// work from work executed in this session — a distinction cold acceptance
	// must not lose.
	Reuse      string    `json:"reuse,omitempty"`
	DurationMS int64     `json:"durationMs"`
	CacheHit   bool      `json:"cacheHit"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

// Batch is the folded report batch for one stream.
type Batch struct {
	Version  string
	Stats    Stats
	Tests    *TestsReport
	Coverage *CoverageReport
	Builds   *BuildsReport
}

// HasReports reports whether the stream yielded anything worth submitting.
func (b *Batch) HasReports() bool {
	return b != nil && (b.Tests != nil || b.Coverage != nil || b.Builds != nil)
}

// Parser incrementally folds a JSONL stream into a Batch. It implements
// io.Writer so it can tee a wrapped command's stdout without buffering the
// whole stream; call Batch() at run end.
type Parser struct {
	buf       bytes.Buffer
	oversized bool
	flushed   bool
	// maxLine is the line cap: MaxLineBytes for stdout, MaxArtifactLineBytes
	// for a session artifact.
	maxLine int

	stats    Stats
	projects map[string]*projectAgg
	order    []string
	tasks    []TaskBuild
	starts   map[string]time.Time

	// projectNames maps a v2 identity's project id to its report name. A batch
	// result names its members by project id only (batchMemberResult).
	projectNames map[string]string
	// pendingMembers holds batch member summaries whose project id no record has
	// named yet. They fold when a record with that id arrives.
	pendingMembers map[string][]pendingMember
	// directTests and directCoverage record each project+task whose own result
	// event supplied that summary, and directFailing each project+task whose
	// own diagnostic events supplied failing cases. Batch member data never
	// overrides them.
	directTests    map[string]bool
	directCoverage map[string]bool
	directFailing  map[string]bool

	// sessionEnds counts the v2 session:end records; lastSessionEnd is the most
	// recent one. endsOnSessionEnd reports whether the last line seen was that
	// session:end. FoldRun reads them to find and verify the session artifact.
	sessionEnds      int
	lastSessionEnd   sessionEnd
	endsOnSessionEnd bool
	// elidedRecords sums the elision every bounded session:end reports.
	elidedRecords int
}

// sessionEnd is what FoldRun needs from a v2 session:end record: the exact line,
// which the session artifact must end with, and its bounded machineOutput.
type sessionEnd struct {
	line          []byte
	machineOutput *v2MachineOutput
}

// NewParser returns an empty stream parser with the stdout line cap,
// MaxLineBytes.
func NewParser() *Parser {
	return newParser(MaxLineBytes)
}

func newParser(maxLine int) *Parser {
	return &Parser{
		maxLine:        maxLine,
		projects:       make(map[string]*projectAgg),
		starts:         make(map[string]time.Time),
		projectNames:   make(map[string]string),
		pendingMembers: make(map[string][]pendingMember),
		directTests:    make(map[string]bool),
		directCoverage: make(map[string]bool),
		directFailing:  make(map[string]bool),
	}
}

// Write implements io.Writer. It never returns an error: parse trouble is
// counted in Stats, because a broken report stream must not break the tee'd
// command's output path.
func (p *Parser) Write(b []byte) (int, error) {
	total := len(b)
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			p.bufferPartial(b)
			break
		}
		p.bufferPartial(b[:i])
		p.completeLine()
		b = b[i+1:]
	}
	return total, nil
}

// Batch flushes any trailing unterminated line and returns the folded batch.
func (p *Parser) Batch() *Batch {
	if !p.flushed {
		p.flushed = true
		if p.oversized || p.buf.Len() > 0 {
			p.completeLine()
		}
	}
	batch := &Batch{Version: ContractVersion, Stats: p.stats}
	var testProjects []ProjectTests
	var covProjects []ProjectCoverage
	for _, name := range p.order {
		agg := p.projects[name]
		failing := p.failingCases(name, agg)
		if agg.hasTests || len(failing) > 0 {
			project := agg.tests
			project.Project = name
			project.FailingCases = failing
			testProjects = append(testProjects, project)
		}
		if agg.hasCoverage {
			project := agg.coverage
			project.Project = name
			covProjects = append(covProjects, project)
		}
	}
	if len(testProjects) > 0 {
		batch.Tests = &TestsReport{Projects: testProjects}
	}
	if len(covProjects) > 0 {
		batch.Coverage = &CoverageReport{Projects: covProjects}
	}
	if len(p.tasks) > 0 {
		batch.Builds = &BuildsReport{Tasks: p.tasks}
	}
	return batch
}

func (p *Parser) bufferPartial(b []byte) {
	if p.oversized {
		return // discard the rest of the runaway line
	}
	if p.buf.Len()+len(b) > p.maxLine {
		p.oversized = true
		p.buf.Reset()
		return
	}
	p.buf.Write(b)
}

func (p *Parser) completeLine() {
	if p.oversized {
		p.oversized = false
		p.stats.Lines++
		p.stats.OversizedLines++
		p.endsOnSessionEnd = false
		return
	}
	line := bytes.TrimSpace(p.buf.Bytes())
	if len(line) > 0 {
		p.stats.Lines++
		p.endsOnSessionEnd = false
		p.parseLine(line)
	}
	p.buf.Reset()
}

// streamLine is the loose top-level shape of one v1 JSONL event, and the shape
// v2 task records are mapped onto (parseV2Record) so both contracts share one
// fold. Every field is optional by design — tolerance is the contract.
type streamLine struct {
	Event    string          `json:"event"`
	Kind     string          `json:"kind"`
	Package  string          `json:"package"`
	Job      string          `json:"job"`
	Status   string          `json:"status"`
	Duration int64           `json:"duration"`
	Cache    bool            `json:"cache"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
	Time     time.Time       `json:"time"`
}

// versionProbe peeks the putnami v2 discriminators off a line WITHOUT touching
// the rest of it. It has to run first because the two contracts disagree on the
// type of the "event" member: v1 spells the envelope name there as a string,
// while a v2 task:event carries the inner runtime event OBJECT. Decoding a v2
// line into streamLine would therefore fail as malformed, so the probe routes
// before the v1 decode ever runs. A mixed file is not a real case, but it costs
// nothing to make it a per-line decision.
type versionProbe struct {
	Record          string `json:"record"`
	ProtocolVersion int    `json:"protocolVersion"`
}

func (p *Parser) parseLine(raw []byte) {
	var probe versionProbe
	if json.Unmarshal(raw, &probe) == nil && probe.Record != "" {
		if probe.ProtocolVersion == PutnamiProtocolVersionV2 {
			p.parseV2Record(raw)
		} else {
			p.stats.UnknownEvents++
		}
		return
	}
	var line streamLine
	if err := json.Unmarshal(raw, &line); err != nil {
		p.stats.MalformedLines++
		return
	}
	// The runner owns its own marker kinds around the Putnami event stream. A known,
	// minimally valid marker is framing, not a malformed Putnami event and not
	// report data. An unknown kind or an incomplete payload stays malformed so a
	// broken runner envelope cannot suppress the honest partial-stream signal.
	if line.Event == "" {
		if validRunnerFraming(raw, line.Kind) {
			return
		}
		p.stats.MalformedLines++
		return
	}
	switch line.Event {
	case "job:start":
		p.starts[taskTimingKey(line.Package, line.Job)] = line.Time
	case "session:end":
		// Recognized; the batch is built from job:end and job:event records.
	case "job:end":
		started := p.starts[taskTimingKey(line.Package, line.Job)]
		delete(p.starts, taskTimingKey(line.Package, line.Job))
		if started.IsZero() && !line.Time.IsZero() && line.Duration > 0 {
			started = line.Time.Add(-time.Duration(line.Duration) * time.Millisecond)
		}
		p.tasks = append(p.tasks, TaskBuild{
			Project:    line.Package,
			Task:       line.Job,
			Status:     line.Status,
			DurationMS: line.Duration,
			CacheHit:   line.Cache,
			StartedAt:  started,
			FinishedAt: line.Time,
		})
	case "job:event":
		p.parseJobEvent(line)
	default:
		p.stats.UnknownEvents++
	}
}

// v2Record is the loose local twin of protocols/cli SessionStreamRecord. Like
// the report payload twins above it is a copy rather than an import: this module
// stays off both the server and the putnami dependency trees (it compiles under
// GOWORK=off), and tolerance means every member is optional here even where the
// contract makes it required.
type v2Record struct {
	Record        string           `json:"record"`
	Identity      *v2Identity      `json:"identity"`
	Event         json.RawMessage  `json:"event"`
	Task          *v2TaskRecord    `json:"task"`
	Time          time.Time        `json:"time"`
	MachineOutput *v2MachineOutput `json:"machineOutput"`
}

// v2MachineOutput twins the members of protocols/cli MachineOutputSummary that
// FoldRun reads off a bounded session:end: how many records the live copy
// elided, and the session whose events.jsonl artifact holds them all.
type v2MachineOutput struct {
	Elided struct {
		Ordinary struct {
			Records int `json:"records"`
		} `json:"ordinary"`
		Failure struct {
			Records int `json:"records"`
		} `json:"failure"`
	} `json:"elided"`
	Artifact struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
	} `json:"artifact"`
}

func (m *v2MachineOutput) elidedRecords() int {
	if m == nil {
		return 0
	}
	return m.Elided.Ordinary.Records + m.Elided.Failure.Records
}

// v2Identity twins protocols/cli TaskIdentity. Key is deliberately NOT read —
// see projectName/taskName below.
type v2Identity struct {
	Project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Task struct {
		Name string `json:"name"`
	} `json:"task"`
}

// v2TaskRecord twins the members of protocols/cli TaskRecord this fold needs.
// Diagnostics ride here too, and are deliberately not folded — see parseV2Record.
type v2TaskRecord struct {
	Status     string `json:"status"`
	Reuse      string `json:"reuse"`
	DurationMs int64  `json:"durationMs"`
	TaskWallMs int64  `json:"taskWallMs"`
}

// reportedDurationMS keeps the two protocol clocks honest. durationMs is the
// task execution clock, so it is correctly zero when no task executed. A cache
// hit instead reports the wall time spent materializing its result in
// taskWallMs. Older v2 producers put the replay cost in durationMs and omitted
// taskWallMs, hence the compatibility fallback.
func (t *v2TaskRecord) reportedDurationMS(cacheHit bool) int64 {
	if cacheHit && t.TaskWallMs > 0 {
		return t.TaskWallMs
	}
	return t.DurationMs
}

// v2 task reuse provenance. CacheHit is local-or-remote cache ONLY: coalescing
// is reuse but not a hit, which is exactly the predicate v1's boolean "cache"
// member encoded (putnami jobs.ReuseKind.CacheHit).
const (
	v2ReuseLocalCache  = "local-cache"
	v2ReuseRemoteCache = "remote-cache"
)

// projectName is the report's project key for a v2 task.
//
// IT IS identity.project.name, NOT identity.key. v1's job:end put
// job.Project.Name in "package" and job.JobDef.Name in "job" (putnami
// internal/output/jsonl.go, JobComplete), so the clean identity members restore
// the v1 strings byte-for-byte. The plan-key-in-both-fields quirk v2's contract
// calls out belongs to v1's session:end failures array (jsonl.go, Finish), and
// this parser has always skipped session:end — the quirk never reached a batch,
// so there is nothing to preserve.
//
// The choice is forced by what the ingest keys on, not by taste: the records
// reports feature joins coverage across runs on this exact
// string — ApplyCoverageDelta builds its baseline map from ProjectCoverage.Project
// — and the tests/coverage entries are keyed from task:event records by the same
// helper. A key ("/protocols/cli:build~tidy") would be per-TASK, not per-project,
// so it would fragment tests/coverage and break the baseline join against every
// report a v1-era CLI already stored.
//
// validReportKey confirms the identity carries the exact v1-compatible strings
// this parser keys reports on. The v2 contract requires both; accepting a
// partial identity would put data under an id (or an empty task) and silently
// fragment report history.
func (i *v2Identity) validReportKey() bool {
	return i != nil && i.Project.Name != "" && i.Task.Name != ""
}

func (i *v2Identity) projectName() string { return i.Project.Name }

// taskName is the report's task key: the canonical plan name, which is the same
// string v1's job:end put in "job".
func (i *v2Identity) taskName() string { return i.Task.Name }

// parseV2Record folds one putnami v2 session record onto the v1 fold. The two
// contracts carry the same report inputs under different names, so v2 is mapped
// rather than given a second aggregation path — one fold, two wire spellings.
func (p *Parser) parseV2Record(raw []byte) {
	var rec v2Record
	if json.Unmarshal(raw, &rec) != nil {
		p.stats.MalformedLines++
		return
	}
	if rec.Identity.validReportKey() {
		p.learnProject(rec.Identity.Project.ID, rec.Identity.projectName())
	}
	switch rec.Record {
	case "task:start":
		if rec.Identity.validReportKey() {
			p.starts[taskTimingKey(rec.Identity.projectName(), rec.Identity.taskName())] = rec.Time
		}
	case "session:end":
		// v2's session:end carries a run summary, but the batch is
		// per-project/per-task report data and has nowhere to put a run
		// verdict; the conclusion travels on the run submission instead.
		// FoldRun reads the record to find the session's complete artifact.
		p.sessionEnds++
		p.lastSessionEnd = sessionEnd{line: bytes.Clone(raw), machineOutput: rec.MachineOutput}
		p.endsOnSessionEnd = true
		p.elidedRecords += rec.MachineOutput.elidedRecords()
	case "test:case":
		// Recognized, intentionally not folded: a case is detail under its
		// task's result, whose testSummary already counts it. The complete
		// session artifact always carries these records; normal-mode stdout
		// elides them.
	case "task:end":
		// Identity, its report keys, and task are required by the contract; a
		// partial record is malformed rather than a task with empty or id-based
		// report keys.
		if !rec.Identity.validReportKey() || rec.Task == nil {
			p.stats.MalformedLines++
			return
		}
		// Diagnostics on the task record are deliberately NOT folded, and the
		// record carries no test or coverage summary. A task that ran on its own
		// delivered the same diagnostics as task:event records, and folding both
		// would double every failing case. A cache hit replays its stored result
		// as a task:event, so its summaries fold there. A batch member has no
		// task:event of its own: its summaries and its error diagnostics fold
		// from the batch result's batchResults entry (foldResult).
		key := taskTimingKey(rec.Identity.projectName(), rec.Identity.taskName())
		started := p.starts[key]
		delete(p.starts, key)
		if started.IsZero() && !rec.Time.IsZero() && rec.Task.DurationMs > 0 {
			started = rec.Time.Add(-time.Duration(rec.Task.DurationMs) * time.Millisecond)
		}
		cacheHit := rec.Task.Reuse == v2ReuseLocalCache || rec.Task.Reuse == v2ReuseRemoteCache
		p.tasks = append(p.tasks, TaskBuild{
			Project:    rec.Identity.projectName(),
			Task:       rec.Identity.taskName(),
			Status:     rec.Task.Status,
			Reuse:      rec.Task.Reuse,
			DurationMS: rec.Task.reportedDurationMS(cacheHit),
			CacheHit:   cacheHit,
			StartedAt:  started,
			FinishedAt: rec.Time,
		})
	case "task:event":
		if !rec.Identity.validReportKey() || len(rec.Event) == 0 {
			p.stats.MalformedLines++
			return
		}
		// The v2 "event" member IS v1's "data" member: the same protocols/runtime
		// subprocess event object, with the type v1 also hoisted to the envelope
		// still inside it. Rebuilding the v1 line is therefore lossless.
		var inner struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(rec.Event, &inner) != nil {
			p.stats.MalformedLines++
			return
		}
		p.parseJobEvent(streamLine{
			Package: rec.Identity.projectName(),
			Job:     rec.Identity.taskName(),
			Type:    inner.Type,
			Data:    rec.Event,
		})
	default:
		p.stats.UnknownEvents++
	}
}

func taskTimingKey(project, task string) string { return project + "\x00" + task }

// v2BatchTaskSuffix ends the task name of the workspace-scoped identity a v2
// stream records a shared batch subprocess under (putnami
// machine.BatchTaskEvent). Every member of one batch shares the task, so the
// name without the suffix is each member's task name.
const v2BatchTaskSuffix = "-batch"

// learnProject records the report name of a v2 project id, then folds the batch
// member summaries that were waiting for it. The first name seen for an id
// wins.
func (p *Parser) learnProject(id, name string) {
	if id == "" {
		return
	}
	if _, known := p.projectNames[id]; known {
		return
	}
	p.projectNames[id] = name
	for _, member := range p.pendingMembers[id] {
		p.foldMember(name, member)
	}
	delete(p.pendingMembers, id)
}

func validRunnerFraming(raw []byte, kind string) bool {
	switch kind {
	case "ci.runner.start":
		var marker struct {
			RunID       string `json:"runId"`
			WorkspaceID string `json:"workspaceId"`
			SHA         string `json:"sha"`
			CacheURL    string `json:"cacheUrl"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.WorkspaceID != "" && marker.SHA != "" && marker.CacheURL != ""
	case "ci.runner.drift", "ci.runner.end":
		var marker struct {
			RunID    string `json:"runId"`
			ExitCode *int   `json:"exitCode"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.ExitCode != nil
	case "ci.runner.ingest":
		var marker struct {
			RunID      string `json:"runId"`
			Conclusion string `json:"conclusion"`
			Posted     *bool  `json:"posted"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Conclusion != "" && marker.Posted != nil
	case "ci.runner.publish":
		// One per impacted workload image the runner published to the pr-<number>
		// channel: a success carries the immutable digest, a failure sets
		// published=false and omits it. Both are valid runner framing — not report
		// data the CLI folds — so a healthy publish stream is never annotated
		// reports: partial. The digests reach the saga via the terminal ingest, not
		// this stream.
		var marker struct {
			RunID     string `json:"runId"`
			Package   string `json:"package"`
			Channel   string `json:"channel"`
			Published *bool  `json:"published"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Package != "" && marker.Channel != "" && marker.Published != nil
	case "ci.runner.phase":
		// One per per-phase lifecycle fact the runner derives from the gate JSONL and
		// posts to the credential-authed per-phase ingest: a start carries an
		// empty conclusion, a conclusion carries success/failure/etc. Both are valid
		// runner framing — not report data the CLI folds — so a healthy phase stream
		// is never annotated reports: partial. The facts reach the phase-attempt
		// model via the /phases POST, not this stream.
		var marker struct {
			RunID  string `json:"runId"`
			Phase  string `json:"phase"`
			Status string `json:"status"`
			Posted *bool  `json:"posted"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Phase != "" && marker.Status != "" && marker.Posted != nil
	case "ci.runner.selection":
		// The run's own planned task set, emitted BEFORE the gate
		// and posted to the reports ingest so the control plane can predict the
		// wall while a bounce still costs two minutes. Like the phase and publish
		// markers this is runner FRAMING rather than report data this parser
		// folds — the selection reaches the plane on its own POST — so a run that
		// simply could not compute one must never annotate its stream partial.
		// Tasks is a pointer because an honest zero (a change that plans nothing)
		// is a real answer and must be distinguishable from an absent field.
		var marker struct {
			RunID  string `json:"runId"`
			Tasks  *int   `json:"tasks"`
			Posted *bool  `json:"posted"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Tasks != nil && marker.Posted != nil
	case "ci.runner.gate-recovery":
		// One per run that reached the gate: whether the runner replayed a
		// pure infrastructure timeout, and if not, which guard refused. Like the
		// phase and publish markers this is runner framing rather than report data
		// — the recovery evidence reaches the plane on the terminal ingest — so a
		// run that merely declined to retry must never annotate its stream partial.
		// Attempts is a pointer because a skipped gate honestly reports 0.
		var marker struct {
			RunID    string `json:"runId"`
			Attempts *int   `json:"attempts"`
			Recovery string `json:"recovery"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Attempts != nil && marker.Recovery != ""
	case "ci.runner.heartbeat":
		// One per gate session: how many in-flight liveness beats that
		// session posted, and how many were dropped. Like the phase, publish and
		// recovery markers this is runner framing rather than report data — the
		// beats themselves reach the plane on the /heartbeats POST, not this stream
		// — so a run that beat (or a run whose beats all failed best-effort) must
		// never annotate its stream partial. Posted is a pointer because a session
		// that beat zero times honestly reports 0, which is a fact rather than an
		// absent field.
		var marker struct {
			RunID   string `json:"runId"`
			Session string `json:"session"`
			Posted  *int   `json:"posted"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Session != "" && marker.Posted != nil
	case "ci.runner.changeplan":
		// One per run that reached the gate: whether the immutable change
		// plan the run was handed agreed with the selection the gate actually made.
		// Like the phase, publish, recovery and heartbeat markers this is runner
		// framing rather than report data — the verdict reaches the plane on the
		// terminal ingest, not this stream — so a run that had no plan, or one whose
		// plan disagreed, must never annotate its stream partial. Status is required
		// because "absent" is itself a verdict, and Version is a pointer so a
		// malformed marker declaring no contract version stays malformed.
		var marker struct {
			RunID   string `json:"runId"`
			Status  string `json:"status"`
			Version *int   `json:"version"`
		}
		return json.Unmarshal(raw, &marker) == nil && marker.RunID != "" && marker.Status != "" && marker.Version != nil
	default:
		return false
	}
}

func (p *Parser) parseJobEvent(line streamLine) {
	switch line.Type {
	case "meta", "phase", "progress", "log", "summary", "artifact":
		// Recognized, intentionally not folded: logs and artifact contents are
		// Observability's, and summary is a human string shadowing the
		// structured metric/result data below.
	case "metric":
		var metric struct {
			Name  string  `json:"name"`
			Value float64 `json:"value"`
		}
		if json.Unmarshal(line.Data, &metric) != nil || metric.Name == "" {
			p.stats.MalformedLines++
			return
		}
		p.applyMetric(line.Package, metric.Name, metric.Value)
	case "result":
		var result struct {
			Data resultData `json:"data"`
		}
		if json.Unmarshal(line.Data, &result) != nil {
			p.stats.MalformedLines++
			return
		}
		p.foldResult(line.Package, line.Job, result.Data)
	case "diagnostic":
		var diag struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
			Message  string `json:"message"`
			Location struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"location"`
		}
		if json.Unmarshal(line.Data, &diag) != nil {
			p.stats.MalformedLines++
			return
		}
		// Error-severity diagnostics are the failing-case references; warnings
		// (e.g. UNCOVERED_FILE) are already represented in the coverage data.
		if diag.Severity == "error" {
			p.directFailing[taskTimingKey(line.Package, line.Job)] = true
			agg := p.agg(line.Package)
			agg.failing = append(agg.failing, failingCase{task: line.Job, FailingCase: FailingCase{
				Name:    diag.Code,
				Message: diag.Message,
				File:    diag.Location.File,
				Line:    diag.Location.Line,
			}})
		}
	default:
		p.stats.UnknownEvents++
	}
}

// resultSummaries is the report input of one result: a task's own result data,
// or one batch member's data.
type resultSummaries struct {
	TestSummary *struct {
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Skipped int `json:"skipped"`
		Total   int `json:"total"`
	} `json:"testSummary"`
	CoverageSummary *struct {
		CoveredStatements int     `json:"coveredStatements"`
		TotalStatements   int     `json:"totalStatements"`
		Percentage        float64 `json:"percentage"`
		Files             []struct {
			File              string  `json:"file"`
			CoveredStatements int     `json:"coveredStatements"`
			TotalStatements   int     `json:"totalStatements"`
			Percentage        float64 `json:"percentage"`
		} `json:"files"`
	} `json:"coverageSummary"`
}

func (s resultSummaries) empty() bool {
	return s.TestSummary == nil && s.CoverageSummary == nil
}

// resultData is the data of a result event. BatchResults is set only on the
// result of a shared batch subprocess (protocols/extension, batched
// execution): one entry per member, each with that member's own data.
type resultData struct {
	resultSummaries
	BatchResults []batchMemberResult `json:"batchResults"`
}

// batchMemberResult is one batchResults entry. It names its member by project
// id, which learnProject resolves to the report name.
type batchMemberResult struct {
	ProjectID   string            `json:"projectId"`
	Data        resultSummaries   `json:"data"`
	Diagnostics []batchDiagnostic `json:"diagnostics"`
}

// batchDiagnostic is one batchResults[i].diagnostics entry, in the shape every
// batch producer writes (protocols/extension, batched execution).
type batchDiagnostic struct {
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
	File        string `json:"file"`
	Line        int    `json:"line"`
}

// memberFailingCases maps a member's error diagnostics to failing cases the way
// a task that runs on its own reaches them. That task's extension emits each
// diagnostic as a runtime diagnostic event: the category as its code, the
// description as its message, and a location only when there is a file, so a
// line without a file is dropped (extension-sdk jsonl, DiagnosticWithCode).
// parseJobEvent then maps that event to a FailingCase.
func (m batchMemberResult) memberFailingCases() []FailingCase {
	var cases []FailingCase
	for _, d := range m.Diagnostics {
		if d.Severity != "error" {
			continue
		}
		c := FailingCase{Name: d.Category, Message: d.Description}
		if d.File != "" {
			c.File = d.File
			c.Line = d.Line
		}
		cases = append(cases, c)
	}
	return cases
}

// pendingMember is a batch member's report input waiting for its project name.
type pendingMember struct {
	task      string
	summaries resultSummaries
	failing   []FailingCase
}

// foldResult folds one result event. Its own summaries belong to the event's
// project and task. A batch result's entries belong to each member, under the
// batch task name without v2BatchTaskSuffix.
//
// A v2 stream records no task:event for a batch member: the member's attributed
// result and its diagnostics travel only in this entry, and its task:end
// carries no summary. A member whose project id no record has named yet waits
// in pendingMembers. When no record ever names it, it folds nowhere: keying a
// report by project id would fragment the report history (validReportKey).
func (p *Parser) foldResult(project, task string, data resultData) {
	p.foldSummaries(project, task, data.resultSummaries, false)
	memberTask := strings.TrimSuffix(task, v2BatchTaskSuffix)
	for _, member := range data.BatchResults {
		failing := member.memberFailingCases()
		if member.ProjectID == "" || (member.Data.empty() && len(failing) == 0) {
			continue
		}
		if name, known := p.projectNames[member.ProjectID]; known {
			p.foldMember(name, pendingMember{task: memberTask, summaries: member.Data, failing: failing})
			continue
		}
		p.pendingMembers[member.ProjectID] = append(p.pendingMembers[member.ProjectID],
			pendingMember{task: memberTask, summaries: member.Data, failing: failing})
	}
}

// foldMember folds one batch member's summaries and failing cases into its
// project's aggregate.
func (p *Parser) foldMember(project string, member pendingMember) {
	p.foldSummaries(project, member.task, member.summaries, true)
	if len(member.failing) == 0 {
		return
	}
	agg := p.agg(project)
	for _, c := range member.failing {
		agg.failing = append(agg.failing, failingCase{task: member.task, fromBatch: true, FailingCase: c})
	}
}

// failingCases returns a project's failing cases in stream order. A task's own
// error diagnostics win over batch member diagnostics for the same project and
// task, in either stream order, so a member that also ran on its own does not
// list its failures twice.
func (p *Parser) failingCases(project string, agg *projectAgg) []FailingCase {
	var cases []FailingCase
	for _, c := range agg.failing {
		if c.fromBatch && p.directFailing[taskTimingKey(project, c.task)] {
			continue
		}
		cases = append(cases, c.FailingCase)
	}
	return cases
}

// foldSummaries folds one result's test and coverage summaries into the
// project's aggregate. A summary sets testsFromResult or coverageFromResult,
// so it wins over flat metrics either way.
//
// The rows are per project, not per task: each summary replaces the project's
// previous one, so when one project runs two test tasks, the last summary in
// the stream sets its row. The one exception is batch member data: it never
// replaces a summary that the same project and task's own result event
// supplied, and a later own result replaces it, in either stream order.
func (p *Parser) foldSummaries(project, task string, s resultSummaries, fromBatch bool) {
	key := taskTimingKey(project, task)
	if s.TestSummary != nil && !(fromBatch && p.directTests[key]) {
		agg := p.agg(project)
		agg.hasTests = true
		agg.testsFromResult = true
		agg.tests.Passed = s.TestSummary.Passed
		agg.tests.Failed = s.TestSummary.Failed
		agg.tests.Skipped = s.TestSummary.Skipped
		agg.tests.Total = s.TestSummary.Total
		if !fromBatch {
			p.directTests[key] = true
		}
	}
	if cov := s.CoverageSummary; cov != nil && !(fromBatch && p.directCoverage[key]) {
		agg := p.agg(project)
		agg.hasCoverage = true
		agg.coverageFromResult = true
		agg.coverage.CoveredStatements = cov.CoveredStatements
		agg.coverage.TotalStatements = cov.TotalStatements
		agg.coverage.Percentage = cov.Percentage
		agg.coverage.Files = nil
		for _, f := range cov.Files {
			agg.coverage.Files = append(agg.coverage.Files, FileCoverage{
				File:              f.File,
				CoveredStatements: f.CoveredStatements,
				TotalStatements:   f.TotalStatements,
				Percentage:        f.Percentage,
			})
		}
		if !fromBatch {
			p.directCoverage[key] = true
		}
	}
}

// applyMetric folds the flat metric fallbacks. Result summaries win when both
// arrive; unknown metric names (generate-hash, …) are simply not report input.
func (p *Parser) applyMetric(pkg, name string, value float64) {
	switch name {
	case "tests-total", "tests-passed", "tests-failed", "tests-skipped":
		agg := p.agg(pkg)
		agg.hasTests = true
		if agg.testsFromResult {
			return
		}
		switch name {
		case "tests-total":
			agg.tests.Total = int(value)
		case "tests-passed":
			agg.tests.Passed = int(value)
		case "tests-failed":
			agg.tests.Failed = int(value)
		case "tests-skipped":
			agg.tests.Skipped = int(value)
		}
	case "coverage", "coverage-statements-covered", "coverage-statements-total":
		agg := p.agg(pkg)
		agg.hasCoverage = true
		if agg.coverageFromResult {
			return
		}
		switch name {
		case "coverage":
			agg.coverage.Percentage = value
		case "coverage-statements-covered":
			agg.coverage.CoveredStatements = int(value)
		case "coverage-statements-total":
			agg.coverage.TotalStatements = int(value)
		}
	}
}

// projectAgg accumulates one project's report inputs across the stream.
// Result summaries (the richest source) take precedence over flat metrics.
type projectAgg struct {
	hasTests        bool
	testsFromResult bool
	tests           ProjectTests

	hasCoverage        bool
	coverageFromResult bool
	coverage           ProjectCoverage

	failing []failingCase
}

// failingCase is one failing case with the task it came from, and whether it
// came from batch member data, so failingCases can let a task's own
// diagnostics win.
type failingCase struct {
	task      string
	fromBatch bool
	FailingCase
}

func (p *Parser) agg(pkg string) *projectAgg {
	if existing, ok := p.projects[pkg]; ok {
		return existing
	}
	agg := &projectAgg{}
	p.projects[pkg] = agg
	p.order = append(p.order, pkg)
	return agg
}
