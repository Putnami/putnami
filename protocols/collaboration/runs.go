package collaboration

import (
	"fmt"
	"slices"
	"time"
)

// Operations of missions version 2 that record runs (ADR 0010).
const (
	// OperationStart starts a run of one kind inside a mission.
	OperationStart = "start"
	// OperationReport ends a running run with its final state and result.
	OperationReport = "report"
	// OperationRuns reads a page of a mission's runs.
	OperationRuns = "runs"
)

// Reasons a missions version 2 provider answers about a run.
const (
	// ReasonRunClosed is the conflict of a report on a run that already ended
	// with another report.
	ReasonRunClosed = "run.closed"
	// ReasonRunNotFound is the refusal of a report on a run the provider does
	// not know.
	ReasonRunNotFound = "run.not_found"
	// ReasonRunDenied is the refusal of a report on a run the caller may not
	// report.
	ReasonRunDenied = "run.denied"
	// ReasonRunKindMismatch is the refusal of a report whose result is of
	// another kind than the run's.
	ReasonRunKindMismatch = "run.kind_mismatch"
)

// Bounds on the members of a run. Lengths count characters.
const (
	// MaxRunFindings bounds the findings of one result.
	MaxRunFindings = 100
	// MaxFindingsBytes bounds the titles and bodies of a result's findings,
	// together, in bytes.
	MaxFindingsBytes = 64 << 10
	// MaxFindingBodyLength bounds the body of one finding.
	MaxFindingBodyLength = 8192
	// MaxFindingLine bounds the line a finding points at.
	MaxFindingLine = 10_000_000
	// MaxRunTextLength bounds a build intention and a triage summary.
	MaxRunTextLength = 8192
	// MaxAuditScopeLength bounds the scope of an audit, one line.
	MaxAuditScopeLength = 256
)

// RunKind is the kind of agent work a run executes. Each kind has its own
// closed input and result documents.
type RunKind string

// Run kinds.
const (
	// RunKindAudit audits a scope of the repository at one revision.
	RunKindAudit RunKind = "audit"
	// RunKindBuild delivers a task or an intention as a change.
	RunKindBuild RunKind = "build"
	// RunKindRebase moves a change onto its base.
	RunKindRebase RunKind = "rebase"
	// RunKindReview reviews a change at one commit.
	RunKindReview RunKind = "review"
	// RunKindTriage sorts a task: its state, its labels, its duplicate.
	RunKindTriage RunKind = "triage"
)

// ValidRunKinds is the closed run-kind vocabulary in canonical order.
var ValidRunKinds = []RunKind{RunKindAudit, RunKindBuild, RunKindRebase, RunKindReview, RunKindTriage}

// RunState is where a run stands. A run starts running and ends once, in one
// of the other states; an ended run never changes again.
type RunState string

// Run states.
const (
	// RunStateRunning: the run started and has not ended.
	RunStateRunning RunState = "running"
	// RunStateSucceeded: the run produced its result.
	RunStateSucceeded RunState = "succeeded"
	// RunStateFailed: the run stopped on an error, which failure states.
	RunStateFailed RunState = "failed"
	// RunStateCanceled: the run was stopped before it ended.
	RunStateCanceled RunState = "canceled"
)

// ValidRunStates is the closed run-state vocabulary in canonical order.
var ValidRunStates = []RunState{RunStateCanceled, RunStateFailed, RunStateRunning, RunStateSucceeded}

// FindingSeverity ranks a finding.
type FindingSeverity string

// Finding severities.
const (
	FindingSeverityHigh   FindingSeverity = "high"
	FindingSeverityMedium FindingSeverity = "medium"
	FindingSeverityLow    FindingSeverity = "low"
	FindingSeverityInfo   FindingSeverity = "info"
)

// ValidFindingSeverities is the closed severity vocabulary in canonical order.
var ValidFindingSeverities = []FindingSeverity{FindingSeverityHigh, FindingSeverityInfo, FindingSeverityLow, FindingSeverityMedium}

