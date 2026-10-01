package output

import (
	"fmt"
	"time"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// JobStart is called when a job begins execution. It marks the owning command
// (and project) as running. Cache hits and skips also pass through here but
// settle almost immediately in JobComplete, so they never linger as "running".
func (r *LiveRenderer) JobStart(job *jobs.ScheduledJob) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	r.progressJobStarted(job.Key(), now)

	row := r.rowByID[job.Project.ID]
	if row == nil {
		return
	}
	cmd := row.commands[commandName(job)]
	if cmd == nil {
		return
	}

	if cmd.status == "" {
		cmd.status = statusRunning
	}
	row.doneVisibleUntil = time.Time{}
	if cmd.startTime.IsZero() {
		cmd.startTime = now
	}
	r.startProjectWork(row, now)
	if sn := stepName(job.JobDef.Name); sn != "" {
		cmd.phase = sn
	}
}

// JobEvent routes a job's JSONL events to its command for live display.
func (r *LiveRenderer) JobEvent(job *jobs.ScheduledJob, event jobs.RawJobEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()

	row := r.rowByID[job.Project.ID]
	if row == nil {
		return
	}
	cmd := row.commands[commandName(job)]
	if cmd == nil {
		return
	}

	switch event.Type {
	case jobs.EventTypeProgress:
		current, _ := event.Data["current"].(float64)
		total, _ := event.Data["total"].(float64)
		label, _ := event.Data["message"].(string)
		cmd.progress = &liveProgress{current: current, total: total, label: label}
	case jobs.EventTypePhase:
		if action, _ := event.Data["action"].(string); action == "start" {
			if name, _ := event.Data["name"].(string); name != "" {
				cmd.phase = name
			}
		}
	case jobs.EventTypeSummary:
		if msg, _ := event.Data["message"].(string); msg != "" {
			cmd.summary = msg
		}
	case jobs.EventTypeLog:
		if r.serveMode {
			r.renderServeLog(job, event)
		}
	}
}

// JobComplete is called when a job finishes. It folds the result into the
// command (status, metrics, summary) and stops the owning project's active-work
// timer when this was its last running job.
func (r *LiveRenderer) JobComplete(job *jobs.ScheduledJob, result *jobs.JobResult) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.progressJobDone(job.Key())

	row := r.rowByID[job.Project.ID]
	if row == nil {
		return
	}
	cmd := row.commands[commandName(job)]
	if cmd == nil {
		return
	}

	status := result.Outcome()
	if result.Status == statusFailed {
		// A reused result that failed — a cache hit whose declared output
		// drifted from this checkout — is a failure of this run, whatever its
		// provenance says.
		status = statusFailed
	}
	if status == statusCanceled {
		// Canceled work was killed mid-run, not passed over. Reporting it as
		// aborted keeps it out of the skipped count, where it would read as
		// work the run legitimately decided not to do.
		status = statusAborted
	}

	now := time.Now()
	cmd.doneSteps++
	if status == statusCached {
		cmd.cachedSteps++
	}
	if !r.jobCacheable[job.Key()] {
		cmd.alwaysRunDone++
		cmd.alwaysRunDuration += result.Duration
	}
	if status == statusFailed {
		if sn := stepName(job.JobDef.Name); sn != "" {
			cmd.failedStep = sn
		}
		if cmd.firstDiag == "" {
			cmd.firstDiag = firstDiagnosticLine(result)
		}
	}
	if statusRank(status) > statusRank(cmd.stepWorst) {
		cmd.stepWorst = status
	}

	r.captureMetrics(cmd, result)

	// A command settles only once every pipeline step has completed; until then
	// it stays "running" so a finished early step doesn't mark the project done.
	cmd.progress = nil
	cmd.phase = ""
	if cmd.doneSteps >= cmd.totalSteps {
		cmd.status = cmd.stepWorst
		if cmd.status == "" {
			cmd.status = statusSuccess
		}
		cmd.displayCached = cmd.status == statusCached ||
			(cmd.status == statusSuccess &&
				cmd.cacheableSteps > 0 && cmd.cachedSteps == cmd.cacheableSteps &&
				cmd.alwaysRunSteps > 0 && cmd.alwaysRunDone == cmd.alwaysRunSteps &&
				cmd.alwaysRunDuration < time.Second)
		if cmd.displayCached && cmd.status != statusCached {
			label := cmd.alwaysRunStep
			if cmd.alwaysRunSteps != 1 || label == "" {
				label = fmt.Sprintf("%d always-run", cmd.alwaysRunSteps)
			}
			cmd.alwaysRunNote = fmt.Sprintf("(+%s %s)", label, formatSessionDuration(cmd.alwaysRunDuration))
		}
		cmd.endTime = now
	} else {
		cmd.status = statusRunning
	}
	r.finishProjectWork(row, now)
	r.retainDoneProject(row, now)

	if cmd.status == statusFailed && !cmd.failureRecorded {
		cmd.failureRecorded = true
		r.recordFailure(row, cmd)
	}
}

