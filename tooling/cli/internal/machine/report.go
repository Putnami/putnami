package machine

import (
	"math"
	"sort"
	"time"
	"unicode/utf8"

	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

// The report projection — surface #5 of the ONE canonical reduction
// (protocols/cli/doc/03-report.md).
//
// The four surfaces named in machine.go render a run WHILE it settles; this one
// synthesizes it AFTER it has. Everything it states is a fold over the same
// Tasks list the reduction was folded from, so no number here is recovered by
// re-walking events.jsonl and coercing its untyped event.Data["duration"].
//
// The file is deliberately SECTIONED — the run block, the per-command block,
// the per-job block, the measurements, then the typed cache and scheduler
// subsets — because the contract's optional members landed separately: one
// change edited the per-job diagnostics and another the measurements, neither
// touching the other's region.
//
// Two rules from the contract govern what is NOT here. Absent is not zero: a
// member the run did not measure is omitted rather than reported as a measured
// zero, so an uninstrumented command states no coverage instead of 0%, and this
// document never forces instrumentation to fill a member. And the report is a
// projection, never an input: nothing below can fail a run, which is why the
// entry point returns nil rather than an inconsistent document.

// ReportMeta is what the report's producer knows and the run does not: which
// session this synthesis belongs to, the settled interval it covers, which
// surface drove it, whether the enforcing cadence was in force, the --fix value
// it was given, and the commit a longitudinal consumer files it under.
//
// The interval is required by the contract because a report is written from a
// SETTLED run: the producer passes the session's own start and end, so the wall
// the report states is the wall the recorded session states.
type ReportMeta struct {
	// SessionID is the session this report synthesizes.
	SessionID string
	// StartTime and EndTime bound the settled run.
	StartTime time.Time
	EndTime   time.Time
	// Origin is protocolcli.ReportOriginCLI or ReportOriginMCP. It is the origin
	// field recorded session metadata has never had, which is why an adapter run
	// is reported rather than withheld: readers filter, they do not guess.
	Origin string
	// EnforceCoverage records whether the run ran the enforcing cadence, so a
	// longitudinal consumer compares like with like.
	EnforceCoverage bool
	// Fix is the run's explicit --fix value, nil when the run was not given one.
	Fix *bool
	// Git is the repository state the run was produced against, absent outside a
	// worktree.
	Git *protocolcli.ReportGit
}

// Report projects this run onto the reportFile document.
//
// It returns nil — never a partial or an invented document — when the run's own
// reduction and its task list describe different task sets. The contract ties
// len(jobs)+elidedJobs and the per-command totals to run.counts.total, and a
// document that fails either is one a consumer must reject; rule 2 of
// doc/03-report.md makes the alternative a non-event: "a report that failed to
// be written is a missing file, never a failed run".
func (r Run) Report(
	meta ReportMeta,
	scheduler *jobs.TuningReport,
	cache *jobs.CacheStatsSnapshot,
) *protocolcli.ReportFile {
	// The wall is the session's own — end minus start — for the same reason
	// Session.FinalizeV2 re-derives it: the run's CPU allocation is stated
	// against the duration this document reports, and the two have to divide.
	summary := r.Summary(nonNegative64(meta.EndTime.Sub(meta.StartTime).Milliseconds()))
	counted := r.countedTasks()
	if len(counted) != summary.Counts.Total {
		return nil
	}
	selected, elided := reportJobList(counted, meta.EnforceCoverage)
	return &protocolcli.ReportFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion,
		SessionID:       orUnknown(meta.SessionID),
		StartTime:       stamp(meta.StartTime),
		EndTime:         stamp(meta.EndTime),
		Origin:          reportOrigin(meta.Origin),
		EnforceCoverage: meta.EnforceCoverage,
		Fix:             meta.Fix,
		Git:             meta.Git,
		Run:             reportRun(summary),
		Commands:        reportCommands(counted, meta.EnforceCoverage),
		Jobs:            selected,
		ElidedJobs:      elided,
		Cache:           reportCache(cache),
		Scheduler:       reportScheduler(scheduler),
	}
}

// countedTasks is the task set the run's own reduction COUNTED, in document
// order.
//
// Both filters mirror jobs.SessionReducer.observeTask exactly: a `runOn:
// finally` finalizer is listed, rendered and recorded but never counted (see
// Task.silent), and a status outside the closed vocabulary reaches no bucket.
// Restating either rule loosely would make the report's partition describe a
// different task set than its own verdict does — the one thing the contract's
// accounting clauses exist to catch.
func (r Run) countedTasks() []Task {
	counted := make([]Task, 0, len(r.Tasks))
	for i := range r.Tasks {
		if r.Tasks[i].silent || !countedStatus(r.Tasks[i].Result.Status) {
			continue
		}
		counted = append(counted, r.Tasks[i])
	}
	return counted
}