// AuditVerdict is the outcome of an audit.
type AuditVerdict string

// Audit verdicts.
const (
	// AuditVerdictPass: the scope meets what the audit checks.
	AuditVerdictPass AuditVerdict = "pass"
	// AuditVerdictConcerns: the scope passes with findings to address.
	AuditVerdictConcerns AuditVerdict = "concerns"
	// AuditVerdictFail: the scope does not meet what the audit checks.
	AuditVerdictFail AuditVerdict = "fail"
)

// ValidAuditVerdicts is the closed audit-verdict vocabulary in canonical order.
var ValidAuditVerdicts = []AuditVerdict{AuditVerdictConcerns, AuditVerdictFail, AuditVerdictPass}

// Finding is one problem a review or an audit reports.
type Finding struct {
	// Severity ranks the finding; see the FindingSeverity* constants.
	Severity FindingSeverity `json:"severity"`
	// Title states the finding on one line.
	Title string `json:"title"`
	// File is the repository-relative path the finding points at, when it
	// points at one.
	File string `json:"file,omitempty"`
	// Line is the 1-based line of File the finding points at. It needs File.
	Line int `json:"line,omitempty"`
	// Body explains the finding and what to do about it.
	Body string `json:"body,omitempty"`
}

// BuildRunInput is what a build run delivers: a task, an intention, or both.
type BuildRunInput struct {
	// Task is the task the build delivers.
	Task *Ref `json:"task,omitempty"`
	// Intention states the outcome in words when no task names it.
	Intention string `json:"intention,omitempty"`
	// Change is the branch the build works on, when it is known at start.
	Change *Change `json:"change,omitempty"`
}

// BuildRunResult is where a build run left its change.
type BuildRunResult struct {
	// Stage is the stage of the contribution loop the change reached.
	Stage LoopStage `json:"stage"`
	// Proposal is the change proposal the build published, when it did.
	Proposal *Ref `json:"proposal,omitempty"`
	// Commit is the head commit the build left, when it committed.
	Commit string `json:"commit,omitempty"`
}

// ReviewRunInput is the change a review run reviews, at one commit.
type ReviewRunInput struct {
	// Change is the change reviewed.
	Change Change `json:"change"`
	// Commit is the head commit reviewed.
	Commit string `json:"commit"`
	// Proposal is the change proposal reviewed, when there is one.
	Proposal *Ref `json:"proposal,omitempty"`
}

// ReviewRunResult is a review's verdict and findings.
type ReviewRunResult struct {
	// Commit is the head commit the review read.
	Commit string `json:"commit"`
	// Verdict is the review's verdict; see the ReviewVerdict* constants.
	Verdict ReviewVerdict `json:"verdict"`
	// Findings are the problems found, the most severe first. None is [].
	Findings []Finding `json:"findings"`
}

// RebaseRunInput is the change a rebase run moves onto its base.
type RebaseRunInput struct {
	// Change is the change to rebase.
	Change Change `json:"change"`
	// Commit is the head commit before the rebase, when known.
	Commit string `json:"commit,omitempty"`
}

// RebaseRunResult is where a rebase left the change.
type RebaseRunResult struct {
	// Commit is the head commit after the rebase.
	Commit string `json:"commit"`
	// Onto is the base commit the change now starts from.
	Onto string `json:"onto"`
}

// AuditRunInput is what an audit run audits.
type AuditRunInput struct {
	// Scope names what is audited on one line, such as a project or a path.
	Scope string `json:"scope"`
	// Revision is the commit audited.
	Revision string `json:"revision"`
}

