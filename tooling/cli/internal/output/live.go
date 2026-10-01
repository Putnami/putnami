package output

import (
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"go.putnami.dev/cli/model/jobs"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// projectColorPalette is the rotation of ANSI colors assigned to projects.
// Colors stay stable for a project across the whole run.
var projectColorPalette = []string{Cyan, Magenta, Yellow, Blue, Green, Red}

// Terminal statuses. These are NOT a vocabulary of the live view's own: they
// are the canonical jobs.TaskStatus a task ended with, or — when the result was
// reused — the jobs.ReuseKind outcome, which is exactly what
// jobs.JobResult.Outcome hands JobComplete. Deriving them keeps the live view
// from drifting into a private spelling of the same states.
const (
	statusSuccess   = string(jobs.TaskStatusSuccess)
	statusFailed    = string(jobs.TaskStatusFailed)
	statusSkipped   = string(jobs.TaskStatusSkipped)
	statusCanceled  = string(jobs.TaskStatusCanceled)
	statusCached    = jobs.JobOutcomeCached
	statusCoalesced = jobs.JobOutcomeCoalesced
)

// Display-only statuses. A row or a command can be in a state no single task
// verdict describes, so these have no canonical counterpart.
const (
	statusQueued  = "queued"
	statusBlocked = "blocked"
	statusRunning = "running"
	statusDone    = "done"
	// statusAborted is work the run never got to finish because the session was
	// cut short. It is how the live view presents jobs.TaskStatusCanceled, and
	// is deliberately distinct from statusSkipped: skipped work was decided
	// against (a dependency failed, the task did not apply), while aborted work
	// was killed mid-plan and has no outcome at all.
	statusAborted = "aborted"
)

// LiveRenderer renders a calm, project-centered live view: one stable row per
// project, redrawn in place. Each row shows the project's current status/phase,
// elapsed time, and compact per-task summaries in aligned columns. Detailed
// per-task output is reserved for verbose/debug mode (the TextRenderer).
type LiveRenderer struct {
	out     io.Writer
	errOut  io.Writer
	start   time.Time
	mu      sync.Mutex
	ticker  *time.Ticker
	stopCh  chan struct{}
	doneCh  chan struct{}
	stopped bool
	width   int
	height  int

	// serveMode tails log lines to scrollback above the live table.
	serveMode bool

	// Project rows in stable display order.
	rows         []*projectRow
	rowByID      map[string]*projectRow
	jobToProj    map[string]string // job key → project ID (for dependency mapping)
	jobCacheable map[string]bool   // job key → whether the resolved plan permits cache reuse

	// Layout (computed once per run in Start).
	nameWidth   int      // project-name column width
	statusWidth int      // status/phase column width
	colOrder    []string // summary command columns, in display order
	colWidth    map[string]int

	// summaryFit memoizes the per-row column fit. Its inputs (width, leading
	// width, colOrder, colWidth) are all run-fixed, so the fit is identical for
	// every row in every frame; caching it keeps renderSummary from re-running
	// the fit search per visible row on each redraw. summaryFitWidth records the
	// width the cache was computed for so a terminal resize recomputes it.
	summaryFit      *columnFit
	summaryFitWidth int

	// Differential redraw: the last painted live-zone lines and the running
	// spinner frame. Repainting only the lines that changed (see paint) is what
	// lets the redraw run fast enough to animate the spinner without flicker.
	prevFrame    []string
	spinnerFrame int

	// Global progress bar accumulator, weighted by each task's expected
	// wall-clock cost (the EMA the scheduler learns in .putnami/stats).
	taskProg    map[string]*taskProgress
	totalWeight float64

	// etaDeadline is the projected finish time. It only ever moves earlier, so
	// the displayed remaining time never jumps back up (no Windows-copy bounce).
	etaDeadline time.Time

	// failures is the rolling list of settled command failures shown in the
	// pinned failure pane at the bottom of the live zone.
	failures []liveFailure

	// coverageReplay is immutable display input loaded before the run. reset
	// intentionally preserves it across watch iterations; Start copies matching
	// entries onto that iteration's test commands.
	coverageReplay map[string]CoverageReplay
	// lifecycleMode keeps the table live while workspace installers run, then
	// removes it. The outer install command owns the durable action summary.
	lifecycleMode bool
}

// projectRow is one display line: a project and all of its commands.
type projectRow struct {
	id      string
	name    string
	color   string
	order   int
	visible bool

	commandOrder []string
	commands     map[string]*commandState

	deps map[string]struct{} // upstream project IDs this project waits on

	workDuration time.Duration
	activeStart  time.Time
	activeJobs   int

	doneVisibleUntil time.Time
	serveStatus      string // captured server URL in serve mode
}

// commandState tracks a single project+command (e.g. core-api:test), folding
// any pipeline steps into one logical task.
type commandState struct {
	name       string
	totalSteps int
	doneSteps  int
	startTime  time.Time
	endTime    time.Time

	status      string // "" until started, then statusRunning or a terminal status
	stepWorst   string // worst status seen across completed steps
	failedStep  string
	firstDiag   string // first error diagnostic captured on a failing step
	cachedSteps int
	// cacheableSteps lets the cell distinguish a genuinely fresh command from
	// one whose cacheable work was all restored before a small always-run tail.
	cacheableSteps    int
	alwaysRunSteps    int
	alwaysRunDone     int
	alwaysRunStep     string
	alwaysRunDuration time.Duration
	displayCached     bool
	alwaysRunNote     string

	failureRecorded bool // guards against double-adding to the failure pane

	phase    string // current sub-phase/step name while running
	progress *liveProgress
	summary  string // latest summary message

	metrics      map[string]float64
	artifacts    int
	diagErrors   int
	diagWarnings int

	// Typed per-verb payloads from the result event (protocol/runtime),
	// preferred over raw metrics during final aggregation.
	testSummary      *runtimeproto.TestSummary
	coverageSummary  *runtimeproto.CoverageSummary
	replayedCoverage *CoverageReplay
}

type liveProgress struct {
	current float64
	total   float64
	label   string
}

// settled reports whether the command has reached a terminal state.
func (c *commandState) settled() bool {
	switch c.status {
	case statusSuccess, statusFailed, statusSkipped, statusAborted, statusCached, statusCoalesced:
		return true
	default:
		return false
	}
}

// liveTally is the histogram both end-of-run summary blocks build. It is the
// single place a display status is mapped to a summary bucket, so the Projects
// block and the Tasks block cannot disagree about — say — whether a coalesced
// command passed. Each block used to carry its own switch.
//
// Each caller feeds it from one vocabulary: projectStatus yields done/failed/
// skipped/aborted/blocked, a settled command yields success/cached/coalesced/
// failed/skipped/aborted. The union below is a superset of both, so neither
// caller's counts change.
type liveTally struct {
	passed  int
	failed  int
	skipped int
	aborted int
	blocked int
}

func (t *liveTally) add(status string) {
	switch status {
	case statusDone, statusSuccess, statusCached, statusCoalesced:
		t.passed++
	case statusFailed:
		t.failed++
	case statusSkipped:
		t.skipped++
	case statusAborted:
		t.aborted++
	case statusBlocked:
		t.blocked++
	}
}

// NewLiveRenderer creates a live TUI renderer.
func NewLiveRenderer(out, errOut io.Writer) *LiveRenderer {
	w, h := termSize()
	r := &LiveRenderer{
		out:    out,
		errOut: errOut,
		width:  w,
		height: h,
	}
	r.reset()
	return r
}

// reset clears all per-run state so watch-mode iterations start fresh.
func (r *LiveRenderer) reset() {
	r.rows = nil
	r.rowByID = make(map[string]*projectRow)
	r.jobToProj = make(map[string]string)
	r.jobCacheable = make(map[string]bool)
	r.colOrder = nil
	r.colWidth = make(map[string]int)
	r.summaryFit = nil
	r.summaryFitWidth = 0
	r.prevFrame = nil
	r.spinnerFrame = 0
	r.taskProg = make(map[string]*taskProgress)
	r.totalWeight = 0
	r.etaDeadline = time.Time{}
	r.failures = nil
}

// sortedRows returns rows in stable display order.
func (r *LiveRenderer) sortedRows() []*projectRow {
	rows := make([]*projectRow, len(r.rows))
	copy(rows, r.rows)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].order < rows[j].order })
	return rows
}

// StderrIsTTY returns whether stderr is connected to a terminal.
func StderrIsTTY() bool {
	return isTTY(int(os.Stderr.Fd()))
}

// ShouldUseLiveRenderer returns true if the destination writer and environment
// support live rendering. With no writer it preserves the historical stderr
// check used by terminal-shaped callers.
func ShouldUseLiveRenderer(writers ...io.Writer) bool {
	destination := io.Writer(os.Stderr)
	if len(writers) > 0 {
		destination = writers[0]
	}
	fdWriter, ok := destination.(interface{ Fd() uintptr })
	if !ok || !isTTY(int(fdWriter.Fd())) {
		return false
	}
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("CI") != "" {
		return false
	}
	return true
}