// countedStatus reports whether a verdict reaches one of the four buckets the
// reduction histograms. jobs.TaskCounts.add drops anything else, and so does
// this document.
func countedStatus(status jobs.TaskStatus) bool {
	switch status {
	case jobs.TaskStatusSuccess, jobs.TaskStatusFailed, jobs.TaskStatusCanceled, jobs.TaskStatusSkipped:
		return true
	default:
		return false
	}
}

// --- the run block ---------------------------------------------------------

// reportRun narrows the canonical run summary to the report's verdict.
//
// reportRun is deliberately not runSummary: that shape carries failures[],
// whose length equals counts.failed and whose members embed a full identity and
// every diagnostic — an UNBOUNDED member, against a document whose premise is a
// stated worst case. The failed tasks are not lost; they sort first into the
// job list. abortedBy is dropped for the same reason in the other direction: an
// abort SOURCE is a run-ledger fact, and the report keeps the verdict.
func reportRun(summary protocolcli.RunSummary) protocolcli.ReportRun {
	return protocolcli.ReportRun{
		Outcome:    summary.Outcome,
		ExitCode:   summary.ExitCode,
		Counts:     summary.Counts,
		Reuse:      summary.Reuse,
		DurationMs: summary.DurationMs,
		CPU:        summary.CPU,
	}
}

// reportOrigin closes the origin vocabulary. A producer that named no surface
// is the command line: "cli" is the weaker claim, and readers filter adapter
// runs OUT by default, so guessing "mcp" would hide a real run.
func reportOrigin(origin string) string {
	if origin == protocolcli.ReportOriginMCP {
		return protocolcli.ReportOriginMCP
	}
	return protocolcli.ReportOriginCLI
}

// --- the per-command block -------------------------------------------------

// reportCommands folds the counted tasks into the per-command synthesis.
//
// The vocabulary is COMMANDS: a task names the root command it belongs to, so
// this is a histogram over TaskRef.Command and every counted task lands in
// exactly one row — which is what makes the per-command totals sum to the run's
// own total. The order is the command name, so two runs of the same plan
// produce byte-identical documents.
//
// freshWallMs and cpuMs sum only tasks that EXECUTED: a cache hit spends no
// wall, and counting the duration it replayed would make a fully cached command
// look as expensive as the run that populated the cache.
func reportCommands(tasks []Task, enforced bool) []protocolcli.ReportCommand {
	rows := make(map[string]*commandAgg, 4)
	for i := range tasks {
		task := &tasks[i]
		name := orUnknown(task.Identity.Task.Command)
		agg := rows[name]
		if agg == nil {
			agg = &commandAgg{row: protocolcli.ReportCommand{Command: name}}
			rows[name] = agg
		}
		row := &agg.row
		addCounts(&row.Counts, task.Result.Status)
		if task.Result.Reuse.Reused() {
			addReuse(&row.Reuse, task.Result.Reuse)
		} else {
			row.FreshWallMs += millis(task.Result.Timing.Duration)
			// A fresh task's CPUTime is already its SHARE of its execution's
			// measured total (jobs.executionShare), and a shared node's adopting
			// follower carries none, so each physical execution is counted once
			// and the sum stays at or below the run's actual CPU. Truncating per
			// task can only lower it further, never raise it.
			row.CPUMs += millis(task.Result.Timing.CPUTime)
		}
		countDiagnostics(row, task.Result.Diagnostics)
		agg.addTests(task.Result.Tests)
		agg.addCoverage(task.Result.Coverage)
	}
	commands := make([]protocolcli.ReportCommand, 0, len(rows))
	for _, agg := range rows {
		agg.row.Counts.Total = agg.row.Counts.Sum()
		// Producer omissions are real error details the bounded diagnostic
		// projection could not carry. The synthetic accounting marker is excluded
		// by countDiagnostics, so adding the omitted count preserves the number of
		// causal errors without counting that marker as another failure.
		agg.row.Errors += agg.failureDetailsTruncated
		agg.row.Tests = agg.tests()
		agg.row.Coverage = agg.coverage(enforced)
		commands = append(commands, agg.row)
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Command < commands[j].Command })
	return commands
}