// AuditRunResult is an audit's verdict and findings.
type AuditRunResult struct {
	// Scope is what was audited, on one line.
	Scope string `json:"scope"`
	// Revision is the commit audited.
	Revision string `json:"revision"`
	// Verdict is the audit's outcome; see the AuditVerdict* constants.
	Verdict AuditVerdict `json:"verdict"`
	// Findings are the problems found, the most severe first. None is [].
	Findings []Finding `json:"findings"`
}

// TriageRunInput is the task a triage run sorts.
type TriageRunInput struct {
	// Task is the task triaged.
	Task Ref `json:"task"`
}

// TriageRunResult is what a triage decided about its task.
type TriageRunResult struct {
	// Summary states the decision and its reason.
	Summary string `json:"summary"`
	// State is the semantic state the task should take, when the triage
	// moves it.
	State TaskState `json:"state,omitempty"`
	// Labels are the labels the task should carry, when the triage sets them.
	Labels []string `json:"labels,omitempty"`
	// DuplicateOf is the task this one duplicates, when it does.
	DuplicateOf *Ref `json:"duplicateOf,omitempty"`
}

// RunInput is a run's input: exactly one member, named by the run's kind.
type RunInput struct {
	Audit  *AuditRunInput  `json:"audit,omitempty"`
	Build  *BuildRunInput  `json:"build,omitempty"`
	Rebase *RebaseRunInput `json:"rebase,omitempty"`
	Review *ReviewRunInput `json:"review,omitempty"`
	Triage *TriageRunInput `json:"triage,omitempty"`
}

// Kinds returns the kinds whose member the input carries, in canonical order.
func (i RunInput) Kinds() []RunKind {
	return presentKinds(i.Audit != nil, i.Build != nil, i.Rebase != nil, i.Review != nil, i.Triage != nil)
}

// RunResult is a succeeded run's result: exactly one member, named by the
// run's kind.
type RunResult struct {
	Audit  *AuditRunResult  `json:"audit,omitempty"`
	Build  *BuildRunResult  `json:"build,omitempty"`
	Rebase *RebaseRunResult `json:"rebase,omitempty"`
	Review *ReviewRunResult `json:"review,omitempty"`
	Triage *TriageRunResult `json:"triage,omitempty"`
}

// Kinds returns the kinds whose member the result carries, in canonical
// order.
func (r RunResult) Kinds() []RunKind {
	return presentKinds(r.Audit != nil, r.Build != nil, r.Rebase != nil, r.Review != nil, r.Triage != nil)
}

// Kind returns the one kind the result carries, or "" when it carries none
// or several.
func (r RunResult) Kind() RunKind {
	if kinds := r.Kinds(); len(kinds) == 1 {
		return kinds[0]
	}
	return ""
}

func presentKinds(audit, build, rebase, review, triage bool) []RunKind {
	var kinds []RunKind
	for i, present := range []bool{audit, build, rebase, review, triage} {
		if present {
			kinds = append(kinds, ValidRunKinds[i])
		}
	}
	return kinds
}

// Run is one execution of agent work inside a mission.
type Run struct {
	// ID is the provider's bare id of the run.
	ID string `json:"id"`
	// Mission is the mission the run belongs to.
	Mission MissionName `json:"mission"`
	// Kind is the kind of work; see the RunKind* constants.
	Kind RunKind `json:"kind"`
	// State is where the run stands; see the RunState* constants.
	State RunState `json:"state"`
	// Input is what the run was given, keyed by Kind.
	Input RunInput `json:"input"`
	// Result is what a succeeded run produced, keyed by Kind. Only a
	// succeeded run carries it, and it always does.
	Result *RunResult `json:"result,omitempty"`
	// Failure states why a failed run stopped, on one line. Only a failed run
	// carries it, and it always does.
	Failure string `json:"failure,omitempty"`
	// Session is the agent session that ran it, when the caller named one.
	Session *SessionRef `json:"session,omitempty"`
	// StartedAt is when the provider started the run, in UTC.
	StartedAt string `json:"startedAt"`
	// EndedAt is when the provider ended the run, in UTC. Every state but
	// running carries it.
	EndedAt string `json:"endedAt,omitempty"`
}

