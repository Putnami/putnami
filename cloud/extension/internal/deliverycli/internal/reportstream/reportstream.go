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

// MaxLineBytes caps a single JSONL line. Longer lines are discarded whole and
// counted as oversized — a runaway line must never balloon the parser.
const MaxLineBytes = 1 << 20

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
	// OversizedLines counts lines discarded for exceeding MaxLineBytes.
	OversizedLines int
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

	stats    Stats
	projects map[string]*projectAgg
	order    []string
	tasks    []TaskBuild
	starts   map[string]time.Time
}

// NewParser returns an empty stream parser.
func NewParser() *Parser {
	return &Parser{projects: make(map[string]*projectAgg), starts: make(map[string]time.Time)}
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
		if agg.hasTests || len(agg.failing) > 0 {
			project := agg.tests
			project.Project = name
			project.FailingCases = agg.failing
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
	if p.buf.Len()+len(b) > MaxLineBytes {
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
		return
	}
	line := bytes.TrimSpace(p.buf.Bytes())
	if len(line) > 0 {
		p.stats.Lines++
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
	Record   string          `json:"record"`
	Identity *v2Identity     `json:"identity"`
	Event    json.RawMessage `json:"event"`
	Task     *v2TaskRecord   `json:"task"`
	Time     time.Time       `json:"time"`
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
	switch rec.Record {
	case "task:start":
		if rec.Identity.validReportKey() {
			p.starts[taskTimingKey(rec.Identity.projectName(), rec.Identity.taskName())] = rec.Time
		}
	case "session:end":
		// Recognized. v2's session:end carries a run summary, but the
		// batch is per-project/per-task report data and has nowhere to put a run
		// verdict; the conclusion travels on the run submission instead.
	case "task:end":
		// Identity, its report keys, and task are required by the contract; a
		// partial record is malformed rather than a task with empty or id-based
		// report keys.
		if !rec.Identity.validReportKey() || rec.Task == nil {
			p.stats.MalformedLines++
			return
		}
		// Diagnostics on the task record are deliberately NOT folded: they are the
		// same diagnostics the task:event stream already delivered, and folding
		// both would double every failing case. (A cache hit replays neither —
		// a known upstream gap in the Putnami CLI.)
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
			Data struct {
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
			} `json:"data"`
		}
		if json.Unmarshal(line.Data, &result) != nil {
			p.stats.MalformedLines++
			return
		}
		if result.Data.TestSummary != nil {
			agg := p.agg(line.Package)
			agg.hasTests = true
			agg.testsFromResult = true
			agg.tests.Passed = result.Data.TestSummary.Passed
			agg.tests.Failed = result.Data.TestSummary.Failed
			agg.tests.Skipped = result.Data.TestSummary.Skipped
			agg.tests.Total = result.Data.TestSummary.Total
		}
		if cov := result.Data.CoverageSummary; cov != nil {
			agg := p.agg(line.Package)
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
		}
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
			agg := p.agg(line.Package)
			agg.failing = append(agg.failing, FailingCase{
				Name:    diag.Code,
				Message: diag.Message,
				File:    diag.Location.File,
				Line:    diag.Location.Line,
			})
		}
	default:
		p.stats.UnknownEvents++
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

	failing []FailingCase
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