// commandAgg accumulates one command's row while it is being folded. The row
// itself holds everything the contract states directly; the measurements need
// state the emitted shape has no place for — which granularities the command's
// tasks measured at, and whether anything measured at all.
type commandAgg struct {
	row protocolcli.ReportCommand
	// passed/failed/skipped are the test counters summed over the command's
	// tasks; a command none of whose tasks measured stays at three zeros and
	// states no block at all.
	passed, failed, skipped int
	// failureDetailsTruncated is summed from the canonical runtime TestSummary.
	// It is both emitted on ReportTests and folded into row.Errors.
	failureDetailsTruncated int
	// covered maps a granularity onto its accumulator, and granularities keeps
	// the first-seen order so the pick below is deterministic for a given plan.
	covered       map[string]*coverageAgg
	granularities []string
}

// --- the coverage and test measurements ------------------------------------

// addTests folds one task's test summary into the command's four counters.
//
// The contract DEFINES total as passed + failed + skipped and its validator
// enforces the arithmetic, so the total is derived from the three classified
// counters rather than summed as an independent fourth: a producer whose own
// total disagreed with its classes would otherwise emit a document a consumer
// must reject.
func (a *commandAgg) addTests(measured *runtimeproto.TestSummary) {
	if measured == nil {
		return
	}
	a.passed += nonNegative(measured.Passed)
	a.failed += nonNegative(measured.Failed)
	a.skipped += nonNegative(measured.Skipped)
	a.failureDetailsTruncated += nonNegative(measured.FailureDetailsTruncated)
}

// tests states the command's test outcome, or nothing when no task measured
// one. A zero-test measurement stays absent unless it carries omission
// accounting: in that case the report must expose the otherwise-lost count.
func (a *commandAgg) tests() *protocolcli.ReportTests {
	total := a.passed + a.failed + a.skipped
	if total == 0 && a.failureDetailsTruncated == 0 {
		return nil
	}
	return &protocolcli.ReportTests{
		Total:                   total,
		Passed:                  a.passed,
		Failed:                  a.failed,
		Skipped:                 a.skipped,
		FailureDetailsTruncated: a.failureDetailsTruncated,
	}
}

// addCoverage folds one task's coverage measurement into the accumulator for
// its OWN granularity.
//
// Grouping is not optional: the emitted block states one granularity, and Go
// counts statements where TypeScript counts lines, so a single command over a
// mixed workspace mixes units. Averaging across them is what the contract calls
// the reader's decision to make knowingly — this projection therefore never
// makes it silently.
func (a *commandAgg) addCoverage(measured *runtimeproto.CoverageSummary) {
	if !statable(measured) {
		return
	}
	if a.covered == nil {
		a.covered = make(map[string]*coverageAgg, 2)
	}
	group := a.covered[measured.Granularity]
	if group == nil {
		group = &coverageAgg{granularity: measured.Granularity}
		a.covered[measured.Granularity] = group
		a.granularities = append(a.granularities, measured.Granularity)
	}
	group.add(measured)
}

// coverage states the command's coverage at the granularity most of its tasks
// measured at, ties broken by first-seen order so the same plan always states
// the same one. A command whose tasks measured nothing states nothing.
func (a *commandAgg) coverage(enforced bool) *protocolcli.ReportCoverage {
	var pick *coverageAgg
	for _, granularity := range a.granularities {
		if group := a.covered[granularity]; pick == nil || group.measurements > pick.measurements {
			pick = group
		}
	}
	if pick == nil {
		return nil
	}
	return pick.summary(enforced)
}

// coverageAgg accumulates the coverage measurements taken at ONE granularity.
type coverageAgg struct {
	granularity  string
	measurements int
	covered      int
	total        int
	percentages  []float64
}

// add folds one measurement in, preferring its counts. This is the live
// renderer's taskAgg.coverage rule (internal/output/live_summary.go), kept the
// same here on purpose: a run must not report one coverage figure while it
// settles and a different one after.
func (a *coverageAgg) add(measured *runtimeproto.CoverageSummary) {
	a.measurements++
	if measured.Total > 0 {
		a.total += measured.Total
		a.covered += min(nonNegative(measured.Covered), measured.Total)
		return
	}
	a.percentages = append(a.percentages, measured.Percentage)
}