// MissionsStartInput is the missions.start request. Its content is the
// mission's repository and key, the kind, the input and the session; the
// mission's title is not part of it.
type MissionsStartInput struct {
	// Mission is the mission the run belongs to.
	Mission MissionName `json:"mission"`
	// Kind is the kind of work the run executes.
	Kind RunKind `json:"kind"`
	// Input is the run's input, keyed by Kind.
	Input RunInput `json:"input"`
	// Session is the agent session that runs it, when there is one.
	Session *SessionRef `json:"session,omitempty"`
	// IdempotencyKey makes a retry return the run already started.
	IdempotencyKey string `json:"idempotencyKey"`
}

// MissionsReportInput is the missions.report request. Its natural key is the
// run: a run takes one report.
type MissionsReportInput struct {
	// Run is the id of the run that ends.
	Run string `json:"run"`
	// State is the final state: succeeded, failed or canceled.
	State RunState `json:"state"`
	// Result is a succeeded run's result, keyed by the run's kind. Only a
	// succeeded report carries it, and it always does.
	Result *RunResult `json:"result,omitempty"`
	// Failure states why a failed run stopped. Only a failed report carries
	// it, and it always does.
	Failure string `json:"failure,omitempty"`
}

// MissionsRunResult is the result of missions.start and missions.report.
type MissionsRunResult struct {
	// Run is the run as the provider holds it after the call.
	Run Run `json:"run"`
	// Replayed is true when the call repeated one already applied, so
	// nothing changed.
	Replayed bool `json:"replayed"`
}

// MissionsRunsInput is the missions.runs request.
type MissionsRunsInput struct {
	// Mission is the mission whose runs to read.
	Mission MissionName `json:"mission"`
	// Kind keeps only the runs of one kind, when set.
	Kind RunKind `json:"kind,omitempty"`
	// Page asks for one bounded page.
	Page *PageRequest `json:"page,omitempty"`
}

// MissionsRunsResult is the missions.runs result.
type MissionsRunsResult struct {
	// Items are a page of the mission's runs, the latest start first, then
	// by id, each once.
	Items []Run `json:"items"`
	// Page carries the cursor of the next page.
	Page Page `json:"page"`
}

func (c *checker) runKind(field string, kind RunKind) {
	if !slices.Contains(ValidRunKinds, kind) {
		c.errorf(ErrorCodeInvalidValue, field, "unknown run kind %q", kind)
	}
}

func (c *checker) runState(field string, state RunState) {
	if !slices.Contains(ValidRunStates, state) {
		c.errorf(ErrorCodeInvalidState, field, "unknown run state %q", state)
	}
}

// oneKind checks that a keyed document carries exactly one member, the one
// want names; want "" accepts any one member.
func (c *checker) oneKind(field string, kinds []RunKind, want RunKind) {
	switch {
	case len(kinds) == 0:
		c.errorf(ErrorCodeRequired, field, "%s carries one member, named by the run's kind", field)
	case len(kinds) > 1:
		c.errorf(ErrorCodeInvalidValue, field, "%s carries one member, not %d", field, len(kinds))
	case want != "" && slices.Contains(ValidRunKinds, want) && kinds[0] != want:
		c.errorf(ErrorCodeInvalidValue, field, "%s carries %s; the run's kind is %s", field, kinds[0], want)
	}
}