func (r *LiveRenderer) retainDoneProject(row *projectRow, now time.Time) {
	if r.projectStatus(row) != statusDone || !row.visible {
		return
	}
	row.doneVisibleUntil = now.Add(liveDoneRetention)
}

func (r *LiveRenderer) startProjectWork(row *projectRow, now time.Time) {
	if row.activeJobs == 0 {
		row.activeStart = now
	}
	row.activeJobs++
}

func (r *LiveRenderer) finishProjectWork(row *projectRow, now time.Time) {
	if row.activeJobs <= 0 {
		return
	}
	row.activeJobs--
	if row.activeJobs > 0 {
		return
	}
	if !row.activeStart.IsZero() && now.After(row.activeStart) {
		row.workDuration += now.Sub(row.activeStart)
	}
	row.activeStart = time.Time{}
}

func (r *LiveRenderer) finishAllProjectWork(row *projectRow, now time.Time) {
	if row.activeJobs <= 0 {
		return
	}
	if !row.activeStart.IsZero() && now.After(row.activeStart) {
		row.workDuration += now.Sub(row.activeStart)
	}
	row.activeJobs = 0
	row.activeStart = time.Time{}
}

// captureMetrics folds a result's metric, artifact, diagnostic, and summary
// events into the command for compact display and final aggregation.
func (r *LiveRenderer) captureMetrics(cmd *commandState, result *jobs.JobResult) {
	for _, ev := range result.Events {
		switch ev.Type {
		case jobs.EventTypeMetric:
			name, _ := ev.Data["name"].(string)
			val, ok := metricValue(ev.Data)
			if name == "" || !ok {
				continue
			}
			if unit, _ := ev.Data["unit"].(string); unit == "percent" {
				cmd.metrics[name] = val
			} else {
				cmd.metrics[name] += val
			}
		case jobs.EventTypeArtifact:
			cmd.artifacts++
		case jobs.EventTypeDiagnostic:
			switch sev, _ := ev.Data["severity"].(string); sev {
			case "error":
				cmd.diagErrors++
			case "warning", "warn":
				cmd.diagWarnings++
			}
		case jobs.EventTypeSummary:
			if msg, _ := ev.Data["message"].(string); msg != "" {
				cmd.summary = msg
			}
		}
	}

	// Typed per-verb payloads (protocol/runtime) from the result data.
	if ts, cs, _, err := runtimeproto.ExtractResultPayloads(result.Data); err == nil {
		if ts != nil {
			cmd.testSummary = ts
		}
		if cs != nil {
			cmd.coverageSummary = cs
		}
	}
}

// statusPrecedence orders step statuses from least to most severe, so a command
// can adopt the worst status any of its pipeline steps reached. The ORDER is the
// contract; statusRank returns a position in it.
//
// Reading it upwards: skipped work is the least eventful outcome; a cache hit
// outranks it because something was actually produced; a coalesced task
// outranks a hit because this run waited on a cold miss; a fresh success
// outranks both because the work ran here; aborted outranks success because a
// command whose later steps were killed did not finish, however well its earlier
// steps went; and a failure outranks everything.
//
// The entries are the canonical vocabulary (jobs.TaskStatus / jobs.ReuseKind
// outcomes) plus the live view's own "aborted", which is how it presents
// TaskStatusCanceled. Pinned by TestStatusRank_Precedence.
var statusPrecedence = []string{
	statusSkipped,
	statusCached,
	statusCoalesced,
	statusSuccess,
	statusAborted,
	statusFailed,
}

// statusRank scores a step status by its position in statusPrecedence. An
// unsettled status ("" or running) ranks below every terminal one.
func statusRank(s string) int {
	for i, candidate := range statusPrecedence {
		if candidate == s {
			return i + 1
		}
	}
	return 0
}

// projectSettled reports whether every command of a project is terminal.
func (r *LiveRenderer) projectSettled(row *projectRow) bool {
	for _, cmd := range row.commands {
		if !cmd.settled() {
			return false
		}
	}
	return true
}