// summary folds the accumulator into one measurement: unit-weighted when the
// producers reported counts — a 5-statement package must not weigh as much as a
// 5000-statement one — and otherwise the plain mean of the reported
// percentages, which is all an aggregate can say when nothing stated a
// denominator. The counts are then omitted rather than invented.
func (a *coverageAgg) summary(enforced bool) *protocolcli.ReportCoverage {
	switch {
	case a.total > 0:
		return &protocolcli.ReportCoverage{
			Percentage:  boundedPercentage(100 * float64(a.covered) / float64(a.total)),
			Granularity: a.granularity,
			Covered:     a.covered,
			Total:       a.total,
			Enforced:    enforced,
		}
	case len(a.percentages) > 0:
		var sum float64
		for _, percentage := range a.percentages {
			sum += percentage
		}
		return &protocolcli.ReportCoverage{
			Percentage:  boundedPercentage(sum / float64(len(a.percentages))),
			Granularity: a.granularity,
			Enforced:    enforced,
		}
	default:
		return nil
	}
}

// reportCoverage projects ONE task's recorded measurement onto the contract's
// shape, with covered clamped to total: the two counts state the same
// measurement twice, and a pair that cannot both be true is a document a
// consumer must reject.
//
// A measurement whose granularity is outside the closed vocabulary is dropped
// whole. The unit travels WITH the number by contract, so a percentage whose
// unit cannot be named is not a measurement this document can state.
func reportCoverage(measured *runtimeproto.CoverageSummary, enforced bool) *protocolcli.ReportCoverage {
	if !statable(measured) {
		return nil
	}
	total := nonNegative(measured.Total)
	return &protocolcli.ReportCoverage{
		Percentage:  boundedPercentage(measured.Percentage),
		Granularity: measured.Granularity,
		Covered:     min(nonNegative(measured.Covered), total),
		Total:       total,
		Enforced:    enforced,
	}
}

// statable reports whether a recorded measurement is one this document can
// state. It is the single predicate: the selection, the per-command fold and
// the per-job member all ask it, so a task can never be SELECTED as
// coverage-bearing and then emit no coverage.
func statable(measured *runtimeproto.CoverageSummary) bool {
	return measured != nil && knownGranularity(measured.Granularity)
}

// knownGranularity closes the coverage unit vocabulary to the four the contract
// enumerates — the same four protocol/runtime's payload declares.
func knownGranularity(granularity string) bool {
	switch granularity {
	case protocolcli.CoverageStatements, protocolcli.CoverageLines,
		protocolcli.CoverageFunctions, protocolcli.CoverageBranches:
		return true
	default:
		return false
	}
}

// boundedPercentage keeps a covered share inside [0,100]. It is the only
// non-integer member the contract carries, and a producer that computed one out
// of range would otherwise cost the whole document.
func boundedPercentage(percentage float64) float64 {
	switch {
	case percentage < 0 || math.IsNaN(percentage):
		return 0
	case percentage > 100:
		return 100
	default:
		return percentage
	}
}

// addCounts folds one verdict into a command's histogram, on the same buckets
// the reduction uses.
func addCounts(counts *protocolcli.RunCounts, status jobs.TaskStatus) {
	switch status {
	case jobs.TaskStatusSuccess:
		counts.Succeeded++
	case jobs.TaskStatusFailed:
		counts.Failed++
	case jobs.TaskStatusCanceled:
		counts.Canceled++
	case jobs.TaskStatusSkipped:
		counts.Skipped++
	}
}

// addReuse folds one reused task's provenance in.
func addReuse(reuse *protocolcli.RunReuse, kind jobs.ReuseKind) {
	switch kind {
	case jobs.ReuseLocalCache:
		reuse.LocalCache++
	case jobs.ReuseRemoteCache:
		reuse.RemoteCache++
	case jobs.ReuseCoalesced:
		reuse.Coalesced++
	}
}

// countDiagnostics counts every real error/warning the command's tasks
// reported, including records the bounded job list will drop. The producer's
// synthetic omission marker is accounting rather than another error; its
// omitted details are added separately from the typed test summary.
func countDiagnostics(row *protocolcli.ReportCommand, reported []jobs.TaskDiagnostic) {
	for i := range reported {
		if reported[i].Code == protocolcli.TestFailureDetailsTruncatedCode {
			continue
		}
		switch diagnosticSeverity(reported[i].Severity) {
		case "error":
			row.Errors++
		case "warning":
			row.Warnings++
		}
	}
}

// --- the per-job block -----------------------------------------------------