func (c *checker) runInput(field string, kind RunKind, input RunInput) {
	c.oneKind(field, input.Kinds(), kind)
	if in := input.Audit; in != nil {
		c.bounded(field+".audit.scope", in.Scope, MaxAuditScopeLength, true)
		c.commit(field+".audit.revision", in.Revision)
	}
	if in := input.Build; in != nil {
		if in.Task == nil && in.Intention == "" {
			c.errorf(ErrorCodeRequired, field+".build", "a build names its task or its intention")
		}
		if in.Task != nil {
			c.ref(field+".build.task", *in.Task)
		}
		c.text(field+".build.intention", in.Intention, MaxRunTextLength, false)
		if in.Change != nil {
			c.change(field+".build.change", *in.Change, false)
		}
	}
	if in := input.Rebase; in != nil {
		c.change(field+".rebase.change", in.Change, false)
		if in.Commit != "" {
			c.commit(field+".rebase.commit", in.Commit)
		}
	}
	if in := input.Review; in != nil {
		c.change(field+".review.change", in.Change, false)
		c.commit(field+".review.commit", in.Commit)
		if in.Proposal != nil {
			c.ref(field+".review.proposal", *in.Proposal)
		}
	}
	if in := input.Triage; in != nil {
		c.ref(field+".triage.task", in.Task)
	}
}

func (c *checker) runResult(field string, kind RunKind, result RunResult) {
	c.oneKind(field, result.Kinds(), kind)
	if r := result.Audit; r != nil {
		c.bounded(field+".audit.scope", r.Scope, MaxAuditScopeLength, true)
		c.commit(field+".audit.revision", r.Revision)
		if !slices.Contains(ValidAuditVerdicts, r.Verdict) {
			c.errorf(ErrorCodeInvalidValue, field+".audit.verdict", "unknown audit verdict %q", r.Verdict)
		}
		c.findings(field+".audit.findings", r.Findings)
	}
	if r := result.Build; r != nil {
		c.loopStage(field+".build.stage", r.Stage)
		if r.Stage == "" {
			c.errorf(ErrorCodeRequired, field+".build.stage", "%s.build.stage is required", field)
		}
		if r.Proposal != nil {
			c.ref(field+".build.proposal", *r.Proposal)
		}
		if r.Commit != "" {
			c.commit(field+".build.commit", r.Commit)
		}
	}
	if r := result.Rebase; r != nil {
		c.commit(field+".rebase.commit", r.Commit)
		c.commit(field+".rebase.onto", r.Onto)
	}
	if r := result.Review; r != nil {
		c.commit(field+".review.commit", r.Commit)
		c.reviewVerdict(field+".review.verdict", r.Verdict)
		c.findings(field+".review.findings", r.Findings)
	}
	if r := result.Triage; r != nil {
		c.text(field+".triage.summary", r.Summary, MaxRunTextLength, true)
		if r.State != "" {
			c.taskState(field+".triage.state", r.State)
		}
		c.labels(field+".triage.labels", r.Labels)
		if r.DuplicateOf != nil {
			c.ref(field+".triage.duplicateOf", *r.DuplicateOf)
		}
	}
}

func (c *checker) findings(field string, findings []Finding) {
	if findings == nil {
		c.errorf(ErrorCodeRequired, field, "%s is required; none is []", field)
		return
	}
	if len(findings) > MaxRunFindings {
		c.errorf(ErrorCodeTooLarge, field, "at most %d findings", MaxRunFindings)
	}
	total := 0
	for i, finding := range findings {
		entry := fmt.Sprintf("%s[%d]", field, i)
		if !slices.Contains(ValidFindingSeverities, finding.Severity) {
			c.errorf(ErrorCodeInvalidValue, entry+".severity", "unknown severity %q", finding.Severity)
		}
		c.title(entry+".title", finding.Title)
		if finding.File != "" {
			c.filePath(entry+".file", finding.File)
		}
		switch {
		case finding.Line != 0 && finding.File == "":
			c.errorf(ErrorCodeRequired, entry+".file", "a finding that names a line names its file")
		case finding.Line < 0 || finding.Line > MaxFindingLine:
			c.errorf(ErrorCodeInvalidValue, entry+".line", "line is between 1 and %d", MaxFindingLine)
		}
		c.text(entry+".body", finding.Body, MaxFindingBodyLength, false)
		total += len(finding.Title) + len(finding.Body)
	}
	if total > MaxFindingsBytes {
		c.errorf(ErrorCodeTooLarge, field, "the titles and bodies of %s total %d bytes, above %d", field, total, MaxFindingsBytes)
	}
}