// The selection priority the contract states: failed, then diagnostic-bearing,
// then coverage-bearing, then longest wall. The third class has members now
// that a task carries the measurement its result payload reported — a passing,
// diagnostic-free test task is exactly the row a coverage reader came for, and
// without this class the bound would elide it behind any longer build.
const (
	rankFailed = iota
	rankDiagnostic
	rankCoverage
	rankOrdinary
)

// reportJobList selects at most protocolcli.ReportMaxJobs of the counted tasks
// and counts what the bound left out.
//
// A report therefore elides successes before failures, which is why the run
// block carries no failures[] at all. The order is total: the class first, then
// the longer wall, then document order — a stable sort over a list the
// reduction already ordered — so the same run always selects the same jobs and
// writes them in the same sequence.
func reportJobList(tasks []Task, enforced bool) ([]protocolcli.ReportJob, int) {
	order := make([]int, len(tasks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		left, right := &tasks[order[a]], &tasks[order[b]]
		leftRank, rightRank := noteworthiness(left), noteworthiness(right)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		return left.Result.Timing.Duration > right.Result.Timing.Duration
	})

	kept := len(order)
	if kept > protocolcli.ReportMaxJobs {
		kept = protocolcli.ReportMaxJobs
	}
	selected := make([]protocolcli.ReportJob, 0, kept)
	for _, index := range order[:kept] {
		selected = append(selected, reportJob(tasks[index], enforced))
	}
	return selected, len(order) - kept
}

// noteworthiness classifies one task for the bounded selection: what the bound
// was sized to keep is the informative task, and a measurement is information
// no other member of the document can recover once the task is elided — the
// per-command row states the command's aggregate, never this project's share.
func noteworthiness(task *Task) int {
	switch {
	case task.Result.Status == jobs.TaskStatusFailed:
		return rankFailed
	case len(task.Result.Diagnostics) > 0:
		return rankDiagnostic
	case statable(task.Result.Coverage):
		return rankCoverage
	default:
		return rankOrdinary
	}
}

// reportJob flattens one task's identity to the three strings a consumer joins
// on, plus the command that ties it to its per-command row. The full
// TaskIdentity stays in the session: repeating it 64 times would spend the
// report's whole budget on identity a reader can look up.
//
// The key is DERIVED here rather than carried, so the report cannot spell an
// identity two ways even if a producer upstream did.
func reportJob(task Task, enforced bool) protocolcli.ReportJob {
	project := orUnknown(task.Identity.Project.ID)
	name := orUnknown(task.Identity.Task.Name)
	job := protocolcli.ReportJob{
		Key:     project + ":" + name,
		Project: project,
		Task:    name,
		Command: orUnknown(task.Identity.Task.Command),
		Outcome: taskStatus(task.Result.Status),
		Reuse:   taskReuse(task.Result.Reuse),
		// The reused result's recorded duration, carried through unchanged.
		DurationMs: millis(task.Result.Timing.Duration),
		// Absent for a task that spawned nothing and where the platform measured
		// nothing — a zero would read as "ran and cost nothing".
		CPUMs: millis(task.Result.Timing.CPUTime),
	}
	// The task's OWN measurement, never the command's: a per-job row states what
	// this project measured, which is the number a reader drills into a job for.
	job.Coverage = reportCoverage(task.Result.Coverage, enforced)
	var reportDropped int
	job.Diagnostics, reportDropped = reportJobDiagnostics(task.Result.Diagnostics)
	producerOmitted := 0
	if task.Result.Tests != nil {
		producerOmitted = nonNegative(task.Result.Tests.FailureDetailsTruncated)
	}
	job.FailureDetailsTruncated = producerOmitted
	job.TruncatedCount = reportDropped + producerOmitted
	return job
}

// --- the per-job diagnostics -----------------------------------------------