// runOutcome checks the members a state decides: a result exactly when the
// run succeeded, a failure exactly when it failed.
func (c *checker) runOutcome(prefix string, kind RunKind, state RunState, result *RunResult, failure string) {
	join := func(member string) string {
		if prefix == "" {
			return member
		}
		return prefix + "." + member
	}
	switch {
	case state == RunStateSucceeded && result == nil:
		c.errorf(ErrorCodeRequired, join("result"), "a succeeded run carries its result")
	case state != RunStateSucceeded && result != nil:
		c.errorf(ErrorCodeInvalidState, join("result"), "only a succeeded run carries a result, not a %s one", state)
	case result != nil:
		c.runResult(join("result"), kind, *result)
	}
	switch {
	case state == RunStateFailed:
		c.bounded(join("failure"), failure, MaxErrorLength, true)
	case failure != "":
		c.errorf(ErrorCodeInvalidState, join("failure"), "only a failed run carries a failure, not a %s one", state)
	}
}

func (c *checker) run(field string, run Run) {
	c.missionID(field+".id", run.ID)
	c.missionName(field+".mission", run.Mission)
	c.runKind(field+".kind", run.Kind)
	c.runState(field+".state", run.State)
	c.runInput(field+".input", run.Kind, run.Input)
	c.runOutcome(field, run.Kind, run.State, run.Result, run.Failure)
	if run.Session != nil {
		c.session(field+".session", *run.Session)
	}
	c.utcTimestamp(field+".startedAt", run.StartedAt)
	switch {
	case run.State == RunStateRunning && run.EndedAt != "":
		c.errorf(ErrorCodeInvalidState, field+".endedAt", "a running run has not ended")
	case run.State != RunStateRunning:
		c.utcTimestamp(field+".endedAt", run.EndedAt)
		started, startErr := time.Parse(time.RFC3339Nano, run.StartedAt)
		ended, endErr := time.Parse(time.RFC3339Nano, run.EndedAt)
		if startErr == nil && endErr == nil && ended.Before(started) {
			c.errorf(ErrorCodeInvalidValue, field+".endedAt", "a run ends no earlier than it started")
		}
	}
}

func (c *checker) startInput(in *MissionsStartInput) {
	c.missionName("mission", in.Mission)
	c.runKind("kind", in.Kind)
	c.runInput("input", in.Kind, in.Input)
	if in.Session != nil {
		c.session("session", *in.Session)
	}
	c.idempotencyKey(in.IdempotencyKey)
}

func (c *checker) reportInput(in *MissionsReportInput) {
	c.missionID("run", in.Run)
	c.runState("state", in.State)
	if in.State == RunStateRunning {
		c.errorf(ErrorCodeInvalidState, "state", "a report ends the run: succeeded, failed or canceled")
	}
	c.runOutcome("", "", in.State, in.Result, in.Failure)
}

func (c *checker) runsInput(in *MissionsRunsInput) {
	c.missionName("mission", in.Mission)
	if in.Kind != "" {
		c.runKind("kind", in.Kind)
	}
	c.page(in.Page)
}

func (c *checker) runsResult(r *MissionsRunsResult) {
	c.itemsPresent(r.Items == nil)
	c.listBound("items", len(r.Items))
	seen := map[string]bool{}
	for i, run := range r.Items {
		field := fmt.Sprintf("items[%d]", i)
		c.run(field, run)
		if seen[run.ID] {
			c.errorf(ErrorCodeInvalidValue, field+".id", "run %s is repeated", run.ID)
		}
		seen[run.ID] = true
	}
	c.nextCursor(r.Page.Next)
}