// reportJobDiagnostics bounds one task's diagnostics to the contract caps.
//
// The truncation is deterministic and drops the least informative first:
// accounting markers sort first, then errors, warnings and informational
// records; ties retain document order. The bound keeps that head, so an omission
// marker cannot itself disappear. Report-side omissions only occur when the list
// is full. reportJob separately adds producer-side omissions, which may
// legitimately accompany a short list.
//
// The totals the bound drops are not lost: the per-command errors/warnings
// counters fold every real reported diagnostic, elided or not, and
// reportCommands separately adds producer-side omitted failure details. The
// synthetic accounting marker is informational rather than another error.
func reportJobDiagnostics(reported []jobs.TaskDiagnostic) ([]protocolcli.Diagnostic, int) {
	if len(reported) == 0 {
		return nil, 0
	}
	order := make([]int, len(reported))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		aMarker := reported[order[a]].Code == protocolcli.TestFailureDetailsTruncatedCode
		bMarker := reported[order[b]].Code == protocolcli.TestFailureDetailsTruncatedCode
		if aMarker != bMarker {
			return aMarker
		}
		return severityRank(reported[order[a]].Severity) < severityRank(reported[order[b]].Severity)
	})

	kept := min(len(order), protocolcli.ReportMaxJobDiagnostics)
	out := make([]protocolcli.Diagnostic, 0, kept)
	for _, index := range order[:kept] {
		d := &reported[index]
		out = append(out, protocolcli.Diagnostic{
			Severity: diagnosticSeverity(d.Severity),
			Message:  truncateMessage(orUnknown(d.Message)),
			Code:     d.Code,
			File:     d.File,
			Line:     nonNegative(d.Line),
			Column:   nonNegative(d.Column),
		})
	}
	truncated := len(order) - kept
	return out, truncated
}

// severityRank orders non-accounting diagnostics for the bounded selection:
// errors first, then warnings, then everything informational.
func severityRank(severity string) int {
	switch diagnosticSeverity(severity) {
	case "error":
		return 0
	case "warning":
		return 1
	default:
		return 2
	}
}

// truncateMessage bounds one message to the contract's byte cap, keeping the
// END and marking the dropped head with an ellipsis. A message this long is
// captured command output, and its verdict lives in the tail — a Go test
// failure prints setup logs first and `--- FAIL` (or the panic, or the teardown
// error) last, so a head-biased cut kept the noise and dropped the evidence.
// The cut lands on a rune boundary so the message stays valid UTF-8, and the
// untruncated text remains in the session's own task record.
func truncateMessage(message string) string {
	if len(message) <= protocolcli.ReportMaxMessageBytes {
		return message
	}
	const marker = "… "
	start := len(message) - (protocolcli.ReportMaxMessageBytes - len(marker))
	for start < len(message) && !utf8.RuneStart(message[start]) {
		start++
	}
	return marker + message[start:]
}

// --- the typed cache and scheduler subsets ---------------------------------

// reportCache states the cache economics as a TYPED subset of the scheduler's
// own snapshot. The session file's free-shape `cache: any` is exactly what a
// contracted document must not repeat: a consumer binding to it binds to
// whatever the scheduler happened to serialize that release.
//
// The block is absent when no remote cache participated — an all-zero block
// would claim the cache was asked and answered nothing, which is a different
// fact. Per-command byte traffic is deliberately not offered anywhere: any
// split of a deduplicated transfer is a fiction, so bytes are stated once.
func reportCache(cache *jobs.CacheStatsSnapshot) *protocolcli.ReportCache {
	if cache == nil || (cache.KeysRequested == 0 && cache.Hits == 0 && cache.Misses == 0 &&
		cache.Restored == 0 && cache.Uploads == 0 && cache.BytesFetched == 0 && cache.BytesUploaded == 0) {
		return nil
	}
	hits := nonNegative64(cache.Hits)
	// A restore promotes its key to a hit in the same reducer
	// (jobs.CacheStats.recordProviderHit), so restored is a subset of hits by
	// construction. The floor is the same guard nonNegative64 is: it keeps a
	// counter that drifted from producing a document a consumer must reject.
	restored := nonNegative64(cache.Restored)
	if restored > hits {
		restored = hits
	}
	return &protocolcli.ReportCache{
		Hits:          hits,
		Misses:        nonNegative64(cache.Misses),
		Restored:      restored,
		Uploads:       nonNegative64(cache.Uploads),
		TimeSavedMs:   nonNegative64(cache.TimeSavedMs),
		BytesFetched:  nonNegative64(cache.BytesFetched),
		BytesUploaded: nonNegative64(cache.BytesUploaded),
	}
}

// reportScheduler states the two scheduler facts a budget consumer reads out of
// the free-form scheduler report. It is absent when the run recorded no scheduler report; a plan
// with no positive-duration chain really did measure a zero critical path, so
// that zero is kept.
func reportScheduler(tuning *jobs.TuningReport) *protocolcli.ReportScheduler {
	if tuning == nil || tuning.Parallel.Workers < 1 {
		return nil
	}
	scheduler := &protocolcli.ReportScheduler{Parallelism: tuning.Parallel.Workers}
	if tuning.CriticalPath != nil {
		scheduler.CriticalPathMs = nonNegative64(tuning.CriticalPath.DurationMs)
	}
	return scheduler
}
